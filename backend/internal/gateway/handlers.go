package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

// errorBody — тело ошибки. Список полей обязателен: клиенту нужно знать не
// только код, но и что делать, а поле detail обязано быть человеком, а не
// именем функции.
type errorBody struct {
	Error string `json:"error"`
	// Detail обязателен: клиент должен отличить «неверный идентификатор» от
	// «сервис упал» без разбора текста.
	Detail string `json:"detail"`
	// Errors — пополнения, когда ошибок несколько. Одиночные повторяются в
	// Detail, чтобы простой случай не требовал разбора списка.
	Errors []string `json:"errors,omitempty"`
}

// writeJSON отдаёт JSON. Заголовок выставляется до записи тела, иначе
// браузер успеет получить 200 с текстом и не перечитает код.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	// Переводы строк в ответе безвредны и делают вывод читаемым, но только
	// когда это не лента событий WebSocket.
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// Тело уже начато, код ответа не изменить. Остаётся прервать
		// соединение, чтобы клиент не принял обрезанный JSON за целый.
		return
	}
}

func writeError(w http.ResponseWriter, code int, kind, detail string, more ...string) {
	writeJSON(w, code, errorBody{Error: kind, Detail: detail, Errors: more})
}

// health — ответ /healthz. Живость, а не готовность: процесс отвечает, и это
// всё, что проверяет оркестратор, чтобы решить, надо ли его перезапустить.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"uptime_s": s.now().Sub(s.startedAt).Seconds(),
	})
}

// readiness — ответ /readyz. Готовность означает способность обслуживать
// прогнозы, а не просто не падать: гейтвей без модели и без расписания
// работает и показывает телеметрию, но обещать прогнозы не может.
func (s *Server) readiness(w http.ResponseWriter, _ *http.Request) {
	checks := []check{}
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail})
	}
	add("store", s.cfg.Store != nil, "")
	if s.cfg.Store == nil {
		add("schedule", false, "нет плана-графика: маршруты и остановки не отдаются")
	} else {
		add("schedule", true, "")
	}
	if s.cfg.Predictor == nil {
		add("predictor", false,
			"нет предиктора: прогнозы не считаются, инциденты не создаются")
	} else {
		add("predictor", true, "")
	}
	if s.cfg.ML != nil {
		// Расхождение контракта признаков делает каждый прогноз
		// правдоподобно неверным — это неготовность, а не деградация.
		st := s.cfg.ML.Stats()
		switch {
		case st.ContractErr != nil:
			add("model", false, st.ContractErr.Error())
		case st.Version != "":
			add("model", true, "контракт совпал, модель "+st.Version)
		default:
			add("model", true, "обращений к модели ещё не было")
		}
	}
	ready := true
	for _, c := range checks {
		if !c.OK {
			ready = false
		}
	}
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"ready":       ready,
		"uptime_s":    s.now().Sub(s.startedAt).Seconds(),
		"checks":      checks,
		"incidents":   s.incidents.Stats(),
		"predictions": s.preds.Stats(),
	})
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// vehicleView — карточка транспортного средства. Это то, что видит человек.
type vehicleView struct {
	UnitID uint32 `json:"unit_id"`
	// TRID — номер транспортного средства. Ноль, если привязки нет: это
	// нормальное состояние приборной панели, а не ошибка.
	TRID       int64      `json:"tr_id"`
	HasTRID    bool       `json:"has_tr_id"`
	LastSeen   *time.Time `json:"last_seen"`
	StalenessS float64    `json:"staleness_s"`
	// Stale — последний пакет старше порога. Позиция на карте тогда не
	// актуальна, и панель обязана это показывать.
	Stale     bool    `json:"stale"`
	SpeedKmh  float64 `json:"speed_kmh"`
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	// LocationValid — координаты достоверны. При false панель не должна
	// показывать метку на карте.
	LocationValid  bool    `json:"location_valid"`
	CourseDeg      float64 `json:"course_deg"`
	Satellites     uint8   `json:"satellites"`
	PointsInWindow int     `json:"points_in_window"`
	// Prediction — последний прогноз, если он есть.
	Prediction *predictionView `json:"prediction"`
	// Risk — уровень риска по последнему прогнозу. Пусто без прогноза.
	Risk Risk `json:"risk"`
	// OpenIncident — идентификатор текущего инцидента, если он открыт.
	OpenIncident string `json:"open_incident_id,omitempty"`
}

