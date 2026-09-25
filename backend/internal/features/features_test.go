package features

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/stopdetect"
)

// base — момент запуска сценария. Остановки строятся относительно него.
var base = time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)

// stopAt — остановка в заданной минуте на заданной широте.
func stopAt(minute int, lon float64) schedule.Stop {
	return schedule.Stop{
		ActionID:  int64(100 + minute),
		TRID:      7,
		TimeBegin: base.Add(time.Duration(minute) * time.Minute),
		Lon:       lon,
		Lat:       55.8,
	}
}

// routeStops — двадцать одна остановка с шагом минута и ~437 м. Маршрут
// достаточно длинный, чтобы цель всегда попадала в окно горизонта (10–15
// минут), иначе расчёт признаков проверял бы не то.
func routeStops() []schedule.Stop {
	stops := make([]schedule.Stop, 0, 21)
	for i := 0; i <= 20; i++ {
		stops = append(stops, stopAt(i, 37.600+0.007*float64(i)))
	}
	return stops
}

// point — точка телеметрии с заданной скоростью.
func point(offset time.Duration, lon, lat, speed float64) statestore.Point {
	ev := base.Add(offset)
	return statestore.Point{
		UnitID:        4242,
		EventTime:     ev,
		ReceiveTime:   ev.Add(3 * time.Second),
		Longitude:     lon,
		Latitude:      lat,
		SpeedKmh:      speed,
		LocationValid: true,
		Satellites:    8,
		LagS:          3,
	}
}

// standingHistory — машина едет, затем стоит на остановке в минуту 3.
// Момент прогноза T — середина простоя, base+3m30s.
func standingHistory() []statestore.Point {
	points := []statestore.Point{
		point(0, 37.600, 55.8, 25),
		point(20*time.Second, 37.603, 55.8, 30),
		point(40*time.Second, 37.607, 55.8, 28),
		point(60*time.Second, 37.611, 55.8, 30),
		point(80*time.Second, 37.614, 55.8, 27),
		point(100*time.Second, 37.618, 55.8, 30),
		point(120*time.Second, 37.621, 55.8, 0),
		point(140*time.Second, 37.621, 55.8, 0),
		point(160*time.Second, 37.621, 55.8, 0),
		point(180*time.Second, 37.621, 55.8, 0),
		point(200*time.Second, 37.621, 55.8, 0),
		point(220*time.Second, 37.621, 55.8, 0),
		point(240*time.Second, 37.621, 55.8, 0),
	}
	return points
}

// inputAt собирает вход для расчёта. Состояние берётся как последняя точка с
// временем события не позже момента прогноза, а не просто последний элемент
// среза: иначе добавление будущих точек подменяло бы состояние на
// несуществующее, и инвариант no_future_leak проверял бы сам себя.
func inputAt(stops []schedule.Stop, history []statestore.Point, when time.Time) Input {
	in := Input{
		T:       when,
		TRID:    7,
		UnitID:  4242,
		History: history,
		Stops:   stops,
	}
	var last statestore.Point
	found := false
	for _, p := range history {
		if p.EventTime.After(when) {
			continue
		}
		if !found || p.EventTime.After(last.EventTime) {
			last, found = p, true
		}
	}
	if found {
		in.State = statestore.State{
			Point:          last,
			SeenAt:         last.ReceiveTime,
			StalenessS:     when.Sub(last.ReceiveTime).Seconds(),
			PointsInWindow: countAtOrBefore(history, when),
		}
		in.HasState = true
	}
	if target, ok := targetOf(stops, when); ok {
		in.Target = target
		in.HasTarget = true
	}
	return in
}

// countAtOrBefore считает точки, попавшие в окно на момент прогноза.
func countAtOrBefore(history []statestore.Point, when time.Time) int {
	n := 0
	for _, p := range history {
		if !p.EventTime.IsZero() && !p.EventTime.After(when) {
			n++
		}
	}
	return n
}

// targetOf повторяет правило выбора цели снаружи пакета, чтобы тесты
// проверяли расчёт признаков, а не сам выбор цели.
func targetOf(stops []schedule.Stop, t time.Time) (schedule.Target, bool) {
	from, to := t.Add(10*time.Minute), t.Add(15*time.Minute)
	var in []schedule.Stop
	for _, s := range stops {
		if s.TimeBegin.After(from) && !s.TimeBegin.After(to) {
			in = append(in, s)
		}
	}
	if len(in) == 0 {
		return schedule.Target{}, false
	}
	best := in[0]
	tied := 1
	for _, s := range in[1:] {
		if s.ActionID < best.ActionID {
			best = s
		}
	}
	for _, s := range in {
		if s.TimeBegin.Equal(best.TimeBegin) {
			tied++
		}
	}
	return schedule.Target{Stop: best, Candidates: len(in), Tied: tied}, true
}

