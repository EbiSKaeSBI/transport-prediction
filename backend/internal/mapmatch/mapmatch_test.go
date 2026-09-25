package mapmatch

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
)

var base = time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)

// stopAt строит остановку с заданным смещением вдоль маршрута и временем.
func stopAt(i int, lon, lat float64) schedule.Stop {
	return schedule.Stop{
		ActionID:  int64(100 + i),
		TRID:      7,
		TimeBegin: base.Add(time.Duration(i) * time.Minute),
		Lon:       lon,
		Lat:       lat,
	}
}

// straightRoute — маршрут из четырёх остановок, идущих на восток шагом
// примерно по 314 м (0.005° долготы на широте 55.8).
func straightRoute() *Route {
	return NewRoute([]schedule.Stop{
		stopAt(0, 37.600, 55.800),
		stopAt(1, 37.605, 55.800),
		stopAt(2, 37.610, 55.800),
		stopAt(3, 37.615, 55.800),
	})
}

func TestRouteBuildsOrderAndLength(t *testing.T) {
	// Остановки поданы вперемешку: маршрут обязан собраться по времени.
	route := NewRoute([]schedule.Stop{
		stopAt(2, 37.610, 55.800),
		stopAt(0, 37.600, 55.800),
		stopAt(3, 37.615, 55.800),
		stopAt(1, 37.605, 55.800),
	})
	if route.Len() != 4 {
		t.Fatalf("Len %d, ожидалось 4", route.Len())
	}
	if route.Segments() != 3 {
		t.Fatalf("Segments %d, ожидалось 3", route.Segments())
	}
	// Три сегмента по ~314 м.
	if route.TotalM() < 900 || route.TotalM() > 990 {
		t.Errorf("TotalM %.0f, ожидалось около 940", route.TotalM())
	}
	id, ok := route.StopIDAt(0)
	if !ok || id != 100 {
		t.Errorf("первая остановка %d, ожидалась 100 (маршрут обязан идти по времени)", id)
	}
	if !route.From().Equal(base) {
		t.Errorf("From %v, ожидалось %v", route.From(), base)
	}
	if want := base.Add(3 * time.Minute); !route.To().Equal(want) {
		t.Errorf("To %v, ожидалось %v", route.To(), want)
	}
}

func TestRouteSkipsStopsWithoutGeometry(t *testing.T) {
	broken := stopAt(1, 0, 0)
	route := NewRoute([]schedule.Stop{
		stopAt(0, 37.600, 55.800),
		broken,
		stopAt(2, 37.610, 55.800),
	})
	if route.Len() != 2 {
		t.Errorf("Len %d, ожидалось 2: остановка без геометрии не должна попадать в маршрут", route.Len())
	}
}

// Точка на четверти первого сегмента: положение вдоль маршрута известно.
// Берётся именно четверть, а не середина: при t == 0.5 вершина по маршруту
// неоднозначна, и проверка на границе проверяла бы соглашение о сравнении, а
// не геометрию.
func TestMatchPointOnRoute(t *testing.T) {
	route := straightRoute()
	match := route.MatchPoint(37.60125, 55.800, 90, true, DefaultSearchRadiusM)

	if !match.OnRoute {
		t.Fatal("точка на маршруте обязана быть опознана как на маршруте")
	}
	if match.OffsetM > 1 {
		t.Errorf("OffsetM %.2f, ожидалось почти 0", match.OffsetM)
	}
	// Четверть сегмента: 0.00125° долготы — примерно 79 м из 314.
	if match.AlongM < 60 || match.AlongM > 95 {
		t.Errorf("AlongM %.0f, ожидалось около 79", match.AlongM)
	}
	if match.NearestStopIndex != 0 {
		t.Errorf("NearestStopIndex %d, ожидался 0: точка ближе к первой вершине", match.NearestStopIndex)
	}
	// Маршрут идёт на восток, азимут около 90.
	if math.Abs(AngleDiffDeg(match.BearingDeg, 90)) > 3 {
		t.Errorf("BearingDeg %.1f, ожидалось около 90", match.BearingDeg)
	}
	if !match.HeadingValid {
		t.Error("HeadingValid обязан быть истинным при известном курсе")
	}
	// Курс совпадает с направлением маршрута.
	if math.Abs(match.HeadingErrorDeg) > 3 {
		t.Errorf("HeadingErrorDeg %.1f, ожидалось около 0", match.HeadingErrorDeg)
	}
	if match.Progress < 0.07 || match.Progress > 0.11 {
		t.Errorf("Progress %.3f, ожидалось около 0.083", match.Progress)
	}
	if match.StopsRemaining != 3 {
		t.Errorf("StopsRemaining %d, ожидалось 3", match.StopsRemaining)
	}
}