// predictionView — прогноз в ответе API.
type predictionView struct {
	SampleID      string           `json:"sample_id"`
	TargetStopID  int64            `json:"target_stop_id"`
	HorizonS      float64          `json:"horizon_s"`
	DeltaS        float64          `json:"delta_s"`
	PredictedDevS float64          `json:"predicted_dev_s"`
	PLate         float64          `json:"p_late"`
	Source        predictor.Source `json:"source"`
	Stale         bool             `json:"stale"`
	ModelVersion  string           `json:"model_version,omitempty"`
	Missing       []string         `json:"missing_features"`
	Risk          Risk             `json:"risk"`
	AsOf          time.Time        `json:"as_of"`
}

// viewOf превращает прогноз в вид ответа.
func viewOf(p predictor.Prediction) predictionView {
	return predictionView{
		SampleID:      p.SampleID,
		TargetStopID:  p.TargetStopID,
		HorizonS:      p.HorizonS,
		DeltaS:        p.DeltaS,
		PredictedDevS: p.PredictedDevS,
		PLate:         p.PLate,
		Source:        p.Source,
		Stale:         p.Stale,
		ModelVersion:  p.ModelVersion,
		Missing:       p.Missing,
		Risk:          Classify(p),
		AsOf:          p.AsOf,
	}
}

// vehicles — GET /api/v1/vehicles.
func (s *Server) vehicles(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable",
			"хранилище телеметрии не настроено")
		return
	}
	units := s.cfg.Store.Units()
	out := make([]vehicleView, 0, len(units))
	for _, unit := range units {
		out = append(out, s.vehicle(unit))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"vehicles": out,
		"count":    len(out),
	})
}

// vehicle собирает карточку одной машины. Отсутствующие части заменяются
// нулями с явными флагами, а не ошибкой: машина может быть известна расписанию
// и не иметь ни одного пакета телеметрии, и это не поломка.
//
// Карточка одна и та же для REST и для ленты. Раньше лента несла отдельную
// узкую форму, и это выглядело безобидно, пока поля не разъехались: карточка
// из ленты не знала координат, а панели они нужны не меньше прогноза. Две
// формы одной сущности разъезжаются всегда, а лента при этом ещё и молчит:
// расхождение видно только глазами на карте.
func (s *Server) vehicle(unit uint32) vehicleView {
	v := vehicleView{UnitID: unit}
	if s.cfg.Binding != nil {
		if tr, ok := s.cfg.Binding.TRID(unit); ok {
			v.TRID, v.HasTRID = tr, true
		}
	}
	if s.cfg.Store == nil {
		return v
	}
	state, ok := s.cfg.Store.State(unit)
	if !ok {
		return v
	}
	seen := state.SeenAt
	v.LastSeen = &seen
	v.StalenessS = state.StalenessS
	v.Stale = state.Stale
	v.PointsInWindow = state.PointsInWindow
	p := state.Point
	v.SpeedKmh = p.SpeedKmh
	v.Latitude = p.Latitude
	v.Longitude = p.Longitude
	v.LocationValid = p.LocationValid
	v.CourseDeg = p.CourseDeg
	v.Satellites = p.Satellites

	if pred, ok := s.preds.Latest(unit); ok {
		view := viewOf(pred)
		v.Prediction = &view
		v.Risk = view.Risk
	}
	for _, inc := range s.incidents.List("", 0) {
		if inc.UnitID == unit {
			v.OpenIncident = inc.ID
			break
		}
	}
	return v
}