func TestBuildWhileDwelling(t *testing.T) {
	stops := routeStops()
	history := standingHistory()
	when := base.Add(3*time.Minute + 30*time.Second)
	in := inputAt(stops, history, when)
	if !in.HasTarget {
		t.Fatal("для этого T цель обязана находиться в окне горизонта")
	}

	res := NewBuilder(DefaultConfig()).Build(in)
	f := res.Features

	if !res.Detected.IsDwelling {
		t.Fatal("машина стоит, IsDwelling обязан быть истинным")
	}
	if f.DwellCurrentS <= 60 {
		t.Errorf("DwellCurrentS %.0f, ожидалось больше 60 с простоя", f.DwellCurrentS)
	}
	// Машина стоит, но скорость последнего завершённого отрезка остаётся
	// доступной: это и есть оценка темпа, с которым машина пройдёт путь до
	// цели. Усреднение по текущему (нулевому) отрезку дало бы 0 и потеряло
	// бы главный признак движения.
	if f.SpeedSegAvg == nil {
		t.Error("SpeedSegAvg обязан считаться по последнему завершённому отрезку")
	} else if *f.SpeedSegAvg < 25 || *f.SpeedSegAvg > 31 {
		t.Errorf("SpeedSegAvg %.1f, ожидалось около 28 (среднее по подъезду)", *f.SpeedSegAvg)
	}
	if f.SpeedSegMax == nil || *f.SpeedSegMax < 29 {
		t.Errorf("SpeedSegMax %v, ожидалось около 30", f.SpeedSegMax)
	}
	if f.SpeedCurrent != 0 {
		t.Errorf("SpeedCurrent %v, ожидалось 0", f.SpeedCurrent)
	}

	// Привязка должна лечь на маршрут: координаты точно совпадают с остановкой.
	if !res.Match.OnRoute {
		t.Errorf("точка на остановке не привязалась к маршруту, смещение %.0f м", res.Match.OffsetM)
	}
	if f.RouteProgress == nil {
		t.Error("RouteProgress обязан быть вычислен на маршруте")
	} else if *f.RouteProgress < 0 || *f.RouteProgress > 1 {
		t.Errorf("RouteProgress %.3f вне [0, 1]", *f.RouteProgress)
	}
	if f.DistanceToTargetM == nil {
		t.Error("DistanceToTargetM обязано быть вычислено")
	}
	if f.StopsRemaining == nil {
		t.Error("StopsRemaining обязано быть вычислено")
	} else if *f.StopsRemaining < 0 {
		t.Errorf("StopsRemaining %d отрицательно", *f.StopsRemaining)
	}

	// Машина стоит на остановке минуты 3, а по расписанию должна была
	// прибыть в 08:03:00. Считаем в 08:03:30, то есть на полминуты позже:
	// slack = плановое время - T, поэтому знак отрицательный. Это и есть
	// наблюдаемое отставание, а не ошибка знака.
	if f.SlackS == nil {
		t.Fatal("SlackS обязано считаться на маршруте")
	}
	if *f.SlackS > -29 || *f.SlackS < -31 {
		t.Errorf("SlackS %.1f с, ожидалось около -30 с (отставание на полминуты)", *f.SlackS)
	}
	if f.PlanTravelS == nil || *f.PlanTravelS <= 0 {
		t.Error("PlanTravelS обязан быть положительным")
	}

	// Глубина окна считается по обрезанной истории: точки после момента
	// прогноза не могли накопиться в хранилище.
	var clipped int
	for _, p := range history {
		if !p.EventTime.After(when) {
			clipped++
		}
	}
	if res.Quality.PointsInWindow != int32(clipped) {
		t.Errorf("PointsInWindow %d, ожидалось %d", res.Quality.PointsInWindow, clipped)
	}
	if res.Quality.LagS != 3 {
		t.Errorf("LagS %v, ожидалось 3", res.Quality.LagS)
	}
	if res.Quality.StalenessS < 0 {
		t.Errorf("StalenessS %v отрицательно", res.Quality.StalenessS)
	}
}

