package predictor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/features"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
)

// modelInfoBody — ответ /model/info с корректным набором признаков.
func modelInfoBody() map[string]any {
	return map[string]any{
		"version":  "2026.09-test",
		"features": FeatureContract(),
	}
}

// frameAt собирает кадр, который можно отдать модели: cur_dev_s измерен,
// все остальные признаки — как есть.
func frameAt(t *testing.T, asOf time.Time, dev float64) horizon.Frame {
	t.Helper()
	return horizon.Frame{
		SampleID: horizon.FormatSampleID(7, asOf),
		UnitID:   4242,
		TRID:     7,
		AsOf:     asOf,
		Target: []horizon.Arrival{
			{ActionID: 114, Planned: asOf.Add(10 * time.Minute), HorizonS: 600},
		},
		Values: map[string]*float64{
			"cur_dev_s":     &dev,
			"horizon_s":     ptr(600.0),
			"speed_current": ptr(0.0),
		},
		Features: features.Set{CurDevS: &dev},
	}
}

func ptr[T any](v T) *T { return &v }

// base — опорный момент, чтобы времена в тестах читались.
var base = time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)

// TestPredictRequestIsFlatWire — wire-контракт §4.5: имена признаков ключами
// верхнего уровня, а не вложенным «features» (FastAPI валидирует плоский
// dict, extras и есть фичи). Фейки обеих сторон форму тела не проверяли,
// поэтому вложенный запрос уходил на 422 молча: этот тест — единственная
// защита формы запроса.
func TestPredictRequestIsFlatWire(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(modelInfoBody())
		case "/predict":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("тело не читается: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(predictResponse{
				DelayS: ptr(5.0), PLate: ptr(0.1), ModelVersion: "v1",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL})
	c.Predict(t.Context(), frameAt(t, base, 42))
	if _, nested := got["features"]; nested {
		t.Error("запрос вёз вложенный «features»: сервер ждёт плоский dict")
	}
	for _, name := range []string{"sample_id", "cur_dev_s", "horizon_s", "speed_current"} {
		if _, ok := got[name]; !ok {
			t.Errorf("нет ключа %q верхнего уровня", name)
		}
	}
	if v, ok := got["cur_dev_s"].(float64); !ok || v != 42 {
		t.Errorf("cur_dev_s = %v, хотим 42", got["cur_dev_s"])
	}
}

func TestMLClientChecksContractBeforePredicting(t *testing.T) {
	var predicts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(modelInfoBody())
		case "/predict":
			predicts.Add(1)
			_ = json.NewEncoder(w).Encode(predictResponse{
				DelayS: ptr(90.0), PLate: ptr(0.4), ModelVersion: "v1",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL})
	p := c.Predict(t.Context(), frameAt(t, base, 60))
	if p.Source != SourceML {
		t.Fatalf("источник %q, ожидалась модель", p.Source)
	}
	if p.DeltaS != 30 {
		t.Errorf("добавка %v, ожидалось 30", p.DeltaS)
	}
	// cur_dev_s = 60, сервис ответил delay_s=90 — добавка обязана выйти 30.
	if p.PredictedDevS != 90 {
		t.Errorf("отклонение %v, ожидалось 90", p.PredictedDevS)
	}
	if p.PLate != 0.4 {
		t.Errorf("p_late %v, ожидалось 0.4", p.PLate)
	}
	if p.ModelVersion != "v1" {
		t.Errorf("версия модели %q", p.ModelVersion)
	}
	if n := predicts.Load(); n != 1 {
		t.Errorf("запросов прогноза %d, ожидался 1", n)
	}
}

