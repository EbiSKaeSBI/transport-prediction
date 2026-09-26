package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

var base = time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)

// testServer собирает гейтвей с управляемыми часами и одним устройством.
func testServer(t *testing.T) *Server {
	t.Helper()
	store := statestore.New()
	at := base
	store.Append(statestore.Point{
		UnitID: 4242, EventTime: at, ReceiveTime: at.Add(250 * time.Millisecond),
		Latitude: 55.8, Longitude: 37.6023, SpeedKmh: 18,
		LocationValid: true, Satellites: 10, CourseDeg: 90,
	})
	binding, err := schedule.LoadBinding(strings.NewReader(bindingCSV))
	if err != nil {
		t.Fatalf("привязка не загрузилась: %v", err)
	}
	return New(Config{
		Store:   store,
		Binding: binding,
		Now:     func() time.Time { return at },
	})
}

// bindingCSV — телеметрия одной машины: привязка читается из заголовков
// tr_id и unit_id, поэтому нужен настоящий формат, а не выдуманный.
const bindingCSV = `tr_id,unit_id
7,4242
`

// get делает запрос и требует указанный код.
func get(t *testing.T, s *Server, path string, want int) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != want {
		t.Fatalf("%s: код %d, ожидался %d; тело: %s", path, rec.Code, want, rec.Body)
	}
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: тело не разобрано: %v; сырое: %s", path, err, rec.Body)
		}
	}
	return body
}

// predictionAt собирает прогноз для тестов инцидентов.
func predictionAt(unit uint32, dev, plate float64) predictor.Prediction {
	d := dev - 30
	return predictor.Prediction{
		SampleID: "7_1767686400", UnitID: unit, TRID: 7,
		AsOf: base, TargetStopID: 114, HorizonS: 600,
		CurDevS: &d, PredictedDevS: dev, PLate: plate, HasPLate: true,
		Source: predictor.SourceML,
	}
}

func TestHealthIsAlwaysOK(t *testing.T) {
	s := testServer(t)
	body := get(t, s, "/healthz", http.StatusOK)
	if body["status"] != "ok" {
		t.Errorf("статус %v, ожидался ok", body["status"])
	}
}

// Готовность и живость — разные вещи. Гейтвей без модели продолжает
// показывать телеметрию, но обещать прогнозы не может, и /readyz обязан
// это сказать, а не рапортовать об успехе.
func TestReadinessFailsWithoutPredictor(t *testing.T) {
	s := testServer(t)
	body := get(t, s, "/readyz", http.StatusServiceUnavailable)
	if body["ready"] != false {
		t.Errorf("ready %v без предиктора, ожидалось false", body["ready"])
	}
	checks, _ := body["checks"].([]any)
	found := false
	for _, c := range checks {
		m, _ := c.(map[string]any)
		if m["name"] == "predictor" && m["ok"] == false {
			found = true
		}
	}
	if !found {
		t.Errorf("в проверках нет неготового предиктора: %v", body["checks"])
	}
}

func TestReadinessFailsWithoutStore(t *testing.T) {
	s := New(Config{Now: func() time.Time { return base }})
	get(t, s, "/readyz", http.StatusServiceUnavailable)
	// /healthz при этом обязан отвечать: процесс жив, он просто бесполезен.
	get(t, s, "/healthz", http.StatusOK)
}

func TestVehiclesListShowsState(t *testing.T) {
	s := testServer(t)
	body := get(t, s, "/api/v1/vehicles", http.StatusOK)
	list, _ := body["vehicles"].([]any)
	if len(list) != 1 {
		t.Fatalf("машин %d, ожидалась 1: %v", len(list), body)
	}
	v, _ := list[0].(map[string]any)
	if v["unit_id"] != float64(4242) {
		t.Errorf("unit_id %v, ожидался 4242", v["unit_id"])
	}
	if v["speed_kmh"] != float64(18) {
		t.Errorf("скорость %v, ожидалось 18", v["speed_kmh"])
	}
	if v["location_valid"] != true {
		t.Error("координаты должны быть отмечены достоверными")
	}
}

