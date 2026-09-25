package horizon

import (
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/features"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

var base = time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)

func stopAt(minute int, lon float64) schedule.Stop {
	return schedule.Stop{
		ActionID:  int64(100 + minute),
		TRID:      7,
		TimeBegin: base.Add(time.Duration(minute) * time.Minute),
		Lon:       lon,
		Lat:       55.8,
	}
}

// routeStops — маршрут на 21 остановку с шагом минута.
func routeStops() []schedule.Stop {
	stops := make([]schedule.Stop, 0, 21)
	for i := 0; i <= 20; i++ {
		stops = append(stops, stopAt(i, 37.600+0.007*float64(i)))
	}
	return stops
}

func pointAt(offset time.Duration, lon, lat, speed float64) statestore.Point {
	ev := base.Add(offset)
	return statestore.Point{
		UnitID:        4242,
		EventTime:     ev,
		ReceiveTime:   ev.Add(2 * time.Second),
		Longitude:     lon,
		Latitude:      lat,
		SpeedKmh:      speed,
		LocationValid: true,
		Satellites:    8,
		CourseDeg:     90,
		LagS:          2,
	}
}

// request собирает запрос: машина стоит на остановке минуты minute.
func request(stops []schedule.Stop, at time.Time, minute int) Request {
	last := pointAt(at.Sub(base), stops[minute].Lon, stops[minute].Lat, 0)
	history := []statestore.Point{last}
	return Request{
		T:       at,
		TRID:    7,
		UnitID:  4242,
		Stops:   stops,
		History: history,
		State: statestore.State{
			Point:          last,
			SeenAt:         at,
			StalenessS:     0,
			PointsInWindow: 1,
		},
		HasState: true,
	}
}

func TestPlanBuildsFrame(t *testing.T) {
	stops := routeStops()
	now := base
	dec := New().Plan(request(stops, now, 1))
	if !dec.Create() {
		t.Fatalf("кадр не построен, причина %q", dec.Reason)
	}
	frame := dec.Frame

	if frame.PrimaryStopID() != 111 {
		t.Errorf("цель %d, ожидалась остановка минуты 11", frame.PrimaryStopID())
	}
	// Горизонт обязан лежать в (10 мин, 15 мин] — это и есть инвариант
	// horizon_window из контракта.
	if h := frame.HorizonS(); h < 601 || h > 900 {
		t.Errorf("горизонт %.0f с вне (600, 900]", h)
	}
	if len(frame.Target) != 1 {
		t.Errorf("вариантов прибытия %d, ожидался 1: цель неоднозначна?", len(frame.Target))
	}
	if frame.Ambiguous {
		t.Error("Ambiguous не должен быть истинным при единственной цели")
	}
	if frame.SampleID == "" {
		t.Error("SampleID обязан быть заполнен")
	}
	// horizon_s — обязательный признак контракта и не nullable.
	if _, ok := frame.Values["horizon_s"]; !ok {
		t.Error("horizon_s обязан присутствовать в кадре")
	}
	if _, ok := frame.Values["speed_current"]; !ok {
		t.Error("speed_current обязан присутствовать: он не nullable")
	}
	if frame.AsOf != now {
		t.Errorf("AsOf %v, ожидалось %v", frame.AsOf, now)
	}
}