// vehicleByID — GET /api/v1/vehicles/{id}.
func (s *Server) vehicleByID(w http.ResponseWriter, r *http.Request) {
	unit, err := parseUnitID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.cfg.Store != nil {
		if _, ok := s.cfg.Store.State(unit); !ok {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("устройство %d не известно", unit))
			return
		}
	}
	writeJSON(w, http.StatusOK, s.vehicle(unit))
}

// trajectory — GET /api/v1/vehicles/{id}/trajectory.
//
// Точки отдаются из кольца телеметрии, то есть их не может быть больше, чем
// DefaultCapacity, и глубина истории в минутах зависит от частоты пакетов:
// медиана шага около 9 с. Ограничение объявлено в ответе явно, иначе клиент
// решит, что у него вся история смены.
func (s *Server) trajectory(w http.ResponseWriter, r *http.Request) {
	unit, err := parseUnitID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable",
			"хранилище телеметрии не настроено")
		return
	}
	points := s.cfg.Store.Window(unit)
	if len(points) == 0 {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("по устройству %d точек телеметрии нет", unit))
		return
	}
	from, to, err := parseWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	filtered := points
	if !from.IsZero() || !to.IsZero() {
		filtered = points[:0:0]
		for _, p := range points {
			if !from.IsZero() && p.EventTime.Before(from) {
				continue
			}
			if !to.IsZero() && p.EventTime.After(to) {
				continue
			}
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		writeError(w, http.StatusNotFound, "not_found",
			"в запрошенном интервале точек нет")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unit_id":  unit,
		"points":   filtered,
		"count":    len(filtered),
		"held":     len(points),
		"capacity": statestore.DefaultCapacity,
		"from":     filtered[0].EventTime,
		"to":       filtered[len(filtered)-1].EventTime,
	})
}

// routes — GET /api/v1/routes.
//
// Маршрут здесь — это план-график одного транспортного средства за смену:
// отдельной сущности маршрута в исходных данных нет, а вводить её значило бы
// изобрести структуру, которой в расписании не соответствует ничего.
func (s *Server) routes_(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Schedule == nil {
		writeError(w, http.StatusServiceUnavailable, "schedule_unavailable",
			"нет плана-графика")
		return
	}
	type routeView struct {
		TRID    int64      `json:"tr_id"`
		Stops   int        `json:"stops"`
		FirstAt *time.Time `json:"first_planned"`
		LastAt  *time.Time `json:"last_planned"`
	}
	trids := s.cfg.Schedule.Vehicles()
	slices.Sort(trids)
	out := make([]routeView, 0, len(trids))
	for _, tr := range trids {
		stops := s.cfg.Schedule.Stops(tr)
		rv := routeView{TRID: tr, Stops: len(stops)}
		if len(stops) > 0 {
			first, last := stops[0].TimeBegin, stops[len(stops)-1].TimeBegin
			rv.FirstAt, rv.LastAt = &first, &last
		}
		out = append(out, rv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": out, "count": len(out)})
}

// routeStops — GET /api/v1/routes/{id}/stops.
func (s *Server) routeStops(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Schedule == nil {
		writeError(w, http.StatusServiceUnavailable, "schedule_unavailable",
			"нет плана-графика")
		return
	}
	tr, err := parseTRID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	stops := s.cfg.Schedule.Stops(tr)
	if len(stops) == 0 {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("у транспортного средства %d остановок в расписании нет", tr))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tr_id": tr,
		"stops": stops,
		"count": len(stops),
	})
}

// incidents — GET /api/v1/incidents.
func (s *Server) incidentsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := IncidentStatus(q.Get("status"))
	switch status {
	case "", StatusOpen, StatusAcked, StatusResolved:
	default:
		writeError(w, http.StatusBadRequest, "bad_request",
			"status может быть open, acked или resolved",
			"получено: "+string(status))
		return
	}
	limit, err := parseLimit(q.Get("limit"), 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	list := s.incidents.List(status, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"incidents": list,
		"count":     len(list),
	})
}

