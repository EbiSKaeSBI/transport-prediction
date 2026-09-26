package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// metrics — GET /metrics, текстовый формат Prometheus.
//
// Формат пишется руками, а не через prometheus/client_golang: набор метрик
// фиксирован и состоит из десятка чисел, а библиотека потянула бы за собой
// целое дерево зависимостей в пакет, которому по ADR 0002 положено держать
// минимальную поверхность. Ручной вывод означает, что при ошибке в метрике
// страдает только /metrics, а не весь процесс.
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var b strings.Builder
	metric := func(name, help, kind string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	value := func(name, help string, v float64) {
		metric(name, help, "gauge")
		fmt.Fprintf(&b, "%s %s\n", name, formatFloat(v))
	}
	counter := func(name, help string, v uint64) {
		metric(name, help, "counter")
		fmt.Fprintf(&b, "%s %d\n", name, v)
	}

	inc := s.incidents.Stats()
	value("transport_gateway_vehicles", "сколько устройств сейчас в хранилище телеметрии",
		float64(s.vehicleCount()))
	value("transport_gateway_incidents_open", "открытых инцидентов", float64(inc.Open))
	value("transport_gateway_incidents_acked", "инцидентов взято в работу", float64(inc.Acked))
	value("transport_gateway_incidents_resolved", "закрытых инцидентов", float64(inc.Resolved))
	incStats := s.incidents.Counters()
	counter("transport_gateway_incidents_created_total", "сколько инцидентов открыто за всё время",
		incStats.Created)
	counter("transport_gateway_incidents_acked_total", "сколько раз инцидент взяли в работу",
		incStats.Acked)

	pred := s.preds.Stats()
	value("transport_gateway_predictions_stored", "сколько прогнозов в хранилище", float64(pred.Stored))
	counter("transport_gateway_predictions_total", "сколько прогнозов прошло за всё время", pred.Total)

	// Доля деградации интересует дежурного больше абсолютного числа
	// прогнозов: она растёт именно тогда, когда модель молчит.
	if fb, ok := s.cfg.Predictor.(fallbackReporter); ok {
		fst := fb.Stats()
		if fst.Total > 0 {
			value("transport_gateway_prediction_source_ratio",
				"доля прогнозов, взятых не из модели",
				float64(fst.Degraded())/float64(fst.Total))
		}
		counter("transport_gateway_predictions_from_model_total",
			"прогнозов от модели", fst.FromModel)
		counter("transport_gateway_predictions_from_cache_total",
			"прогнозов из кэша устаревших ответов", fst.FromCache)
		counter("transport_gateway_predictions_from_baseline_total",
			"прогнозов по baseline", fst.FromBase)
	}

	// Состояние клиента модели: доля деградации выше отвечает на вопрос
	// «много ли прогнозов минуло модель», эти метрики — на вопрос «почему».
	if ml := s.cfg.ML; ml != nil {
		ms := ml.Stats()
		contractOK := 1.0
		if ms.ContractErr != nil {
			contractOK = 0
		}
		value("transport_gateway_ml_contract_ok",
			"1 если контракт признаков модели совпал с кадром", contractOK)
		counter("transport_gateway_ml_rejected_total",
			"обращений к модели отклонено автоматом клиента", ms.Rejected)
	}

	// Латентность прогнозов: при отказе модели её рост — первый признак
	// того, что пора смотреть в её логи.
	lat := s.latency.Snapshot()
	if lat.Count > 0 {
		value("transport_gateway_prediction_latency_p50_s", "медиана латентности прогноза", lat.P50)
		value("transport_gateway_prediction_latency_p95_s", "95-й перцентиль", lat.P95)
		value("transport_gateway_prediction_latency_p99_s", "99-й перцентиль", lat.P99)
		value("transport_gateway_prediction_latency_max_s", "максимум по окну", lat.Max)
		counter("transport_gateway_prediction_latency_samples_total", "замеров латентности всего", lat.Total)
	}

	// Очередь прогнозов. До этого блока отброшенные кадры были видны
	// только в текстовой сводке при остановке, а очередь — нигде: молчащая
	// потеря кадров при живом процессе выглядит как «всё спокойно», и это
	// худший вид молчания, потому что данные уже не вернуть.
	if s.cfg.Queue != nil {
		q := s.cfg.Queue()
		value("transport_gateway_prediction_queue_depth", "кадров ждут модели", float64(q.Depth))
		counter("transport_gateway_predictions_submitted_total", "кадров принято планировщиком",
			uint64(max64(q.Submitted, 0)))
		counter("transport_gateway_predictions_predicted_total", "кадров дошло до ответа",
			uint64(max64(q.Predicted, 0)))
		counter("transport_gateway_predictions_dropped_total",
			"кадров отброшено из-за полной очереди", uint64(max64(q.Dropped, 0)))
		counter("transport_gateway_predictions_abandoned_total",
			"кадров осталось в очереди при остановке", uint64(max64(q.Abandoned, 0)))
		counter("transport_gateway_prediction_batches_total", "обращений к модели батчем",
			uint64(max64(q.Batches, 0)))
		counter("transport_gateway_prediction_batched_frames_total", "кадров, обслуженных батчем",
			uint64(max64(q.BatchedFrames, 0)))
		if q.BatchSize.Total > 0 {
			bs := q.BatchSize
			value("transport_gateway_prediction_batch_size_p50", "медиана размера пачки", bs.P50)
			value("transport_gateway_prediction_batch_size_p95", "95-й перцентиль размера пачки", bs.P95)
			value("transport_gateway_prediction_batch_size_p99", "99-й перцентиль размера пачки", bs.P99)
		}
	}

	// Латентность самой модели. Отдельно от латентности прогноза выше:
	// вместе они отвечают на вопрос «мы медленные или модель», который по
	// одному числу неразличим.
	if s.cfg.Inference != nil {
		inf := s.cfg.Inference()
		if inf.Total > 0 {
			value("transport_gateway_inference_latency_p50_s", "медиана времени обращения к модели", inf.P50)
			value("transport_gateway_inference_latency_p95_s", "95-й перцентиль", inf.P95)
			value("transport_gateway_inference_latency_p99_s", "99-й перцентиль", inf.P99)
			value("transport_gateway_inference_latency_max_s", "максимум по окну", inf.Max)
			counter("transport_gateway_inference_latency_samples_total", "замеров инференции всего", inf.Total)
		}
	}

	// Лента. Молчащая лента при живой панели выглядит как «всё спокойно»,
	// поэтому число подписчиков и число отброшенных событий — не
	// справочные, а сигнальные метрики.
	if h := s.Hub(); h != nil {
		hs := h.Stats()
		value("transport_gateway_ws_clients", "подписчиков ленты событий", float64(hs.Clients))
		counter("transport_gateway_ws_sent_total", "событий разослано за всё время", hs.Sent)
		counter("transport_gateway_ws_dropped_total",
			"событий отброшено как слишком частые", hs.Dropped)
		counter("transport_gateway_ws_refused_total",
			"подписчиков отключено за неспособность читать", hs.Refused)
	}

	value("transport_gateway_uptime_s", "сколько работает гейтвей",
		s.now().Sub(s.startedAt).Seconds())
	if !readyzOK(s.cfg) {
		value("transport_gateway_ready", "1, если гейтвей готов считать прогнозы", 0)
	} else {
		value("transport_gateway_ready", "1, если гейтвей готов считать прогнозы", 1)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// QueueStats — снимок очереди прогнозов в момент чтения /metrics.
type QueueStats struct {
	// Depth — кадров ждут модели прямо сейчас. Растёт, когда модель не
	// справляется, и это первый признак, что пора смотреть в её сторону.
	Depth int
	// Submitted, Predicted, Dropped, Abandoned — счётчики планировщика за
	// всё время работы.
	Submitted, Predicted, Dropped, Abandoned int64
	// Batches, BatchedFrames — обращения батчем и кадров в них.
	Batches, BatchedFrames int64
	// BatchSize — распределение размеров пачек. Помогает отличить «батчинг
	// не настроен» от «батчинг настроен, но выродился в пачки по одному
	// кадру»: счётчики в обоих случаях выглядят правдоподобно.
	BatchSize latency.Quantiles
}

// fallbackReporter — предиктор, умеющий рассказать о своей деградации.
// Отдельный интерфейс, потому что знать о нём должен только /metrics, а
// Predictor про счётчики не знает и знать не должен.
type fallbackReporter interface {
	Stats() predictor.StatsFallback
}

func (s *Server) vehicleCount() int {
	if s.cfg.Store == nil {
		return 0
	}
	return len(s.cfg.Store.Units())
}

// max64 защищает вывод счётчиков от отрицательных значений. Счётчики
// планировщика растут монотонно, но берутся извне, а метрика с минусом в
// экспорте выглядит как ошибка данных и портит графики у того, кто их читает.
func max64(v, floor int64) int64 {
	if v < floor {
		return floor
	}
	return v
}

func readyzOK(cfg Config) bool {
	return cfg.Store != nil && cfg.Schedule != nil && cfg.Predictor != nil
}

// formatFloat печатает число в формате, который переживает разбор Prometheus.
// Через strconv с 'g' целые значения печатаются как 1, а не 1.0, и это
// экономит несколько байт на каждой метрике.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
