package predictor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
)

// flaky — модель, которая умеет и отвечать, и отказывать, и ведёт счёт
// попыток, чтобы тест мог проверить, куда именно ушёл ответ.
type flaky struct {
	err    error
	delta  float64
	plate  float64
	calls  int
	frames int
}

func (f *flaky) Predict(_ context.Context, frame horizon.Frame) Prediction {
	f.calls++
	if f.err != nil {
		return Prediction{}
	}
	f.frames++
	dev, _ := frame.Value("cur_dev_s")
	return Prediction{
		SampleID: frame.SampleID, UnitID: frame.UnitID, TRID: frame.TRID,
		AsOf: frame.AsOf, TargetStopID: frame.PrimaryStopID(), HorizonS: frame.HorizonS(),
		DeltaS: f.delta, PredictedDevS: dev + f.delta, PLate: f.plate,
		ModelVersion: "v-test", Source: SourceML,
	}
}

func (f *flaky) predict(_ context.Context, frame horizon.Frame) (Prediction, error) {
	if f.err != nil {
		return Prediction{}, f.err
	}
	return f.Predict(nil, frame), nil
}

var errModelDown = errors.New("модель не отвечает")

// fallbackAt собирает цепочку с часами, совпадающими со временем кадров.
// Кадры в тестах датированы январём, а реальные часы идут на месяцы вперёд,
// и без подмены кэш протухал бы мгновенно.
func fallbackAt(t *testing.T, primary, lastResort Predictor, ttl time.Duration) *Fallback {
	t.Helper()
	fb := NewFallback(primary, lastResort, ttl)
	fb.now = func() time.Time { return base }
	return fb
}

func TestFallbackServesModelWhenItWorks(t *testing.T) {
	model := &flaky{delta: 25, plate: 0.5}
	fb := NewFallback(model, BaselinePredictor{}, time.Minute)

	p := fb.Predict(t.Context(), frameAt(t, base, 100))
	if p.Source != SourceML {
		t.Fatalf("источник %q, ожидалась модель", p.Source)
	}
	if p.PredictedDevS != 125 {
		t.Errorf("отклонение %v, ожидалось 125", p.PredictedDevS)
	}
	if p.Stale {
		t.Error("ответ модели не должен помечаться устаревшим")
	}
}

// Главное свойство кэша: ответ достаётся из прошлого, поэтому он обязан
// сохранить идентификатор и время того кадра, к которому относится.
// Подставить сюда SampleID текущего кадра нельзя — тогда клиент и инцидент
// решили бы, что ответ получен только что.
func TestFallbackServesStalePredictionWithItsOwnIdentity(t *testing.T) {
	model := &flaky{delta: 25, plate: 0.5}
	fb := fallbackAt(t, model, BaselinePredictor{}, time.Minute)

	old := frameAt(t, base, 100)
	if p := fb.Predict(t.Context(), old); p.Source != SourceML {
		t.Fatal("первый прогноз должен был прийти от модели")
	}

	model.err = errModelDown
	fresh := frameAt(t, base.Add(30*time.Second), 120)
	p := fb.Predict(t.Context(), fresh)

	if p.Source != SourceCached {
		t.Fatalf("источник %q, ожидался кэш", p.Source)
	}
	if !p.Stale {
		t.Error("ответ из кэша обязан быть помечен устаревшим")
	}
	if p.SampleID != old.SampleID {
		t.Errorf("SampleID %q, ожидался %q от старого кадра", p.SampleID, old.SampleID)
	}
	if !p.AsOf.Equal(base) {
		t.Errorf("AsOf %v, ожидалось %v от старого кадра", p.AsOf, base)
	}
	if p.PredictedDevS != 125 {
		t.Errorf("отклонение %v, ожидалось 125 из кэша", p.PredictedDevS)
	}
}

func TestFallbackToBaselineWhenNothingCached(t *testing.T) {
	model := &flaky{err: errModelDown}
	fb := fallbackAt(t, model, BaselinePredictor{}, time.Minute)

	p := fb.Predict(t.Context(), frameAt(t, base, 90))
	if p.Source != SourceBaseline {
		t.Fatalf("источник %q, ожидался baseline", p.Source)
	}
	if p.DeltaS != 0 {
		t.Errorf("добавка baseline %v, ожидался 0", p.DeltaS)
	}
	if p.PredictedDevS != 90 {
		t.Errorf("baseline обязан вернуть cur_dev_s: получили %v, ожидалось 90", p.PredictedDevS)
	}
	if p.Stale {
		t.Error("baseline не устаревает: он посчитан на текущем кадре")
	}
}

// Прогноз, пролежавший дольше ttl, показывать нельзя: к этому моменту
// машина могла уехать, и ответ перестал быть ответом про неё.
func TestFallbackIgnoresExpiredCache(t *testing.T) {
	now := base
	model := &flaky{delta: 25, plate: 0.5}
	fb := NewFallback(model, BaselinePredictor{}, 30*time.Second)
	fb.now = func() time.Time { return now }

	fb.Predict(t.Context(), frameAt(t, base, 100))
	model.err = errModelDown

	now = base.Add(31 * time.Second)
	p := fb.Predict(t.Context(), frameAt(t, now, 100))
	if p.Source != SourceBaseline {
		t.Fatalf("источник %q для просроченного кэша, ожидался baseline", p.Source)
	}
}