// incidentByID — GET /api/v1/incidents/{id}.
func (s *Server) incidentByID(w http.ResponseWriter, r *http.Request) {
	inc, err := s.incidents.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "инцидент не найден")
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

// ackIncident — POST /api/v1/incidents/{id}/ack.
func (s *Server) ackIncident(w http.ResponseWriter, r *http.Request) {
	var req ackRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.AckedBy) == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"поле acked_by обязательно: инцидент без виновника бессмыслен")
		return
	}
	inc, err := s.incidents.Ack(r.PathValue("id"), strings.TrimSpace(req.AckedBy))
	switch {
	case errors.Is(err, errIncidentNotFound):
		writeError(w, http.StatusNotFound, "not_found", "инцидент не найден")
	case errors.Is(err, errIncidentResolved):
		// 409, а не 404: инцидент есть, но подтверждать уже нечего. 404
		// увел бы клиента искать несуществующий идентификатор.
		writeJSON(w, http.StatusConflict, inc)
	default:
		// Подтверждение человеком — событие, которое другие должны увидеть
		// сразу: дежурный сменился, и вторая смена обязана знать, что про
		// этот простой уже знают.
		s.publishIncident(inc)
		writeJSON(w, http.StatusOK, inc)
	}
}

type ackRequest struct {
	AckedBy string `json:"acked_by"`
	// Note — свободный текст. Не валидируется: это заметка человека, и
	// запрещать ему писать что-то осмысленное здесь незачем.
	Note string `json:"note,omitempty"`
}

// prediction — GET /api/v1/predictions/{sample_id}.
func (s *Server) prediction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sample_id")
	p, ok := s.preds.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			"прогноз с таким sample_id не найден или уже вытеснен из истории")
		return
	}
	writeJSON(w, http.StatusOK, viewOf(p))
}

// predictRequest — тело POST /api/v1/predict.
//
// Эндпоинт считает прогноз по переданным признакам, ничего не запоминая и не
// создавая инцидентов. Он нужен для двух вещей: проверить модель на кадре,
// которого не было в обучении, и убедиться, что цепочка деградации ведёт себя
// так, как ожидает приборная панель. Без него проверка деградации требует
// валить сервис модели руками.
type predictRequest struct {
	SampleID     string              `json:"sample_id"`
	UnitID       uint32              `json:"unit_id"`
	TRID         int64               `json:"tr_id"`
	T            *time.Time          `json:"t"`
	TargetStopID int64               `json:"target_stop_id"`
	Ambiguous    bool                `json:"target_ambiguous"`
	Features     map[string]*float64 `json:"features"`
}

// predict — POST /api/v1/predict.
func (s *Server) predict(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Predictor == nil {
		writeError(w, http.StatusServiceUnavailable, "predictor_unavailable",
			"предиктор не настроен")
		return
	}
	var req predictRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	frame, err := req.frame()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error(),
			errList(err)...)
		return
	}
	p := s.cfg.Predictor.Predict(r.Context(), frame)
	// Ответ отдаётся с той же латентностью, что и при наблюдении, но в
	// хранилище не кладётся: это разовый запрос, а не прогноз конвейера.
	// Иначе POST /predict засорял бы историю и вытеснял настоящие прогнозы.
	writeJSON(w, http.StatusOK, viewOf(p))
}

// errList достаёт список расхождений из ошибки сверки признаков.
func errList(err error) []string {
	if err == nil {
		return nil
	}
	return strings.Split(err.Error(), "; ")
}

