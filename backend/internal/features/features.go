// Package features считает признаки, которые по контракту features/v1.yaml
// отнесены к слою Go: секции schedule, go_state и quality.
//
// Пакет ничего не знает о протоколе NDTP и о файлах: на вход подаётся
// состояние устройства, история точек, окно расписания и момент прогноза T.
// Это делает набор признаков одинаковым для онлайна и для replay, что и
// требует инвариант train_serve_parity.
package features

import (
	"math"
	"slices"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/mapmatch"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/stopdetect"
)

// Пороги и константы калибровки. Значения обоснованы в docs/adr/0006.
const (
	// DefaultSearchRadiusM — радиус, в котором точка считается привязанной к
	// маршруту.
	DefaultSearchRadiusM = mapmatch.DefaultSearchRadiusM
	// DefaultZeroSpeedKmh — граница «стоит / едет». Совпадает с порогом
	// stopdetect: ниже него пакет не может считаться движением.
	DefaultZeroSpeedKmh = 0.5
	// DefaultLayoverS — минимальная плановая пауза, которую имеет смысл
	// называть перерывом. Пауза короче этого — обычный интервал между
	// остановками, а не отдых водителя.
	DefaultLayoverS = 180
	// driftWindow — сколько последних простоев входит в наклон дрейфа.
	driftWindow = 3
	// repeatVisitRadiusM — радиус, в котором две строки расписания считаются
	// проездами одной остановки. Идентификатора остановки в данных нет,
	// поэтому повтор определяется только по совпадению координат.
	repeatVisitRadiusM = 50
	// lateToleranceS — допуск, ниже которого простой не считается опозданием.
	// Секунда: расписание округлено до секунд, а телеметрия приходит с
	// задержкой канала, и без допуска каждый второй простой был бы «опозданием».
	lateToleranceS = 1
)

// Config — настройки расчёта признаков.
type Config struct {
	// SearchRadiusM — радиус привязки к маршруту.
	SearchRadiusM float64
	// ZeroSpeedKmh — граница «стоит / едет».
	ZeroSpeedKmh float64
	// LayoverS — минимальная плановая пауза, считаемая перерывом.
	LayoverS float64
	// Peak — часы пик, если они известны. При пустом наборе признак is_peak
	// не вычисляется на уровне Go, а остаётся за слоем контекста.
	Peak func(hour int) bool
}

// DefaultConfig возвращает откалиброванные настройки.
func DefaultConfig() Config {
	return Config{
		SearchRadiusM: DefaultSearchRadiusM,
		ZeroSpeedKmh:  DefaultZeroSpeedKmh,
		LayoverS:      DefaultLayoverS,
	}
}

// Input — всё, что нужно для расчёта набора признаков одного момента.
type Input struct {
	// T — момент прогноза. Все признаки отражают состояние строго на T:
	// точка с временем события позже T в расчёт не попадает.
	T time.Time
	// TRID — номер транспортного средства по расписанию.
	TRID int64
	// UnitID — идентификатор устройства.
	UnitID uint32
	// State — последнее состояние устройства. HasState == false, если
	// пакетов не было вовсе.
	State statestore.State
	// HasState — было ли хоть одно состояние.
	HasState bool
	// History — точки окна, уже отфильтрованные по времени события <= T.
	History []statestore.Point
	// Stops — окно маршрута вокруг T: от первой остановки, которую машина
	// ещё не проехала, до последней известной. Оно же служит источником
	// плановых времён.
	Stops []schedule.Stop
	// Target — выбранная целевая остановка. HasTarget == false, если её нет:
	// без цели инцидент не создаётся.
	Target schedule.Target
	// HasTarget — найдена ли целевая остановка.
	HasTarget bool
}

