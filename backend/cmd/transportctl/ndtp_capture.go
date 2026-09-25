package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtpserver"
)

type cellRecord struct {
	Type    string `json:"type"`
	Number  uint8  `json:"number"`
	Size    int    `json:"size"`
	Decoded any    `json:"decoded"`
}

type frameRecord struct {
	Seq       int          `json:"seq"`
	UnitID    uint32       `json:"unit_id"`
	ServiceID uint16       `json:"service_id"`
	Type      uint16       `json:"type"`
	Flags     uint16       `json:"flags"`
	RawLen    int          `json:"raw_len"`
	Cells     []cellRecord `json:"cells"`
}

type captureHandler struct {
	mu        sync.Mutex
	seq       int
	malformed int
	bin       *bufio.Writer
	meta      *bufio.Writer
	units     map[uint32]int
	byType    map[string]int
	limit     int
	cancel    context.CancelFunc
}

func (h *captureHandler) OnHandshake(unitID uint32, req ndtp.ConnRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.units[unitID]++
}

func (h *captureHandler) OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell) {
	record := frameRecord{
		Seq:       h.seq,
		UnitID:    unitID,
		ServiceID: frame.NPH.ServiceID,
		Type:      frame.NPH.Type,
		Flags:     frame.NPH.Flags,
		RawLen:    len(frame.Raw),
	}
	for _, cell := range cells {
		name := cell.Type().String()
		h.byType[name]++
		record.Cells = append(record.Cells, cellRecord{
			Type:    name,
			Number:  cell.Number,
			Size:    cell.PayloadCellSize(),
			Decoded: cell.Payload,
		})
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.bin.Write(frame.Raw)
	json.NewEncoder(h.meta).Encode(record)
	h.seq++
	if h.limit > 0 && h.seq >= h.limit {
		h.bin.Flush()
		h.meta.Flush()
		h.cancel()
	}
}

func (h *captureHandler) OnMalformed(unitID uint32, frame ndtp.Frame, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bin.Write(frame.Raw)
	json.NewEncoder(h.meta).Encode(map[string]any{
		"seq":       h.seq,
		"unit_id":   unitID,
		"raw_len":   len(frame.Raw),
		"malformed": true,
		"error":     err.Error(),
	})
	h.seq++
	h.malformed++
	if h.limit > 0 && h.seq >= h.limit {
		h.bin.Flush()
		h.meta.Flush()
		h.cancel()
	}
}

func (h *captureHandler) OnDisconnect(unitID uint32) {}

func runCapture(args []string) error {
	fs := flag.NewFlagSet("ndtp-capture", flag.ContinueOnError)
	listen := fs.String("listen", ":9201", "адрес прослушивания NDTP")
	out := fs.String("out", filepath.Join("internal", "ndtp", "testdata", "golden"), "каталог для golden-файлов")
	duration := fs.Duration("duration", 10*time.Second, "сколько собирать пакеты")
	limit := fs.Int("limit", 0, "остановиться после N пакетов (0 — не ограничивать)")
	quiet := fs.Bool("quiet", false, "только итоговая сводка")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *quiet {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fmt.Errorf("не удалось создать %s: %w", *out, err)
	}
	binFile, err := os.Create(filepath.Join(*out, "packets.bin"))
	if err != nil {
		return err
	}
	defer binFile.Close()
	metaFile, err := os.Create(filepath.Join(*out, "packets.jsonl"))
	if err != nil {
		return err
	}
	defer metaFile.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := &captureHandler{
		bin:    bufio.NewWriter(binFile),
		meta:   bufio.NewWriter(metaFile),
		units:  map[uint32]int{},
		byType: map[string]int{},
		limit:  *limit,
		cancel: cancel,
	}
	server := &ndtpserver.Server{
		Addr:    *listen,
		Handler: handler,
		Logger:  logger,
	}

	timer := time.NewTimer(*duration)
	defer timer.Stop()
	go func() {
		select {
		case <-timer.C:
			cancel()
		case <-ctx.Done():
		}
	}()

	serveErr := server.ListenAndServe(ctx)

	handler.bin.Flush()
	handler.meta.Flush()

	fmt.Printf("пакетов: %d\n", handler.seq)
	fmt.Printf("не разобрано: %d\n", handler.malformed)
	fmt.Printf("устройств: %d\n", len(handler.units))
	for unit, count := range handler.units {
		fmt.Printf("  unit %d: %d handshake\n", unit, count)
	}
	fmt.Println("типы ячеек:")
	for name, count := range handler.byType {
		fmt.Printf("  %-20s %d\n", name, count)
	}
	fmt.Printf("файлы: %s/packets.bin, %s/packets.jsonl\n", *out, *out)
	return serveErr
}
