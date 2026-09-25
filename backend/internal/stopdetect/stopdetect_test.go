package stopdetect

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

var base = time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)

// at строит точку телеметрии.
func at(seq int, lon, lat, speed float64) statestore.Point {
	event := base.Add(time.Duration(seq) * 10 * time.Second)
	return statestore.Point{
		UnitID:        1,
		EventTime:     event,
		ReceiveTime:   event,
		Longitude:     lon,
		Latitude:      lat,
		SpeedKmh:      speed,
		LocationValid: true,
	}
}

// stopNear — остановка расписания в заданной точке.
func stopNear(id int64, lon, lat float64) schedule.Stop {
	return schedule.Stop{ActionID: id, TRID: 100, Lon: lon, Lat: lat}
}

// route — ряд остановок вдоль маршрута с шагом около 350 м по долготе.
func route() []schedule.Stop {
	// 0.005° долготы на широте 56° — примерно 310 м.
	out := make([]schedule.Stop, 0, 10)
	for i := range 10 {
		out = append(out, stopNear(int64(1000+i), 37.6+0.005*float64(i), 55.8))
	}
	return out
}

// Светофоры не должны считаться остановками: 68.9 % неподвижных серий в
// данных короче 10 с, и без порога длительности они стали бы остановками.
func TestDetectIgnoresShortStops(t *testing.T) {
	// Движение, затем стоянка 10 с (светофор), затем движение.
	points := []statestore.Point{
		at(0, 37.600, 55.800, 20),
		at(1, 37.6005, 55.800, 0),
		at(2, 37.601, 55.800, 25),
	}
	result := New().Detect(points, route())

	if result.HasLast || result.IsDwelling {
		t.Errorf("светофор не должен давать простоя: HasLast=%v IsDwelling=%v",
			result.HasLast, result.IsDwelling)
	}
	if result.StopsServed != 0 {
		t.Errorf("StopsServed %d, ожидался 0", result.StopsServed)
	}
}

// Настоящая стоянка на остановке: 40 с простоя у опознанной остановки.
func TestDetectFindsStopVisit(t *testing.T) {
	// Подъезд к остановке 1002 (37.61, 55.8), простой 40 с, отъезд.
	points := []statestore.Point{
		at(0, 37.6000, 55.800, 30),
		at(1, 37.6050, 55.800, 25),
		at(2, 37.6100, 55.800, 0),
		at(3, 37.6100, 55.800, 0),
		at(4, 37.6100, 55.800, 0),
		at(5, 37.6100, 55.800, 0),
		at(6, 37.6100, 55.800, 0),
		at(7, 37.6150, 55.800, 30),
	}
	result := New().Detect(points, route())

	if result.IsDwelling {
		t.Error("машина уехала, IsDwelling обязан быть ложным")
	}
	if !result.HasLast {
		t.Fatal("завершённый простой должен быть найден")
	}
	if !result.Last.Matched {
		t.Fatal("простой у опознанной остановки обязан быть опознан")
	}
	if result.Last.StopID != 1002 {
		t.Errorf("StopID %d, ожидался 1002", result.Last.StopID)
	}
	// Прибытие на 20-й секунде, отправление на 70-й.
	if want := 50.0; math.Abs(result.Last.DwellS-want) > 1e-9 {
		t.Errorf("DwellS %.1f, ожидалось %.1f", result.Last.DwellS, want)
	}
	if result.Last.Points != 5 {
		t.Errorf("Points %d, ожидалось 5", result.Last.Points)
	}
	if result.StopsServed != 1 {
		t.Errorf("StopsServed %d, ожидалась 1", result.StopsServed)
	}
	if result.Last.Open {
		t.Error("простой завершён, Open обязан быть ложным")
	}
}

