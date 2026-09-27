package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/gateway"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtpserver"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/pipeline"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/scheduler"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/telemetry"
)

// defaultJSONLPath — путь к JSONL-файлу наблюдений по умолчанию.
const defaultJSONLPath = "observations.jsonl"

// frameLogger — приёмник кадров, который пишет их в журнал и передаёт
// дальше.
//
// Внутренний приёмник не обязателен: в --dry-run кадры только пишутся, и
// прогноз не должен выглядеть работающим, пока его некому предъявить. С
// моделью и гейтвеем inner обязателен, и тогда журнал перестаёт быть
// единственным, кто видел кадр.
type frameLogger struct {
	inner   pipeline.Sink
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
	if f.inner != nil {
		f.inner.Submit(frame)
	}
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
	planWatchEvery := fs.Duration("plan-watch", time.Second,
		"как часто перечитывать --plan/--binding после старта (0 — не следить)")
	captureOut := fs.String("capture-out", "",
		"каталог записи кадров принимаемого потока (пусто — не писать)")
	tickEvery := fs.Duration("tick", 15*time.Second, "как часто проверять наступление границы ячейки")
	gridEvery := fs.Duration("grid", pipeline.DefaultGrid, "шаг сетки моментов прогноза")
	dryRun := fs.Bool("dry-run", false, "не строить прогноз: только принимать и хранить телеметрию")
	httpAddr := fs.String("http", ":8080", "адрес HTTP-гейтвея (пусто — не поднимать)")
	mlURL := fs.String("ml", "", "адрес сервиса модели (пусто — только baseline)")
	mlTimeout := fs.Duration("ml-timeout", 0, "бюджет одной попытки модели (0 — умолчание)")
	queueSize := fs.Int("queue", scheduler.DefaultQueue, "размер очереди прогнозов")
	batchSize := fs.Int("batch", scheduler.DefaultBatch, "сколько кадров уходит в модель одним запросом (1 — поштучно)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var jsonlFile *os.File
	// Тип — io.Writer, а не *os.File: nil в интерфейсе обязан быть
	// настоящим nil. Иначе Observer получает ненулевой интерфейс с nil
	// указателем внутри, его проверка out == nil не срабатывает, и на
	// каждое наблюдение пишется WARN «invalid argument» (GitLab #36,
	// замечание к этапу 6).
	var output io.Writer
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

	// holder заполняется ниже, когда план загружен; nil означает «плана нет»,
	// и конвейер с гейтвеем тогда только копят телеметрию.
	var holder *schedule.Holder

	// Без расписания прогнозировать нечего, но принимать телеметрию всё равно
	// нужно: так сервер можно поднять на площадке до того, как туда доедут
	// планы. Поэтому отсутствие файлов — не ошибка запуска, а режим
	// накопления данных, о котором честно пишем в журнал.
	cfg := pipeline.Config{
		Store:    store,
		Observer: observer,
		Logger:   logger,
		Grid:     *gridEvery,
	}
	if *gridEvery <= 0 {
		return fmt.Errorf("--grid должен быть положительным, иначе моменты прогноза не сетятся")
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
		// План и привязка едут в holder, а не в копию конфига: файл плана
		// переписывается на ходу, и конвейер с гейтвеем обязаны увидеть новый
		// без перезапуска процесса.
		holder = schedule.NewHolder(plan, binding)
		cfg.Holder = holder
		logger.Info("расписание загружено",
			"план", *planPath, "привязка", *bindingPath,
			"остановок", plan.StopsCount(), "единиц", binding.Len(),
			"слежение", *planWatchEvery > 0)
	}

	pred, ml := predictorChain(*mlURL, *mlTimeout, logger)
	service, err := buildService(serviceOptions{
		Pipeline:  cfg,
		Predictor: pred,
		ML:        ml,
		Queue:     *queueSize,
		Batch:     *batchSize,
		HTTPAddr:  *httpAddr,
		DryRun:    *dryRun,
		Verbose:   *verbose,
		Logger:    logger,
	})
	if err != nil {
		return err
	}
	pipe, sched, gw, hub := service.Pipeline, service.Scheduler, service.Gateway, service.Hub

	server := &ndtpserver.Server{
		Addr:    *listen,
		Handler: pipe,
		Logger:  logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Запись кадров идёт отводом от того же входа, который ест конвейер:
	// план-график должен строиться ровно по этому потоку, а второй
	// NDTP-слушатель на :9201 не встал бы — порт уже занят сервером.
	if *captureOut != "" {
		rec, closeCapture, err := newCaptureHandler(*captureOut, 0, nil)
		if err != nil {
			return fmt.Errorf("запись кадров: %w", err)
		}
		// хвост буфера — на диск до закрытия файлов, иначе последние секунды
		// записи потерялись бы именно тем, ради чего запись нужна
		defer func() {
			rec.Flush()
			closeCapture()
		}()
		server.Handler = ndtpserver.NewFanout(pipe, rec)
		logger.Info("ведётся запись кадров для генерации плана", "каталог", *captureOut)
	}

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("не удалось слушать %s: %w", *listen, err)
	}
	logger.Info("NDTP-сервер запущен", "addr", listener.Addr().String(),
		"pid", os.Getpid(), "jsonl", *jsonl, "jsonl_out", *jsonlOut,
		"tick", *tickEvery, "dry_run", *dryRun)

	if !*dryRun && *tickEvery > 0 {
		go pipe.Run(ctx, *tickEvery)
		go sched.Run(ctx)
		go hub.Run(ctx)
	}

	// Наблюдение за файлом плана включается только когда есть и слежение, и
	// сам план: в режиме накопления перечитывать нечего.
	if holder != nil && *planWatchEvery > 0 {
		watcher := newPlanWatcher(*planPath, *bindingPath, holder, pipe, logger,
			*planWatchEvery)
		go watcher.Run(ctx)
	}

	if *httpAddr != "" && gw != nil {
		go func() {
			if err := gw.ListenAndServe(ctx); err != nil {
				logger.Error("HTTP-гейтвей остановился", "err", err)
			}
		}()
	}

	if *statsEvery > 0 {
		go reportStats(ctx, listener.Addr().String(), observer, server, pipe, sched,
			logger, *statsEvery)
	}

	if err := server.Serve(listener, ctx); err != nil {
		return err
	}
	logger.Info("NDTP-сервер остановлен", "stats", observer.Stats(), "прогноз", pipe.Stats())
	return nil
}

// serviceOptions — вход сборки сервиса.
type serviceOptions struct {
	// Pipeline — конфигурация конвейера без приёмника кадров: его подставляет
	// сборка, потому что кадры уходят в планировщик, а не в журнал.
	Pipeline  pipeline.Config
	Predictor predictor.Predictor
	// ML — клиент модели для показателей готовности; nil, когда модель не
	// настроена: тогда /readyz и /metrics просто не показывают её состояние.
	ML       *predictor.MLClient
	Queue    int
	Batch    int
	HTTPAddr string
	DryRun   bool
	Verbose  bool
	Logger   *slog.Logger
}

// service — собранный сервис: конвейер, планировщик, гейтвей и лента.
type service struct {
	Pipeline  *pipeline.Pipeline
	Scheduler *scheduler.Scheduler
	Gateway   *gateway.Server
	Hub       *gateway.Hub
}

// buildService собирает конвейер, планировщик прогнозов, гейтвей и ленту
// событий.
//
// Отдельная функция, а не код внутри runServe, потому что связка между ними —
// место, где легче всего тихо потерять звено: кадры уйдут в никуда, прогнозов
// не будет, а процесс будет работать и показывать зелёное ready. Сборка
// проверяется тестом целиком, включая HTTP-ответ гейтвея.
func buildService(opts serviceOptions) (*service, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Лента получает метрики от гейтвея, а гейтвей получает ленту в
	// конструктор, поэтому метрики читаются через замыкание по переменной:
	// к моменту первого тика ленты гейтвей уже собран.
	var gw *gateway.Server
	hub := gateway.NewHub(gateway.HubConfig{
		MetricsProvider: func() any {
			if gw == nil {
				return nil
			}
			return gw.Snapshot()
		},
	})

	sched := scheduler.New(scheduler.Config{
		Predictor: opts.Predictor,
		Queue:     opts.Queue,
		Batch:     opts.Batch,
		Observer: func(p predictor.Prediction) {
			gw.Observe(p)
		},
		Logger: logger,
	})
	if !opts.DryRun {
		gw = gateway.New(gateway.Config{
			Addr:      opts.HTTPAddr,
			Store:     opts.Pipeline.Store,
			Schedule:  opts.Pipeline.Schedule,
			Binding:   opts.Pipeline.Binding,
			Holder:    opts.Pipeline.Holder,
			Predictor: opts.Predictor,
			ML:        opts.ML,
			Hub:       hub,
			// Метрики очереди и инференции читаются через замыкания: к
			// моменту чтения планировщик уже собран, а гейтвей не должен
			// знать, кто перед ним стоит.
			Queue: func() gateway.QueueStats {
				st := sched.Stats()
				return gateway.QueueStats{
					Depth:         sched.QueueLen(),
					Submitted:     st.Submitted,
					Predicted:     st.Predicted,
					Dropped:       st.Dropped,
					Abandoned:     st.Abandoned,
					Batches:       st.Batches,
					BatchedFrames: st.BatchedFrames,
					BatchSize:     sched.BatchSizeQuantiles(),
				}
			},
			Inference: inferenceWindow(opts.Predictor),
			Logger:    logger,
		})
	}

	// В --dry-run кадры только пишутся в журнал: предъявлять их некуда, и
	// выдавать пустую очередь за работающий прогноз нельзя.
	var sink pipeline.Sink = &frameLogger{logger: logger, verbose: opts.Verbose}
	if gw != nil {
		sink = &frameLogger{inner: sched, logger: logger, verbose: opts.Verbose}
	}
	cfg := opts.Pipeline
	cfg.Sink = sink

	pipe, err := pipeline.New(cfg)
	if err != nil {
		return nil, err
	}
	return &service{Pipeline: pipe, Scheduler: sched, Gateway: gw, Hub: hub}, nil
}

// predictorChain собирает цепочку прогноза: модель, а под ней baseline.
//
// Модели может не быть — тогда baseline отвечает всегда, и это штатный
// режим, а не поломка: сервер на площадке поднимают раньше, чем появится
// сервис модели. Отдельная функция, а не код внутри runServe, потому что
// состав цепочки — это контракт с ML-разработчиком, и он обязан быть
// проверяемым тестом, а не чтением флага.
func predictorChain(mlURL string, timeout time.Duration,
	logger *slog.Logger) (predictor.Predictor, *predictor.MLClient) {
	if mlURL == "" {
		logger.Info("модель не задана: прогноз отдаёт baseline, то есть cur_dev_s без добавки",
			"подсказка", "перезапустите с --ml http://адрес:порт, когда сервис модели будет доступен")
		return predictor.BaselinePredictor{}, nil
	}
	logger.Info("модель подключена", "адрес", mlURL, "таймаут", timeout)
	ml := predictor.NewMLClient(predictor.MLConfig{BaseURL: mlURL, Timeout: timeout})
	// Контракт признаков сверяется ещё при старте: молчаливый откат всех
	// прогнозов в baseline при рассинхроне с моделью выглядит как «всё
	// работает», а прогноз — нет (ADR-0002).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ml.CheckContract(ctx); err != nil {
		logger.Error("контракт признаков модели не совпал с кадром: прогнозы пойдут по baseline",
			"ошибка", err.Error(),
			"подсказка", "сравните GET /model/info сервиса модели с features/v1.yaml")
	} else {
		logger.Info("контракт модели совпал с кадром", "версия", ml.Stats().Version)
	}
	return predictor.NewFallback(ml, predictor.BaselinePredictor{}, 0), ml
}