// Отказ — нормальный исход, но только с внятной причиной.
func TestPlanRefusesWithReason(t *testing.T) {
	stops := routeStops()
	planner := New()

	// Окно расписания пусто.
	dec := planner.Plan(Request{T: base, TRID: 7, HasState: true, Stops: nil})
	if dec.Create() || dec.Reason != ReasonNoStops {
		t.Errorf("пустое окно: Create=%v, Reason=%q", dec.Create(), dec.Reason)
	}

	// Телеметрии не было вовсе.
	dec = planner.Plan(Request{T: base, TRID: 7, Stops: stops, HasState: false})
	if dec.Create() || dec.Reason != ReasonStale {
		t.Errorf("без телеметрии: Create=%v, Reason=%q", dec.Create(), dec.Reason)
	}

	// Цели в окне горизонта нет: маршрут короче горизонта. Короткий маршрут
	// здесь нужен именно потому, что на длинном маршруте остановки минут 19
	// и 20 всё ещё попали бы в окно.
	short := []schedule.Stop{stopAt(0, 37.600), stopAt(1, 37.607), stopAt(2, 37.614)}
	dec = planner.Plan(request(short, base, 1))
	if dec.Create() || dec.Reason != ReasonNoTarget {
		t.Errorf("нет цели: Create=%v, Reason=%q", dec.Create(), dec.Reason)
	}

	// Состояние устарело: последний пакет был 10 минут назад.
	req := request(stops, base, 1)
	req.State.SeenAt = base.Add(-10 * time.Minute)
	req.History[0].EventTime = base.Add(-10 * time.Minute)
	dec = planner.Plan(req)
	if dec.Create() || dec.Reason != ReasonStale {
		t.Errorf("устаревшее состояние: Create=%v, Reason=%q", dec.Create(), dec.Reason)
	}
}

// Неоднозначная цель обязана превратиться в пару прибытий, а не в одну
// остановку: расписание само по себе не говорит, какая из двух имелась в виду.
func TestPlanExpandsAmbiguousTargetIntoPair(t *testing.T) {
	stops := routeStops()
	// Превращаем остановку минуты 11 в пару: та же минута, соседняя точка.
	paired := append([]schedule.Stop(nil), stops...)
	paired[11] = schedule.Stop{
		ActionID: 111, TRID: 7, TimeBegin: base.Add(11 * time.Minute),
		Lon: 37.9, Lat: 55.8,
	}
	paired = append(paired, schedule.Stop{
		ActionID: 1111, TRID: 7, TimeBegin: base.Add(11 * time.Minute),
		Lon: 37.95, Lat: 55.8,
	})
	sortByTime(paired)

	dec := New().Plan(request(paired, base, 1))
	if !dec.Create() {
		t.Fatalf("кадр не построен, причина %q", dec.Reason)
	}
	if !dec.Frame.Ambiguous {
		t.Error("Ambiguous обязан быть истинным")
	}
	if len(dec.Frame.Target) != 2 {
		t.Fatalf("вариантов прибытия %d, ожидалось 2", len(dec.Frame.Target))
	}
	// Первым идёт вариант с меньшим идентификатором, второй помечен как
	// альтернативный: порядок обязан быть детерминированным.
	if dec.Frame.Target[0].ActionID >= dec.Frame.Target[1].ActionID {
		t.Errorf("варианты не упорядочены: %d, %d",
			dec.Frame.Target[0].ActionID, dec.Frame.Target[1].ActionID)
	}
	if dec.Frame.Target[0].Alternative {
		t.Error("основная цель не должна помечаться как альтернатива")
	}
	if !dec.Frame.Target[1].Alternative {
		t.Error("второй вариант ничьи обязан помечаться как альтернативный")
	}
	if v, ok := dec.Frame.Values["target_ambiguous"]; !ok || v != 1 {
		t.Errorf("target_ambiguous = %v (ok=%v), ожидалась 1", v, ok)
	}
}

// Один и тот же кадр обязан получать один и тот же sample_id: на нём держится
// дедупликация в очереди прогнозов.
func TestSampleIDIsDeterministic(t *testing.T) {
	stops := routeStops()
	planner := New()
	first := planner.Plan(request(stops, base, 1)).Frame
	second := planner.Plan(request(stops, base, 1)).Frame
	if first.SampleID != second.SampleID {
		t.Errorf("sample_id разошёлся: %q и %q", first.SampleID, second.SampleID)
	}
	// Другой момент прогноза — другой идентификатор.
	other := planner.Plan(request(stops, base.Add(time.Minute), 1)).Frame
	if other.SampleID == first.SampleID {
		t.Error("разные моменты прогноза дали один sample_id")
	}
	// Ключ строится по tr_id, а не по unit_id: раздача маркирует точки как
	// "<tr_id>_<unix(T)>", и только такой формат сопоставим со строкой
	// сабмита. Здесь tr_id = 7, unit_id = 4242.
	// Формат обязан совпадать с ключом раздачи: sample_id в
	// labels/*.csv и в sample_submission.csv устроен как "<tr_id>_<unix(T)>".
	if want := "7_1767686400"; first.SampleID != want {
		t.Errorf("sample_id %q, ожидался %q", first.SampleID, want)
	}
	if got := FormatSampleID(7, base); got != first.SampleID {
		t.Errorf("FormatSampleID дал %q, ожидалось %q", got, first.SampleID)
	}
}