// Долгий светофор в стороне от маршрута — это простой, но не остановка.
func TestDetectSeparatesLongStopFromScheduledStop(t *testing.T) {
	// Маршрут кончается на долготе 37.645; простой на 37.70 — за 3.4 км
	// от ближайшей остановки, то есть далеко за пределами радиуса в 150 м.
	points := []statestore.Point{
		at(0, 37.700, 55.800, 25),
		at(1, 37.700, 55.800, 0),
		at(2, 37.700, 55.800, 0),
		at(3, 37.700, 55.800, 0),
		at(4, 37.700, 55.800, 0),
		at(5, 37.700, 55.800, 0),
		at(6, 37.700, 55.800, 0),
		at(7, 37.700, 55.800, 30),
	}
	result := New().Detect(points, route())

	if !result.HasLast {
		t.Fatal("простой должен быть найден")
	}
	if result.Last.Matched {
		t.Error("простой в 3.4 км от маршрута не может считаться остановкой")
	}
	if result.StopsServed != 0 {
		t.Errorf("StopsServed %d, ожидался 0: обслуженных остановок не было", result.StopsServed)
	}
	if result.Last.MatchDistM < 1000 {
		t.Errorf("MatchDistM %.0f м, ожидалось больше 1000", result.Last.MatchDistM)
	}
}

// Текущий простой обязан быть помечен как открытый и обрезанный: окно кончено,
// а машина всё ещё стоит. Молча обрезанная длительность выглядела бы как
// короткая, и задержка на остановке потеряла бы смысл.
func TestDetectMarksOpenAndTruncatedDwell(t *testing.T) {
	points := make([]statestore.Point, 0, 20)
	for seq := range 20 {
		points = append(points, at(seq, 37.610, 55.800, 0))
	}
	result := New().Detect(points, route())

	if !result.IsDwelling {
		t.Fatal("машина стоит, IsDwelling обязан быть истинным")
	}
	if !result.Current.Open {
		t.Error("текущий простой обязан быть открытым")
	}
	if !result.Current.Truncated {
		t.Error("простой, упирающийся в конец окна, обязан быть помечен обрезанным")
	}
	if !result.Current.Matched {
		t.Error("простой у остановки должен быть опознан")
	}
	if result.HasLast {
		t.Error("завершённых простоев в окне нет, HasLast обязан быть ложным")
	}
	if result.Current.Depart.IsZero() == false {
		t.Error("у открытого простоя не должно быть времени отправления")
	}
}

// Точки без достоверных координат не должны считаться ни движением, ни
// простоем: Nav00 может отсутствовать в пакете.
func TestDetectSkipsInvalidPositions(t *testing.T) {
	points := []statestore.Point{
		at(0, 0, 0, 0),
		at(1, 0, 0, 0),
		at(2, 0, 0, 0),
	}
	points[0].LocationValid = false
	points[1].LocationValid = false
	points[2].LocationValid = false
	result := New().Detect(points, route())

	if result.UsedPoints != 0 {
		t.Errorf("UsedPoints %d, ожидался 0", result.UsedPoints)
	}
	if result.SkippedPoints != 3 {
		t.Errorf("SkippedPoints %d, ожидалось 3", result.SkippedPoints)
	}
	if result.IsDwelling || result.HasLast {
		t.Error("без пригодных точек простоев быть не должно")
	}
}

// Пустое окно не должно падать: так бывает, когда телеметрия ещё не пришла,
// и это штатная ситуация на старте смены.
func TestDetectEmptyWindow(t *testing.T) {
	result := New().Detect(nil, route())
	if result.UsedPoints != 0 || result.SkippedPoints != 0 {
		t.Errorf("пустое окно: UsedPoints %d SkippedPoints %d", result.UsedPoints, result.SkippedPoints)
	}
	if result.IsDwelling || result.HasLast {
		t.Error("пустое окно не должно давать простоя")
	}
	if !result.MovingSince.IsZero() {
		t.Errorf("MovingSince %v, ожидался нулевой: движение не наблюдалось", result.MovingSince)
	}
}

