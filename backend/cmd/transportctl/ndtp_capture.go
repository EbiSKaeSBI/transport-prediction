package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	// lastFlush — когда в буфер последний раз сбрасывали на диск. Запись
	// читает генератор плана, не дожидаясь остановки сервера, поэтому хвост
	// буфера обязан попадать в файл сам, а не в момент закрытия.
	lastFlush time.Time
	closers   []io.Closer
}

// captureFlushEvery — как часто сбрасывать буфер записи на диск. Секунда с
// запасом: план строится по последней позиции машины, и секунда давности на
// метке времени не влияет на выбор остановки, а вот недописанный кадр в файл
// попасть не должен.
const captureFlushEvery = time.Second

// newCaptureHandler открывает каталог записи и возвращает готовый обработчик
// вместе с func для закрытия файлов. Живёт отдельно от runCapture, потому что
// ту же запись ведёт и сервер: план должен строиться ровно по тому потоку,
// который уже ест конвейер, а не по второму подключению к источнику.
func newCaptureHandler(out string, limit int,
	cancel context.CancelFunc,
) (*captureHandler, func(), error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, nil, fmt.Errorf("не удалось создать %s: %w", out, err)
	}
	binFile, err := os.Create(filepath.Join(out, "packets.bin"))
	if err != nil {
		return nil, nil, err
	}
	metaFile, err := os.Create(filepath.Join(out, "packets.jsonl"))
	if err != nil {
		binFile.Close()
		return nil, nil, err
	}
	closeAll := func() {
		binFile.Close()
		metaFile.Close()
	}
	h := &captureHandler{
		bin:       bufio.NewWriter(binFile),
		meta:      bufio.NewWriter(metaFile),
		units:     map[uint32]int{},
		byType:    map[string]int{},
		limit:     limit,
		cancel:    cancel,
		lastFlush: time.Now(),
		closers:   []io.Closer{binFile, metaFile},
	}
	return h, closeAll, nil
}

// flushIfDue сбрасывает буферы, если с прошлого раза прошла секунда. Под
// мьютексом: буферы пишутся из обработчиков соединений, и flush без блокировки
// гонялся бы с записью.
func (h *captureHandler) flushIfDue() {
	now := time.Now()
	if now.Sub(h.lastFlush) < captureFlushEvery {
		return
	}
	h.lastFlush = now
	h.bin.Flush()
	h.meta.Flush()
}

func (h *captureHandler) OnHandshake(unitID uint32, req ndtp.ConnRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.units[unitID]++
}

func (h *captureHandler) OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell) {
	// Мьютекс берётся до чтения h.seq и записи h.byType: сокетов на каждый
	// unitId свой, и без блокировки несколько юнитов роняют процесс
	// «concurrent map writes» прямо в подсчёте ячеек.
	h.mu.Lock()
	defer h.mu.Unlock()
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

	h.bin.Write(frame.Raw)
	json.NewEncoder(h.meta).Encode(record)
	h.seq++
	h.flushIfDue()
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

// Flush сбрасывает буферы записи на диск.
func (h *captureHandler) Flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bin.Flush()
	h.meta.Flush()
}

// snapshot отдаёт сводку записи для печати. Под мьютексом, иначе счётчики
// читаются посреди записи и в отчёте бывают отрицательные дельты.
func (h *captureHandler) snapshot() (int, int, map[uint32]int, map[string]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	units := make(map[uint32]int, len(h.units))
	for unit, count := range h.units {
		units[unit] = count
	}
	byType := make(map[string]int, len(h.byType))
	for name, count := range h.byType {
		byType[name] = count
	}
	return h.seq, h.malformed, units, byType
}

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler, closeAll, err := newCaptureHandler(*out, *limit, cancel)
	if err != nil {
		return err
	}
	defer closeAll()
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
	seq, malformed, units, byType := handler.snapshot()

	fmt.Printf("пакетов: %d\n", seq)
	fmt.Printf("не разобрано: %d\n", malformed)
	fmt.Printf("устройств: %d\n", len(units))
	for unit, count := range units {
		fmt.Printf("  unit %d: %d handshake\n", unit, count)
	}
	fmt.Println("типы ячеек:")
	for name, count := range byType {
		fmt.Printf("  %-20s %d\n", name, count)
	}
	fmt.Printf("файлы: %s/packets.bin, %s/packets.jsonl\n", *out, *out)
	return serveErr
}