// Без телеметрии набор признаков не падает, а остаётся честно неполным:
// нули в nullable-полях означали бы уверенное «машина стоит в центре
// маршрута».
func TestBuildWithoutStateIsHonest(t *testing.T) {
	stops := routeStops()
	now := base.Add(2 * time.Minute)
	in := Input{T: now, TRID: 7, Stops: stops}
	if target, ok := targetOf(stops, now); ok {
		in.Target, in.HasTarget = target, true
	}
	res := NewBuilder(DefaultConfig()).Build(in)
	f := res.Features

	if res.Match.OnRoute {
		t.Error("без координат привязка невозможна")
	}
	for name, v := range map[string]*float64{
		"SpeedSegAvg":       f.SpeedSegAvg,
		"RouteProgress":     f.RouteProgress,
		"DistanceToTargetM": f.DistanceToTargetM,
		"StopsRemaining":    nil,
		"DwellLastS":        f.DwellLastS,
		"SlackS":            f.SlackS,
	} {
		if v != nil {
			t.Errorf("%s = %v без телеметрии, ожидался nil", name, *v)
		}
	}
	if f.StopsRemaining != nil {
		t.Errorf("StopsRemaining = %d без привязки, ожидался nil", *f.StopsRemaining)
	}
	if res.Quality.PointsInWindow != 0 {
		t.Errorf("PointsInWindow %d, ожидалось 0", res.Quality.PointsInWindow)
	}
}

// Главный инвариант no_future_leak: точки строго после T не должны влиять ни
// на один признак, даже если попасть в историю. Обход всех перестановок
// хвоста проверяет, что ни один признак не зависит от порядка или наличия
// будущих пакетов.
func TestBuildIgnoresPointsAfterT(t *testing.T) {
	stops := routeStops()
	history := standingHistory()
	when := base.Add(3*time.Minute + 30*time.Second)

	// Хвост из точек после when: они представляют то, что пришло позже, и не
	// должны влиять ни на что.
	future := []statestore.Point{
		point(300*time.Second, 37.630, 55.8, 50),
		point(320*time.Second, 37.640, 55.8, 60),
		point(400*time.Second, 37.700, 55.8, 90),
	}
	reference := NewBuilder(DefaultConfig()).Build(inputAt(stops, history, when))

	for seed := int64(0); seed < 20; seed++ {
		tail := append([]statestore.Point(nil), future...)
		rand.New(rand.NewSource(seed)).Shuffle(len(tail), func(i, j int) {
			tail[i], tail[j] = tail[j], tail[i]
		})
		// Тот же момент, но история заполнена будущими точками.
		in := inputAt(stops, append(append([]statestore.Point(nil), history...), tail...), when)
		got := NewBuilder(DefaultConfig()).Build(in)
		if !sameFeatures(reference.Features, got.Features) {
			t.Fatalf("seed %d: перемешивание будущих точек изменило признаки:\n%+v\n%+v",
				seed, reference.Features, got.Features)
		}
		if reference.Quality != got.Quality {
			t.Fatalf("seed %d: изменилась секция quality: %+v и %+v", seed, reference.Quality, got.Quality)
		}
	}
}

// Смена опознанной остановки обязана менять distance_to_target_m:
// иначе признак не отражает ничего.
func TestDistanceToTargetFollowsTarget(t *testing.T) {
	// Машина стоит на остановке минуты 1, момент прогноза — 08:01:30, целью
	// будет ближайшая остановка в (11:30, 16:30], то есть минута 12.
	long := routeStops()
	history := []statestore.Point{
		point(0, 37.600, 55.8, 20),
		point(30*time.Second, 37.607, 55.8, 0),
		point(60*time.Second, 37.607, 55.8, 0),
		point(90*time.Second, 37.607, 55.8, 0),
		point(100*time.Second, 37.607, 55.8, 0),
	}
	now := base.Add(1*time.Minute + 30*time.Second)
	in := inputAt(long, history, now)
	if !in.HasTarget {
		t.Fatal("цель обязана находиться в окне")
	}
	res := NewBuilder(DefaultConfig()).Build(in)
	if res.Features.DistanceToTargetM == nil {
		t.Fatal("расстояние до цели обязано считаться")
	}
	// Цель — минута 12, машина на минуте 1. Шаг маршрута 0.007° долготы
	// на широте 55.8 — примерно 437 м, одиннадцать сегментов дают около
	// 4.8 км.
	if d := *res.Features.DistanceToTargetM; d < 4_500 || d > 5_200 {
		t.Errorf("расстояние до цели %.0f м, ожидалось около 4800", d)
	}
	if res.Features.StopsRemaining == nil || *res.Features.StopsRemaining < 10 {
		t.Errorf("StopsRemaining %v, ожидалось не меньше 10", res.Features.StopsRemaining)
	}
}

