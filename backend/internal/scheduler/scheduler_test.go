package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// stubPredictor отвечает заранее заданным ответом и умеет задерживаться.
type stubPredictor struct {
	mu      sync.Mutex
	calls   int
	delay   time.Duration
	fail    bool
	lastIDs []string
}

func (s *stubPredictor) Predict(ctx context.Context, f horizon.Frame) predictor.Prediction {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return predictor.Prediction{
				SampleID: f.SampleID, UnitID: f.UnitID, TRID: f.TRID,
				Source: predictor.SourceBaseline,
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastIDs = append(s.lastIDs, f.SampleID)
	p := predictor.Prediction{
		SampleID: f.SampleID, UnitID: f.UnitID, TRID: f.TRID,
		TargetStopID: f.PrimaryStopID(), Source: predictor.SourceML,
	}
	if s.fail {
		// Отказ модели наружу не выходит: это работа Fallback. Здесь он
		// означает baseline, то есть деградацию без падения.
		p.Source = predictor.SourceBaseline
	}
	return p
}

func (s *stubPredictor) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testFrame(id string, unit uint32) horizon.Frame {
	var f horizon.Frame
	f.SampleID = id
	f.UnitID = unit
	f.AsOf = time.Date(2026, 1, 6, 8, 5, 0, 0, time.UTC)
	return f
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Кадр обязан дойти до модели и её ответ — до наблюдателя.
func TestSubmitReachesPredictorAndObserver(t *testing.T) {
	stub := &stubPredictor{}
	got := make(chan predictor.Prediction, 1)
	s := New(Config{
		Predictor: stub,
		Observer:  func(p predictor.Prediction) { got <- p },
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	s.Submit(testFrame("7_1767686700", 4242))
	select {
	case p := <-got:
		if p.SampleID != "7_1767686700" {
			t.Errorf("наблюдатель получил %q", p.SampleID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("наблюдатель ничего не получил за две секунды")
	}
	cancel()
	<-done

	if s.Stats().Predicted != 1 {
		t.Errorf("Predicted %d, ожидался 1", s.Stats().Predicted)
	}
	if s.Stats().BySource[predictor.SourceML] != 1 {
		t.Errorf("ответов ml %d, ожидался 1", s.Stats().BySource[predictor.SourceML])
	}
}

// Переполненная очередь обязана ронять кадры, а не блокировать конвейер.
// Конвейер строит кадры в своём горутине тика, и если Submit ждёт модели,
// недоступность ML останавливает приём телеметрии.
func TestSubmitDoesNotBlockOnFullQueue(t *testing.T) {
	s := New(Config{
		Predictor: &stubPredictor{},
		Queue:     2,
		Logger:    quietLogger(),
	})
	// Воркеров нет: ничто не разбирает очередь.
	for i := range 50 {
		s.Submit(testFrame("7_1", uint32(i)))
	}
	stats := s.Stats()
	if stats.Submitted != 50 {
		t.Errorf("Submitted %d, ожидалось 50", stats.Submitted)
	}
	if stats.Dropped != 48 {
		t.Errorf("Dropped %d, ожидалось 48", stats.Dropped)
	}
	if got := s.QueueLen(); got != 2 {
		t.Errorf("в очереди %d кадров, ожидалось 2", got)
	}
	if stats.Predicted != 0 {
		t.Errorf("Predicted %d без воркеров, ожидался 0", stats.Predicted)
	}
}

// Без модели кадры некуда нести, но Submit обязан оставаться безопасным
// и честным: это отказ, а не успех.
func TestSubmitWithoutPredictorCountsDrops(t *testing.T) {
	s := New(Config{Logger: quietLogger()})
	s.Submit(testFrame("7_1", 1))
	if got := s.Stats().Dropped; got != 1 {
		t.Errorf("Dropped %d без модели, ожидался 1", got)
	}
}

// Порядок ответов обязан совпадать с порядком кадров: инциденты в gateway
// ждут прогнозов по времени кадра.
func TestAnswersKeepFrameOrder(t *testing.T) {
	stub := &stubPredictor{}
	s := New(Config{Predictor: stub, Workers: 1, Logger: quietLogger()})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	ids := make([]string, 0, 20)
	for i := range 20 {
		id := "7_" + string(rune('a'+i))
		ids = append(ids, id)
		s.Submit(testFrame(id, uint32(i)))
	}
	waitFor(t, func() bool { return s.Stats().Predicted == 20 })
	cancel()
	<-done

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.lastIDs) != len(ids) {
		t.Fatalf("модель получила %d кадров, ожидалось %d", len(stub.lastIDs), len(ids))
	}
	for i, id := range ids {
		if stub.lastIDs[i] != id {
			t.Fatalf("кадр %d: модель получила %q, ожидалось %q", i, stub.lastIDs[i], id)
		}
	}
}

// Отмена контекста обязана останавливать воркеры, а очередь — быть
// посчитанной потерянной. Молча пропавшие кадры при остановке процесса
// выглядели бы как «модель ничего не вернула», а это другое.
func TestCancelStopsWorkersAndCountsAbandoned(t *testing.T) {
	s := New(Config{
		Predictor: &stubPredictor{delay: 50 * time.Millisecond},
		Queue:     16,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	for i := range 8 {
		s.Submit(testFrame("7_1", uint32(i)))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run не вернулся после отмены")
	}
	if got := s.Stats().Abandoned; got == 0 {
		t.Error("Abandoned 0: очередь должна была быть посчитана потерянной")
	}
}

// Медленная модель не должна приводить к гонке за счётчики.
func TestStatsAreSafeUnderLoad(t *testing.T) {
	s := New(Config{Predictor: &stubPredictor{}, Workers: 4, Queue: 64, Logger: quietLogger()})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 200 {
				s.Submit(testFrame("7_1", uint32(w*200+i)))
			}
		}(w)
	}
	wg.Wait()
	waitFor(t, func() bool { return s.Stats().Predicted+s.Stats().Dropped == 800 })
	cancel()
	<-done

	stats := s.Stats()
	if stats.Submitted != 800 {
		t.Errorf("Submitted %d, ожидалось 800", stats.Submitted)
	}
	if stats.Predicted+stats.Dropped != 800 {
		t.Errorf("Predicted %d + Dropped %d, ожидалось 800", stats.Predicted, stats.Dropped)
	}
	if got := stats.BySource[predictor.SourceML]; got != stats.Predicted {
		t.Errorf("по источнику %d ml из %d предсказанных", got, stats.Predicted)
	}
}

// Отмена не должна ронять воркер, даже если модель ответила не с того
// контекста: Predictor не возвращает ошибку, и падать тут нечему.
func TestPredictorThatIgnoresContextStillStops(t *testing.T) {
	s := New(Config{Predictor: blockingPredictor{}, Logger: quietLogger()})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	s.Submit(testFrame("7_1", 1))
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run завис на модели, игнорирующей отмену")
	}
}

// blockingPredictor игнорирует отмену и отвечает с задержкой: ровно то, что
// делает сетевой клиент, если сеть зависла.
type blockingPredictor struct{}

func (blockingPredictor) Predict(ctx context.Context, f horizon.Frame) predictor.Prediction {
	time.Sleep(30 * time.Millisecond)
	return predictor.Prediction{SampleID: f.SampleID, Source: predictor.SourceBaseline}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("условие не выполнилось за три секунды")
}