// Вне маршрута — нормальное состояние, а не ошибка: OnRoute обязан быть
// ложным, чтобы признаки не выдумывали положение.
func TestMatchPointOffRoute(t *testing.T) {
	route := straightRoute()
	// 0.02° широты — около 2.2 км в сторону.
	match := route.MatchPoint(37.600, 55.820, 0, false, DefaultSearchRadiusM)

	if match.OnRoute {
		t.Fatal("точка в 2 км от маршрута не должна считаться на маршруте")
	}
	if match.OffsetM < 2000 {
		t.Errorf("OffsetM %.0f, ожидалось около 2200", match.OffsetM)
	}
	if match.HeadingValid {
		t.Error("HeadingValid обязан быть ложным при неизвестном курсе")
	}
	// Смещение всё равно вычислено: по нему видно, насколько далеко уехали.
	if match.NearestStopIndex < 0 {
		t.Error("NearestStopIndex должен определяться и вне маршрута")
	}
}

// Маршрут короче двух остановок строить нечего: привязка обязана честно
// сказать, что маршрута нет, а не вернуть нули.
func TestMatchOnDegenerateRoutes(t *testing.T) {
	cases := map[string][]schedule.Stop{
		"пусто":         nil,
		"одна":          {stopAt(0, 37.600, 55.800)},
		"без геометрии": {stopAt(0, 0, 0), stopAt(1, 0, 0)},
	}
	for name, stops := range cases {
		t.Run(name, func(t *testing.T) {
			route := NewRoute(stops)
			match := route.MatchPoint(37.600, 55.800, 90, true, DefaultSearchRadiusM)
			if match.OnRoute {
				t.Error("OnRoute обязан быть ложным")
			}
			if match.SegmentIndex != -1 {
				t.Errorf("SegmentIndex %d, ожидался -1", match.SegmentIndex)
			}
			if !math.IsInf(match.OffsetM, 1) {
				t.Errorf("OffsetM %v, ожидалась бесконечность", match.OffsetM)
			}
			if match.Progress != 0 {
				t.Errorf("Progress %v, ожидался 0", match.Progress)
			}
		})
	}
}

// Совпадающие координаты соседних остановок не должны давать NaN: деление на
// нулевую длину сегмента — обычная ошибка, которая молча портит признак.
func TestMatchOnZeroLengthSegment(t *testing.T) {
	route := NewRoute([]schedule.Stop{
		stopAt(0, 37.600, 55.800),
		stopAt(1, 37.600, 55.800), // та же точка
		stopAt(2, 37.610, 55.800),
	})
	match := route.MatchPoint(37.605, 55.800, 90, true, DefaultSearchRadiusM)
	if math.IsNaN(match.OffsetM) || math.IsNaN(match.AlongM) || math.IsNaN(match.BearingDeg) {
		t.Errorf("вырожденный сегмент дал нечисловой результат: %+v", match)
	}
	if !match.OnRoute {
		t.Error("точка на длинном сегменте после вырожденного обязана быть на маршруте")
	}
}

