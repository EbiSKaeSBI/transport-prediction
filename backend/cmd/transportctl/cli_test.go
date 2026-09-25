package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/features"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

// testFrame — минимальный кадр для проверки формы JSON.
func testFrame() *horizon.Frame {
	asOf := time.Date(2026, 1, 6, 8, 3, 30, 0, time.UTC)
	planned := asOf.Add(630 * time.Second)
	return &horizon.Frame{
		SampleID:  "4242-114-1767686610",
		UnitID:    4242,
		TRID:      7,
		AsOf:      asOf,
		Ambiguous: true,
		Target: []horizon.Arrival{
			{ActionID: 114, Planned: planned, HorizonS: 630},
			{ActionID: 1114, Planned: planned, HorizonS: 630, Alternative: true},
		},
		Values:   map[string]*float64{"horizon_s": ptr(630.0), "slack_s": ptr(-190.5)},
		Features: features.Set{IsTerminalStop: false, TargetAmbiguous: true},
		Quality:  features.Quality{StalenessS: 0, PointsInWindow: 15, LagS: 0.25},
		Window:   []statestore.Point{{UnitID: 4242}},
	}
}

// historyUntil и stateAt проверяются отдельно: обе обязаны обрезать историю
// по времени события, а не по времени приёма.
func TestHistoryAndStateNeverSeeFuture(t *testing.T) {
	base := time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)
	tk := &track{trID: 7, unitID: 4242, points: []statestore.Point{
		{EventTime: base, ReceiveTime: base},
		{EventTime: base.Add(time.Minute), ReceiveTime: base.Add(time.Minute)},
		// Пакет отправлен до T, но событие случилось после: в прогноз он
		// попасть не может, иначе это утечка.
		{EventTime: base.Add(5 * time.Minute), ReceiveTime: base.Add(30 * time.Second)},
	}}
	at := base.Add(2 * time.Minute)
	history := historyUntil(tk, at)
	if len(history) != 2 {
		t.Fatalf("точек в истории %d, ожидалось 2", len(history))
	}
	for _, p := range history {
		if p.EventTime.After(at) {
			t.Errorf("точка %v попала в историю после момента %v", p.EventTime, at)
		}
	}
	if _, ok := stateAt(history, at); !ok {
		t.Error("состояние обязано существовать, если точка не позже T")
	}
	if _, ok := stateAt(history, base.Add(-time.Minute)); ok {
		t.Error("до первой точки состояния быть не должно")
	}
	if got := historyUntil(tk, base.Add(-time.Hour)); got != nil {
		t.Error("до первой точки история обязана быть пустой")
	}
}

func TestParseEventTimeAcceptsEveryWrittenFormat(t *testing.T) {
	want := time.Date(2026, 1, 6, 12, 30, 31, 0, time.UTC)
	for _, raw := range []string{
		"2026-01-06 12:30:31.000000",
		"2026-01-06 12:30:31",
		"2026-01-06T12:30:31Z",
		"2026-01-06T12:30:31.000Z",
	} {
		got, ok := parseEventTime(raw)
		if !ok {
			t.Errorf("%q не разобрано", raw)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%q разобрано как %v, ожидалось %v", raw, got, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("%q дало зону %v, ожидался UTC: без зоны файл читается как UTC, "+
				"иначе момент прогноза сместится на часы", raw, got.Location())
		}
	}
	// Микросекунды не должны теряться: в файлах телеметрии они есть, и
	// потеря 500 мс на метке с горизонтом 600 с незаметна, но на границе
	// окна меняет, попала ли остановка в цель.
	got, ok := parseEventTime("2026-01-06 12:30:31.500000")
	if !ok || got.Nanosecond() != 500_000_000 {
		t.Errorf("микросекунды потеряны: %v (ok=%v)", got, ok)
	}
	if _, ok := parseEventTime("вчера"); ok {
		t.Error("мусор обязан отклоняться, а не превращаться в нулевое время")
	}
}

