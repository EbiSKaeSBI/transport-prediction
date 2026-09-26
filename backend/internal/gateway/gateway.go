// Package gateway отдаёт состояние транспорта наружу: по HTTP для
// приборной панели и по WebSocket для живого обновления.
//
// Что пакет принципиально не делает. Он не строит кадры и не считает признаки
// — это дело конвейера и пакета horizon. Гейтвей берёт уже посчитанное и
// отвечает на вопросы, которые задаёт человек: где машина, что с ней будет
// и что делать. Всё состояние живёт в памяти процесса, и это записано в
// ADR 0007 вместе с ценой: перезапуск обнуляет инциденты и историю
// прогнозов. Плата выбрана сознательно — внешнее хранилище в гейтвее означало
// бы, что приборная панель перестаёт работать вместе с базой, а она должна
// показывать текущую ситуацию всегда, даже когда всё остальное упало.
//
// Три наблюдения, которые определили устройство пакета.
//
// Первое: снимка всей системы не существует. Хранилище телеметрии шардировано,
// и снимок, собранный «сверху», увидел бы часть шардов уже обновлёнными, а
// часть — нет. Поэтому карточка машины честно помечает себя снимком на
// момент чтения каждого шарда, и гейтвей не берёт глобальную блокировку
// хранилища ради читателя: заблокированное чтение остановило бы приём
// телеметрии на всё время, пока человек открывает страницу.
//
// Второе: ответ обязан называть источник. Прогноз модели, устаревший
// прогноз модели и baseline — это три разных утверждения с разной надёжностью,
// и отдать их одним числом значит соврать. Поле source есть в каждом ответе.
//
// Третье: деградация модели обязана быть видна в метриках, а не только в
// логах. Доля прогнозов, взятых не из модели, растёт именно тогда, когда
// модель молчит, и именно это интересует дежурного в первую очередь.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

// Config — зависимости гейтвея.
type Config struct {
	// Addr — адрес прослушивания, например ":8080".
	Addr string
	// Store — накопитель телеметрии. Обязателен: без него карточка машины
	// пуста.
	Store *statestore.Store
	// Schedule — план-график для маршрутов и остановок.
	Schedule *schedule.Schedule
	// Holder — тот же план под перепривязку на ходу. Задаётся вместо
	// Schedule, когда гейтвей должен видеть новый план без перезапуска.
	Holder *schedule.Holder
	// Binding — соответствие unit_id и tr_id.
	Binding *schedule.Binding
	// Predictor — цепочка прогнозов. При nil ответы не считаются, и
	// /readyz объясняет почему.
	Predictor predictor.Predictor
	// ML — клиент модели сверх цепочки: из его Stats() /readyz и /metrics
	// показывают расхождение контракта и состояние автомата. При nil
	// (режим baseline без модели) проверка модели не выставляется.
	ML *predictor.MLClient
	// IncidentCap, PredictionCap, LatencyWindow — вместимости хранилищ.
	// Неположительные значения берутся умолчательными.
	IncidentCap   int
	PredictionCap int
	LatencyWindow int
	// Queue — состояние очереди прогнозов для /metrics. При nil метрики
	// очереди не публикуются, и это штатно: гейтвей умеет работать без
	// планировщика, например в тестах карточки.
	//
	// Функция, а не интерфейс с типом планировщика: гейтвей не должен знать
	// про пакет scheduler, иначе зависимость потянется в обе стороны, а
	// сборка сервиса — единственное место, где обе половины встречаются.
	Queue func() QueueStats
	// Inference — окно замеров обращения к модели для /metrics. При nil
	// метрики инференции не публикуются. Отдельно от Queue, потому что
	// измеряет другое: очередь — сколько ждём, инференция — сколько жмёт
	// сама модель, и разница между ними и есть ответ на вопрос «мы медленные
	// или модель».
	Inference func() latency.Quantiles
	// Now подменяет часы в тестах. При nil time.Now.
	Now func() time.Time
	// Hub — лента событий. При nil гейтвей работает без WebSocket:
	// /ws/stream отвечает закрытием сразу, а остальные эндпоинты не
	// страдают. Отсутствие ленты не повод не поднимать сервер.
	//
	// Ленту нужно ещё запустить: Hub.Run(ctx) шлёт метрики по таймеру и
	// закрывает подписчиков при остановке. Запуск остаётся на вызывающем,
	// потому что конструктор, который сам поднимает горутину, не может её
	// потом остановить.
	Hub *Hub
	// Logger при nil берётся slog.Default.
	Logger *slog.Logger
}

// Server — HTTP-сервер гейтвея.
type Server struct {
	cfg       Config
	mux       *http.ServeMux
	incidents *Incidents
	preds     *Predictions
	latency   *latency.Window
	log       *slog.Logger
	now       func() time.Time
	startedAt time.Time
	// http — сервер для управляемой остановки. Поле нужно, чтобы Shutdown
	// дождался текущих запросов, а не обрушил их.
	http *http.Server
}