// Машина далеко от маршрута. Ближайшая точка полилинии находится, но сама
// принадлежность машины этому рейсу не доказана, поэтому признаки геометрии
// обязаны остаться пустыми. Нули выглядели бы как уверенные «ровно 0 м до
// цели, ровно в середине маршрута» и отравили бы обучение молча.
func TestOffRouteLeavesGeometryEmpty(t *testing.T) {
	stops := routeStops()
	history := []statestore.Point{
		point(0, 37.600, 55.8, 20),
		// 20 км к востоку от маршрута: проекция есть, привязки нет.
		point(30*time.Second, 37.790, 55.8, 0),
		point(60*time.Second, 37.790, 55.8, 0),
		point(90*time.Second, 37.790, 55.8, 0),
	}
	now := base.Add(90 * time.Second)
	in := inputAt(stops, history, now)
	if !in.HasTarget {
		t.Fatal("цель обязана находиться в окне")
	}
	res := NewBuilder(DefaultConfig()).Build(in)
	if res.Match.OnRoute {
		t.Fatal("фикстура должна быть вне маршрута")
	}
	f := res.Features
	for name, value := range map[string]*float64{
		"slack_s":            f.SlackS,
		"distance_to_target": f.DistanceToTargetM,
		"route_progress":     f.RouteProgress,
		"heading_error_deg":  f.HeadingErrorDeg,
	} {
		if value != nil {
			t.Errorf("%s вне маршрута = %v, ожидался nil", name, *value)
		}
	}
	if f.StopsRemaining != nil {
		t.Errorf("stops_remaining вне маршрута = %v, ожидался nil", *f.StopsRemaining)
	}
	// Признаки, не зависящие от геометрии, обязаны выжить: простой на
	// остановке одинаков и в пути, и рядом с маршрутом.
	if f.DwellCurrentS <= 0 {
		t.Errorf("простой вне маршрута = %v, ожидался > 0", f.DwellCurrentS)
	}
}

// Машина на маршруте в тех же условиях обязана получить геометрию: тест выше
// не может проходить из-за того, что расчёт сломан целиком.
func TestOnRouteKeepsGeometry(t *testing.T) {
	stops := routeStops()
	history := []statestore.Point{
		point(0, 37.600, 55.8, 20),
		point(30*time.Second, 37.6014, 55.8, 0),
		point(60*time.Second, 37.6014, 55.8, 0),
		point(90*time.Second, 37.6014, 55.8, 0),
	}
	now := base.Add(90 * time.Second)
	res := NewBuilder(DefaultConfig()).Build(inputAt(stops, history, now))
	if !res.Match.OnRoute {
		t.Fatal("фикстура должна быть на маршруте: тест на геометрию иначе бессмыслен")
	}
	if res.Features.DistanceToTargetM == nil || res.Features.RouteProgress == nil {
		t.Fatal("на маршруте геометрия обязана считаться")
	}
}

// Отставание накапливается: три подряд опоздавшие остановки обязаны дать
// положительный наклон дрейфа и счётчик 3. Опозданием считается прибытие
// позже планового, поэтому моменты прибытия в тесте заданы со сдвигом
// 30, 60 и 90 секунд — наклон выходит ровно 30 с на остановку.
func TestDriftAndConsecutiveLate(t *testing.T) {
	// Остановки на минутах 1, 3 и 5 плюс хвост маршрута.
	stops := append(routeStops(),
		schedule.Stop{ActionID: 121, TRID: 7, TimeBegin: base.Add(21 * time.Minute), Lon: 37.747, Lat: 55.8},
	)
	var history []statestore.Point
	// Остановка минуты 1: план 08:01:00, факт 08:01:30 — опоздание 30 с.
	history = append(history,
		point(90*time.Second, 37.607, 55.8, 0),
		point(100*time.Second, 37.607, 55.8, 0),
		point(110*time.Second, 37.607, 55.8, 0),
		point(120*time.Second, 37.607, 55.8, 0),
		point(135*time.Second, 37.609, 55.8, 25),
	)
	// Остановка минуты 3: план 08:03:00, факт 08:04:00 — опоздание 60 с.
	history = append(history,
		point(240*time.Second, 37.621, 55.8, 0),
		point(255*time.Second, 37.621, 55.8, 0),
		point(270*time.Second, 37.621, 55.8, 0),
		point(280*time.Second, 37.621, 55.8, 0),
		point(295*time.Second, 37.624, 55.8, 25),
	)
	// Остановка минуты 5: план 08:05:00, факт 08:06:30 — опоздание 90 с.
	history = append(history,
		point(390*time.Second, 37.635, 55.8, 0),
		point(405*time.Second, 37.635, 55.8, 0),
		point(420*time.Second, 37.635, 55.8, 0),
		point(430*time.Second, 37.635, 55.8, 0),
		point(445*time.Second, 37.637, 55.8, 25),
	)
	// Момент прогноза после того, как машина тронулась с третьей остановки.
	now := base.Add(7*time.Minute + 30*time.Second)

	res := NewBuilder(DefaultConfig()).Build(inputAt(stops, history, now))
	if res.Features.ConsecutiveLateStops != 3 {
		t.Errorf("ConsecutiveLateStops %d, ожидалось 3; простые: %+v",
			res.Features.ConsecutiveLateStops, res.Detected.Visits)
	}
	if res.Features.DriftLast3Slope == nil {
		t.Fatal("DriftLast3Slope обязано считаться по трём простоям")
	}
	// Наклон положителен: отставание растёт от остановки к остановке.
	if slope := *res.Features.DriftLast3Slope; slope < 20 || slope > 40 {
		t.Errorf("DriftLast3Slope %.1f с/остановка, ожидалось около 30", slope)
	}
}