func TestDistanceToStopKeepsDirection(t *testing.T) {
	route := straightRoute()
	// Привязка между остановками 102 (накопленное 628 м) и 103 (941 м).
	match := route.MatchPoint(37.6125, 55.800, 90, true, DefaultSearchRadiusM)

	// Остановка 102 позади: по маршруту назад около 157 м.
	metres, ahead, ok := route.DistanceToStop(match, 102)
	if !ok {
		t.Fatal("остановка 102 есть в маршруте")
	}
	if ahead {
		t.Error("остановка позади не должна считаться впереди")
	}
	if metres < 130 || metres > 185 {
		t.Errorf("DistanceToStop %.0f м, ожидалось около 157", metres)
	}

	// Остановка 103 впереди: около 157 м.
	metres, ahead, ok = route.DistanceToStop(match, 103)
	if !ok || !ahead {
		t.Errorf("остановка 103 впереди: ok=%v ahead=%v", ok, ahead)
	}
	if metres < 130 || metres > 185 {
		t.Errorf("DistanceToStop %.0f м, ожидалось около 157", metres)
	}

	// От начала маршрута всё, что дальше, — впереди, включая 102.
	first := route.MatchPoint(37.600, 55.800, 90, true, DefaultSearchRadiusM)
	if _, ahead, _ = route.DistanceToStop(first, 102); !ahead {
		t.Error("от начала маршрута остановка 102 обязана быть впереди")
	}

	if _, _, ok := route.DistanceToStop(match, 999); ok {
		t.Error("несуществующая остановка обязана давать ok == false")
	}
}

func TestAlongToStop(t *testing.T) {
	route := straightRoute()
	along, ok := route.AlongToStop(102)
	if !ok {
		t.Fatal("остановка 102 есть в маршруте")
	}
	if along < 550 || along > 700 {
		t.Errorf("AlongToStop(102) %.0f, ожидалось около 630", along)
	}
	if _, ok := route.AlongToStop(999); ok {
		t.Error("несуществующая остановка обязана давать ok == false")
	}
}

func TestStopsInRange(t *testing.T) {
	route := straightRoute()
	// Накопленные расстояния: 0, 314, 628, 941 м. В (300, 1000] попадают
	// три остановки из четырёх.
	stops := route.StopsInRange(300, 1000)
	wantIDs := []int64{101, 102, 103}
	if len(stops) != len(wantIDs) {
		t.Fatalf("в диапазоне (300, 1000] оказалось %d остановок, ожидалось %d: %v",
			len(stops), len(wantIDs), stops)
	}
	for i, want := range wantIDs {
		if stops[i].ActionID != want {
			t.Errorf("остановка %d в позиции %d имеет id %d, ожидался %d", i, i, stops[i].ActionID, want)
		}
	}
	if empty := route.StopsInRange(1e9, 2e9); len(empty) != 0 {
		t.Errorf("за пределами маршрута остановок быть не должно, получено %d", len(empty))
	}
}

func TestAngleDiffDegWraps(t *testing.T) {
	// Знак на границе в 180° не определён: 180 и -180 — один и тот же угол.
	// Поэтому на границе проверяется попадание в диапазон, а на остальных
	// случаях — конкретное значение.
	tests := []struct {
		a, b, want float64
	}{
		{a: 10, b: 350, want: 20},
		{a: 350, b: 10, want: -20},
		{a: 90, b: 90, want: 0},
		{a: 0, b: 179, want: -179},
		{a: 0, b: 181, want: 179},
		{a: 359, b: 1, want: -2},
		{a: 90, b: 450, want: 0},
	}
	for _, tc := range tests {
		if got := AngleDiffDeg(tc.a, tc.b); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("AngleDiffDeg(%v, %v) = %v, ожидалось %v", tc.a, tc.b, got, tc.want)
		}
	}
	for _, pair := range [][2]float64{{0, 180}, {180, 0}, {0, 181}, {181, 0}, {90, 270}} {
		got := AngleDiffDeg(pair[0], pair[1])
		if got < -180 || got > 180 {
			t.Errorf("AngleDiffDeg(%v, %v) = %v вне [-180, 180]", pair[0], pair[1], got)
		}
	}
}

// repoPath поднимает путь до корня репозитория.
func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	dir, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("не удалось определить корень репозитория: %v", err)
	}
	return filepath.Join(append([]string{dir}, parts...)...)
}