// Set — набор признаков Go-слоя. Указатели означают «признак не вычислен»
// и сериализуются в null: модель обязана видеть пропуск, а не ноль.
type Set struct {
	// Секция schedule.
	PlanTravelS    *float64 `json:"plan_travel_s"`
	SlackS         *float64 `json:"slack_s"`
	CurDevS        *float64 `json:"cur_dev_s"`
	HeadwayS       *float64 `json:"headway_s"`
	TripIndex      *int32   `json:"trip_index"`
	IsTerminalStop bool     `json:"is_terminal_stop"`
	ManualFill     bool     `json:"manual_fill"`

	// Секция go_state.
	SpeedCurrent         float64  `json:"speed_current"`
	SpeedSegAvg          *float64 `json:"speed_seg_avg"`
	SpeedSegMax          *float64 `json:"speed_seg_max"`
	DwellCurrentS        float64  `json:"dwell_current_s"`
	DwellLastS           *float64 `json:"dwell_last_s"`
	DistanceToTargetM    *float64 `json:"distance_to_target_m"`
	StopsRemaining       *int32   `json:"stops_remaining"`
	HeadingErrorDeg      *float64 `json:"heading_error_deg"`
	RouteProgress        *float64 `json:"route_progress"`
	LayoverMin           *float64 `json:"layover_min"`
	DriftLast3Slope      *float64 `json:"drift_last3_slope"`
	ConsecutiveLateStops int32    `json:"consecutive_late_stops"`
	TargetAmbiguous      bool     `json:"target_ambiguous"`
	TargetCandidates     int      `json:"target_candidates"`
}

// Quality — секция quality.
type Quality struct {
	// StalenessS — секунд с последнего пакета на момент T.
	StalenessS float64 `json:"staleness_s"`
	// PointsInWindow — глубина накопленной истории.
	PointsInWindow int32 `json:"points_in_window"`
	// LagS — задержка канала на последнем пакете.
	LagS float64 `json:"lag_s"`
}

// Result — полный результат расчёта.
type Result struct {
	T      time.Time `json:"t"`
	TRID   int64     `json:"tr_id"`
	UnitID uint32    `json:"unit_id"`
	// TargetStopID — выбранная цель, 0 если цели нет.
	TargetStopID int64 `json:"target_stop_id"`
	// Route — ломаная маршрута вокруг T. Нужна слою horizon.
	Route *mapmatch.Route `json:"-"`
	// Match — привязка к маршруту на T.
	Match mapmatch.Match `json:"-"`
	// Detected — разбор простоев по окну.
	Detected stopdetect.Result `json:"-"`
	// Window — обрезанная по T история, на которой посчитаны признаки.
	// Отдаётся наружу, чтобы слой прогноза передал модели ровно то окно,
	// которое видел расчёт: другое окно означало бы расхождение признаков
	// и данных, и паритет online/replay сломался бы незаметно.
	Window []statestore.Point `json:"-"`
	// Features — секции schedule и go_state.
	Features Set `json:"features"`
	// Quality — секция quality.
	Quality Quality `json:"quality"`
}

// RouteWindow возвращает историю, на которой посчитаны признаки.
func (r Result) RouteWindow() []statestore.Point { return r.Window }

// Builder считает признаки по заданной конфигурации. Builder не хранит
// состояние и безопасен для конкурентного использования.
type Builder struct {
	cfg      Config
	detector *stopdetect.Detector
}

// NewBuilder создаёт расчётщик признаков.
func NewBuilder(cfg Config) *Builder {
	if cfg.SearchRadiusM <= 0 {
		cfg.SearchRadiusM = DefaultSearchRadiusM
	}
	if cfg.ZeroSpeedKmh <= 0 {
		cfg.ZeroSpeedKmh = DefaultZeroSpeedKmh
	}
	if cfg.LayoverS <= 0 {
		cfg.LayoverS = DefaultLayoverS
	}
	return &Builder{
		cfg: cfg,
		detector: stopdetect.New(
			stopdetect.WithSpeedThreshold(cfg.ZeroSpeedKmh),
			stopdetect.WithStopRadius(cfg.SearchRadiusM),
		),
	}
}