// frame собирает кадр из запроса. Имена признаков сверяются с контрактом:
// опечатка в имени должна быть ошибкой 400, а не признаком, который молча
// уехал в модель как отсутствующий.
func (req predictRequest) frame() (horizon.Frame, error) {
	if req.T == nil {
		return horizon.Frame{}, errors.New(
			"поле t обязательно: без момента прогноза SampleID не детерминирован")
	}
	asOf := req.T.UTC()
	if req.TargetStopID == 0 {
		return horizon.Frame{}, errors.New(
			"поле target_stop_id обязательно: не к чему предсказывать опоздание")
	}
	contract := predictor.FeatureContract()
	var unknown []string
	for name := range req.Features {
		if !slices.Contains(contract, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return horizon.Frame{}, fmt.Errorf(
			"в признаках есть имена, которых нет в контракте: %s",
			strings.Join(unknown, ", "))
	}
	// Пропущенные признаки дописываются как nil, а не оставляются без ключа:
	// иначе запрос уехал бы с неполным набором, и именно та ошибка, которую
	// чинили в horizon, вернулась бы через API.
	values := make(map[string]*float64, len(contract))
	for _, name := range contract {
		values[name] = req.Features[name]
	}
	horizonS, _ := req.value("horizon_s")
	unit := req.UnitID
	tr := req.TRID
	if tr == 0 && unit != 0 {
		tr = int64(unit)
	}
	sampleID := req.SampleID
	if sampleID == "" {
		sampleID = horizon.FormatSampleID(tr, asOf)
	}
	return horizon.Frame{
		SampleID: sampleID,
		UnitID:   unit,
		TRID:     tr,
		AsOf:     asOf,
		Target: []horizon.Arrival{{
			ActionID: req.TargetStopID,
			Planned:  asOf.Add(time.Duration(horizonS) * time.Second),
			HorizonS: horizonS,
		}},
		Values:    values,
		Ambiguous: req.Ambiguous,
	}, nil
}

// value достаёт числовой признак из запроса.
func (req predictRequest) value(name string) (float64, bool) {
	p, ok := req.Features[name]
	if !ok || p == nil {
		return 0, false
	}
	if math.IsNaN(*p) || math.IsInf(*p, 0) {
		return 0, false
	}
	return *p, true
}

// parseUnitID достаёт unit_id из пути. Ошибка с примером формата обязательна:
// панель присылает сюда и номер ТС, и строковый идентификатор, и оба должны
// отличаться от молчаливого нуля.
func parseUnitID(r *http.Request) (uint32, error) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if raw == "" {
		return 0, errors.New("не указан идентификатор устройства")
	}
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("идентификатор устройства %q должен быть целым числом", raw)
	}
	if v == 0 {
		return 0, errors.New("идентификатор устройства 0 недопустим")
	}
	return uint32(v), nil
}

func parseTRID(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if raw == "" {
		return 0, errors.New("не указан номер транспортного средства")
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("номер транспортного средства %q должен быть целым числом", raw)
	}
	if v == 0 {
		return 0, errors.New("номер транспортного средства 0 недопустим")
	}
	return v, nil
}

// parseWindow разбирает границы времени в формате RFC 3339.
func parseWindow(r *http.Request) (from, to time.Time, err error) {
	q := r.URL.Query()
	parse := func(key string) (time.Time, error) {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			return time.Time{}, nil
		}
		t, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			return time.Time{}, fmt.Errorf(
				"параметр %s=%q должен быть временем RFC 3339, например 2026-01-06T08:00:00Z",
				key, raw)
		}
		return t.UTC(), nil
	}
	if from, err = parse("from"); err != nil {
		return
	}
	if to, err = parse("to"); err != nil {
		return
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		err = errors.New("to раньше from")
	}
	return
}

func parseLimit(raw string, def int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("limit=%q должен быть неотрицательным целым числом", raw)
	}
	return v, nil
}

// decodeBody читает тело запроса с потолком размера. Без потолка один
// запрос может съесть память процесса, а тело здесь необязательное по объёму.
func decodeBody(r *http.Request, dst any) error {
	const maxBody = 1 << 20
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return errors.New("тело запроса слишком велико")
		}
		return fmt.Errorf("тело запроса не разобрано: %w", err)
	}
	return nil
}