// Панель присылает сюда и номер ТС, и строковый идентификатор, и оба должны
// отличаться от молчаливого нуля: устройство 0 в карточке выглядело бы как
// «машина есть, но неизвестна».
func TestBadIdentifiersAreRejected(t *testing.T) {
	s := testServer(t)
	for _, id := range []string{"abc", "0", "-1", "%20"} {
		got := get(t, s, "/api/v1/vehicles/"+id, http.StatusBadRequest)
		if got["detail"] == nil {
			t.Errorf("для %q нет пояснения: %v", id, got)
		}
	}
	got := get(t, s, "/api/v1/vehicles/999999", http.StatusNotFound)
	if got["detail"] == nil {
		t.Errorf("для несуществующей машины нет пояснения: %v", got)
	}
}

func TestTrajectoryReturnsHeldWindow(t *testing.T) {
	s := testServer(t)
	body := get(t, s, "/api/v1/vehicles/4242/trajectory", http.StatusOK)
	points, _ := body["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("точек %d, ожидалась 1", len(points))
	}
	// Вместимость хранилища обязана попадать в ответ: иначе клиент решит,
	// что у него вся история смены, а её там физически нет.
	if body["capacity"] == nil {
		t.Errorf("в ответе нет вместимости: %v", body)
	}
}

func TestTrajectoryEmptyIsNotFound(t *testing.T) {
	s := testServer(t)
	get(t, s, "/api/v1/vehicles/7777/trajectory", http.StatusNotFound)
}

func TestTrajectoryWindowFilter(t *testing.T) {
	store := statestore.New()
	for i := range 5 {
		at := base.Add(time.Duration(i) * time.Minute)
		store.Append(statestore.Point{
			UnitID: 4242, EventTime: at, ReceiveTime: at,
			Latitude: 55.8, Longitude: 37.6023, LocationValid: true,
		})
	}
	s := New(Config{Store: store, Now: func() time.Time { return base }})
	body := get(t, s, "/api/v1/vehicles/4242/trajectory"+
		"?from=2026-01-06T08:01:00Z&to=2026-01-06T08:03:00Z", http.StatusOK)
	if body["count"] != float64(3) {
		t.Errorf("в окне %v точек, ожидалось 3: %v", body["count"], body["points"])
	}
	// Кривой интервал — ошибка клиента, а не пустой результат.
	get(t, s, "/api/v1/vehicles/4242/trajectory?from=не-время", http.StatusBadRequest)
	get(t, s, "/api/v1/vehicles/4242/trajectory?from=2026-01-06T08:03:00Z"+
		"&to=2026-01-06T08:01:00Z", http.StatusBadRequest)
}

func TestRoutesNeedSchedule(t *testing.T) {
	s := testServer(t)
	get(t, s, "/api/v1/routes", http.StatusServiceUnavailable)
}

func TestRoutesAndStops(t *testing.T) {
	// Расписание читается настоящим загрузчиком, чтобы эндпоинт проверял
	// то же, что читает конвейер, а не подменённую структуру.
	sch, err := schedule.Load(strings.NewReader(scheduleCSV))
	if err != nil {
		t.Fatalf("расписание не загрузилось: %v", err)
	}
	s := New(Config{Store: statestore.New(), Schedule: sch,
		Now: func() time.Time { return base }})

	body := get(t, s, "/api/v1/routes", http.StatusOK)
	if body["count"] == float64(0) {
		t.Error("маршрутов 0, расписание загружено")
	}
	stops := get(t, s, "/api/v1/routes/7/stops", http.StatusOK)
	list, _ := stops["stops"].([]any)
	if len(list) == 0 {
		t.Error("остановок 0, расписание загружено")
	}
	get(t, s, "/api/v1/routes/0/stops", http.StatusBadRequest)
	get(t, s, "/api/v1/routes/999/stops", http.StatusNotFound)
}