// Build рассчитывает набор признаков для одного момента времени.
func (b *Builder) Build(in Input) Result {
	res := Result{T: in.T, TRID: in.TRID, UnitID: in.UnitID}

	// История обрезается по моменту прогноза до всего расчёта, а не внутри
	// отдельных признаков. Инвариант no_future_leak требует, чтобы точки,
	// случившиеся позже T, не влияли ни на что: доверить это каждому
	// вызывающему значило бы оставить утечку одному забытому вызову.
	history := clipHistory(in.History, in.T)
	res.Window = history

	route := mapmatch.NewRoute(in.Stops)
	detected := b.detector.Detect(history, in.Stops)
	res.Detected = detected
	res.Route = route

	// Привязка: только по достоверным координатам последней точки.
	if in.HasState && in.State.Point.LocationValid {
		res.Match = route.MatchPoint(
			in.State.Point.Longitude, in.State.Point.Latitude,
			in.State.Point.CourseDeg, headingKnown(in.State.Point),
			b.cfg.SearchRadiusM,
		)
	} else {
		res.Match = route.MatchPoint(0, 0, 0, false, b.cfg.SearchRadiusM)
	}

	res.Features = Set{
		IsTerminalStop:       false,
		ConsecutiveLateStops: int32(consecutiveLate(detected.Visits, in.Stops)),
	}
	if in.HasTarget {
		res.TargetStopID = in.Target.Stop.ActionID
		res.Features.ManualFill = in.Target.Stop.ManualFill
		res.Features.TargetAmbiguous = in.Target.Ambiguous()
		res.Features.TargetCandidates = in.Target.Candidates
	}

	b.fillScheduleFeatures(&res.Features, in, route, res.Match)
	b.fillStateFeatures(&res.Features, in, route, res.Match, detected)
	res.Quality = b.quality(in)

	return res
}

// fillScheduleFeatures считает признаки, производные от расписания.
func (b *Builder) fillScheduleFeatures(out *Set, in Input, route *mapmatch.Route, match mapmatch.Match) {
	if !in.HasTarget || len(in.Stops) == 0 {
		return
	}
	target := in.Target.Stop

	// Индекс цели в окне маршрута: он же признак «дальше по рейсу».
	if index := indexOfStop(in.Stops, target.ActionID); index >= 0 {
		trip := int32(index)
		out.TripIndex = &trip
		out.IsTerminalStop = index == len(in.Stops)-1
	}

	// Плановое время в пути до цели от последней остановки, время которой
	// машина уже прошла. Это опорная величина: без неё slack_s не с чем
	// сравнивать.
	reference, hasReference := lastStopBefore(in.Stops, in.T)
	if hasReference {
		if travel := target.TimeBegin.Sub(reference.TimeBegin).Seconds(); travel > 0 {
			out.PlanTravelS = f64(travel)
		}
	}

	// Запас: насколько машина отстаёт от плана в текущей точке маршрута.
	// Положительное значение — опережение, отрицательное — отставание.
	// Считается только при достоверной привязке: без координат плановое
	// время в точке неизвестно, и выдумывать его нельзя. Вне маршрута
	// плановое время в проекции на ближайшую точку не имеет смысла: точка
	// может принадлежать совсем другому рейсу, и такая «привязка» была бы
	// молчаливым выдумыванием.
	if route.Segments() > 0 && match.OnRoute {
		if planned, ok := route.PlannedTimeAtAlong(match.AlongM); ok {
			out.SlackS = f64(planned.Sub(in.T).Seconds())
		}
	}

	// CurDevS — задержка на последней остановке, факт которой уже известен.
	//
	// Это не то же самое, что slack_s. Slack отвечает на вопрос «где машина
	// сейчас относительно плана», и меняется непрерывно с движением.
	// CurDevS отвечает на вопрос «с каким опозданием машина приехала на
	// последнюю пройденную остановку», и это единственная величина о
	// фактическом punctuality, известная до факта на цели.
	//
	// Организаторы считают её так же, и сверка на размеченной выборке это
	// подтверждает: совпадение до секунды в 97.7% меток, а оставшиеся
	// расхождения приходятся на строки, где две остановки имеют одно
	// плановое время. В validate эта величина приходит подсказкой в
	// points.csv, но в online такого файла нет, поэтому считать её
	// обязан конвейер — иначе модель получила бы при обучении признак,
	// которого на площадке не существует.
	//
	// Факт берётся только у остановок, у которых он уже случился к T:
	// planned <= T само по себе этого не гарантирует, машина может
	// приехать на опоздании позже T. Такая строка — это утечка, а не
	// признак.
	if last, ok := lastFactBefore(in.Stops, in.T); ok {
		out.CurDevS = f64(last.TimeFactBegin.Sub(last.TimeBegin).Seconds())
	}

	// Headway — плановый интервал между visits этой же машины на той же
	// остановке. Без повторного проезда признак не существует.
	if previous, ok := previousVisit(in.Stops, target); ok {
		if gap := target.TimeBegin.Sub(previous).Seconds(); gap > 0 {
			out.HeadwayS = f64(gap)
		}
	}
}