// Убывающее отставание обязано давать отрицательный наклон: знак наклона
// различает «догоняем» и «отдаём».
func TestDriftSlopeSignFollowsTrend(t *testing.T) {
	// Опоздания 90, 60, 30 секунд — машина догоняет расписание.
	stops := append(routeStops(),
		schedule.Stop{ActionID: 121, TRID: 7, TimeBegin: base.Add(21 * time.Minute), Lon: 37.747, Lat: 55.8},
	)
	var history []statestore.Point
	dwell := func(at time.Duration, lon float64) {
		history = append(history,
			point(at, lon, 55.8, 0),
			point(at+10*time.Second, lon, 55.8, 0),
			point(at+20*time.Second, lon, 55.8, 0),
			point(at+30*time.Second, lon, 55.8, 0),
			point(at+45*time.Second, lon+0.002, 55.8, 25),
		)
	}
	// Планы остановок: 08:01:00, 08:03:00, 08:05:00. Факты подобраны так,
	// чтобы опоздания убывали: 90, 60 и 30 секунд.
	dwell(150*time.Second, 37.607) // факт 08:02:30 — опоздание 90 с
	dwell(240*time.Second, 37.621) // факт 08:04:00 — опоздание 60 с
	dwell(330*time.Second, 37.635) // факт 08:05:30 — опоздание 30 с
	res := NewBuilder(DefaultConfig()).Build(inputAt(stops, history, base.Add(6*time.Minute+40*time.Second)))
	if res.Features.DriftLast3Slope == nil {
		t.Fatal("DriftLast3Slope обязано считаться")
	}
	if slope := *res.Features.DriftLast3Slope; slope > -20 || slope < -40 {
		t.Errorf("DriftLast3Slope %.1f с/остановка, ожидалось около -30", slope)
	}
}

// Вовремя пройденная остановка обрывает счётчик опозданий.
func TestConsecutiveLateStopsResetsOnTime(t *testing.T) {
	// stopAt(minute) даёт ActionID = 100 + minute: остановки минут 1 и 3
	// имеют идентификаторы 101 и 103.
	// Первая остановка опоздала, вторая — вовремя. Счётчик обязан обнулиться,
	// потому что вовремя пройденная остановка означает, что машина догнала
	// расписание.
	visits := []stopdetect.Visit{
		{StopID: 101, Matched: true, Arrive: base.Add(time.Minute + 30*time.Second)},
		{StopID: 103, Matched: true, Arrive: base.Add(3 * time.Minute)},
	}
	stops := []schedule.Stop{stopAt(1, 37.600), stopAt(3, 37.607)}
	if got := consecutiveLate(visits, stops); got != 0 {
		t.Errorf("consecutiveLate %d, ожидалось 0: последняя остановка не опоздала", got)
	}

	// Обе опоздали — счётчик 2. Первая тоже должна быть позже плана: ровно в
	// расписание опозданием не считается.
	visits = []stopdetect.Visit{
		{StopID: 101, Matched: true, Arrive: base.Add(time.Minute + 30*time.Second)},
		{StopID: 103, Matched: true, Arrive: base.Add(3*time.Minute + 30*time.Second)},
	}
	if got := consecutiveLate(visits, stops); got != 2 {
		t.Errorf("consecutiveLate %d, ожидалось 2", got)
	}
}

// Неопознанный простой (светофор) не является опозданием — планового времени
// у него нет. При этом он обрывает цепочку: «consecutive» относится к
// последовательности опознанных остановок, и простой между ними делает
// соседство непосредственным только в смысле порядка, а не счётчика.
// Накопленное значение при этом сохраняется, а не сбрасывается.
func TestConsecutiveLateTerminatesOnUnmatched(t *testing.T) {
	visits := []stopdetect.Visit{
		{StopID: 101, Matched: true, Arrive: base.Add(time.Minute + 30*time.Second)},
		{Matched: false, Arrive: base.Add(2 * time.Minute), Depart: base.Add(2*time.Minute + 30*time.Second)},
		{StopID: 103, Matched: true, Arrive: base.Add(3*time.Minute + 60*time.Second)},
	}
	stops := []schedule.Stop{stopAt(1, 37.600), stopAt(3, 37.607)}
	if got := consecutiveLate(visits, stops); got != 1 {
		t.Errorf("consecutiveLate %d, ожидалось 1: неопознанный простой обрывает цепочку, "+
			"но не обнуляет накопленное", got)
	}
}