// Отсутствующий признак обязан остаться пропуском, а не превратиться в ноль:
// иначе модель обучилась бы на подставных значениях.
func TestNullFeaturesStayAbsentFromFrame(t *testing.T) {
	// values вызывается напрямую с пустым набором: ни один признак не
	// вычислен, поэтому в карте обязаны остаться только те, что по контракту
	// не nullable.
	empty := values(features.Set{}, 660)
	for _, name := range FeatureNames() {
		_, present := empty[name]
		if nullableFeature(name) {
			if present {
				t.Errorf("nullable признак %s не должен попадать в карту без данных", name)
			}
			continue
		}
		if !present {
			t.Errorf("обязательный признак %s отсутствует в карте", name)
		}
	}
	if v := empty["is_terminal_stop"]; v != 0 {
		t.Errorf("is_terminal_stop = %v, ожидался 0", v)
	}
	if v := empty["target_ambiguous"]; v != 0 {
		t.Errorf("target_ambiguous = %v, ожидался 0", v)
	}
	if v := empty["horizon_s"]; v != 660 {
		t.Errorf("horizon_s = %v, ожидалось 660", v)
	}
}

// Признаки из набора обязаны попадать в карту: молча пропавший признак
// выглядел бы как «модель его не использует», и расхождение с обучением
// обнаружилось бы только на скоринге.
func TestFrameValuesCoverAllNames(t *testing.T) {
	stops := routeStops()
	frame := New().Plan(request(stops, base, 1)).Frame
	if frame == nil {
		t.Fatal("кадр не построен")
	}
	missing := 0
	for _, name := range FeatureNames() {
		if _, ok := frame.Values[name]; !ok {
			// Признак может быть пропущен, если он nullable и не вычислен.
			if nullableFeature(name) {
				continue
			}
			t.Errorf("не nullable признак %s отсутствует в карте", name)
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("в карте не хватает %d обязательных признаков", missing)
	}
	if len(frame.Values) > len(FeatureNames()) {
		t.Errorf("в карте %d признаков при %d объявленных: появились лишние",
			len(frame.Values), len(FeatureNames()))
	}
}

// Цель позади по маршруту предсказывать бессмысленно: машина развернулась
// или стоит за пределами смены, и до цели по маршруту надо ехать назад.
func TestPlanRefusesTargetBehind(t *testing.T) {
	stops := routeStops()
	now := base
	// Машина физически находится у последней остановки маршрута, а момент
	// прогноза такой, что целью является минута 11 — то есть позади по
	// направлению движения.
	last := pointAt(0, stops[len(stops)-1].Lon, stops[len(stops)-1].Lat, 0)
	req := Request{
		T: now, TRID: 7, UnitID: 4242, Stops: stops,
		History: []statestore.Point{last},
		State: statestore.State{
			Point: last, SeenAt: now, PointsInWindow: 1,
		},
		HasState: true,
	}
	dec := New().Plan(req)
	if dec.Create() {
		t.Fatal("цель позади по маршруту обязана отклоняться")
	}
	if dec.Reason != ReasonTargetBehind {
		t.Errorf("Reason %q, ожидался %q", dec.Reason, ReasonTargetBehind)
	}
	if !dec.HasTarget {
		t.Error("цель найдена, и это должно быть видно даже при отказе")
	}
}

// Окно, отданное модели, обязано совпадать с тем, на котором считались
// признаки, и не содержать точек после момента прогноза.
func TestFrameWindowIsClippedToAsOf(t *testing.T) {
	stops := routeStops()
	now := base
	last := pointAt(0, stops[1].Lon, stops[1].Lat, 0)
	future := pointAt(5*time.Minute, stops[10].Lon, stops[10].Lat, 40)
	req := Request{
		T: now, TRID: 7, UnitID: 4242, Stops: stops,
		History: []statestore.Point{last, future},
		State: statestore.State{
			Point: last, SeenAt: now, PointsInWindow: 2,
		},
		HasState: true,
	}
	frame := New().Plan(req).Frame
	if frame == nil {
		t.Fatal("кадр не построен")
	}
	if len(frame.Window) != 1 {
		t.Fatalf("в окно попало %d точек, ожидалась 1: точка после T обязана отсеяться", len(frame.Window))
	}
	if frame.Window[0].EventTime.After(now) {
		t.Error("в окне осталась точка после момента прогноза")
	}
}

// Порог возраста состояния настраивается: в replay возраст определяется
// временем события, и искусственный порог отбрасывал бы валидные кадры.
func TestStateAgeThresholdIsConfigurable(t *testing.T) {
	stops := routeStops()
	req := request(stops, base, 1)
	req.State.SeenAt = base.Add(-10 * time.Minute)
	req.History[0].EventTime = base.Add(-10 * time.Minute)

	if dec := New().Plan(req); dec.Create() {
		t.Error("состояние десятиминутной давности обязано отклоняться по умолчанию")
	}
	strict := New(WithStateAge(time.Second, time.Minute))
	if dec := strict.Plan(req); dec.Create() {
		t.Error("при нижней границе в секунду кадр обязан отклоняться: T опережает пакет")
	}
	loose := New(WithStateAge(-time.Hour, time.Hour))
	if dec := loose.Plan(req); !dec.Create() {
		t.Errorf("с отключённой проверкой возраста кадр обязан строиться, получено %q", dec.Reason)
	}
}

func TestHorizonSAccessorsOnEmptyFrame(t *testing.T) {
	var f Frame
	if f.HorizonS() != 0 || f.PrimaryStopID() != 0 {
		t.Error("пустой кадр обязан отдавать нули, а не паниковать")
	}
}

// Ключ кадра обязан быть тем же ключом, что и в раздаче: без него онлайн-кадр
// невозможно соединить с эталоном и с сабмитом.
func TestFormatSampleIDMatchesDatasetKey(t *testing.T) {
	if got := FormatSampleID(7, base); got != "7_1767686400" {
		t.Errorf("FormatSampleID %q, ожидался ключ раздачи 7_1767686400", got)
	}
	// Секунды, а не наносекунды: в раздаче T записан с точностью до секунды,
	// и ключ с наносекундами не совпал бы ни с одной строкой.
	precise := base.Add(400 * time.Millisecond)
	if got := FormatSampleID(7, precise); got != "7_1767686400" {
		t.Errorf("FormatSampleID с долями секунды %q, ожидалось округление до секунды", got)
	}
}

// nullableFeature сообщает, что признак по контракту может быть пропущен.
// Список взят из features/v1.yaml: любое расхождение с контрактом должно
// ломать тест, а не молча проходить в модель.
func nullableFeature(name string) bool {
	switch name {
	case "plan_travel_s", "slack_s", "cur_dev_s", "headway_s", "trip_index",
		"speed_seg_avg", "speed_seg_max", "dwell_last_s",
		"distance_to_target_m", "stops_remaining", "heading_error_deg",
		"route_progress", "layover_min", "drift_last3_slope":
		return true
	}
	return false
}

func sortByTime(stops []schedule.Stop) {
	for i := 1; i < len(stops); i++ {
		for j := i; j > 0; j-- {
			if stops[j].TimeBegin.Before(stops[j-1].TimeBegin) ||
				(stops[j].TimeBegin.Equal(stops[j-1].TimeBegin) && stops[j].ActionID < stops[j-1].ActionID) {
				stops[j], stops[j-1] = stops[j-1], stops[j]
				continue
			}
			break
		}
	}
}