// Главный страх: модель обучена на другом наборе признаков. Если такое
// пропустить, модель разберёт кадр по позициям, а не по именам, и ответит
// правдоподобной неправдой. Проверка обязана случиться до первого запроса.
func TestMismatchedContractRefusesToPredict(t *testing.T) {
	var predicts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/predict" {
			predicts.Add(1)
			_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(1.0), PLate: ptr(0.1)})
			return
		}
		names := FeatureContract()
		// Модель забыла один признак и завела свой.
		names = slices.DeleteFunc(names, func(n string) bool { return n == "headway_s" })
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":  "v1",
			"features": append(names, "brand_new_feature"),
		})
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL, Retries: ptr(0)})
	_, err := c.predict(t.Context(), frameAt(t, base, 60))
	if err == nil {
		t.Fatal("расхождение контракта обязано быть ошибкой")
	}
	if !strings.Contains(err.Error(), "headway_s") {
		t.Errorf("в ошибке нет имени потерянного признака: %v", err)
	}
	if !strings.Contains(err.Error(), "brand_new_feature") {
		t.Errorf("в ошибке нет имени лишнего признака: %v", err)
	}
	if n := predicts.Load(); n != 0 {
		t.Errorf("запросов прогноза %d при несовпадении контракта, ожидался 0", n)
	}
}

// Порядок имён не важен, множество — да. Иначе пришлось бы блокировать
// выгрузку кадров из-за того, что в чужом репозитории переставили строки.
func TestContractIgnoresOrder(t *testing.T) {
	names := FeatureContract()
	slices.Reverse(names)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"features": names})
			return
		}
		_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(1.0), PLate: ptr(0.1)})
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL})
	p := c.Predict(t.Context(), frameAt(t, base, 0))
	if p.Source != SourceML {
		t.Fatalf("источник %q, ожидалась модель", p.Source)
	}
}

// Дубль признака — тоже расхождение: модель считает его двумя разными, и
// молча согласиться с этим нельзя.
func TestContractRejectsDuplicates(t *testing.T) {
	err := checkNames(append(FeatureContract(), "horizon_s"))
	if err == nil {
		t.Fatal("дубль признака обязано быть расхождением")
	}
	if !strings.Contains(err.Error(), "horizon_s") {
		t.Errorf("в ошибке нет имени дубля: %v", err)
	}
}

func TestModelWithoutFeatureNamesIsMismatch(t *testing.T) {
	if err := checkNames(nil); err == nil {
		t.Fatal("модель без списка признаков обязана считаться несогласованной")
	}
}

// Ответ без обязательных полей обязан быть отказом, а не «опоздания нет».
// Пропущенное поле и нулевое значение выглядят в JSON одинаково, и без
// проверки модель, забывшая delta_s, отвечала бы уверенным нулём.
func TestResponseWithoutDeltaIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(modelInfoBody())
			return
		}
		_, _ = w.Write([]byte(`{"p_late":0.2}`))
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL, Retries: ptr(0)})
	if _, err := c.predict(t.Context(), frameAt(t, base, 0)); err == nil {
		t.Fatal("ответ без delta_s обязан быть отказом")
	}
}

func TestPLateOutOfRangeIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(modelInfoBody())
			return
		}
		_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(1.0), PLate: ptr(1.4)})
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL, Retries: ptr(0)})
	if _, err := c.predict(t.Context(), frameAt(t, base, 0)); err == nil {
		t.Fatal("p_late вне [0,1] обязан быть отказом")
	}
}

// Сетевая беда лечится повтором: это ровно тот случай, ради которого повторы
// и существуют.
func TestRetriesRecoverFromServerError(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(modelInfoBody())
			return
		}
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(7.0), PLate: ptr(0.1)})
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL, Retries: ptr(2), Timeout: 20 * time.Millisecond})
	p := c.Predict(t.Context(), frameAt(t, base, 0))
	if p.DeltaS != 7 {
		t.Fatalf("после повторов добавка %v, ожидалось 7", p.DeltaS)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("попыток %d, ожидалось 3", n)
	}
}

// 4xx — это наша ошибка, а не временная беда модели. Повторяться он не
// должен: запрос неправильный, и второй попыткой мы лишь нагрузим сервис.
func TestClientErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(modelInfoBody())
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL, Retries: ptr(2), Timeout: 20 * time.Millisecond})
	if _, err := c.predict(t.Context(), frameAt(t, base, 0)); err == nil {
		t.Fatal("400 обязан быть отказом")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("попыток %d на 400, ожидалась 1: повтор тут не помогает", n)
	}
}