// Перерыв обязан распознаваться только на terminal-остановке и только если
// пауза достаточно длинная.
func TestLayoverOnlyOnTerminalWithLongPause(t *testing.T) {
	// Пауза 20 минут после цели на минуте 4, но цель не terminal.
	short := []schedule.Stop{stopAt(0, 37.600), stopAt(4, 37.628), stopAt(24, 37.700)}
	now := base.Add(time.Minute)
	res := NewBuilder(DefaultConfig()).Build(inputAt(short, standingHistory()[:2], now))
	if res.Features.LayoverMin != nil {
		t.Errorf("LayoverMin %v на не-terminal остановке, ожидался nil", *res.Features.LayoverMin)
	}

	// Та же пауза, но цель — последняя остановка окна.
	terminal := []schedule.Stop{stopAt(0, 37.600), stopAt(4, 37.628)}
	res = NewBuilder(DefaultConfig()).Build(inputAt(terminal, standingHistory()[:2], now))
	if !res.Features.IsTerminalStop {
		t.Skip("цель не оказалась terminal: T таково, что окно обрезано")
	}
	if res.Features.LayoverMin == nil {
		t.Fatal("LayoverMin обязан считаться на terminal-остановке с длинной паузой")
	}
	if m := *res.Features.LayoverMin; m < 19 || m > 21 {
		t.Errorf("LayoverMin %.1f мин, ожидалось около 20", m)
	}
}

// Headway — интервал между повторными проездами одной остановки; при единственном
// проезде признак не существует.
func TestHeadwayNeedsRepeat(t *testing.T) {
	// Обычный маршрут: каждая остановка проезжается один раз за смену, и
	// признак не существует.
	now := base
	in := inputAt(routeStops(), standingHistory()[:2], now)
	if in.Target.Stop.ActionID != 100+11 {
		t.Fatalf("цель %d, ожидалась остановка минуты 11", in.Target.Stop.ActionID)
	}
	res := NewBuilder(DefaultConfig()).Build(in)
	if res.Features.HeadwayS != nil {
		t.Errorf("HeadwayS %v для единственного проезда, ожидался nil", *res.Features.HeadwayS)
	}

	// При T = 08:00:00 целью становится ближайшая остановка окна — минута 11.
	// Делаем её геометрически совпадающей с остановкой минуты 2: получается
	// повторный проезд одной точки маршрута. Повтор опознаётся только по
	// совпадению координат — идентификатора остановки в расписании нет, и
	// сравнивать больше нечего.
	looped := routeStops()
	looped[11] = schedule.Stop{
		ActionID: 111, TRID: 7, TimeBegin: base.Add(11 * time.Minute),
		Lon: looped[2].Lon, Lat: looped[2].Lat,
	}
	in = inputAt(looped, standingHistory()[:2], now)
	res = NewBuilder(DefaultConfig()).Build(in)
	if in.Target.Stop.ActionID != 111 {
		t.Fatalf("цель %d, ожидалась остановка 111", in.Target.Stop.ActionID)
	}
	if res.Features.HeadwayS == nil {
		t.Fatal("HeadwayS обязан считаться при повторном проезде")
	}
	if h := *res.Features.HeadwayS; h < 535 || h > 545 {
		t.Errorf("HeadwayS %.0f с, ожидалось около 540 (проходы на минутах 2 и 11)", h)
	}
}

// Неоднозначность цели обязана доходить до модели отдельным признаком.
func TestTargetAmbiguityReachesFeatures(t *testing.T) {
	stops := []schedule.Stop{
		stopAt(0, 37.600),
		stopAt(12, 37.642), // две строки на минуте 12
		{ActionID: 1121, TRID: 7, TimeBegin: base.Add(12 * time.Minute), Lon: 37.9, Lat: 55.8},
		stopAt(14, 37.656),
	}
	now := base
	in := inputAt(stops, standingHistory()[:2], now)
	if !in.Target.Ambiguous() {
		t.Skip("в подготовленном окне не возникло ничьи")
	}
	res := NewBuilder(DefaultConfig()).Build(in)
	if !res.Features.TargetAmbiguous {
		t.Error("признак target_ambiguous обязан доходить до модели")
	}
	if res.Features.TargetCandidates != in.Target.Candidates {
		t.Errorf("target_candidates %d, ожидалось %d", res.Features.TargetCandidates, in.Target.Candidates)
	}
}

