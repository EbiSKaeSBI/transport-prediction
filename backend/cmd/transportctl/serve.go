package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtpserver"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/telemetry"
)

// defaultJSONLPath — путь к JSONL-файлу наблюдений по умолчанию.
const defaultJSONLPath = "observations.jsonl"

type serveHandler struct {
	observer *telemetry.Observer
	logger   *slog.Logger
	verbose  bool
}

func (h *serveHandler) OnHandshake(unitID uint32, req ndtp.ConnRequest) {
	h.observer.OnHandshake(unitID, req)
}

func (h *serveHandler) OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell) {
	h.observer.OnRealtime(unitID, frame, cells)
	if !h.verbose {
		return
	}
	observation, ok := h.observer.Latest(unitID)
	if !ok {
		return
	}
	h.logger.Info("телеметрия",
		"unit", unitID,
		"event_time", observation.EventTime.Format(time.RFC3339),
		"lat", observation.Latitude,
		"lon", observation.Longitude,
		"speed", observation.Speed,
		"valid", observation.LocationValid,
		"odometer", observation.TotalKm)
}

func (h *serveHandler) OnMalformed(unitID uint32, frame ndtp.Frame, err error) {
	h.observer.OnMalformed(unitID, frame, err)
}

func (h *serveHandler) OnDisconnect(unitID uint32) {
	h.observer.OnDisconnect(unitID)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", ":9201", "адрес прослушивания NDTP")
	jsonl := fs.Bool("jsonl", false, "писать наблюдения в JSONL-файл")
	jsonlOut := fs.String("jsonl-out", defaultJSONLPath, "путь к JSONL-файлу")
	verbose := fs.Bool("verbose", false, "логировать каждое наблюдение")
	quiet := fs.Bool("quiet", false, "только ошибки")
	statsEvery := fs.Duration("stats", 30*time.Second, "как часто печатать сводку (0 — не печатать)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var jsonlFile *os.File
	var output *os.File
	if *jsonl {
		file, err := os.Create(*jsonlOut)
		if err != nil {
			return fmt.Errorf("не удалось создать %s: %w", *jsonlOut, err)
		}
		jsonlFile = file
		defer jsonlFile.Close()
		output = jsonlFile
	}

	observer := telemetry.New(output, telemetry.WithLogger(logger))
	handler := &serveHandler{observer: observer, logger: logger, verbose: *verbose}
	server := &ndtpserver.Server{
		Addr:    *listen,
		Handler: handler,
		Logger:  logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("не удалось слушать %s: %w", *listen, err)
	}
	logger.Info("NDTP-сервер запущен", "addr", listener.Addr().String(),
		"pid", os.Getpid(), "jsonl", *jsonl, "jsonl_out", *jsonlOut)

	if *statsEvery > 0 {
		go reportStats(ctx, listener.Addr().String(), observer, server, logger, *statsEvery)
	}

	if err := server.Serve(listener, ctx); err != nil {
		return err
	}
	logger.Info("NDTP-сервер остановлен", "stats", observer.Stats())
	return nil
}

func reportStats(ctx context.Context, addr string, observer *telemetry.Observer,
	server *ndtpserver.Server, logger *slog.Logger, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := observer.Stats()
			metrics := server.Metrics.Snapshot()
			units := observer.Units()
			sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })
			logger.Info("сводка",
				"addr", addr,
				"устройств", len(units),
				"пакетов", stats.Packets,
				"наблюдений", stats.Observations,
				"без_позиции", stats.NoPosition,
				"битых", stats.Malformed,
				"устаревших", stats.Stale,
				"соединений", metrics.Connections,
				"активных", metrics.ActiveConns,
				"crc_ошибок", metrics.CRCErrors,
				"ошибок_декодирования", metrics.DecodeErrors,
				"ячеек", metrics.CellsDecoded,
				"байт", metrics.BytesRead)
		}
	}
}