// 429 — наоборот, повторять надо: сервис прямо просит.
func TestTooManyRequestsIsRetried(t *testing.T) {
	if !retryable(&statusError{code: http.StatusTooManyRequests}) {
		t.Error("429 обязан считаться повторяемым")
	}
	if retryable(&statusError{code: http.StatusBadRequest}) {
		t.Error("400 не должен считаться повторяемым")
	}
	if retryable(&statusError{code: http.StatusInternalServerError}) != true {
		t.Error("500 обязан считаться повторяемым")
	}
}

// Автомат отказов обязан перестать стучаться, а не долбить упавший сервис по
// одному кадру на каждую машину.
func TestBreakerOpensAndProbesAfterCooldown(t *testing.T) {
	var calls atomic.Int64
	now := base
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(modelInfoBody())
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{
		BaseURL:          srv.URL,
		Retries:          ptr(0),
		Timeout:          10 * time.Millisecond,
		FailureThreshold: 2,
		Cooldown:         time.Minute,
		Now:              func() time.Time { return now },
	})
	frame := frameAt(t, base, 0)

	for range 2 {
		if _, err := c.predict(t.Context(), frame); err == nil {
			t.Fatal("падение сервиса обязано быть отказом")
		}
	}
	if s := c.Stats().Breaker; s != breakerOpen {
		t.Fatalf("состояние %q после двух неудач, ожидался open", s)
	}
	// Молчание: ни одного запроса, пока cooldown не истёк.
	before := calls.Load()
	for range 5 {
		if _, err := c.predict(t.Context(), frame); err == nil {
			t.Fatal("разомкнутый клиент обязан отказывать")
		}
	}
	if after := calls.Load(); after != before {
		t.Errorf("разомкнутый клиент сделал %d запросов, ожидалось 0", after-before)
	}
	if s := c.Stats().Breaker; s != breakerOpen {
		t.Errorf("состояние %q, ожидался open", s)
	}

	// По истечении cooldown — ровно одна проба, и неуспешная проба молчание
	// продлевает.
	now = now.Add(2 * time.Minute)
	if _, err := c.predict(t.Context(), frame); err == nil {
		t.Fatal("проба на упавшем сервисе обязана провалиться")
	}
	if after := calls.Load(); after != before+1 {
		t.Errorf("после cooldown запросов %d, ожидался 1", after-before)
	}
	if s := c.Stats().Breaker; s != breakerOpen {
		t.Errorf("после неудачной пробы состояние %q, ожидался open", s)
	}
}

// Успех замыкает автомат: после серии неудач работающая модель не должна
// остаться в молчании.
func TestBreakerClosesOnSuccess(t *testing.T) {
	now := base
	up := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(modelInfoBody())
			return
		}
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(3.0), PLate: ptr(0.1)})
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{
		BaseURL: srv.URL, Retries: ptr(0), Timeout: 10 * time.Millisecond,
		FailureThreshold: 1, Cooldown: time.Minute,
		Now: func() time.Time { return now },
	})
	frame := frameAt(t, base, 0)
	_, _ = c.predict(t.Context(), frame)
	if s := c.Stats().Breaker; s != breakerOpen {
		t.Fatalf("состояние %q, ожидался open", s)
	}
	up.Store(true)
	now = now.Add(2 * time.Minute)
	if p := c.Predict(t.Context(), frame); p.Source != SourceML {
		t.Fatalf("источник %q после восстановления, ожидалась модель", p.Source)
	}
	if s := c.Stats().Breaker; s != breakerClosed {
		t.Errorf("состояние %q после успеха, ожидался closed", s)
	}
}

// Сверка контракта не должна обновляться на каждом кадре: при упавшем
// сервисе каждый кадр ходил бы ещё и за /model/info, удваивая давление
// именно тогда, когда модели тяжело.
func TestContractCheckedOnceWhileHealthy(t *testing.T) {
	var infos atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			infos.Add(1)
			_ = json.NewEncoder(w).Encode(modelInfoBody())
			return
		}
		_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(1.0), PLate: ptr(0.1)})
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL})
	for range 5 {
		c.Predict(t.Context(), frameAt(t, base, 0))
	}
	if n := infos.Load(); n != 1 {
		t.Errorf("обращений к /model/info %d, ожидалась 1", n)
	}
}
