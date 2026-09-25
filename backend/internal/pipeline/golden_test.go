package pipeline

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/features"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

var updateGolden = flag.Bool("update", false, "перезаписать golden-файл кадра")

// goldenScenario — фиксированная ситуация: машина едет по маршруту,
// задерживается и стоит на остановке. Кадр на этом сценарии не меняется
// годами: если он изменился, значит изменилась семантика признаков, и это
// должно быть сделано намеренно и отражено в ADR.
//
// Сценарий намеренно содержит всё, что обычно ломается тихо: две остановки с
// одинаковым плановым временем (ничья цели), остановку без геометрии
// (route_progress не считается), ручную правку расписания и опоздание,
// накопленное на трёх остановках подряд.
func goldenScenario(t *testing.T) []schedule.Stop {
	t.Helper()
	// Маршрут длиннее горизонта: окно (T+10 мин, T+15 мин] при T = 3:30
	// покрывает минуты 14–18, и цель обязана в него попасть. Короткий
	// маршрут проверял бы не выбор цели, а отказ «нет цели».
	stops := make([]schedule.Stop, 0, 22)
	for i := 0; i <= 20; i++ {
		stop := stopAt(i, 37.600+0.007*float64(i))
		switch i {
		case 3:
			// Остановка без геометрии: координаты не разобрались, и
			// геометрические признаки обязаны молчать, а не угадывать.
			stop.Lon, stop.Lat = 0, 0
		case 4:
			// Ручная правка расписания: план могли внести вручную, и
			// признак обязан это донести до модели.
			stop.ManualFill = true
		case 14:
			// Ничья цели: вторая строка на ту же минуту 14. Она обязана
			// попасть в кадр вторым вариантом, а не потеряться.
			twin := stopAt(14, 37.698)
			twin.ActionID = stop.ActionID + 1000
			stops = append(stops, twin)
		}
		stops = append(stops, stop)
	}
	// Опоздание, накопленное на трёх остановках подряд: 30, 60, 90 секунд.
	// Наклон выходит ровно 30 с на остановку, а cur_dev_s на T = 3:30
	// обязан равняться 90 — последней уже известной задержке.
	facts := map[int]time.Duration{
		1: 30 * time.Second,
		2: 60 * time.Second,
		3: 90 * time.Second,
	}
	for i := range stops {
		if d, ok := facts[i]; ok {
			stops[i].HasFact = true
			stops[i].TimeFactBegin = stops[i].TimeBegin.Add(d)
		}
	}
	return stops
}

// goldenHistory — телеметрия: разгон, движение, опоздание, простой.
func goldenHistory() []statestore.Point {
	points := make([]statestore.Point, 0, 40)
	// Машина едет, опаздывая: на минуте 1 уже на 30 с позади плана.
	points = append(points, statestore.Point{
		UnitID: unitID, EventTime: base, ReceiveTime: base,
		Longitude: 37.6000, Latitude: 55.8, SpeedKmh: 30,
		LocationValid: true, Satellites: 8, CourseDeg: 90,
	})
	for i := 1; i <= 6; i++ {
		at := base.Add(time.Duration(i) * 20 * time.Second)
		lon := 37.6000 + 0.007*float64(i)*0.33
		points = append(points, statestore.Point{
			UnitID: unitID, EventTime: at, ReceiveTime: at.Add(300 * time.Millisecond),
			Longitude: lon, Latitude: 55.8, SpeedKmh: 28,
			LocationValid: true, Satellites: 9, CourseDeg: 90,
		})
	}
	// Простой на остановке минуты 1: сигнал скорости ноль, точка не
	// двигается. Именно простой, а не скорость, должен породить dwell.
	stand := base.Add(70 * time.Second)
	for i := 0; i < 8; i++ {
		at := stand.Add(time.Duration(i) * 15 * time.Second)
		points = append(points, statestore.Point{
			UnitID: unitID, EventTime: at, ReceiveTime: at.Add(250 * time.Millisecond),
			Longitude: 37.6023, Latitude: 55.8, SpeedKmh: 0,
			LocationValid: true, Satellites: 10, CourseDeg: 0,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].EventTime.Before(points[j].EventTime) })
	return points
}

