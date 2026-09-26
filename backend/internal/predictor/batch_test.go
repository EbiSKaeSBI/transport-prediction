package predictor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
)

// frameOf собирает кадр конкретной машины. Разные UnitID нужны там, где
// проверяется кэш Fallback: он ключует прогнозы по машине, и два кадра
// одной машины в батче перетирали бы друг друга.
func frameOf(t *testing.T, unit uint32, trid int64, asOf time.Time, dev float64) horizon.Frame {
	t.Helper()
	f := frameAt(t, asOf, dev)
	f.UnitID = unit
	f.TRID = trid
	f.SampleID = horizon.FormatSampleID(trid, asOf)
	return f
}

// batchOf — три кадра разных машин.
func batchOf(t *testing.T) []horizon.Frame {
	t.Helper()
	return []horizon.Frame{
		frameOf(t, 101, 11, base, 60),
		frameOf(t, 102, 12, base, 30),
		frameOf(t, 103, 13, base, 90),
	}
}

// batchHandler — мок /predict/batch, который считает обращения и отвечает
// переданной функцией. Сама подстановка ответов вынесена в тесты: важно
// уметь отдать ответы вразнобой, не в том порядке, в каком спросили.
func batchHandler(t *testing.T, reply func(frames []predictRequest) []predictResponse) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(modelInfoBody())
		case "/predict/batch":
			calls.Add(1)
			var in batchRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(batchResponse{Predictions: reply(in.Frames)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// total — сервис по ADR-0007 отвечает готовой суммой: к измеренному
// отклонению кадра добавляем выученную приращию. Фейк считает её так же,
// иначе клиент при разборе вычтет не то.
func total(f predictRequest, delta float64) float64 {
	if v, ok := f.Features["cur_dev_s"]; ok && v != nil {
		return *v + delta
	}
	return delta
}

// okFor — честный ответ по каждому кадру: добавка 10, p_late 0.5.
func okFor(frames []predictRequest) []predictResponse {
	out := make([]predictResponse, len(frames))
	for i, f := range frames {
		out[i] = predictResponse{
			SampleID: f.SampleID, DelayS: ptr(total(f, 10.0)), PLate: ptr(0.5), ModelVersion: "v1",
		}
	}
	return out
}

func TestPredictBatchSendsOneRequest(t *testing.T) {
	srv, calls := batchHandler(t, okFor)
	c := NewMLClient(MLConfig{BaseURL: srv.URL})

	ps, err := c.PredictBatch(t.Context(), batchOf(t))
	if err != nil {
		t.Fatalf("батч: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("обращений к /predict/batch %d, ожидалось 1: батчинг обязан заменить круги, а не добавить запрос", n)
	}
	if len(ps) != 3 {
		t.Fatalf("прогнозов %d, ожидалось 3", len(ps))
	}
	for i, p := range ps {
		if p.Source != SourceML {
			t.Errorf("кадр %d: источник %q, ожидалась модель", i, p.Source)
		}
	}
}

// Главное свойство батча: ответы приходят в том же порядке, что и кадры.
// Сервис вправе считать батч как угодно — хоть параллельно, хоть
// отсортировав ответы по времени обработки, — и клиент обязан разложить их по
// кадрам сам. Сопоставление по позиции дало бы прогноз одной машины в
// ответе про другую, и на линии это неверное время ожидания, у которого нет
// способа себя оправдать.
func TestPredictBatchMatchesReorderedResponses(t *testing.T) {
	srv, _ := batchHandler(t, func(frames []predictRequest) []predictResponse {
		out := okFor(frames)
		slices.Reverse(out)
		return out
	})
	c := NewMLClient(MLConfig{BaseURL: srv.URL})

	frames := batchOf(t)
	ps, err := c.PredictBatch(t.Context(), frames)
	if err != nil {
		t.Fatalf("батч: %v", err)
	}
	for i, p := range ps {
		if p.SampleID != frames[i].SampleID {
			t.Errorf("позиция %d: прогноз кадра %q попал на кадр %q",
				i, p.SampleID, frames[i].SampleID)
		}
		if p.UnitID != frames[i].UnitID {
			t.Errorf("позиция %d: unit %d вместо %d", i, p.UnitID, frames[i].UnitID)
		}
	}
}

// Кадр в батче должен узнаваться по имени, а добавка приходить та, что
// посчитана для него. Здесь каждый ответ получает добавку, равную номеру
// кадра, — так видно, что разложение по кадрам настоящее, а не совпадение
// первых байтов.
func TestPredictBatchKeepsPerFrameAnswer(t *testing.T) {
	srv, _ := batchHandler(t, func(frames []predictRequest) []predictResponse {
		out := make([]predictResponse, len(frames))
		for i, f := range frames {
			out[i] = predictResponse{
				SampleID: f.SampleID, DelayS: ptr(total(f, float64(i))), PLate: ptr(0.5),
			}
		}
		slices.Reverse(out)
		return out
	})
	c := NewMLClient(MLConfig{BaseURL: srv.URL})

	ps, err := c.PredictBatch(t.Context(), batchOf(t))
	if err != nil {
		t.Fatalf("батч: %v", err)
	}
	for i, p := range ps {
		if p.DeltaS != float64(i) {
			t.Errorf("кадр %d: добавка %v вместо %v", i, p.DeltaS, float64(i))
		}
	}
}

// Ответ про чужой кадр обязан быть отказом, а не «попробуем приложить к
// первому попавшему».
func TestPredictBatchRejectsForeignSampleID(t *testing.T) {
	srv, _ := batchHandler(t, func(frames []predictRequest) []predictResponse {
		out := okFor(frames)
		out[0].SampleID = "не наш кадр"
		return out
	})
	c := NewMLClient(MLConfig{BaseURL: srv.URL})

	ps, err := c.PredictBatch(t.Context(), batchOf(t))
	if err == nil {
		t.Fatal("чужой кадр в ответе обязан быть отказом")
	}
	if ps != nil {
		t.Errorf("при отказе прогнозов быть не должно, получено %d", len(ps))
	}
}

// Недостающий ответ — тоже отказ: приложить его не к чему, а выдумывать
// «как-нибудь» здесь нельзя.
func TestPredictBatchRejectsMissingAnswer(t *testing.T) {
	srv, _ := batchHandler(t, func(frames []predictRequest) []predictResponse {
		return okFor(frames)[:1]
	})
	c := NewMLClient(MLConfig{BaseURL: srv.URL})

	if _, err := c.PredictBatch(t.Context(), batchOf(t)); err == nil {
		t.Fatal("недостающий ответ обязан быть отказом")
	}
}

// Повтор одного кадра в ответе означает, что сервис посчитал что-то не то, и
// приложить его однозначно нельзя.
func TestPredictBatchRejectsDuplicatedAnswer(t *testing.T) {
	srv, _ := batchHandler(t, func(frames []predictRequest) []predictResponse {
		out := okFor(frames)
		out[1] = out[0]
		return out
	})
	c := NewMLClient(MLConfig{BaseURL: srv.URL})

	if _, err := c.PredictBatch(t.Context(), batchOf(t)); err == nil {
		t.Fatal("дубль ответа обязан быть отказом")
	}
}

// Один кадр идёт одиночным запросом, а не батчем из одного элемента: держать
// для этого отдельный путь незачем, и только так клиент совместим с сервисом,
// который /predict/batch не реализовал.
func TestSingleFrameSkipsBatchEndpoint(t *testing.T) {
	var singles, batches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(modelInfoBody())
		case "/predict":
			singles.Add(1)
			_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(1.0), PLate: ptr(0.1)})
		case "/predict/batch":
			batches.Add(1)
			_ = json.NewEncoder(w).Encode(batchResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL})
	if _, err := c.PredictBatch(t.Context(), batchOf(t)[:1]); err != nil {
		t.Fatalf("одиночный кадр: %v", err)
	}
	if singles.Load() != 1 {
		t.Errorf("одиночных запросов %d, ожидался 1", singles.Load())
	}
	if batches.Load() != 0 {
		t.Errorf("батчей %d, ожидался 0: одиночный кадр ходит в /predict", batches.Load())
	}
}