// Признаки не должны зависеть от того, передан ли срез истории целиком или
// обрезанным: обрезка не должна давать других значений на общих точках.
func TestBuildStableUnderHistoryTruncation(t *testing.T) {
	stops := routeStops()
	history := standingHistory()
	now := base.Add(4*time.Minute + 30*time.Second)
	// Обрезаем хвост после момента прогноза — набор признаков не должен
	// измениться.
	var kept []statestore.Point
	for _, p := range history {
		if !p.EventTime.After(now) {
			kept = append(kept, p)
		}
	}
	a := NewBuilder(DefaultConfig()).Build(inputAt(stops, history, now))
	b := NewBuilder(DefaultConfig()).Build(inputAt(stops, kept, now))
	if !sameFeatures(a.Features, b.Features) {
		t.Errorf("обрезание будущего изменило признаки:\n%+v\n%+v", a.Features, b.Features)
	}
}

// Скорость на отрезке усредняется по точкам движения после последнего
// простоя и не включает нули стояния.
func TestSegmentSpeedAveragesMovingPointsOnly(t *testing.T) {
	history := []statestore.Point{
		point(0, 37.600, 55.8, 20),
		point(10*time.Second, 37.601, 55.8, 30),
		point(20*time.Second, 37.602, 55.8, 40),
		point(30*time.Second, 37.603, 55.8, 0),
		point(40*time.Second, 37.603, 55.8, 0),
		point(50*time.Second, 37.603, 55.8, 0),
	}
	now := base.Add(50 * time.Second)
	avg, max, ok := segmentSpeed(history, now, DefaultZeroSpeedKmh)
	if !ok {
		t.Fatal("скорость на отрезке обязана считаться")
	}
	if max != 40 {
		t.Errorf("SpeedSegMax %v, ожидалось 40", max)
	}
	// Среднее по 20, 30 и 40 = 30; нули стояния не должны входить в среднее.
	if math.Abs(avg-30) > 0.001 {
		t.Errorf("SpeedSegAvg %.2f, ожидалось 30", avg)
	}
}

func TestSegmentSpeedEmpty(t *testing.T) {
	if _, _, ok := segmentSpeed(nil, base, DefaultZeroSpeedKmh); ok {
		t.Error("без точек скорость не вычисляется")
	}
	// Один ненулевой пакет — минимум для среднего.
	one := []statestore.Point{point(0, 37.6, 55.8, 15)}
	if _, _, ok := segmentSpeed(one, base, DefaultZeroSpeedKmh); !ok {
		t.Error("одна точка в движении достаточна для среднего")
	}
}

func TestHeadingKnownRejectsZero(t *testing.T) {
	if headingKnown(statestore.Point{CourseDeg: 0, Satellites: 8}) {
		t.Error("курс 0 не является наблюдением")
	}
	if headingKnown(statestore.Point{CourseDeg: 90, Satellites: 0}) {
		t.Error("без спутников курс недостоверен")
	}
	if !headingKnown(statestore.Point{CourseDeg: 90, Satellites: 8}) {
		t.Error("курс 90 при 8 спутниках достоверен")
	}
}

// На реальных данных признаки обязаны считаться и вести себя разумно:
// без привязки к остановкам план-признаки вырождаются в нули.
func TestFeaturesOnRealData(t *testing.T) {
	planPath := filepath.Join("..", "..", "..", "validate", "schedule_plan.csv")
	plan, err := schedule.LoadFile(planPath)
	if err != nil {
		t.Skipf("нет %s: %v", planPath, err)
	}
	builder := NewBuilder(DefaultConfig())
	trID := plan.Vehicles()[0]
	stops := plan.Stops(trID)
	if len(stops) < 10 {
		t.Skip("у первой машины слишком короткое расписание")
	}

	// Момент прогноза — середина рейса.
	mid := stops[len(stops)/2]
	now := mid.TimeBegin.Add(-12 * time.Minute)
	target, ok := plan.TargetStop(trID, now)
	if !ok {
		t.Skip("в середине рейса нет цели в окне горизонта")
	}
	window := plan.Window(trID, now.Add(-10*time.Minute), now.Add(40*time.Minute))
	if len(window) < 5 {
		t.Skip("слишком короткое окно маршрута")
	}

	// Синтетическая телеметрия: стоим на предыдущей остановке.
	var history []statestore.Point
	var last statestore.Point
	for i := 0; i < 8; i++ {
		last = statestore.Point{
			UnitID: 1, EventTime: now.Add(time.Duration(-i*10) * time.Second),
			ReceiveTime: now.Add(time.Duration(-i*10)*time.Second + 2*time.Second),
			Longitude:   window[0].Lon, Latitude: window[0].Lat,
			SpeedKmh: 0, LocationValid: true, Satellites: 8, LagS: 2,
		}
		history = append(history, last)
	}
	res := builder.Build(Input{
		T: now, TRID: trID, UnitID: 1, HasState: true,
		State: statestore.State{
			Point: last, SeenAt: last.ReceiveTime, StalenessS: 2, PointsInWindow: 8,
		},
		History: history, Stops: window,
		Target: target, HasTarget: true,
	})

	if res.TargetStopID != target.Stop.ActionID {
		t.Errorf("TargetStopID %d, ожидался %d", res.TargetStopID, target.Stop.ActionID)
	}
	if !res.Match.OnRoute {
		t.Errorf("точка на остановке не привязалась: смещение %.0f м", res.Match.OffsetM)
	}
	if res.Features.PlanTravelS == nil || *res.Features.PlanTravelS <= 0 {
		t.Error("PlanTravelS не вычислен на реальном расписании")
	}
	if res.Features.SlackS == nil {
		t.Error("SlackS не вычислен на реальном расписании")
	}
	if res.Features.TripIndex == nil {
		t.Error("TripIndex не вычислен на реальном расписании")
	}
	if res.Quality.PointsInWindow != 8 {
		t.Errorf("PointsInWindow %d, ожидалось 8", res.Quality.PointsInWindow)
	}
}