// Кэш принадлежит машине. Ответ по первой не должен уезжать второй: у них
// разные маршруты и разное отклонение, и подмена была бы выдумкой.
func TestCacheIsPerVehicle(t *testing.T) {
	model := &flaky{delta: 25, plate: 0.5}
	fb := fallbackAt(t, model, BaselinePredictor{}, time.Minute)

	first := frameAt(t, base, 100)
	fb.Predict(t.Context(), first)

	other := frameAt(t, base, 500)
	other.UnitID = 9999
	other.TRID = 9
	other.SampleID = horizon.FormatSampleID(9, base)

	model.err = errModelDown
	p := fb.Predict(t.Context(), other)
	if p.Source != SourceBaseline {
		t.Fatalf("источник %q для чужой машины, ожидался baseline", p.Source)
	}
	if p.PredictedDevS != 500 {
		t.Errorf("отклонение %v, ожидалось 500 от её собственного кадра", p.PredictedDevS)
	}
}

// Модели нет вовсе. Это штатная ситуация площадки без ML, а не повод падать
// на каждом кадре, и статистика обязана показывать деградацию, а не работу
// модели.
func TestFallbackWithoutModelDegradesToBaseline(t *testing.T) {
	fb := fallbackAt(t, nil, nil, time.Minute)
	p := fb.Predict(t.Context(), frameAt(t, base, 42))
	if p.Source != SourceBaseline {
		t.Fatalf("источник %q без модели, ожидался baseline", p.Source)
	}
	if p.PredictedDevS != 42 {
		t.Errorf("отклонение %v, ожидалось 42", p.PredictedDevS)
	}
	if s := fb.Stats(); s.FromBase != 1 || s.FromModel != 0 {
		t.Errorf("статистика %+v: baseline должен считаться, модель — нет", s)
	}
}

func TestFallbackStatsCountEveryPath(t *testing.T) {
	model := &flaky{delta: 25, plate: 0.5}
	fb := fallbackAt(t, model, BaselinePredictor{}, time.Minute)

	fb.Predict(t.Context(), frameAt(t, base, 100)) // модель
	model.err = errModelDown
	fb.Predict(t.Context(), frameAt(t, base, 100)) // кэш

	other := frameAt(t, base, 100)
	other.UnitID = 7777
	fb.Predict(t.Context(), other) // baseline

	s := fb.Stats()
	if s.Total != 3 {
		t.Errorf("всего %d, ожидалось 3", s.Total)
	}
	if s.FromModel != 1 || s.FromCache != 1 || s.FromBase != 1 {
		t.Errorf("разбивка %+v, ожидалось по одному на каждый путь", s)
	}
	if s.Degraded() != 2 {
		t.Errorf("деградации %d из 3, ожидалось 2", s.Degraded())
	}
}

// Кэш ограничен: машины, ушедшие из смены, не должны держать память до
// перезапуска процесса.
func TestCacheIsBounded(t *testing.T) {
	model := &flaky{delta: 1, plate: 0.1}
	fb := fallbackAt(t, model, BaselinePredictor{}, time.Minute)

	for i := range maxCached + 50 {
		f := frameAt(t, base, float64(i))
		f.UnitID = uint32(i)
		fb.Predict(t.Context(), f)
	}
	if n := len(fb.cache); n > maxCached {
		t.Errorf("в кэше %d машин при потолке %d", n, maxCached)
	}
	if _, ok := fb.Cached(uint32(maxCached + 49)); !ok {
		t.Error("последняя машина обязана остаться в кэше: её вытеснять не за что")
	}
}

// baseline на кадре без cur_dev_s обязан честно ответить нулём и показать
// пропуск, а не изобразить измеренное отклонение.
func TestBaselineWithoutCurDevReportsItAsMissing(t *testing.T) {
	frame := frameAt(t, base, 0)
	delete(frame.Values, "cur_dev_s")

	p := BaselinePredictor{}.Predict(t.Context(), frame)
	if p.Source != SourceBaseline {
		t.Fatalf("источник %q, ожидался baseline", p.Source)
	}
	if p.PredictedDevS != 0 {
		t.Errorf("отклонение %v при неизмеренном cur_dev_s, ожидался 0", p.PredictedDevS)
	}
	if !contains(p.Missing, "cur_dev_s") {
		t.Errorf("в Missing нет cur_dev_s: %v", p.Missing)
	}
}

func TestBaselineCarriesCurDev(t *testing.T) {
	p := BaselinePredictor{}.Predict(t.Context(), frameAt(t, base, -75))
	if p.PredictedDevS != -75 {
		t.Errorf("отклонение %v, ожидалось -75", p.PredictedDevS)
	}
	if p.DeltaS != 0 {
		t.Errorf("добавка baseline %v, ожидался 0", p.DeltaS)
	}
	if p.TargetStopID != 114 {
		t.Errorf("цель %d, ожидалась 114", p.TargetStopID)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
