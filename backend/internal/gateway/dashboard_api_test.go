package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// Панель дашборда читает эти два эндпоинта вместо разбора текстового
// exposition: формат ответа — часть контракта с фронтом, и он должен быть
// зафиксирован тестом на той же стороне, где его придумали.

func TestLatencyAPIJSONSnapshot(t *testing.T) {
	// Часы должны идти: uptime считается от Now при сборке сервера, и со
	// статическим Now он навсегда ноль, а throughput при нулевом uptime
	// не отдаётся — делить не на что.
	now := base
	s := New(Config{
		Store: testServer(t).cfg.Store,
		Now:   func() time.Time { return now },
		Queue: func() QueueStats {
			return QueueStats{
				Depth: 7, Submitted: 100, Predicted: 92, Dropped: 3,
				Batches: 20, BatchedFrames: 85,
				BatchSize: latency.Quantiles{Count: 20, Total: 20, P50: 4, P95: 16, P99: 16},
			}
		},
		Inference: func() latency.Quantiles {
			return latency.Quantiles{Count: 4, Total: 4, P50: 0.02, P95: 0.05, P99: 0.06, Max: 0.06}
		},
	})
	s.latency.ObserveDuration(30 * time.Millisecond)
	s.latency.ObserveDuration(50 * time.Millisecond)
	now = base.Add(time.Minute)

	body := get(t, s, "/api/v1/metrics/latency", http.StatusOK)

	lat, ok := body["prediction_latency"].(map[string]any)
	if !ok {
		t.Fatalf("нет prediction_latency: %v", body)
	}
	if lat["total"].(float64) != 2 {
		t.Errorf("prediction_latency.total = %v, хотим 2", lat["total"])
	}
	if _, ok := lat["p50_s"]; !ok {
		t.Error("в сводке нет p50_s: имена полей — контракт с панелью")
	}
	if _, ok := body["inference_latency"]; !ok {
		t.Error("нет inference_latency: панель отвечает по ней «мы медленные или модель»")
	}
	q, ok := body["queue"].(map[string]any)
	if !ok {
		t.Fatalf("нет queue: %v", body)
	}
	if q["depth"].(float64) != 7 || q["dropped"].(float64) != 3 {
		t.Errorf("queue = %v, хотим depth=7 и dropped=3", q)
	}
	// Имя вложенного снимка — часть контракта панели: Go-имя «BatchSize»
	// без тега утекло бы в JSON как есть.
	if bs, ok := q["batch_size"].(map[string]any); !ok || bs["total"].(float64) != 20 {
		t.Errorf("распределение размеров пачек потерялось: %v", q)
	}
	tp, ok := body["throughput"].(map[string]any)
	if !ok {
		t.Fatal("нет throughput")
	}
	if tp["predictions_total"].(float64) != 2 {
		t.Errorf("throughput.predictions_total = %v, хотим 2", tp["predictions_total"])
	}
	// uptime = минута (Now смещён на неё, старт — base), значит 2 прогноза
	// дают ровно 2/60 в секунду.
	if got := tp["predictions_per_s"].(float64); got < 0.032 || got > 0.034 {
		t.Errorf("predictions_per_s = %v, хотим ≈0.033 при минуте работы", got)
	}
}

// Пустые окна из ответа выкидываются, а не заполняются нулями: ноль в p50
// панель прочтёт как «быстро», тогда как правда — «ещё ни одного замера».
func TestLatencyAPIHidesEmptyWindows(t *testing.T) {
	s := New(Config{
		Store:     testServer(t).cfg.Store,
		Now:       func() time.Time { return base },
		Inference: func() latency.Quantiles { return latency.Quantiles{} },
	})
	body := get(t, s, "/api/v1/metrics/latency", http.StatusOK)
	for _, key := range []string{"prediction_latency", "inference_latency", "throughput", "queue"} {
		if _, ok := body[key]; ok {
			t.Errorf("пустое окно %q утекло в ответ: %v", key, body)
		}
	}
	if _, ok := body["uptime_s"]; !ok {
		t.Error("нет uptime_s — единственное обязательное поле")
	}
}

// Без клиента модели панель должна получать честное 503 с объяснением, а не
// выдуманный паспорт: baseline-режим — штатный, и дашборд показывает его
// подписанной деградацией (критерий 5).
func TestModelProxyWithoutClient(t *testing.T) {
	s := testServer(t)
	body := get(t, s, "/api/v1/model", http.StatusServiceUnavailable)
	if body["error"] != "model_unavailable" {
		t.Errorf("error = %v, хотим model_unavailable", body["error"])
	}
}

func TestModelProxyServesPassport(t *testing.T) {
	ml := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/info" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":       "vTest",
			"target":        "delay_delta_s",
			"trained_at":    "2026-09-26T13:06:23Z",
			"features":      []string{"cur_dev_s"},
			"feature_count": 19,
			"mae":           map[string]float64{"mae_holdout_model": 68.28},
			"late": map[string]any{
				"version": "vTest_late", "threshold_s": 120.0, "auc_holdout": 0.916,
			},
		})
	}))
	defer ml.Close()

	s := New(Config{
		Store: testServer(t).cfg.Store,
		ML:    predictor.NewMLClient(predictor.MLConfig{BaseURL: ml.URL}),
		Now:   func() time.Time { return base },
	})
	body := get(t, s, "/api/v1/model", http.StatusOK)
	if body["version"] != "vTest" {
		t.Errorf("version = %v, хотим vTest", body["version"])
	}
	if body["trained_at"] != "2026-09-26T13:06:23Z" {
		t.Errorf("trained_at = %v — панель показывает дату обучения этим полем", body["trained_at"])
	}
	mae, ok := body["mae"].(map[string]any)
	if !ok || mae["mae_holdout_model"].(float64) != 68.28 {
		t.Errorf("mae = %v, хотим прокси метрик обучения без переименования", body["mae"])
	}
	if fc, _ := body["feature_count"].(float64); fc != 19 {
		t.Errorf("feature_count = %v, хотим 19", body["feature_count"])
	}
	late, ok := body["late"].(map[string]any)
	if !ok || late["version"] != "vTest_late" || late["auc_holdout"] != 0.916 {
		t.Fatalf("late = %v — паспорт классификатора обязан доезжать через структуру ModelInfo", body["late"])
	}
	// nil-указатель в LateInfo сериализуется null'ом, а не нулём: панель
	// различает «метрики нет» и «AUC нулевая».
	if v, has := late["positive_rate_holdout"]; !has || v != nil {
		t.Errorf("positive_rate_holdout = %v, отсутствующая метрика обязана быть null", late["positive_rate_holdout"])
	}
}
