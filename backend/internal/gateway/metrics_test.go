package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
)

// Очередь прогнозов обязана быть видна в метриках. Отброшенные кадры до
// этого были видны только в текстовой сводке при остановке, а очередь —
// нигде: потеря данных при живом процессе выглядит снаружи как «всё
// спокойно».
func TestMetricsReportPredictionQueue(t *testing.T) {
	s := New(Config{
		Store:     testServer(t).cfg.Store,
		Predictor: &stubPredictor{},
		Now:       func() time.Time { return base },
		Queue: func() QueueStats {
			return QueueStats{
				Depth:         7,
				Submitted:     100,
				Predicted:     92,
				Dropped:       3,
				Abandoned:     1,
				Batches:       20,
				BatchedFrames: 85,
				BatchSize: latency.Quantiles{
					Count: 20, Total: 20, P50: 4, P95: 16, P99: 16,
				},
			}
		},
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d на /metrics", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"transport_gateway_prediction_queue_depth 7",
		"transport_gateway_predictions_submitted_total 100",
		"transport_gateway_predictions_predicted_total 92",
		// Отброшенные кадры — единственная метрика, после которой данные
		// не вернуть: она обязана быть на видном месте.
		"transport_gateway_predictions_dropped_total 3",
		"transport_gateway_predictions_abandoned_total 1",
		"transport_gateway_prediction_batches_total 20",
		"transport_gateway_prediction_batched_frames_total 85",
		"transport_gateway_prediction_batch_size_p50 4",
		"transport_gateway_prediction_batch_size_p95 16",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в метриках нет %q:\n%s", want, body)
		}
	}
}

// Латентность инференции публикуется отдельно от латентности прогноза: вместе
// они отвечают на вопрос «мы медленные или модель», который по одному числу
// неразличим.
func TestMetricsReportInferenceLatency(t *testing.T) {
	s := New(Config{
		Store:     testServer(t).cfg.Store,
		Predictor: &stubPredictor{},
		Now:       func() time.Time { return base },
		Inference: func() latency.Quantiles {
			return latency.Quantiles{
				Count: 50, Total: 50, P50: 0.12, P95: 0.4, P99: 0.9, Max: 1.2,
			}
		},
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"transport_gateway_inference_latency_p50_s 0.12",
		"transport_gateway_inference_latency_p95_s 0.4",
		"transport_gateway_inference_latency_p99_s 0.9",
		"transport_gateway_inference_latency_max_s 1.2",
		"transport_gateway_inference_latency_samples_total 50",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в метриках нет %q:\n%s", want, body)
		}
	}
}

// Без модели замеров нет, и молчание метрик — честный ответ. Метрика с
// нулями выглядела бы как «модель отвечает мгновенно», а это правдоподобная
// неправда.
func TestMetricsOmitInferenceWithoutModel(t *testing.T) {
	s := New(Config{Store: testServer(t).cfg.Store, Predictor: &stubPredictor{},
		Now: func() time.Time { return base }})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if strings.Contains(body, "transport_gateway_inference_latency_p50_s") {
		t.Errorf("без модели метрик инференции быть не должно:\n%s", body)
	}
	if strings.Contains(body, "transport_gateway_prediction_queue_depth") {
		t.Errorf("без планировщика метрик очереди быть не должно:\n%s", body)
	}
}