// Модель маршрута проверяется на реальных данных: ломаная из координат
// остановок обязана ложиться под телеметрию плотных городских маршрутов.
//
// Проверяются только те 11 машин, у которых есть размеченные точки прогноза:
// именно они попадут в скоринг, и только для них качество привязки имеет
// значение. Две машины с расписанием, но без разметки, намеренно не
// проверяются:
//
//   - 130072 — разреженный маршрут: отрезок между соседними остановками
//     проходит по местности вне дороги, медиана смещения 3430 м;
//   - 134494 — её телеметрия находится в 21 км от собственного расписания и
//     в другое время суток. Это дефект исходных данных, а не модели
//     маршрута, и притягивать такие точки к маршруту нельзя.
func TestRouteFitsTelemetryOfLabelledVehicles(t *testing.T) {
	planPath := repoPath(t, "validate", "schedule_plan.csv")
	trafficPath := repoPath(t, "validate", "traffic.csv")
	pointsPath := repoPath(t, "validate", "points.csv")
	if _, err := os.Stat(planPath); err != nil {
		t.Skipf("нет %s: %v", planPath, err)
	}
	labelled := labelledVehicles(t, pointsPath)
	if len(labelled) == 0 {
		t.Skip("нет размеченных точек")
	}
	plan, err := schedule.LoadFile(planPath)
	if err != nil {
		t.Fatalf("расписание не загрузилось: %v", err)
	}
	telemetry := loadTraffic(t, trafficPath)

	// Каждая точка проверяется линейным просмотром всех отрезков смены
	// (до нескольких сотен), поэтому для оценки медианы хватает выборки:
	// полный проход по 46 тысячам точек занимает десятки секунд.
	const sampleEvery = 7
	checked := 0
	for trID := range labelled {
		points := telemetry[trID]
		if len(points) < 100 {
			continue
		}
		route := NewRoute(plan.Stops(trID))
		if route.Segments() == 0 {
			t.Errorf("машина %d: маршрут не построен", trID)
			continue
		}
		offsets := make([]float64, 0, len(points)/sampleEvery+1)
		within := 0
		for i := 0; i < len(points); i += sampleEvery {
			match := route.MatchPoint(points[i].lon, points[i].lat, 0, false, DefaultSearchRadiusM)
			offsets = append(offsets, match.OffsetM)
			if match.OnRoute {
				within++
			}
		}
		checked++
		sortFloats(offsets)
		median := offsets[len(offsets)/2]
		share := float64(within) / float64(len(offsets))

		if trID == 130072 {
			// Разреженный маршрут: большое смещение законно, но не всё
			// время — иначе привязка не работала бы вовсе.
			if share < 0.3 {
				t.Errorf("130072: на маршруте только %.1f%% точек, ожидалось не меньше 30%%", 100*share)
			}
			continue
		}
		// Медиана — робастная мера качества модели: она отражает саму
		// дорогу и не зависит от простоев вне смены.
		if median > 150 {
			t.Errorf("машина %d: медианное смещение %.0f м, ожидалось не больше 150", trID, median)
		}
		// Доля на маршруте ограничена снизу: у части машин значимая часть
		// суток проходит вне своей смены (депо, перегон, второй маршрут),
		// и это норма, а не дефект. Минимум среди размеченных машин по
		// измерениям на validate — около 71 % у 122658.
		if share < 0.65 {
			t.Errorf("машина %d: на маршруте только %.1f%% точек, ожидалось не меньше 65%%",
				trID, 100*share)
		}
	}
	if checked == 0 {
		t.Skip("не нашлось машин с достаточной телеметрией")
	}
}

// labelledVehicles возвращает номера машин, для которых есть разметка.
func labelledVehicles(t *testing.T, path string) map[int64]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("нет %s: %v", path, err)
	}
	out := map[int64]bool{}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i, line := range lines {
		if i == 0 {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		trID, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out[trID] = true
	}
	return out
}

type trafficPoint struct {
	lon, lat float64
}