// lastFactBefore — последняя остановка, у которой известно и плановое время,
// и фактическое, причём оба уже прошли к моменту t.
func lastFactBefore(stops []schedule.Stop, t time.Time) (schedule.Stop, bool) {
	var found schedule.Stop
	ok := false
	for _, stop := range stops {
		if stop.TimeBegin.After(t) {
			continue
		}
		if !stop.HasFact || stop.TimeFactBegin.After(t) {
			continue
		}
		if !ok || stop.TimeBegin.After(found.TimeBegin) {
			found, ok = stop, true
		}
	}
	return found, ok
}

// fillStateFeatures считает признаки состояния.
func (b *Builder) fillStateFeatures(out *Set, in Input, route *mapmatch.Route,
	match mapmatch.Match, detected stopdetect.Result) {
	if in.HasState {
		out.SpeedCurrent = in.State.Point.SpeedKmh
	}

	// Скорость на текущем отрезке: от последней остановки до T. Это темп,
	// которым машина идёт к цели, а не её мгновенная скорость.
	if avg, max, ok := segmentSpeed(in.History, in.T, b.cfg.ZeroSpeedKmh); ok {
		out.SpeedSegAvg = f64(avg)
		out.SpeedSegMax = f64(max)
	}

	// Текущий простой.
	if detected.IsDwelling {
		if dwell := in.T.Sub(detected.Current.Arrive).Seconds(); dwell > 0 {
			out.DwellCurrentS = dwell
		}
	}
	if detected.HasLast {
		out.DwellLastS = f64(detected.Last.DwellS)
	}

	// Привязанные к маршруту признаки считаются, только если привязка
	// вообще состоялась: у пустого маршрута нет ни расстояния до цели, ни
	// прогресса, и молчаливые нули выглядели бы как уверенные признаки.
	// Вне маршрута (OnRoute == false) геометрия маршрута о привязке машины
	// ничего не говорит: нашлось только то, что ближе всего, а не то, что
	// машина едет по этому рейсу. Признаки остаются пустыми, а нулями не
	// подменяются.
	bound := in.HasState && route.Segments() > 0 && match.OnRoute
	if bound && in.HasTarget {
		if metres, _, ok := route.DistanceToStop(match, in.Target.Stop.ActionID); ok {
			out.DistanceToTargetM = f64(metres)
		}
		if index, ok := route.StopIndexAtAlong(match.AlongM); ok {
			if targetIndex := routeStopIndex(route, in.Target.Stop.ActionID); targetIndex >= 0 {
				// Остаток — число остановок впереди, не считая саму цель:
				// именно их машина должна пройти до предсказанного прибытия.
				remaining := targetIndex - index
				if remaining < 0 {
					remaining = 0
				}
				stops := int32(remaining)
				out.StopsRemaining = &stops
			}
		}
	}
	if bound {
		if match.HeadingValid {
			out.HeadingErrorDeg = f64(match.HeadingErrorDeg)
		}
		progress := match.Progress
		if progress < 0 {
			progress = 0
		}
		if progress > 1 {
			progress = 1
		}
		out.RouteProgress = f64(progress)
	}

	// Перерыв: плановая пауза на целевой остановке, если она terminal.
	if out.IsTerminalStop && in.HasTarget {
		index := indexOfStop(in.Stops, in.Target.Stop.ActionID)
		if index >= 0 && index+1 < len(in.Stops) {
			if pause := in.Stops[index+1].TimeBegin.Sub(in.Target.Stop.TimeBegin).Seconds(); pause >= b.cfg.LayoverS {
				out.LayoverMin = f64(pause / 60)
			}
		}
	}

	// Наклон дрейфа: если машина опаздывает всё сильнее от остановки к
	// остановке, задержка накапливается, и это уже другая динамика, чем
	// разовый простой.
	if slope, ok := driftSlope(detected.Visits, in.Stops, driftWindow); ok {
		out.DriftLast3Slope = f64(slope)
	}
}