const trafficHeader = "packet_id,tr_id,unit_id,event_time,device_event_id,location_valid," +
	"gps_time,lon,lat,alt,speed,heading,receive_time,is_hist_data"

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTrafficSkipsUnusableRows(t *testing.T) {
	path := writeFile(t, "traffic.csv", trafficHeader+"\n"+
		// нормальная строка
		"-1,115106,664030,2026-01-06 12:30:31.000000,0,True,,37.774,55.813,165.0,12.5,159.0,2026-01-06 12:30:32.000000,False\n"+
		// без координат: скорость и положение есть, точки не будет
		"-2,115106,664030,2026-01-06 12:30:41.000000,0,False,,,,,0.0,0.0,2026-01-06 12:30:42.000000,False\n"+
		// нечитаемое время
		"-3,115106,664030,вчера,0,True,,37.774,55.813,165.0,0.0,0.0,2026-01-06 12:30:52.000000,False\n"+
		// нулевое время события: пакет есть, а момента в нём нет
		"-7,115106,664030,,0,True,,37.774,55.813,165.0,0.0,0.0,2026-01-06 12:30:53.000000,False\n"+
		// нет tr_id
		"-4,,664030,2026-01-06 12:30:51.000000,0,True,,37.774,55.813,165.0,0.0,0.0,2026-01-06 12:30:52.000000,False\n"+
		// вторая машина, позже по времени — порядок в файле сбит
		"-5,115107,664031,2026-01-06 12:35:00.000000,0,True,,37.700,55.800,10.0,0.0,90.0,2026-01-06 12:35:01.000000,False\n"+
		"-6,115107,664031,2026-01-06 12:31:00.000000,0,True,,37.750,55.800,10.0,5.0,90.0,2026-01-06 12:31:01.000000,False\n")

	tracks, err := loadTraffic(path)
	if err != nil {
		t.Fatalf("loadTraffic: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("машин %d, ожидалось 2", len(tracks))
	}
	first := tracks[664030]
	// Пакет без координат остаётся в истории: он реален, и о нём честно
	// знать, что координат в нём не было. Отбрасывать его молча означало бы
	// занижать число точек и прятать качество телеметрии.
	if first == nil || len(first.points) != 2 {
		t.Fatalf("точек у первой машины %v, ожидалось 2", first)
	}
	if first.points[1].LocationValid {
		t.Error("пакет без координат помечен как достоверный")
	}
	if first.trID != 115106 {
		t.Errorf("tr_id %d, ожидался 115106", first.trID)
	}
	p := first.points[0]
	if !p.LocationValid || p.Longitude != 37.774 || p.SpeedKmh != 12.5 {
		t.Errorf("точка разобрана неверно: %+v", p)
	}
	// Время приёма берётся из своей колонки: подстановка event_time обнулила
	// бы задержку телеметрии, и качество данных выглядело бы идеальным.
	if !p.ReceiveTime.After(p.EventTime) {
		t.Errorf("receive_time %v не позже event_time %v", p.ReceiveTime, p.EventTime)
	}
	second := tracks[664031]
	if len(second.points) != 2 {
		t.Fatalf("точек у второй машины %d, ожидалось 2", len(second.points))
	}
	if !second.points[0].EventTime.Before(second.points[1].EventTime) {
		t.Error("точки не отсортированы по времени события")
	}
}

func TestLoadTrafficFallsBackToEventTime(t *testing.T) {
	// Раздача может не содержать receive_time. Тогда время приёма равно
	// времени события: задержка неизвестна, но подставлять ноль нельзя —
	// это выглядело бы как мгновенная доставка.
	path := writeFile(t, "t.csv",
		"tr_id,unit_id,event_time,location_valid,lon,lat,speed,heading\n"+
			"5,42,2026-01-06 12:30:31,True,37.774,55.813,0,0\n")
	tracks, err := loadTraffic(path)
	if err != nil {
		t.Fatal(err)
	}
	p := tracks[42].points[0]
	if !p.ReceiveTime.Equal(p.EventTime) {
		t.Errorf("receive_time %v, ожидалось равным event_time %v", p.ReceiveTime, p.EventTime)
	}
}

func TestLoadTrafficReportsBadFile(t *testing.T) {
	if _, err := loadTraffic(filepath.Join(t.TempDir(), "нет.csv")); err == nil {
		t.Error("отсутствующий файл обязан давать ошибку, а не пустой результат")
	}
}

func TestLoadLabels(t *testing.T) {
	path := writeFile(t, "labels.csv",
		"sample_id,tr_id,T,target_stop_id,target_time_begin,cur_dev_s,target_delay_s,target_class\n"+
			"129964_1767665100,129964,2026-01-06 02:05:00,53700336299,2026-01-06 02:18:00,0.0,-45.0,ontime\n"+
			"битая строка без чисел,,,,,,,\n")
	labels, err := loadLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 {
		t.Fatalf("меток %d, ожидалась 1: неразбираемые строки обязаны отбрасываться, "+
			"а не превращаться в нулевые метки", len(labels))
	}
	l := labels[0]
	if l.trID != 129964 || l.targetStop != 53700336299 {
		t.Errorf("метка разобрана неверно: %+v", l)
	}
	if l.curDevS != 0 || l.targetLagS != -45 {
		t.Errorf("числа разобраны неверно: cur_dev=%v lag=%v", l.curDevS, l.targetLagS)
	}
	if !l.t.Equal(time.Date(2026, 1, 6, 2, 5, 0, 0, time.UTC)) {
		t.Errorf("T разобрано как %v", l.t)
	}
	if l.class != "ontime" {
		t.Errorf("класс %q", l.class)
	}
}

func TestWriteFrameEmitsStableJSON(t *testing.T) {
	var buffer bytes.Buffer
	writer := &lineWriter{writer: bufio.NewWriter(&buffer)}
	frame := testFrame()
	if err := writeFrame(writer, frame); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	line := strings.TrimSpace(buffer.String())
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("строка кадра не разбирается как JSON: %v\n%s", err, line)
	}
	for _, key := range []string{"sample_id", "unit_id", "tr_id", "t", "target_stop_id",
		"horizon_s", "ambiguous", "variants", "values", "staleness_s", "points_in_window", "lag_s"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("в кадре нет обязательного поля %q", key)
		}
	}
	values, ok := decoded["values"].(map[string]any)
	if !ok {
		t.Fatalf("values не объект: %T", decoded["values"])
	}
	if _, ok := values["horizon_s"]; !ok {
		t.Error("в значениях кадра нет horizon_s: без него модель не знает горизонта")
	}
	if lines := strings.Count(buffer.String(), "\n"); lines != 1 {
		t.Errorf("строк %d, ожидалась 1: по одному кадру на строку JSONL", lines)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	err := run([]string{"такой-команды-нет"})
	if err == nil {
		t.Fatal("неизвестная команда обязана давать ошибку")
	}
	if !strings.Contains(err.Error(), "такой-команды-нет") {
		t.Errorf("ошибка не называет команду: %v", err)
	}
}

func TestRunFeaturesRequiresPlanAndBinding(t *testing.T) {
	// Без расписания прогнозировать нечего. Молчаливый запуск, который
	// напечатает «0 кадров», выглядел бы как успех.
	if err := run([]string{"features"}); err == nil {
		t.Error("features без --plan и --binding обязан падать")
	}
	if err := run([]string{"replay", "--plan", "нет.csv"}); err == nil {
		t.Error("replay с несуществующим планом обязан падать")
	}
}

func TestRunPrintsUsageOnHelp(t *testing.T) {
	if err := run([]string{"--help"}); err != nil {
		t.Errorf("--help: %v", err)
	}
	if err := run(nil); err == nil {
		t.Error("без аргументов обязана быть ошибка")
	}
}

// ptr — адрес значения для карты признаков, где пропуск это nil, а не
// отсутствие ключа.
func ptr[T any](v T) *T { return &v }
