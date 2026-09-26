package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// batchingPredictor — предиктор, который умеет батч, запоминает размеры
// пачек и умеет отказать.
type batchingPredictor struct {
	stubPredictor
	// bmu защищает только размеры пачек и счётчик отказов: мьютекс
	// stubPredictor занят под её собственные счётчики, и смешивать их в одну
	// блокировку незачем.
	bmu     sync.Mutex
	sizes   []int
	refuses bool
}

func (b *batchingPredictor) PredictBatch(_ context.Context, frames []horizon.Frame) ([]predictor.Prediction, error) {
	b.bmu.Lock()
	b.sizes = append(b.sizes, len(frames))
	refuse := b.refuses
	b.bmu.Unlock()
	if refuse {
		return nil, errors.New("модель не справилась с батчем")
	}
	out := make([]predictor.Prediction, len(frames))
	for i, f := range frames {
		out[i] = b.Predict(context.Background(), f)
	}
	return out, nil
}

func (b *batchingPredictor) batchSizes() []int {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	return append([]int(nil), b.sizes...)
}

// liarBatcher отдаёт неправдоподобный батч: меньше прогнозов, чем кадров.
// Так ведёт себя предиктор, нарушивший контракт, и планировщик обязан
// отработать это поштучно, а не потерять кадры.
type liarBatcher struct {
	stubPredictor
}

func (l *liarBatcher) PredictBatch(_ context.Context, frames []horizon.Frame) ([]predictor.Prediction, error) {
	out := make([]predictor.Prediction, 0, len(frames))
	for _, f := range frames[:len(frames)-1] {
		out = append(out, l.Predict(context.Background(), f))
	}
	return out, nil
}

// collector копит ответы наблюдателя под мьютексом: вызывается из воркера.
type collector struct {
	mu    sync.Mutex
	items []predictor.Prediction
}

func (c *collector) add(p predictor.Prediction) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, p)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *collector) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.items))
	for i, p := range c.items {
		out[i] = p.SampleID
	}
	return out
}