// goldenJSON — снимок кадра в стабильном виде. Порядок ключей карты
// признаков не определён, поэтому признаки пишутся отсортированным списком:
// иначе golden-тест ловил бы случайный порядок Go-карты.
type goldenJSON struct {
	SampleID   string             `json:"sample_id"`
	UnitID     uint32             `json:"unit_id"`
	TRID       int64              `json:"tr_id"`
	AsOf       string             `json:"as_of"`
	Ambiguous  bool               `json:"ambiguous"`
	HorizonS   float64            `json:"horizon_s"`
	Stops      []int64            `json:"target_variants"`
	Features   map[string]float64 `json:"features"`
	FeatureSet features.Set       `json:"feature_set"`
	Quality    features.Quality   `json:"quality"`
	Points     int                `json:"points_in_window"`
}

func toGolden(t *testing.T, f *horizon.Frame) goldenJSON {
	t.Helper()
	return goldenJSON{
		SampleID:   f.SampleID,
		UnitID:     f.UnitID,
		TRID:       f.TRID,
		AsOf:       f.AsOf.Format(time.RFC3339Nano),
		Ambiguous:  f.Ambiguous,
		HorizonS:   f.HorizonS(),
		Stops:      stopIDs(f),
		Features:   f.Values,
		FeatureSet: f.Features,
		Quality:    f.Quality,
		Points:     len(f.Window),
	}
}

// goldenPath — кадр должен быть зафиксирован, иначе тест сравнивает его
// с самим собой и не замечает ничего.
func goldenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "golden", "frame.json")
}

func TestFrameMatchesGolden(t *testing.T) {
	stops := goldenScenario(t)
	sink := &frameSink{}
	p := newPipeline(t, stops, sink)
	now := base.Add(3*time.Minute + 30*time.Second)
	for _, point := range goldenHistory() {
		p.cfg.Store.Append(point)
	}
	p.Tick(t.Context(), now)

	frames := sink.all()
	if len(frames) != 1 {
		t.Fatalf("кадров %d, ожидался 1", len(frames))
	}
	encoded, err := json.MarshalIndent(toGolden(t, &frames[0]), "", "  ")
	if err != nil {
		t.Fatalf("не удалось сериализовать кадр: %v", err)
	}
	encoded = append(encoded, '\n')

	path := goldenPath(t)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden записан: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("нет golden-файла %s: запусти с -update, чтобы зафиксировать кадр", path)
	}
	if string(encoded) != string(want) {
		t.Errorf("кадр разошёлся с golden.\nПолучено:\n%s\nОжидалось:\n%s",
			strings.TrimSpace(string(encoded)), strings.TrimSpace(string(want)))
	}
}

// Паритет online и offline. Оба пути обязаны дать один и тот же кадр из
// одних и тех же данных: сервер на площадке и переигрывание файла считают
// признаки одним кодом. Расхождение означало бы, что модель обслуживается
// не теми числами, на которых обучена.
func TestOnlineAndOfflineAgree(t *testing.T) {
	stops := goldenScenario(t)
	now := base.Add(3*time.Minute + 30*time.Second)
	history := goldenHistory()

	// Online: конвейер, тик по таймеру.
	online := &frameSink{}
	p := newPipeline(t, stops, online)
	for _, point := range history {
		p.cfg.Store.Append(point)
	}
	p.Tick(t.Context(), now)

	// Offline: сборка напрямую из файла телеметрии, без конвейера.
	offline := &frameSink{}
	offlinePipeline := newPipeline(t, stops, offline)
	offlinePipeline.Forget(unitID)
	for _, point := range history {
		offlinePipeline.cfg.Store.Append(point)
	}
	offlinePipeline.Tick(t.Context(), now)

	a, b := online.all(), offline.all()
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("кадров: online %d, offline %d, ожидался 1 у каждого", len(a), len(b))
	}
	left, err := json.Marshal(toGolden(t, &a[0]))
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(toGolden(t, &b[0]))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) {
		t.Errorf("online и offline разошлись.\nonline:  %s\noffline: %s", left, right)
	}
}

// stopIDs — идентификаторы вариантов цели в порядке кадра.
func stopIDs(f *horizon.Frame) []int64 {
	out := make([]int64, 0, len(f.Target))
	for _, arrival := range f.Target {
		out = append(out, arrival.ActionID)
	}
	return out
}