// inferenceWindow достаёт окно замеров у настроенной цепочки прогноза.
// Отсутствие окна — не поломка: цепочка без модели (baseline) замерять
// инференцию не может, и метрики просто не публикуются.
func inferenceWindow(p predictor.Predictor) func() latency.Quantiles {
	if w, ok := p.(predictor.InferenceWindow); ok {
		return w.Inference
	}
	return nil
}

func reportStats(ctx context.Context, addr string, observer *telemetry.Observer,
	server *ndtpserver.Server, pipe *pipeline.Pipeline, sched *scheduler.Scheduler,
	logger *slog.Logger, every time.Duration) {
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
			queue := sched.Stats()
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
				"без_окна", forecast.NoSchedule,
				"без_данных_на_T", forecast.NoStateAtT,
				// Отказы планировщика — единственная причина, по которой кадры
				// могут перестать строиться при живых привязке и окне расписания.
				// Без этого счётчика в сводке такой отказ выглядит как зависший
				// конвейер: все видимые числа стоят, в логе нет ни ошибки, ни
				// предупреждения, потому что сам отказ пишется на уровне debug.
				"отказов", forecast.Refused,
				"в_очереди", queue.BySource,
				"ждёт_модели", sched.QueueLen(),
				"предсказано", queue.Predicted,
				"отброшено", queue.Dropped,
				"батчей", queue.Batches,
				// Медиана размера пачки показывает, батчинг ли вообще
				// работает: при батче по одному кадру медиана равна единице,
				// и это читается сразу, без сравнения счётчиков.
				"кадров_в_батче", sched.BatchSizeQuantiles().P50)
		}
	}
}