// sameFeatures сравнивает наборы признаков, различая nil и ноль: указатель на
// ноль и отсутствие признака — разные вещи для модели.
func sameFeatures(a, b Set) bool {
	if a.IsTerminalStop != b.IsTerminalStop ||
		a.ManualFill != b.ManualFill ||
		a.SpeedCurrent != b.SpeedCurrent ||
		a.DwellCurrentS != b.DwellCurrentS ||
		a.ConsecutiveLateStops != b.ConsecutiveLateStops ||
		a.TargetAmbiguous != b.TargetAmbiguous ||
		a.TargetCandidates != b.TargetCandidates {
		return false
	}
	for _, pair := range [][2]*float64{
		{a.PlanTravelS, b.PlanTravelS},
		{a.SlackS, b.SlackS},
		{a.HeadwayS, b.HeadwayS},
		{a.SpeedSegAvg, b.SpeedSegAvg},
		{a.SpeedSegMax, b.SpeedSegMax},
		{a.DwellLastS, b.DwellLastS},
		{a.DistanceToTargetM, b.DistanceToTargetM},
		{a.HeadingErrorDeg, b.HeadingErrorDeg},
		{a.RouteProgress, b.RouteProgress},
		{a.LayoverMin, b.LayoverMin},
		{a.DriftLast3Slope, b.DriftLast3Slope},
	} {
		if !sameFloatPtr(pair[0], pair[1]) {
			return false
		}
	}
	for _, pair := range [][2]*int32{
		{a.TripIndex, b.TripIndex},
		{a.StopsRemaining, b.StopsRemaining},
	} {
		if !sameInt32Ptr(pair[0], pair[1]) {
			return false
		}
	}
	return true
}

func sameFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b || (*a != *a && *b != *b)
}

func sameInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// cur_dev_s — задержка на последней остановке, факт которой известен на T.
// Именно эту величину организаторы кладут в points.csv как подсказку, и
// сверка на размеченной выборке подтверждает определение: последняя строка с
// плановым временем не позже T.
func TestCurDevSFromLastCompletedStop(t *testing.T) {
	stops := routeStops()
	// Факт есть у трёх пройденных остановок: минуты 1, 2 и 3.
	stops[1].HasFact = true
	stops[1].TimeFactBegin = stops[1].TimeBegin.Add(45 * time.Second)
	stops[2].HasFact = true
	stops[2].TimeFactBegin = stops[2].TimeBegin.Add(-30 * time.Second)
	stops[3].HasFact = true
	stops[3].TimeFactBegin = stops[3].TimeBegin.Add(90 * time.Second)

	history := []statestore.Point{
		point(0, 37.607, 55.8, 0),
		point(30*time.Second, 37.607, 55.8, 0),
		point(60*time.Second, 37.607, 55.8, 0),
		point(90*time.Second, 37.607, 55.8, 0),
	}
	now := base.Add(4*time.Minute + 30*time.Second)
	res := NewBuilder(DefaultConfig()).Build(inputAt(stops, history, now))
	if res.Features.CurDevS == nil {
		t.Fatal("cur_dev_s обязан считаться, когда факт пройденной остановки известен")
	}
	if got := *res.Features.CurDevS; got != 90 {
		t.Errorf("cur_dev_s = %v, ожидалось 90 (минута 3, опоздание 90 с)", got)
	}

	// Утечка: остановка, план которой прошёл, но факт наступит позже T, ещё
	// неизвестна. На minute 5 плановое время, факт minute 6.
	future := routeStops()
	future[5].HasFact = true
	future[5].TimeFactBegin = future[5].TimeBegin.Add(30 * time.Second)
	leaked := NewBuilder(DefaultConfig()).Build(inputAt(future, history, now))
	if leaked.Features.CurDevS != nil {
		t.Errorf("cur_dev_s = %v, но факт остановки наступает после T: утечка",
			*leaked.Features.CurDevS)
	}
}