// scheduleCSV — минимальный валидный план-график одной машины. Имена колонок
// и POINT в geom взяты из настоящего загрузчика: выдуманный формат прошёл бы
// тест на подменённой структуре и упал бы в бою.
const scheduleCSV = `tt_action_item_id,tr_id,time_begin,time_fact_begin,order_date,geom,building_address,manual_fill
114,7,2026-01-06 08:10:00,,2026-01-06,POINT (37.6023 55.8),"Остановка 1",0
115,7,2026-01-06 08:20:00,,2026-01-06,POINT (37.61 55.81),"Остановка 2",0
`

func TestPredictionRoundTrip(t *testing.T) {
	s := testServer(t)
	s.Observe(predictionAt(4242, 180, 0.8))

	body := get(t, s, "/api/v1/predictions/7_1767686400", http.StatusOK)
	if body["predicted_dev_s"] != float64(180) {
		t.Errorf("отклонение %v, ожидалось 180", body["predicted_dev_s"])
	}
	// Источник обязан ехать вместе с числом: ответ модели, кэш и baseline —
	// три разных утверждения с разной надёжностью.
	if body["source"] != string(predictor.SourceML) {
		t.Errorf("источник %v, ожидался ml", body["source"])
	}
	if body["risk"] != string(RiskRed) {
		t.Errorf("риск %v при отклонении 180, ожидался red", body["risk"])
	}
	// измеренное отклонение «сейчас» едет рядом с прогнозом у цели: без него
	// панель не может объяснить зелёный цвет машины, которая уже позади
	if body["cur_dev_s"] != float64(150) {
		t.Errorf("cur_dev_s %v, ожидалось 150", body["cur_dev_s"])
	}
	get(t, s, "/api/v1/predictions/нет-такого", http.StatusNotFound)
}

// Нулевая вероятность и отсутствие вероятности — разные вещи, и провод обязан
// это различать. Baseline не оценивает вероятность вовсе, поэтому p_late едет
// null; 0 означал бы «опоздания не будет» — утверждение, которого никто не
// делал, и панель показывала бы его как уверенный прогноз.
func TestBaselinePlateIsNullNotZero(t *testing.T) {
	s := testServer(t)
	p := predictionAt(4242, 180, 0)
	p.Source = predictor.SourceBaseline
	p.PLate = 0
	p.HasPLate = false
	p.Reason = ""
	s.Observe(p)

	body := get(t, s, "/api/v1/predictions/7_1767686400", http.StatusOK)
	v, ok := body["p_late"]
	if !ok {
		t.Fatal("ключ p_late обязан присутствовать: меняется значение, а не форма ответа")
	}
	if v != nil {
		t.Errorf("p_late %v у baseline, ожидался null", v)
	}
}

// У модели с головой вероятностей число обязано дойти без искажения.
func TestModelPlateReachesWire(t *testing.T) {
	s := testServer(t)
	s.Observe(predictionAt(4242, 180, 0.8))

	body := get(t, s, "/api/v1/predictions/7_1767686400", http.StatusOK)
	if body["p_late"] != float64(0.8) {
		t.Errorf("p_late %v, ожидалось 0.8", body["p_late"])
	}
}

// Карточка инцидента наследует то же правило: у инцидента, открытого по
// отклонению без модели, вероятности тоже нет.
func TestIncidentPlateIsNullForBaseline(t *testing.T) {
	s := testServer(t)
	p := predictionAt(4242, 180, 0)
	p.Source = predictor.SourceBaseline
	p.HasPLate = false
	s.Observe(p)

	incs := get(t, s, "/api/v1/incidents?status=open", http.StatusOK)
	list, _ := incs["incidents"].([]any)
	if len(list) != 1 {
		t.Fatalf("открытых инцидентов %d, ожидался 1: %v", len(list), incs)
	}
	card, _ := list[0].(map[string]any)
	if v, ok := card["p_late"]; !ok || v != nil {
		t.Errorf("p_late в карточке инцидента %v (ключ есть: %v), ожидался null", v, ok)
	}
}