// quality считает секцию quality.
func (b *Builder) quality(in Input) Quality {
	q := Quality{}
	if in.HasState {
		q.StalenessS = in.State.StalenessS
		q.PointsInWindow = int32(in.State.PointsInWindow)
		q.LagS = in.State.Point.LagS
	}
	return q
}

// headingKnown сообщает, что курс в точке достоверен. Курс 0 при стоящей
// машине и курс 360 — одно и то же, но 0 не является наблюдением.
func headingKnown(p statestore.Point) bool {
	return p.Satellites > 0 && p.CourseDeg > 0 && p.CourseDeg < 360
}

// segmentSpeed считает среднюю и максимальную скорость на последнем
// завершённом отрезке движения.
//
// Отрезок — это точки движения, следующие за простоем. Если машина сейчас
// едет, это текущий отрезок; если стоит — тот, на котором она только что
// подъехала. Именно так, а не по всему окну: усреднение по окну смешало бы
// вчерашний участок с сегодняшним, а при стоящей машине дало бы
// правдоподобный, но бессмысленный ноль. Скорость последнего отрезка — это
// лучшая доступная оценка темпа, с которым машина пройдёт путь до цели.
func segmentSpeed(history []statestore.Point, now time.Time, zeroKmh float64) (avg, max float64, ok bool) {
	// Пропускаем хвост неподвижных точек: простой закрывает отрезок.
	i := len(history) - 1
	for ; i >= 0; i-- {
		p := history[i]
		if p.EventTime.After(now) {
			continue
		}
		if p.LocationValid && p.SpeedKmh > zeroKmh {
			break
		}
	}
	// Собираем движение от конца отрезка до его начала.
	speeds := make([]float64, 0, 32)
	for ; i >= 0; i-- {
		p := history[i]
		if p.EventTime.After(now) {
			continue
		}
		if !p.LocationValid || p.SpeedKmh <= zeroKmh {
			break
		}
		speeds = append(speeds, p.SpeedKmh)
	}
	if len(speeds) == 0 {
		return 0, 0, false
	}
	sum := 0.0
	for _, v := range speeds {
		sum += v
		if v > max {
			max = v
		}
	}
	return sum / float64(len(speeds)), max, true
}