// Пять кадров, подложенных в очередь до старта воркера, обязаны уйти в модель
// одним обращением: в этом весь смысл батчинга. Пять кругов вместо одного —
// не оптимизация, а нагрузка на сервис модели, которая ничего не купила.
func TestBatchServesQueueInOneCall(t *testing.T) {
	pred := &batchingPredictor{}
	var got collector
	s := New(Config{
		Predictor: pred,
		Queue:     16,
		Batch:     8,
		Observer:  got.add,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)

	want := []string{"f1", "f2", "f3", "f4", "f5"}
	for _, id := range want {
		s.Submit(testFrame(id, 1))
	}
	waitFor(t, func() bool { return got.count() == 5 })

	if sizes := pred.batchSizes(); len(sizes) != 1 || sizes[0] != 5 {
		t.Errorf("размеры пачек %v, ожидалась одна пачка из 5", sizes)
	}
	// Порядок ответов — основа разбора инцидентов в гейтвее (ADR 0008), и
	// батчинг его не вправе ломать.
	for i, id := range got.ids() {
		if id != want[i] {
			t.Errorf("ответ %d: кадр %q вместо %q", i, id, want[i])
		}
	}
}

// Единица — это выключатель батчинга, а не «пачка по одному»: с ним поведение
// обязано остаться прежним, иначе откат флага окажется невозможен.
func TestBatchOneKeepsSingleFramePath(t *testing.T) {
	pred := &batchingPredictor{}
	var got collector
	s := New(Config{
		Predictor: pred,
		Queue:     16,
		Batch:     1,
		Observer:  got.add,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)
	for _, id := range []string{"a", "b", "c"} {
		s.Submit(testFrame(id, 1))
	}
	waitFor(t, func() bool { return got.count() == 3 })

	if sizes := pred.batchSizes(); len(sizes) != 0 {
		t.Errorf("при batch=1 батчей быть не должно, получено %v", sizes)
	}
	if n := pred.count(); n != 3 {
		t.Errorf("одиночных обращений %d, ожидалось 3", n)
	}
}

// Предиктор, не умеющий батч, обязан увидеть поштучную работу даже при
// batch>1, иначе флаг обещал бы экономию, которой нет.
func TestBatchIgnoredWhenPredictorCannotBatch(t *testing.T) {
	pred := &stubPredictor{}
	var got collector
	s := New(Config{
		Predictor: pred,
		Queue:     8,
		Batch:     4,
		Observer:  got.add,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)
	s.Submit(testFrame("only", 1))
	waitFor(t, func() bool { return got.count() == 1 })

	if n := pred.count(); n != 1 {
		t.Errorf("обращений %d, ожидалось 1: поштучный путь обязан сохраниться", n)
	}
}

// Очередь не имеет права терять кадры из-за предиктора, нарушившего контракт
// батча: его неправдоподобный ответ означает поштучный обход, а не тишину.
func TestBatchGarbageFallsBackToSingleFrames(t *testing.T) {
	pred := &liarBatcher{}
	var got collector
	s := New(Config{
		Predictor: pred,
		Queue:     16,
		Batch:     8,
		Observer:  got.add,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)
	for _, id := range []string{"x", "y", "z"} {
		s.Submit(testFrame(id, 1))
	}
	waitFor(t, func() bool { return got.count() == 3 })

	ids := got.ids()
	if len(ids) != 3 {
		t.Fatalf("ответов %d, ожидалось 3: кадры терять нельзя", len(ids))
	}
	for i, want := range []string{"x", "y", "z"} {
		if ids[i] != want {
			t.Errorf("ответ %d: %q вместо %q", i, ids[i], want)
		}
	}
}

// Размеры пачек показывают, работает ли батчинг вообще. Окно берётся из
// планировщика, потому что настроить batch, не видя его в метриках, можно
// только вслепую.
func TestBatchSizeIsObservable(t *testing.T) {
	pred := &batchingPredictor{}
	var got collector
	s := New(Config{
		Predictor: pred,
		Queue:     16,
		Batch:     8,
		Observer:  got.add,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)
	for _, id := range []string{"1", "2", "3", "4"} {
		s.Submit(testFrame(id, 1))
	}
	waitFor(t, func() bool { return got.count() == 4 })

	q := s.BatchSizeQuantiles()
	if q.Total != 1 {
		t.Errorf("замеров пачек %d, ожидался 1", q.Total)
	}
	if q.P50 != 4 {
		t.Errorf("медиана размера пачки %v, ожидалось 4", q.P50)
	}
	st := s.Stats()
	if st.Batches != 1 || st.BatchedFrames != 4 {
		t.Errorf("счётчики батча: пачек %d кадров %d, ожидалось 1 и 4", st.Batches, st.BatchedFrames)
	}
	if st.Predicted != 4 {
		t.Errorf("предсказано %d, ожидалось 4", st.Predicted)
	}
}

// Отказ батча не должен выглядеть как потеря кадров: Fallback разворачивает
// его обратно в поштучный путь, и счётчики обязаны это показать, а не
// выглядеть как «ничего не пришло».
func TestBatchRefusalIsNotFrameLoss(t *testing.T) {
	pred := &batchingPredictor{refuses: true}
	var got collector
	s := New(Config{
		Predictor: pred,
		Queue:     16,
		Batch:     8,
		Observer:  got.add,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)
	for _, id := range []string{"p", "q"} {
		s.Submit(testFrame(id, 1))
	}
	waitFor(t, func() bool { return got.count() == 2 })

	st := s.Stats()
	if st.Predicted != 2 {
		t.Errorf("предсказано %d, ожидалось 2: отказ батча не равен потере кадров", st.Predicted)
	}
	if st.Batches != 0 {
		t.Errorf("успешных батчей %d, ожидался 0", st.Batches)
	}
}

// Тик конвейера приходит пачками, а не по одному кадру, и очередь должна
// пережить момент, когда воркер занят предыдущей пачкой.
func TestBatchDrainsBurstWithinTick(t *testing.T) {
	pred := &batchingPredictor{}
	var got collector
	s := New(Config{
		Predictor: pred,
		Queue:     64,
		Batch:     16,
		Observer:  got.add,
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)

	// Кадры одного тика: 10 штук, Batch=16 — всё должно уйти одной пачкой.
	for i := range 10 {
		s.Submit(testFrame(string(rune('a'+i)), uint32(i+1)))
	}
	waitFor(t, func() bool { return got.count() == 10 })

	sizes := pred.batchSizes()
	if len(sizes) == 0 {
		t.Fatal("батчей не было вовсе")
	}
	total := 0
	for _, n := range sizes {
		total += n
	}
	if total != 10 {
		t.Errorf("в пачках %d кадров вместо 10", total)
	}
	if len(sizes) != 1 {
		t.Errorf("размеры пачек %v, ожидалась одна: 10 кадров помещаются в пачку из 16", sizes)
	}
}
