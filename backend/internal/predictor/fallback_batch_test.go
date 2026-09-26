package predictor

import (
	"context"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
)

// Fallback обязан уметь батч: планировщик получает ровно тот предиктор, что
// собран в serve, и если Fallback не реализует Batcher, батчинг молча
// выключится на ровно том месте, где он нужен.
var _ Batcher = (*Fallback)(nil)

var _ Batcher = (*MLClient)(nil)

// batchingModel — модель, которая умеет и поштучно, и батчем, и умеет отказать.
// Метод predict обязателен: без него asAware не признает модель за первичный
// источник, Fallback подставит «модель не настроена», и проверки батча
// проверяли бы заглушку вместо модели.
type batchingModel struct {
	batches int
	refuse  bool
}

func (m *batchingModel) predict(_ context.Context, f horizon.Frame) (Prediction, error) {
	return Prediction{
		SampleID: f.SampleID, UnitID: f.UnitID, TRID: f.TRID,
		DeltaS: 15, PLate: 0.3, Source: SourceML,
	}, nil
}

func (m *batchingModel) Predict(ctx context.Context, f horizon.Frame) Prediction {
	p, err := m.predict(ctx, f)
	if err != nil {
		return Prediction{}
	}
	return p
}

func (m *batchingModel) PredictBatch(_ context.Context, frames []horizon.Frame) ([]Prediction, error) {
	m.batches++
	if m.refuse {
		return nil, errNoBatch
	}
	out := make([]Prediction, len(frames))
	for i, f := range frames {
		out[i], _ = m.predict(context.Background(), f)
	}
	return out, nil
}

func TestFallbackServesBatchFromModel(t *testing.T) {
	model := &batchingModel{}
	fb := NewFallback(model, BaselinePredictor{}, time.Minute)
	frames := []horizon.Frame{
		frameOf(t, 1, 1, base, 60),
		frameOf(t, 2, 2, base, 30),
		frameOf(t, 3, 3, base, 90),
	}

	ps, err := fb.PredictBatch(t.Context(), frames)
	if err != nil {
		t.Fatalf("батч: %v", err)
	}
	if len(ps) != len(frames) {
		t.Fatalf("прогнозов %d, ожидалось %d", len(ps), len(frames))
	}
	if model.batches != 1 {
		t.Errorf("обращений к модели %d, ожидалось 1", model.batches)
	}
	s := fb.Stats()
	if s.Total != 3 {
		t.Errorf("всего %d, ожидалось 3", s.Total)
	}
	if s.FromModel != 3 {
		t.Errorf("из модели %d, ожидалось 3", s.FromModel)
	}
	if s.Degraded() != 0 {
		t.Errorf("деградаций %d, ожидался ноль", s.Degraded())
	}
}

// Отказ батча — не отказ кадров. Fallback обязан развернуть его обратно в
// поштучный путь, иначе пачка молча пропала бы, а потери кадров — это не
// «модель не ответила», это данные.
func TestFallbackUnrollsBatchRefusal(t *testing.T) {
	model := &batchingModel{refuse: true}
	fb := NewFallback(model, BaselinePredictor{}, time.Minute)
	frames := []horizon.Frame{
		frameOf(t, 1, 1, base, 60),
		frameOf(t, 2, 2, base, 30),
	}

	ps, err := fb.PredictBatch(t.Context(), frames)
	if err != nil {
		t.Fatalf("отказ батча не должен доходить до вызывающего: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("прогнозов %d, ожидалось 2: кадры не теряются", len(ps))
	}
	if model.batches != 1 {
		t.Errorf("обращений батчем %d, ожидался 1: ниже идёт поштучный путь", model.batches)
	}
	s := fb.Stats()
	// Ключевое: три кадра не должны посчитаться дважды — пачкой и поштучно.
	if s.Total != 2 {
		t.Errorf("всего %d, ожидалось 2: пачка и поштучный обход не считаются вместе", s.Total)
	}
	if s.FromModel != 2 {
		t.Errorf("из модели %d, ожидалось 2", s.FromModel)
	}
}

// Батч, ответивший неправдоподобным числом прогнозов, обязан быть отказом
// даже без ошибки: иначе вызывающий получил бы чужие кадры молча.
func TestFallbackRejectsShortBatch(t *testing.T) {
	model := &shortBatcher{}
	fb := NewFallback(model, BaselinePredictor{}, time.Minute)
	frames := []horizon.Frame{frameOf(t, 1, 1, base, 60), frameOf(t, 2, 2, base, 30)}

	ps, err := fb.PredictBatch(t.Context(), frames)
	if err != nil {
		t.Fatalf("разворот в поштучный путь: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("прогнозов %d, ожидалось 2", len(ps))
	}
	if s := fb.Stats(); s.Total != 2 {
		t.Errorf("всего %d, ожидалось 2", s.Total)
	}
}

// shortBatcher отвечает батчем, в котором прогнозов меньше, чем кадров, и
// без всякой ошибки: молчаливое укорочение — худший вид неправды, потому что
// вызывающий не видит отказа.
type shortBatcher struct {
	batchingModel
}

func (s *shortBatcher) PredictBatch(ctx context.Context, frames []horizon.Frame) ([]Prediction, error) {
	out := make([]Prediction, 0, len(frames))
	for _, f := range frames[:len(frames)-1] {
		out = append(out, s.Predict(ctx, f))
	}
	return out, nil
}

// Базовая цепочка без модели обязана и дальше отвечать на батч, иначе
// планировщик без --ml потеряет все кадры.
func TestFallbackWithoutModelStillBatches(t *testing.T) {
	fb := NewFallback(nil, BaselinePredictor{}, time.Minute)
	frames := []horizon.Frame{frameOf(t, 1, 1, base, 60), frameOf(t, 2, 2, base, 30)}

	ps, err := fb.PredictBatch(t.Context(), frames)
	if err != nil {
		t.Fatalf("батч без модели: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("прогнозов %d, ожидалось 2", len(ps))
	}
	for i, p := range ps {
		if p.Source != SourceBaseline {
			t.Errorf("кадр %d: источник %q, ожидался baseline", i, p.Source)
		}
	}
	if s := fb.Stats(); s.FromBase != 2 {
		t.Errorf("из baseline %d, ожидалось 2", s.FromBase)
	}
}