// Результат обязан зависеть только от аргументов: это условие совпадения
// offline-replay с online.
func TestDetectIsPure(t *testing.T) {
	points := []statestore.Point{
		at(0, 37.600, 55.800, 30),
		at(1, 37.605, 55.800, 0),
		at(2, 37.610, 55.800, 0),
		at(3, 37.610, 55.800, 0),
		at(4, 37.610, 55.800, 0),
		at(5, 37.610, 55.800, 0),
		at(6, 37.615, 55.800, 30),
	}
	detector := New()
	first := detector.Detect(points, route())
	second := detector.Detect(points, route())
	// Result содержит срез Visits, поэтому сравнение возможно только
	// по значению: структура со срезом не сравнивается оператором ==.
	if !reflect.DeepEqual(first, second) {
		t.Error("повторный вызов с теми же аргументами дал другой результат: детектор хранит состояние")
	}
}

// Пороговые значения обязаны восстанавливаться при мусорной опции, иначе
// забытый вызов тихо отключил бы проверку длительности.
func TestNewRestoresDefaults(t *testing.T) {
	detector := New(WithSpeedThreshold(0), WithMinDwell(0), WithStopRadius(0), WithLogger(nil))
	if detector.speedThreshold != DefaultSpeedThresholdKmh {
		t.Errorf("speedThreshold %v, ожидалось %v", detector.speedThreshold, DefaultSpeedThresholdKmh)
	}
	if detector.minDwell != DefaultMinDwell {
		t.Errorf("minDwell %v, ожидалось %v", detector.minDwell, DefaultMinDwell)
	}
	if detector.radiusM != DefaultStopRadiusM {
		t.Errorf("radiusM %v, ожидалось %v", detector.radiusM, DefaultStopRadiusM)
	}
	if detector.logger == nil {
		t.Error("логгер обязан остаться заданным по умолчанию")
	}
}

// Несколько остановок подряд: последняя завершённая должна быть последней по
// времени, а не первой в срезе.
func TestDetectReportsLastOfSeveralStops(t *testing.T) {
	points := []statestore.Point{
		at(0, 37.600, 55.800, 30),
		// остановка у 1000
		at(1, 37.600, 55.800, 0), at(2, 37.600, 55.800, 0), at(3, 37.600, 55.800, 0),
		at(4, 37.605, 55.800, 30),
		// остановка у 1001
		at(5, 37.605, 55.800, 0), at(6, 37.605, 55.800, 0), at(7, 37.605, 55.800, 0),
		at(8, 37.610, 55.800, 30),
	}
	result := New().Detect(points, route())

	if result.StopsServed != 2 {
		t.Errorf("StopsServed %d, ожидалось 2", result.StopsServed)
	}
	if result.Last.StopID != 1001 {
		t.Errorf("Last.StopID %d, ожидался 1001 (последняя по времени)", result.Last.StopID)
	}
	// Простой у 1001 начинается на seq 5; seq 8 — уже движение.
	if want := base.Add(50 * time.Second); !result.Last.Arrive.Equal(want) {
		t.Errorf("Last.Arrive %v, ожидалось %v", result.Last.Arrive, want)
	}
	if want := base.Add(80 * time.Second); !result.Last.Depart.Equal(want) {
		t.Errorf("Last.Depart %v, ожидалось %v", result.Last.Depart, want)
	}
	// Машина тронулась на seq 8 — в момент отправления последнего простоя.
	if want := base.Add(80 * time.Second); !result.MovingSince.Equal(want) {
		t.Errorf("MovingSince %v, ожидалось %v", result.MovingSince, want)
	}
}

func TestHaversine(t *testing.T) {
	// Разница широты 0.001° на широте 56° — около 111 м.
	got := haversine(55.8, 37.6, 55.801, 37.6)
	if math.Abs(got-111.2) > 1.0 {
		t.Errorf("haversine вернул %.1f м, ожидалось около 111", got)
	}
	if d := haversine(55.8, 37.6, 55.8, 37.6); d != 0 {
		t.Errorf("расстояние до самой себя %.6f, ожидался 0", d)
	}
	// Точки напротив друг друга не должны «перепрыгивать» полкруга.
	if d := haversine(55.8, 37.6, 55.8, 38.6); d < 60000 || d > 70000 {
		t.Errorf("расстояние в 1° долготы %.0f м, ожидалось около 63 км", d)
	}
}
