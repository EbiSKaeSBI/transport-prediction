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
	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// DefaultQueue — размер очереди по умолчанию.
//
// С запасом на несколько ячеек сетки: очередь обязана пережить минуты
// недоступности модели, но не расти бесконечно.
const DefaultQueue = 1024

// DefaultBatch — сколько кадров уходит в модель одним запросом.
//
// Ровно столько, сколько кадров набирает один тик сетки на небольшой линии
// (ADR 0007): собирать больше бессмысленно, потому что ждать всё равно не
// чего — очередь пуста к следующему тику. Меньше означало бы делить парк на
// несколько обращений и платить за это несколькими кругами вместо одного.
const DefaultBatch = 32

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
	// Batch — сколько кадров собирать в одно обращение к модели. Единица и
	// меньше отключают батчинг: кадры обслуживаются по одному, как было до
	// его появления. Значение больше единицы имеет смысл только вместе с
	// предиктором, который умеет PredictBatch; иначе остаётся поштучный
	// путь, и флаг просто ничего не меняет.
	Batch int
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
	// Batches — обращений к модели батчем.
	Batches int64
	// BatchedFrames — кадров, обслуженных батчем. Делится на Batches и
	// даёт средний размер пачки.
	BatchedFrames int64
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

	// batcher — предиктор, умеющий батч, либо nil. Определяется один раз в
	// New, а не на каждый кадр: проверка на каждом кадре стоила бы
	// сравнения типа в горячем пути, а меняться она не может.
	batcher predictor.Batcher
	// batchSize — окно размеров собранных пачек. Нужно, чтобы по /metrics
	// было видно, батчинг вообще работает или выродился в поштучные
	// запросы по одному кадру: очередь и инференция этого не показывают.
	batchSize *latency.Window
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
	s := &Scheduler{
		cfg:       cfg,
		queue:     make(chan horizon.Frame, cfg.Queue),
		logger:    cfg.Logger,
		stats:     Stats{BySource: make(map[predictor.Source]int64)},
		batchSize: latency.New(latency.DefaultWindow),
	}
	if cfg.Batch > 1 {
		if b, ok := cfg.Predictor.(predictor.Batcher); ok {
			s.batcher = b
		} else {
			s.logger.Info("батчинг выключен: предиктор не умеет PredictBatch",
				"batch", cfg.Batch)
		}
	}
	return s
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
	if s.batcher != nil {
		s.workBatched(ctx)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-s.queue:
			s.predict(ctx, frame)
		}
	}
}

// workBatched обслуживает очередь пачками.
//
// Пачка собирается без ожидания: первый кадр уже получен, дальше забирается
// всё, что лежит в очереди на этот момент. Ждать нечего и незачем — кадры
// одной ячейки приходят одним тиком (ADR 0007) и к моменту, когда воркер
// дошёл до первого из них, лежат в очереди уже все. Ожидание задерживало бы
// одиночный кадр на машине, которой не с кем делить пакет, а выигрыш в
// одиночном случае нулевой.
func (s *Scheduler) workBatched(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-s.queue:
			s.predictBatch(ctx, s.take(frame))
		}
	}
}

// take собирает пачку: первый кадр и всё, что уже лежит в очереди.
func (s *Scheduler) take(first horizon.Frame) []horizon.Frame {
	frames := make([]horizon.Frame, 0, s.cfg.Batch)
	frames = append(frames, first)
	for len(frames) < s.cfg.Batch {
		select {
		case f := <-s.queue:
			frames = append(frames, f)
		default:
			return frames
		}
	}
	return frames
}

// predictBatch обслуживает пачку одним обращением.
//
// Порядок ответов проверяется и на всякий случай: контракт Batcher обещает
// столько же прогнозов в том же порядке, сколько было кадров, но обещание
// чужого типа проверять дешевле, чем разбираться потом, чей прогноз уехал
// не туда. При несовпадении пачка обслуживается поштучно — очередь не
// теряет кадры никогда, даже если батчер отдаёт мусор.
func (s *Scheduler) predictBatch(ctx context.Context, frames []horizon.Frame) {
	ps, err := s.batcher.PredictBatch(ctx, frames)
	if err != nil || len(ps) != len(frames) {
		for _, frame := range frames {
			s.predict(ctx, frame)
		}
		return
	}

	s.mu.Lock()
	s.stats.Predicted += int64(len(ps))
	s.stats.Batches++
	s.stats.BatchedFrames += int64(len(ps))
	for _, p := range ps {
		s.stats.BySource[p.Source]++
	}
	s.mu.Unlock()
	s.batchSize.Observe(float64(len(ps)))

	// Наблюдатель получает ответы строго в порядке кадров: на этом порядке
	// держится разбор инцидентов в гейтвее (ADR 0008).
	if s.cfg.Observer != nil {
		for _, p := range ps {
			s.cfg.Observer(p)
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
		Submitted:     s.stats.Submitted,
		Predicted:     s.stats.Predicted,
		Dropped:       s.stats.Dropped,
		Abandoned:     s.stats.Abandoned,
		Batches:       s.stats.Batches,
		BatchedFrames: s.stats.BatchedFrames,
		BySource:      make(map[predictor.Source]int64, len(s.stats.BySource)),
	}
	for source, n := range s.stats.BySource {
		out.BySource[source] = n
	}
	return out
}

// QueueLen возвращает текущую длину очереди: сколько кадров ждёт модели.
func (s *Scheduler) QueueLen() int { return len(s.queue) }

// BatchSizeQuantiles отдаёт окно размеров собранных пачек. Считается
// отдельно от счётчиков, потому что это не счётчик, а распределение: важно
// не «сколько всего батчей», а «какими они обычно были».
func (s *Scheduler) BatchSizeQuantiles() latency.Quantiles {
	return s.batchSize.Snapshot()
}

// compile-time проверка: планировщик обязан быть приёмником конвейера.
var _ interface{ Submit(horizon.Frame) } = (*Scheduler)(nil)