// clipHistory оставляет точки с временем события не позже now. Точки без
// времени события отбрасываются: они не пригодны ни для окна, ни для
// упорядочивания, а молча считать их «нулевым временем» значило бы
// переместить их в начало смены.
func clipHistory(points []statestore.Point, now time.Time) []statestore.Point {
	out := make([]statestore.Point, 0, len(points))
	for _, p := range points {
		if p.EventTime.IsZero() || p.EventTime.After(now) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// consecutiveLate считает подряд идущие завершённые опознанные простои,
// случившиеся позже расписания. Неопознанные и ушедшие без опоздания обрывают
// счёт: машина, вовремя проехавшая остановку, не «снимает» накопленное
// отставание в реальности, но и не добавляет к нему.
func consecutiveLate(visits []stopdetect.Visit, stops []schedule.Stop) int {
	plan := make(map[int64]time.Time, len(stops))
	for _, s := range stops {
		plan[s.ActionID] = s.TimeBegin
	}
	count := 0
	for i := len(visits) - 1; i >= 0; i-- {
		v := visits[i]
		if v.Open {
			continue
		}
		if !v.Matched {
			return count
		}
		planned, ok := plan[v.StopID]
		if !ok {
			return count
		}
		if !v.Arrive.After(planned.Add(lateToleranceS * time.Second)) {
			return count
		}
		count++
	}
	return count
}

// driftSlope считает наклон зависимости «факт минус план» по последним n
// опознанным простоям. Наклон в секундах на остановку: положительный
// означает нарастающее отставание.
func driftSlope(visits []stopdetect.Visit, stops []schedule.Stop, n int) (float64, bool) {
	plan := make(map[int64]time.Time, len(stops))
	for _, s := range stops {
		plan[s.ActionID] = s.TimeBegin
	}
	type point struct {
		x, y float64
	}
	// Собираем отклонения от плана с конца: последние n опознанных простоев.
	drifts := make([]float64, 0, n)
	for i := len(visits) - 1; i >= 0 && len(drifts) < n; i-- {
		v := visits[i]
		if v.Open || !v.Matched {
			continue
		}
		planned, ok := plan[v.StopID]
		if !ok {
			continue
		}
		drifts = append(drifts, v.Arrive.Sub(planned).Seconds())
	}
	if len(drifts) < 2 {
		return 0, false
	}
	// Обход шёл с конца, поэтому ряд записан от поздней остановки к ранней.
	// Наклон обязан считаться по возрастанию времени: развернуть нужно
	// значения, а индексы проставить уже после разворота. Разворот пар
	// x-y целиком перевернул бы и индексы, и знак наклона вышел бы наоборот.
	slices.Reverse(drifts)
	series := make([]point, 0, len(drifts))
	for i, d := range drifts {
		series = append(series, point{x: float64(i), y: d})
	}
	// Метод наименьших квадратов по x = 0, 1, 2 …
	var sumX, sumY, sumXY, sumXX float64
	for _, p := range series {
		sumX += p.x
		sumY += p.y
		sumXY += p.x * p.y
		sumXX += p.x * p.x
	}
	nf := float64(len(series))
	denom := nf*sumXX - sumX*sumX
	if denom == 0 {
		return 0, false
	}
	slope := (nf*sumXY - sumX*sumY) / denom
	if math.IsNaN(slope) || math.IsInf(slope, 0) {
		return 0, false
	}
	return slope, true
}

// previousVisit ищет предыдущий проезд той же остановки: у неё другой
// tt_action_item_id, но совпадают координаты. Идентификатора остановки в
// расписании нет, поэтому совпадение геометрии — единственный способ отличить
// повторный проезд от другой остановки.
//
// Возвращается ближайший по времени предыдущий проезд. Если остановка
// проезжается один раз за смену, признак не существует и остаётся пустым:
// в реальных данных повторов почти нет, и это норма.
func previousVisit(stops []schedule.Stop, target schedule.Stop) (time.Time, bool) {
	if !target.HasGeometry() {
		return time.Time{}, false
	}
	best := time.Time{}
	found := false
	for _, s := range stops {
		if s.ActionID == target.ActionID || !s.HasGeometry() {
			continue
		}
		if !s.TimeBegin.Before(target.TimeBegin) {
			continue
		}
		if !nearStop(s, target, repeatVisitRadiusM) {
			continue
		}
		if !found || s.TimeBegin.After(best) {
			best, found = s.TimeBegin, true
		}
	}
	return best, found
}

// nearStop сообщает, что две остановки — это одна и та же точка маршрута.
func nearStop(a, b schedule.Stop, radiusM float64) bool {
	return haversineM(a.Lat, a.Lon, b.Lat, b.Lon) <= radiusM
}

// lastStopBefore возвращает последнюю остановку окна, время которой не позже
// момента прогноза. Если машина ещё не прошла ни одной, берётся первая
// остановка окна: иначе плановое время в пути было бы не от чего отсчитывать.
func lastStopBefore(stops []schedule.Stop, now time.Time) (schedule.Stop, bool) {
	for i := len(stops) - 1; i >= 0; i-- {
		if !stops[i].TimeBegin.After(now) {
			return stops[i], true
		}
	}
	return stops[0], true
}

// indexOfStop возвращает позицию остановки в окне или -1.
func indexOfStop(stops []schedule.Stop, actionID int64) int {
	for i := range stops {
		if stops[i].ActionID == actionID {
			return i
		}
	}
	return -1
}

// routeStopIndex возвращает индекс вершины маршрута по идентификатору остановки.
func routeStopIndex(route *mapmatch.Route, actionID int64) int {
	for i := 0; i < route.Len(); i++ {
		if id, ok := route.StopIDAt(i); ok && id == actionID {
			return i
		}
	}
	return -1
}

// f64 оборачивает число в указатель для nullable-признака.
func f64(v float64) *float64 { return &v }

// haversineM — расстояние между точками в метрах.
func haversineM(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusM = 6371000.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(a))
}
