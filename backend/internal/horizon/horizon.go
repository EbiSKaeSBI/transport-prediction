// Package horizon решает, о чём именно предсказывать: создавать ли инцидент,
// какая остановка является целью, и собирает из набора признаков кадр для
// модели.
//
// Пакет не обращается к модели и не хранит состояние. Его работа — превратить
// состояние машины и окно расписания в один однозначно определённый кадр
// (proto/ml.proto, FeatureFrame) либо в мотивированный отказ.
package horizon

import (
	"sort"
	"strconv"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/features"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

// Причины отказа. Они же попадают в логи и в карточку инцидента: «инцидент не
// создан» без причины неотличим от молчаливого сбоя.
const (
	// ReasonNoTarget — в окне горизонта нет ни одной остановки.
	ReasonNoTarget = "no_target_in_horizon"
	// ReasonNoStops — у машины нет окна расписания вообще.
	ReasonNoStops = "no_schedule_window"
	// ReasonStale — последний пакет старше предельного возраста, состояние
	// нельзя считать текущим.
	ReasonStale = "state_stale"
	// ReasonTargetBehind — ближайшая остановка оказалась позади по маршруту:
	// машина едет в обратную сторону или стоит за пределами смены.
	ReasonTargetBehind = "target_behind"
)

// Planner превращает состояние машины в кадр прогноза.
type Planner struct {
	builder  *features.Builder
	features features.Config
	minAge   time.Duration
	maxAge   time.Duration
}

// Option настраивает Planner.
type Option func(*Planner)

// WithFeatureConfig передаёт настройки расчёта признаков.
func WithFeatureConfig(cfg features.Config) Option {
	return func(p *Planner) { p.features = cfg }
}

// WithStateAge задаёт допустимый возраст последнего пакета. Значение ниже
// нуля отключает проверку: в replay возраст определяется сам по себе временем
// события, и искусственный порог отбрасывал бы валидные кадры.
func WithStateAge(min, max time.Duration) Option {
	return func(p *Planner) { p.minAge, p.maxAge = min, max }
}

// DefaultStateAge — рабочие границы возраста состояния. Нижняя граница
// отсекает кадры, где T заметно опережает последний пакет: такой кадр
// предсказывает уже прошедшее. Верхняя отсекает машины, которые исчезли.
const (
	DefaultMinStateAge = -15 * time.Second
	DefaultMaxStateAge = 5 * time.Minute
)

// New создаёт планировщик.
func New(opts ...Option) *Planner {
	p := &Planner{
		features: features.DefaultConfig(),
		minAge:   DefaultMinStateAge,
		maxAge:   DefaultMaxStateAge,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.builder = features.NewBuilder(p.features)
	return p
}

// Request — запрос на прогноз для одного момента времени.
type Request struct {
	// T — момент прогноза.
	T time.Time
	// TRID — номер транспортного средства по расписанию.
	TRID int64
	// UnitID — идентификатор устройства.
	UnitID uint32
	// Stops — окно маршрута вокруг T.
	Stops []schedule.Stop
	// State — последнее состояние устройства.
	State statestore.State
	// HasState — было ли хоть одно состояние.
	HasState bool
	// History — точки окна телеметрии.
	History []statestore.Point
}

// Arrival — один вариант прибытия, для которого строится прогноз.
type Arrival struct {
	// ActionID — идентификатор строки расписания.
	ActionID int64
	// Planned — плановое время прибытия.
	Planned time.Time
	// HorizonS — горизонт до прибытия в секундах: Planned минус T.
	HorizonS float64
	// Alternative — вариант ничьей, а не основная цель. Основная цель
	// выбирается по возрастанию tt_action_item_id, остальные варианты
	// равноправны с точки зрения данных: расписание не говорит, какая из
	// пары имелась в виду.
	Alternative bool
}

// Frame — кадр прогноза, соответствующий predictor.v1.FeatureFrame.
type Frame struct {
	// SampleID — детерминированный идентификатор образца. Один и тот же
	// кадр обязан получать один и тот же идентификатор, иначе дедупликация
	// в очереди прогнозов перестанет работать.
	SampleID string
	// UnitID — идентификатор устройства.
	UnitID uint32
	// TRID — номер транспортного средства.
	TRID int64
	// AsOf — момент прогноза.
	AsOf time.Time
	// Target — варианты прибытия. При неоднозначной цели их два или
	// больше, и все они равноправны.
	Target []Arrival
	// Values — признаки кадра. Отсутствующие признаки в map не попадают:
	// модель обязана видеть пропуск, а не ноль.
	Values map[string]float64
	// Features — полный набор признаков, включая те, что не ушли в модель.
	Features features.Set
	// Quality — секция качества данных.
	Quality features.Quality
	// Window — окно телеметрии, на котором посчитаны признаки.
	Window []statestore.Point
	// Ambiguous — цель неоднозначна: несколько остановок делят минимальное
	// время в окне горизонта.
	Ambiguous bool
}

// Decision — результат планирования: кадр либо мотивированный отказ.
type Decision struct {
	// Frame — кадр для модели. Nil при отказе.
	Frame *Frame
	// Reason — причина отказа. Пустая строка означает успех.
	Reason string
	// Target — выбранная целевая остановка, если она была.
	Target schedule.Target
	// HasTarget — найдена ли цель.
	HasTarget bool
}

// Create сообщает, что прогноз построен.
func (d Decision) Create() bool { return d.Frame != nil }

// Plan строит кадр прогноза либо отказывается с указанием причины.
//
// Отказ — нормальный исход, а не ошибка: инцидент создаётся только когда есть
// цель в окне горизонта и состояние машины пригодно. Молча пропущенный кадр
// выглядел бы как «модель не предсказала», и отладка ушла бы не туда.
func (p *Planner) Plan(req Request) Decision {
	if len(req.Stops) == 0 {
		return Decision{Reason: ReasonNoStops}
	}
	if !req.HasState {
		return Decision{Reason: ReasonStale}
	}
	age := req.T.Sub(req.State.SeenAt)
	if age > p.maxAge {
		return Decision{Reason: ReasonStale}
	}
	if p.minAge >= 0 && age < p.minAge {
		return Decision{Reason: ReasonStale}
	}

	target, ok := targetFor(req.Stops, req.T)
	if !ok {
		return Decision{Reason: ReasonNoTarget}
	}

	in := features.Input{
		T: req.T, TRID: req.TRID, UnitID: req.UnitID,
		State: req.State, HasState: true,
		History: req.History, Stops: req.Stops,
		Target: target, HasTarget: true,
	}
	res := p.builder.Build(in)

	// Цель позади по маршруту означает, что привязка уводит не туда: машина
	// развернулась, стоит за пределами смены или едет по встречке. Предсказать
	// прибытие в остановку, которая позади, бессмысленно — и модель получила
	// бы признаки с недостижимой целью.
	if res.Match.OnRoute && res.Features.DistanceToTargetM != nil {
		if _, ahead, ok := res.Route.DistanceToStop(res.Match, target.Stop.ActionID); ok && !ahead {
			return Decision{Reason: ReasonTargetBehind, Target: target, HasTarget: true}
		}
	}

	arrivals := arrivalsFor(req.Stops, target, req.T)
	horizonS := target.Stop.TimeBegin.Sub(req.T).Seconds()
	frame := &Frame{
		SampleID:  SampleID(req.TRID, req.T),
		UnitID:    req.UnitID,
		TRID:      req.TRID,
		AsOf:      req.T,
		Target:    arrivals,
		Values:    values(res.Features, horizonS),
		Features:  res.Features,
		Quality:   res.Quality,
		Window:    res.RouteWindow(),
		Ambiguous: target.Ambiguous(),
	}
	return Decision{Frame: frame, Target: target, HasTarget: true}
}

// targetFor выбирает целевую остановку и повторяет правило internal/schedule:
// ближайшая по времени в окне горизонта, при равенстве времён — по возрастанию
// tt_action_item_id.
func targetFor(stops []schedule.Stop, t time.Time) (schedule.Target, bool) {
	return schedule.TargetStop(stops, t)
}

// arrivalsFor строит список вариантов прибытия. При неоднозначной цели
// добавляются все остановки, разделяющие минимальное время окна: предсказывать
// надо не одну из них, а пару целиком, иначе модель обучится утверждать, что
// машина физически не дойдёт первой.
func arrivalsFor(stops []schedule.Stop, target schedule.Target, t time.Time) []Arrival {
	primary := Arrival{
		ActionID: target.Stop.ActionID,
		Planned:  target.Stop.TimeBegin,
		HorizonS: target.Stop.TimeBegin.Sub(t).Seconds(),
	}
	out := []Arrival{primary}
	if !target.Ambiguous() {
		return out
	}
	for _, s := range stops {
		if s.ActionID == target.Stop.ActionID {
			continue
		}
		if !s.TimeBegin.Equal(target.Stop.TimeBegin) {
			continue
		}
		out = append(out, Arrival{
			ActionID:    s.ActionID,
			Planned:     s.TimeBegin,
			HorizonS:    s.TimeBegin.Sub(t).Seconds(),
			Alternative: true,
		})
	}
	// Порядок вариантов обязан быть детерминированным, иначе повторный
	// расчёт на тех же данных дал бы другой кадр.
	sort.Slice(out, func(i, j int) bool { return out[i].ActionID < out[j].ActionID })
	return out
}

// SampleID строит детерминированный идентификатор образца.
//
// Ключ имеет ровно тот вид, что и в раздаче: «<tr_id>_<unix(T)>». Это не
// украшение, а требование: онлайн-кадр должен джойниться с эталоном и со
// строкой сабмита без переименования колонок, иначе результат невозможно
// сверить с тем, на чём обучалась модель.
//
// Ключ строится по tr_id, а не по unit_id, хотя unit_id известен конвейеру
// раньше: unit_id меняется при переустановке устройства, тогда как tr_id
// описывает рейс.
//
// Идентификатор остановки в ключ не входит: он уже лежит в кадре отдельным
// полем, а при ничьей все варианты относятся к одному моменту T и были бы
// одним и тем же ключом. Дедупликация по инциденту «машина, остановка» ведётся
// в конвейере по памяти, а не через этот ключ.
//
// Время округляется до секунды: наносекундная точность сделала бы ключ
// нестабильным при пересчёте и не совпала бы с раздачей, где T записан с
// точностью до минуты.
func SampleID(trID int64, t time.Time) string {
	return strconv.FormatInt(trID, 10) + "_" + strconv.FormatInt(t.Unix(), 10)
}

// FormatSampleID — псевдоним SampleID для кода реплея, который зовёт это
// значение FormatSampleID по привычке из прежней версии контракта.
func FormatSampleID(trID int64, t time.Time) string {
	return SampleID(trID, t)
}

// values раскладывает набор признаков в карту для модели. Отсутствующие
// признаки в карту не попадают: null должен оставаться пропуском, иначе модель
// обучилась бы на подставных нулях.
//
// horizon_s добавляется отдельно, потому что он не часть набора признаков Go,
// а величина из секции given: фактический горизонт до выбранной цели. Он
// никогда не nullable, и именно он ограничивает кадр окном (600, 900] с.
func values(f features.Set, horizonS float64) map[string]float64 {
	out := make(map[string]float64, 24)
	addF := func(name string, v *float64) {
		if v != nil {
			out[name] = *v
		}
	}
	addI := func(name string, v *int32) {
		if v != nil {
			out[name] = float64(*v)
		}
	}

	addF("plan_travel_s", f.PlanTravelS)
	addF("slack_s", f.SlackS)
	addF("cur_dev_s", f.CurDevS)
	addF("headway_s", f.HeadwayS)
	addI("trip_index", f.TripIndex)
	out["is_terminal_stop"] = boolF(f.IsTerminalStop)
	out["manual_fill"] = boolF(f.ManualFill)

	out["speed_current"] = f.SpeedCurrent
	addF("speed_seg_avg", f.SpeedSegAvg)
	addF("speed_seg_max", f.SpeedSegMax)
	out["dwell_current_s"] = f.DwellCurrentS
	addF("dwell_last_s", f.DwellLastS)
	addF("distance_to_target_m", f.DistanceToTargetM)
	addI("stops_remaining", f.StopsRemaining)
	addF("heading_error_deg", f.HeadingErrorDeg)
	addF("route_progress", f.RouteProgress)
	addF("layover_min", f.LayoverMin)
	addF("drift_last3_slope", f.DriftLast3Slope)
	out["consecutive_late_stops"] = float64(f.ConsecutiveLateStops)
	out["target_ambiguous"] = boolF(f.TargetAmbiguous)
	out["horizon_s"] = horizonS

	return out
}

func boolF(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// FeatureNames перечисляет признаки, которые гарантированно попадают в кадр.
// Список нужен для сверки с моделью: признак, которого нет в обучении, но
// который приехал в проде, молча игнорируется.
func FeatureNames() []string {
	return []string{
		"plan_travel_s", "slack_s", "cur_dev_s", "headway_s", "trip_index",
		"is_terminal_stop", "manual_fill",
		"speed_current", "speed_seg_avg", "speed_seg_max",
		"dwell_current_s", "dwell_last_s", "distance_to_target_m",
		"stops_remaining", "heading_error_deg", "route_progress",
		"layover_min", "drift_last3_slope", "consecutive_late_stops",
		"target_ambiguous", "horizon_s",
	}
}

// HorizonS возвращает горизонт основной цели в секундах.
func (f Frame) HorizonS() float64 {
	if len(f.Target) == 0 {
		return 0
	}
	return f.Target[0].HorizonS
}

// PrimaryStopID возвращает идентификатор основной цели.
func (f Frame) PrimaryStopID() int64 {
	if len(f.Target) == 0 {
		return 0
	}
	return f.Target[0].ActionID
}