// Сервис без /predict/batch — не поломка, а отсутствующая оптимизация.
// Клиент обязан это заметить один раз и больше не стучаться в заведомо мёртвый
// путь: иначе каждый тик он создаёт нагрузку на сервис, который не сможет её
// обслужить никогда.
func TestBatchEndpointMissingIsRemembered(t *testing.T) {
	var singles, batches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(modelInfoBody())
		case "/predict":
			singles.Add(1)
			_ = json.NewEncoder(w).Encode(predictResponse{DelayS: ptr(1.0), PLate: ptr(0.1)})
		case "/predict/batch":
			batches.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewMLClient(MLConfig{BaseURL: srv.URL, Cooldown: time.Hour})
	for range 3 {
		if _, err := c.PredictBatch(t.Context(), batchOf(t)); err == nil {
			t.Fatal("батч на сервисе без эндпоинта обязан быть отказом")
		}
	}
	if n := batches.Load(); n != 1 {
		t.Errorf("обращений к /predict/batch %d, ожидалось 1: клиент обязан запомнить, что эндпоинта нет", n)
	}
	if n := singles.Load(); n != 0 {
		t.Errorf("одиночных запросов %d, ожидалось 0: Fallback разберётся сам", n)
	}
}

// Метрика латентности прогноза была мёртвой: поле заполнял никто, поэтому
// окно гейтвея не получало ни одного замера, а квантили в /metrics держали
// нули. Прогноз обязан нести время своего ответа.
func TestPredictionCarriesLatency(t *testing.T) {
	srv, _ := batchHandler(t, okFor)
	c := NewMLClient(MLConfig{BaseURL: srv.URL})

	ps, err := c.PredictBatch(t.Context(), batchOf(t))
	if err != nil {
		t.Fatalf("батч: %v", err)
	}
	for i, p := range ps {
		if p.Latency <= 0 {
			t.Errorf("кадр %d: латентность %v, ожидалось ненулевое измерение", i, p.Latency)
		}
	}
	q := c.Inference()
	if q.Total != 1 {
		t.Errorf("замеров инференции %d, ожидался 1: один батч — одно обращение", q.Total)
	}
	if q.P50 <= 0 {
		t.Errorf("медиана инференции %v, ожидалось ненулевое измерение", q.P50)
	}
}