// New собирает гейтвей. Отсутствующие зависимости не делают его сломанным:
// отсутствие расписания означает, что маршруты не отдаются, отсутствие
// предиктора — что прогнозы не считаются. Оба случая видны в /readyz, и
// оба оставляют рабочими /healthz, /metrics и телеметрию.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		incidents: NewIncidents(cfg.IncidentCap),
		preds:     NewPredictions(cfg.PredictionCap),
		latency:   latency.New(cfg.LatencyWindow),
		log:       cfg.Logger,
		now:       cfg.Now,
		startedAt: cfg.Now(),
	}
	// Часы хранилищ те же, что у сервера: иначе тест с подменённым Now
	// получил бы инциденты с одним временем и метрики с другим.
	s.incidents.now = cfg.Now
	// Поиск соседней остановки инцидента читает план на каждый вызов, а не
	// захватывает ссылку при старте: план-график перепривязывается на ходу, и
	// захваченный указатель навсегда оставил бы инциденты на старой сетке
	// остановок. Пустой Holder даёт то же поведение, что и отсутствие плана.
	s.incidents.SetScheduleLookup(func(trID, target int64) int64 {
		sched := currentSchedule(cfg)
		if sched == nil {
			return 0
		}
		stops := sched.Stops(trID)
		for i, st := range stops {
			if st.ActionID == target {
				if i == 0 {
					return 0 // цель первая: соседа нет, не выдумываем
				}
				return stops[i-1].ActionID
			}
		}
		return 0
	})
	s.preds.now = cfg.Now
	s.routes()
	return s
}

// Observe принимает прогноз, сохраняет его и приводит инциденты в
// соответствие. Это единственная точка, где внешний мир влияет на состояние
// гейтвея, и её вызывает планировщик горизонта на каждом тике.
//
// Возвращает событие по инциденту, чтобы планировщик отправил в WebSocket
// именно изменение, а не весь список инцидентов подряд.
//
// Наблюдение и есть главная работа гейтвея: обновляет карточку и двигает
// жизненный цикл инцидента. Оба следствия уходят в ленту немедленно, и
// инцидент — только когда он действительно изменился, иначе панель получала
// бы событие на каждом тике ради цифры, которая не поменялась.
func (s *Server) Observe(p predictor.Prediction) IncidentEvent {
	s.preds.Put(p)
	if p.Latency > 0 {
		s.latency.ObserveDuration(p.Latency)
	}
	ev := s.incidents.Update(p)
	s.publishVehicle(p)
	// Прежняя цель публикуется первой: карточка, которая сейчас закрывается,
	// должна уйти из панели раньше, чем откроется следующая, иначе в ленте
	// две тревоги одной машины меняются местами.
	if ev.Superseded != nil {
		s.publishIncident(*ev.Superseded)
	}
	if ev.Incident != nil {
		s.publishIncident(*ev.Incident)
	}
	return ev
}

// Handler возвращает корневой обработчик. Отдельный метод нужен, чтобы
// сервер запускался как под тестами на httptest, так и вживую.
func (s *Server) Handler() http.Handler { return s.mux }

// Incidents, Predictions и Latency доступны наружу для сборки снимков в
// тестах и для планировщика, который обязан знать, что инцидент открыт.
func (s *Server) Incidents() *Incidents     { return s.incidents }
func (s *Server) Predictions() *Predictions { return s.preds }

// Hub возвращает ленту событий. Может быть nil, если гейтвей подняли без неё.
func (s *Server) Hub() *Hub { return s.cfg.Hub }

// Snapshot возвращает начальное состояние для нового подписчика. Отдельный
// метод, потому что это ровно то, что нужно приборной панели при подключении,
// и его полезно проверять тестом без поднятия WebSocket.
func (s *Server) Snapshot() []Event { return s.snapshot() }

// ListenAndServe принимает соединения до отмены ctx. Отмена ctx останавливает
// сервер и дожидается текущих запросов: оборванный на середине ответ в
// /ws/stream или в карточке хуже, чем лишние секунды остановки.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.cfg.Addr == "" {
		return errors.New("gateway: не задан адрес прослушивания")
	}
	s.http = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout намеренно не задан: он обрывает WebSocket через
		// фиксированное время жизни соединения, а такие соединения должны
		// жить, пока клиент не отключился. Для обычных запросов ограничение
		// всё же есть — в обёртке ниже.
	}
	errc := make(chan error, 1)
	go func() {
		s.log.Info("gateway слушает", slog.String("addr", s.cfg.Addr))
		errc <- s.http.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("gateway: остановка: %w", err)
		}
		return nil
	}
}
