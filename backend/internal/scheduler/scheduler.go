// Package scheduler разводит конвейер и модель.
//
// Между ними обязана стоять очередь, и это не оптимизация. Конвейер
// строит кадры на границе ячейки сетки, а модель отвечает по сети: если
// ждать ответа на каждом кадре, медленный ML-сервис остановит приём
// телеметрии, потому что NDTP-пакеты приходят независимо от прогноза.
// Обратная сторона: очередь обязана быть ограниченной, иначе при упавшей
// модели она растёт до падения по памяти. Переполнение означает отброшенные
// кадры, и это обязано быть видно в счётчиках, а не в молчании.
package scheduler

import (
	"context"
	"log/slog"
	"sync"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// DefaultQueue — размер очереди по умолчанию.
//
// С запасом на несколько ячеек сетки: очередь обязана пережить минуты
// недоступности модели, но не расти бесконечно.
const DefaultQueue = 1024

// Config — зависимости планировщика.
type Config struct {
	// Predictor — источник прогноза. Обязателен: без него кадры некуда
	// нести, и Submit будет только считать.
	Predictor predictor.Predictor
	// Queue — размер очереди в кадрах. Ноль берётся как DefaultQueue.
	Queue int
	// Workers — число воркеров. По умолчанию один, и это не лень: ответы
	// приходят в gateway.incidents, который ждёт их по времени кадра в
	// порядке поступления, и параллельные воркеры разворочили бы этот
	// порядок. Больше одного имеет смысл только вместе с правкой
	// обработки инцидентов.
	Workers int
	// Observer — куда уходят ответы. Вызывается из воркера, поэтому
	// обязан быть быстрым и не падать. Функция, а не интерфейс: у
	// gateway.Observe есть возвращаемое значение, и подставить такой
	// метод в поле-функцию нельзя, а заводить ради этого пакет scheduler
	// в gateway незачем.
	Observer func(predictor.Prediction)
	// Logger — журнал. При nil используется slog.Default.
	Logger *slog.Logger
}

// Stats — счётчики планировщика.
type Stats struct {
	// Submitted — кадров, принятых Submit.
	Submitted int64
	// Predicted — кадров, дошедших до Predictor.
	Predicted int64
	// Dropped — кадров, отброшенных из-за полной очереди. Модель не
	// справилась, и кадр пропал: это потеря данных, а не отказ.
	Dropped int64
	// Abandoned — кадров, оставшихся в очереди при остановке.
	Abandoned int64
	// BySource — ответов по источнику: ml, cached, baseline.
	BySource map[predictor.Source]int64
}

// Scheduler принимает кадры конвейера и зовёт модель.
type Scheduler struct {
	cfg      Config
	queue    chan horizon.Frame
	logger   *slog.Logger
	mu       sync.Mutex
	stats    Stats
	loggedUp int64
	wg       sync.WaitGroup
}

// New создаёт планировщик. Воркеры не запускаются: пока не вызван Run,
// кадры копятся в очереди до её переполнения.
func New(cfg Config) *Scheduler {
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Scheduler{
		cfg:    cfg,
		queue:  make(chan horizon.Frame, cfg.Queue),
		logger: cfg.Logger,
		stats:  Stats{BySource: make(map[predictor.Source]int64)},
	}
}

// Submit принимает кадр от конвейера. Это pipeline.Sink.
//
// Метод не блокируется никогда. Конвейер строит кадры в своём горутине тика
// и не имеет права зависнуть из-за сети: если очередь полна, кадр
// отбрасывается и это считается. Ждать здесь нечего — освободить очередь
// может только модель, а её недоступность не должна останавливать приём
// телеметрии.
func (s *Scheduler) Submit(frame horizon.Frame) {
	s.mu.Lock()
	s.stats.Submitted++
	s.mu.Unlock()
	if s.cfg.Predictor == nil {
		s.countDrop()
		return
	}
	select {
	case s.queue <- frame:
	default:
		s.countDrop()
	}
}

// countDrop считает отброшенный кадр и жалуется в журнал.
//
// Жалоба идёт на первый отброшенный кадр и дальше на каждый сотый: при
// упавшей модели отбрасывается всё подряд, и строка на каждый кадр
// превратила бы журнал в шум, в котором не видно ничего.
func (s *Scheduler) countDrop() {
	s.mu.Lock()
	s.stats.Dropped++
	n := s.stats.Dropped
	s.mu.Unlock()
	if n == 1 || n%100 == 0 {
		s.logger.Warn("очередь прогнозов переполнена, кадры отбрасываются",
			"отброшено", n, "размер_очереди", s.cfg.Queue)
	}
}

// Run запускает воркеры и блокируется до отмены контекста.
//
// При отмене воркеры бросают текущий кадр и очередь не разбирают. Это
// осознанно: при остановке процесса ждать ответа модели на каждый оставшийся
// кадр — значит зависнуть на таймауте модели, а сетка в пять минут означает,
// что теряется не больше одной ячейки, и она будет посчитана заново.
func (s *Scheduler) Run(ctx context.Context) {
	for range s.cfg.Workers {
		s.wg.Add(1)
		go s.work(ctx)
	}
	<-ctx.Done()
	s.wg.Wait()
	abandoned := len(s.queue)
	if abandoned > 0 {
		s.mu.Lock()
		s.stats.Abandoned += int64(abandoned)
		s.mu.Unlock()
		s.logger.Warn("очередь прогнозов не разобрана при остановке", "кадров", abandoned)
	}
}

func (s *Scheduler) work(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-s.queue:
			s.predict(ctx, frame)
		}
	}
}

func (s *Scheduler) predict(ctx context.Context, frame horizon.Frame) {
	p := s.cfg.Predictor.Predict(ctx, frame)
	s.mu.Lock()
	s.stats.Predicted++
	s.stats.BySource[p.Source]++
	s.mu.Unlock()
	if s.cfg.Observer != nil {
		s.cfg.Observer(p)
	}
}

// Stats возвращает снимок счётчиков.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{
		Submitted: s.stats.Submitted,
		Predicted: s.stats.Predicted,
		Dropped:   s.stats.Dropped,
		Abandoned: s.stats.Abandoned,
		BySource:  make(map[predictor.Source]int64, len(s.stats.BySource)),
	}
	for source, n := range s.stats.BySource {
		out.BySource[source] = n
	}
	return out
}

// QueueLen возвращает текущую длину очереди: сколько кадров ждёт модели.
func (s *Scheduler) QueueLen() int { return len(s.queue) }

// compile-time проверка: планировщик обязан быть приёмником конвейера.
var _ interface{ Submit(horizon.Frame) } = (*Scheduler)(nil)