// Прогноз, посчитанный через POST, обязан попасть в карточку машины: иначе
// приборная панель не увидит ответ на свой же запрос.
func TestPredictFillsVehicleCard(t *testing.T) {
	stub := &stubPredictor{delta: 20, plate: 0.1}
	s := New(Config{
		Store: testServer(t).cfg.Store, Predictor: stub,
		Now: func() time.Time { return base },
	})
	body := postPredict(t, s, `{"unit_id":4242,"tr_id":7,"t":"2026-01-06T08:00:00Z",
		"target_stop_id":114,"features":{"cur_dev_s":50}}`)
	if body["delta_s"] != float64(20) {
		t.Errorf("добавка %v, ожидалось 20", body["delta_s"])
	}
	if body["predicted_dev_s"] != float64(70) {
		t.Errorf("отклонение %v, ожидалось 70 (50+20)", body["predicted_dev_s"])
	}
}

// POST /predict не должен засорять историю: разовый запрос, а не прогноз
// конвейера, и иначе он вытеснял бы настоящие прогнозы.
func TestPredictDoesNotStoreHistory(t *testing.T) {
	s := New(Config{Store: testServer(t).cfg.Store, Predictor: &stubPredictor{},
		Now: func() time.Time { return base }})
	postPredict(t, s, `{"unit_id":4242,"t":"2026-01-06T08:00:00Z",
		"target_stop_id":114,"features":{}}`)
	if st := s.preds.Stats(); st.Stored != 0 {
		t.Errorf("в хранилище %d прогнозов после POST, ожидался 0", st.Stored)
	}
}

// Опечатка в имени признака обязана быть ошибкой 400. Молча пропущенный
// признак уехал бы в модель как «не измерено», и ошибка обнаружилась бы
// только на скоринге.
func TestPredictRejectsUnknownFeatureName(t *testing.T) {
	s := New(Config{Store: statestore.New(), Predictor: &stubPredictor{},
		Now: func() time.Time { return base }})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/predict",
		strings.NewReader(`{"unit_id":1,"t":"2026-01-06T08:00:00Z","target_stop_id":114,
			"features":{"cur_dev_с":10}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "cur_dev_") {
		t.Errorf("в ответе нет имени опечатки: %s", rec.Body)
	}
}

// Пропущенные признаки обязаны доехать до предиктора как nil, а не без
// ключей: иначе через API вернётся ровно тот дефект, который чинили в
// horizon, и модель снова получит 17 колонок вместо 21.
func TestPredictFillsEveryContractName(t *testing.T) {
	sp := &stubPredictor{}
	s := New(Config{Store: statestore.New(), Predictor: sp,
		Now: func() time.Time { return base }})
	postPredict(t, s, `{"unit_id":1,"t":"2026-01-06T08:00:00Z","target_stop_id":114,
		"features":{"cur_dev_s":10}}`)

	contract := predictor.FeatureContract()
	if len(sp.seen) != len(contract) {
		t.Fatalf("предиктор увидел %d признаков, контракт %d", len(sp.seen), len(contract))
	}
	for _, name := range contract {
		if _, ok := sp.seen[name]; !ok {
			t.Errorf("признак %s не доехал до предиктора как ключ", name)
		}
	}
	if sp.seen["cur_dev_s"] == nil {
		t.Error("переданный cur_dev_s потерялся")
	}
	if sp.seen["headway_s"] != nil {
		t.Error("непереданный headway_s должен быть nil, а не нулём")
	}
}

func TestPredictRequiresMomentAndTarget(t *testing.T) {
	s := New(Config{Store: statestore.New(), Predictor: &stubPredictor{},
		Now: func() time.Time { return base }})
	for _, body := range []string{
		`{"unit_id":1,"target_stop_id":114,"features":{}}`,
		`{"unit_id":1,"t":"2026-01-06T08:00:00Z","features":{}}`,
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/predict", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("код %d для %s, ожидался 400", rec.Code, body)
		}
	}
}

func TestPredictNeedsPredictor(t *testing.T) {
	s := New(Config{Store: statestore.New(), Now: func() time.Time { return base }})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/predict",
		strings.NewReader(`{"unit_id":1,"t":"2026-01-06T08:00:00Z","target_stop_id":114}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d без предиктора, ожидался 503", rec.Code)
	}
}