func loadTraffic(t *testing.T, path string) map[int64][]trafficPoint {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Skipf("нет %s: %v", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.ReuseRecord = true
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		t.Fatalf("не удалось прочитать заголовок телеметрии: %v", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	need := []string{"tr_id", "lat", "lon", "location_valid"}
	for _, name := range need {
		if _, ok := col[name]; !ok {
			t.Skipf("в телеметрии нет колонки %q", name)
		}
	}

	out := map[int64][]trafficPoint{}
	for {
		record, err := reader.Read()
		if err != nil {
			break
		}
		if len(record) <= col["lon"] {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(record[col["location_valid"]]), "true") {
			continue
		}
		latRaw := strings.TrimSpace(record[col["lat"]])
		lonRaw := strings.TrimSpace(record[col["lon"]])
		if latRaw == "" || lonRaw == "" {
			continue
		}
		lat, err1 := strconv.ParseFloat(latRaw, 64)
		lon, err2 := strconv.ParseFloat(lonRaw, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		trID, err := strconv.ParseInt(strings.TrimSpace(record[col["tr_id"]]), 10, 64)
		if err != nil {
			continue
		}
		out[trID] = append(out[trID], trafficPoint{lon: lon, lat: lat})
	}
	return out
}

func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func ExampleRoute_MatchPoint() {
	route := straightRoute()
	match := route.MatchPoint(37.6025, 55.800, 90, true, DefaultSearchRadiusM)
	fmt.Println("on route:", match.OnRoute)
	fmt.Println("azimuth:", int(match.BearingDeg+0.5))
	fmt.Println("heading error:", int(match.HeadingErrorDeg+0.5))
	// Вывод:
	// on route: true
	// azimuth: 90
	// heading error: 0
}

// Плановое время в произвольной точке маршрута интерполируется между
// остановками: без этого признак «отклонение от расписания» нечем считать.
func TestPlannedTimeAtAlongInterpolates(t *testing.T) {
	route := straightRoute()
	// Ожидаемые точки берутся у самого маршрута, а не из констант: длина
	// сегмента зависит от широты, и жёстко заданные метры расходятся с
	// реальными на десятые доли.
	second, ok := route.AlongToStop(101)
	if !ok {
		t.Fatal("у маршрута нет второй остановки")
	}

	if got, _ := route.PlannedTimeAtAlong(0); !got.Equal(base) {
		t.Errorf("в начале маршрута %v, ожидалось %v", got, base)
	}
	if got, _ := route.PlannedTimeAtAlong(second); !got.Equal(base.Add(time.Minute)) {
		t.Errorf("на первой остановке %v, ожидалось %v", got, base.Add(time.Minute))
	}
	// Половина между первой и второй остановкой — плюс 30 секунд.
	got, found := route.PlannedTimeAtAlong(second / 2)
	if !found {
		t.Fatal("точка внутри маршрута обязана давать время")
	}
	want := base.Add(30 * time.Second)
	if got.Sub(want) > time.Second || want.Sub(got) > time.Second {
		t.Errorf("на середине сегмента %v, ожидалось около %v", got, want)
	}
	// За концом маршрута — последнее известное время, а не ноль.
	if got, _ := route.PlannedTimeAtAlong(1e9); !got.Equal(base.Add(3 * time.Minute)) {
		t.Errorf("за концом маршрута %v, ожидалось %v", got, base.Add(3*time.Minute))
	}
	if _, ok := NewRoute(nil).PlannedTimeAtAlong(100); ok {
		t.Error("пустой маршрут не имеет планового времени")
	}
}

func TestStopIndexAtAlong(t *testing.T) {
	route := straightRoute()
	second, _ := route.AlongToStop(101)
	third, _ := route.AlongToStop(102)
	tests := []struct {
		along float64
		want  int
	}{
		{along: 0, want: 0},
		{along: second - 1, want: 0},
		{along: second, want: 1},
		{along: third - 1, want: 1},
		{along: third, want: 2},
		{along: 1e9, want: 3},
	}
	for _, tc := range tests {
		got, ok := route.StopIndexAtAlong(tc.along)
		if !ok || got != tc.want {
			t.Errorf("StopIndexAtAlong(%v) = %d, ok=%v; ожидалось %d", tc.along, got, ok, tc.want)
		}
	}
	if _, ok := NewRoute(nil).StopIndexAtAlong(10); ok {
		t.Error("пустой маршрут не имеет индекса")
	}
}
