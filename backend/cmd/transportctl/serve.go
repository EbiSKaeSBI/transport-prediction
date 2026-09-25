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

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtpserver"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/pipeline"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/telemetry"
)

// defaultJSONLPath — путь к JSONL-файлу наблюдений по умолчанию.
const defaultJSONLPath = "observations.jsonl"

// frameLogger — заглушка приёмника кадров до появления клиента модели. Кадры
// считаются и логируются, но никуда не отправляются, чтобы прогноз не
// выглядел работающим, пока его некому предъявить.
type frameLogger struct {
	logger  *slog.Logger
	verbose bool
}

func (f *frameLogger) Submit(frame horizon.Frame) {
	level := slog.LevelDebug
	if f.verbose {
		level = slog.LevelInfo
	}
	f.logger.Log(context.Background(), level, "кадр прогноза",
		"sample_id", frame.SampleID,
		"unit_id", frame.UnitID,
		"tr_id", frame.TRID,
		"target", frame.PrimaryStopID(),
		"ambiguous", frame.Ambiguous,
		"horizon_s", frame.HorizonS(),
		"признаков", len(frame.Values))
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", ":9201", "адрес прослушивания NDTP")
	jsonl := fs.Bool("jsonl", false, "писать наблюдения в JSONL-файл")
	jsonlOut := fs.String("jsonl-out", defaultJSONLPath, "путь к JSONL-файлу")
	verbose := fs.Bool("verbose", false, "логировать каждое наблюдение и каждый кадр")
	quiet := fs.Bool("quiet", false, "только ошибки")
	statsEvery := fs.Duration("stats", 30*time.Second, "как часто печатать сводку (0 — не печатать)")
	planPath := fs.String("plan", "", "CSV планового графика (обязателен для прогноза)")
	bindingPath := fs.String("binding", "", "CSV соответствия tr_id и unit_id (обязателен)")
	tickEvery := fs.Duration("tick", 15*time.Second, "как часто строить прогноз")
	dryRun := fs.Bool("dry-run", false, "не строить прогноз: только принимать и хранить телеметрию")
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
	store := statestore.New()

	// Без расписания прогнозировать нечего, но принимать телеметрию всё равно
	// нужно: так сервер можно поднять на площадке до того, как туда доедут
	// планы. Поэтому отсутствие файлов — не ошибка запуска, а режим
	// накопления данных, о котором честно пишем в журнал.
	cfg := pipeline.Config{
		Store:    store,
		Observer: observer,
		Logger:   logger,
		Sink:     &frameLogger{logger: logger, verbose: *verbose},
	}
	if *dryRun {
		logger.Info("режим --dry-run: телеметрия принимается и копится, прогноз не строится",
			"подсказка", "перезапустите с --plan и --binding, когда появится расписание")
	} else {
		if *planPath == "" || *bindingPath == "" {
			return fmt.Errorf("нужны --plan и --binding (или --dry-run, чтобы только копить телеметрию)")
		}
		plan, err := schedule.LoadFile(*planPath)
		if err != nil {
			return fmt.Errorf("план %s: %w", *planPath, err)
		}
		binding, err := schedule.LoadBindingFile(*bindingPath)
		if err != nil {
			return fmt.Errorf("привязка %s: %w", *bindingPath, err)
		}
		cfg.Schedule = plan
		cfg.Binding = binding
		logger.Info("расписание загружено",
			"план", *planPath, "привязка", *bindingPath,
			"остановок", plan.StopsCount(), "единиц", binding.Len())
	}

	pipe, err := pipeline.New(cfg)
	if err != nil {
		return err
	}

	server := &ndtpserver.Server{
		Addr:    *listen,
		Handler: pipe,
		Logger:  logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("не удалось слушать %s: %w", *listen, err)
	}
	logger.Info("NDTP-сервер запущен", "addr", listener.Addr().String(),
		"pid", os.Getpid(), "jsonl", *jsonl, "jsonl_out", *jsonlOut,
		"tick", *tickEvery, "dry_run", *dryRun)

	if !*dryRun && *tickEvery > 0 {
		go pipe.Run(ctx, *tickEvery)
	}

	if *statsEvery > 0 {
		go reportStats(ctx, listener.Addr().String(), observer, server, pipe, logger, *statsEvery)
	}

	if err := server.Serve(listener, ctx); err != nil {
		return err
	}
	logger.Info("NDTP-сервер остановлен", "stats", observer.Stats(), "прогноз", pipe.Stats())
	return nil
}

func reportStats(ctx context.Context, addr string, observer *telemetry.Observer,
	server *ndtpserver.Server, pipe *pipeline.Pipeline, logger *slog.Logger, every time.Duration) {
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
			forecast := pipe.Stats()
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
				"байт", metrics.BytesRead,
				"кадров", forecast.Frames,
				"повторов", forecast.Deduped,
				"без_привязки", forecast.NoBinding,
				"без_окна", forecast.NoSchedule)
		}
	}
}