// Метод проверяется стандартной библиотекой: POST на /healthz обязан вернуть
// 405, а не 404, иначе опечатка в методе выглядит как отсутствующий путь.
func TestWrongMethodIsRejected(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("код %d на POST /healthz, ожидался 405", rec.Code)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/нет-такого", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("код %d, ожидался 404", rec.Code)
	}
}

// predictionFrame — кадр для сквозного пути «посчитали, потом положили».
func predictionFrame() horizon.Frame {
	return horizon.Frame{
		SampleID: "7_1767686400", UnitID: 4242, TRID: 7, AsOf: base,
		Target: []horizon.Arrival{{ActionID: 114, HorizonS: 600}},
		Values: map[string]*float64{"cur_dev_s": ptr(200.0)},
	}
}

func ptr[T any](v T) *T { return &v }

func TestMetricsReportDegradation(t *testing.T) {
	model := &stubPredictor{delta: 10, plate: 0.1}
	fb := predictor.NewFallback(model, predictor.BaselinePredictor{}, time.Minute)
	s := New(Config{Store: testServer(t).cfg.Store, Predictor: fb,
		Now: func() time.Time { return base }})

	model.err = errStubDown
	// Счётчики деградации наполняет Predict, а не Observe: Observe только
	// принимает уже посчитанный прогноз. Поэтому тест идёт по пути
	// планировщика — посчитать, потом положить, — иначе проверял бы счётчики,
	// которые никто не двигает.
	frame := predictionFrame()
	for range 3 {
		s.Observe(fb.Predict(t.Context(), frame))
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d на /metrics", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE transport_gateway_incidents_open gauge",
		"transport_gateway_incidents_open 1",
		"transport_gateway_predictions_from_baseline_total 3",
		"transport_gateway_prediction_source_ratio 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в метриках нет %q:\n%s", want, body)
		}
	}
}

// postPredict отправляет POST /predict и требует 200.
func postPredict(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/predict",
		strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /predict: код %d, ожидался 200; тело: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("ответ не разобран: %v", err)
	}
	return out
}

// stubPredictor — предиктор, который отвечает заранее заданным числом и
// запоминает признаки, чтобы тесты видели, что до него доехало.
type stubPredictor struct {
	delta float64
	plate float64
	err   error
	seen  map[string]*float64
}

func (p *stubPredictor) Predict(_ context.Context, f horizon.Frame) predictor.Prediction {
	p.seen = f.Values
	if p.err != nil {
		return predictor.Prediction{}
	}
	dev, _ := f.Value("cur_dev_s")
	return predictor.Prediction{
		SampleID: f.SampleID, UnitID: f.UnitID, TRID: f.TRID, AsOf: f.AsOf,
		TargetStopID: f.PrimaryStopID(), HorizonS: f.HorizonS(),
		DeltaS: p.delta, PredictedDevS: dev + p.delta, PLate: p.plate,
		HasPLate: true, Source: predictor.SourceML,
	}
}

func (p *stubPredictor) predict(_ context.Context, f horizon.Frame) (predictor.Prediction, error) {
	if p.err != nil {
		return predictor.Prediction{}, p.err
	}
	return p.Predict(nil, f), nil
}

var errStubDown = errors.New("заглушка модели не отвечает")
