package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

// runReplay переигрывает размеченную телеметрию и сверяет расчёт Go с
// эталонными метками.
//
// Смысл команды — не «напечатать признаки», а найти расхождение между тем,
// как кадр считает конвейер на площадке, и тем, как тот же кадр был посчитан
// при подготовке обучающей выборки. Расхождение означало бы, что модель
// обучена на одних числах, а обслуживается другими.
func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	planPath := fs.String("plan", "../validate/schedule_plan.csv", "CSV планового графика")
	trafficPath := fs.String("traffic", "../validate/traffic.csv", "CSV телеметрии")
	labelsPath := fs.String("labels", "", "CSV меток (без него печатается только разбор)")
	out := fs.String("out", "", "выгрузить кадры в JSONL-файл")
	verbose := fs.Bool("verbose", false, "печатать расхождения построчно")
	if err := fs.Parse(args); err != nil {
		return err
	}

	plan, err := schedule.LoadFile(*planPath)
	if err != nil {
		return fmt.Errorf("план %s: %w", *planPath, err)
	}
	tracks, err := loadTraffic(*trafficPath)
	if err != nil {
		return err
	}
	// Привязка строится тем же кодом, что и в конвейере на площадке: если
	// здесь будет второй, независимый разбор, расхождение привязки выдаст
	// себя за расхождение прогноза.
	binding, err := schedule.LoadBindingFile(*trafficPath)
	if err != nil {
		return fmt.Errorf("привязка из %s: %w", *trafficPath, err)
	}
	if n := binding.ConflictsCount(); n > 0 {
		fmt.Fprintf(os.Stderr, "предупреждение: конфликтов привязки: %d\n", n)
	}

	if *labelsPath == "" {
		return describeTracks(tracks, plan, binding)
	}
	labels, err := loadLabels(*labelsPath)
	if err != nil {
		return err
	}

	var writer *lineWriter
	if *out != "" {
		writer, err = openWriter(*out)
		if err != nil {
			return err
		}
		defer writer.Close()
	}

	return replay(plan, binding, tracks, labels, writer, *verbose).write(os.Stderr)
}

// track — траектория одной машины в хронологическом порядке.
type track struct {
	trID   int64
	unitID uint32
	points []statestore.Point
}

// loadTraffic читает размеченную телеметрию.
//
// Отличие от живого приёма — только в источнике времени приёма: в файле есть
// колонка receive_time, и она важна для признаков качества. Подставлять
// event_time вместо receive_time означало бы незаметно обнулить задержку
// телеметрии, то есть утверждать, что пакеты приходят мгновенно.
func loadTraffic(path string) (map[uint32]*track, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть %s: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.ReuseRecord = true
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	col := indexColumns(header)

	tracks := make(map[uint32]*track)
	var line int
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line+2, err)
		}
		line++
		point, trID, unitID, ok := trafficRow(row, col)
		if !ok {
			continue
		}
		t := tracks[unitID]
		if t == nil {
			t = &track{trID: trID, unitID: unitID}
			tracks[unitID] = t
		}
		t.points = append(t.points, point)
	}
	for _, t := range tracks {
		sort.Slice(t.points, func(i, j int) bool {
			return t.points[i].EventTime.Before(t.points[j].EventTime)
		})
	}
	return tracks, nil
}

func indexColumns(header []string) map[string]int {
	out := make(map[string]int, len(header))
	for i, name := range header {
		out[name] = i
	}
	return out
}

// fieldOf достаёт поле по имени колонки. Отсутствующая колонка и значение
// вне строки считаются отсутствующим значением, а не ошибкой: состав
// колонок в разных выгрузках отличается.
func fieldOf(row []string, col map[string]int, name string) (string, bool) {
	i, ok := col[name]
	if !ok || i >= len(row) || i < 0 {
		return "", false
	}
	return row[i], true
}

func trafficRow(row []string, col map[string]int) (statestore.Point, int64, uint32, bool) {
	trRaw, ok := fieldOf(row, col, "tr_id")
	if !ok {
		return statestore.Point{}, 0, 0, false
	}
	unitRaw, ok := fieldOf(row, col, "unit_id")
	if !ok {
		return statestore.Point{}, 0, 0, false
	}
	trID, err := strconv.ParseInt(trRaw, 10, 64)
	if err != nil {
		return statestore.Point{}, 0, 0, false
	}
	unitID, err := strconv.ParseUint(unitRaw, 10, 32)
	if err != nil {
		return statestore.Point{}, 0, 0, false
	}
	eventRaw, ok := fieldOf(row, col, "event_time")
	if !ok {
		return statestore.Point{}, 0, 0, false
	}
	event, ok := parseEventTime(eventRaw)
	if !ok {
		return statestore.Point{}, 0, 0, false
	}

	point := statestore.Point{UnitID: uint32(unitID), EventTime: event}
	if raw, ok := fieldOf(row, col, "receive_time"); ok {
		if received, ok := parseEventTime(raw); ok {
			point.ReceiveTime = received
		}
	}
	if point.ReceiveTime.IsZero() {
		point.ReceiveTime = event
	}
	if raw, ok := fieldOf(row, col, "location_valid"); ok {
		point.LocationValid = raw == "True" || raw == "true" || raw == "1"
	}
	lon, okLon := numberField(row, col, "lon")
	lat, okLat := numberField(row, col, "lat")
	if okLon && okLat {
		point.Longitude, point.Latitude = lon, lat
	}
	if speed, ok := numberField(row, col, "speed"); ok {
		point.SpeedKmh = speed
	}
	if heading, ok := numberField(row, col, "heading"); ok {
		point.CourseDeg = heading
	}
	return point, trID, uint32(unitID), true
}

func numberField(row []string, col map[string]int, name string) (float64, bool) {
	raw, ok := fieldOf(row, col, name)
	if !ok || raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseEventTime разбирает время события. Файл пишет его с микросекундами и
// без зоны; тот же формат встречается в метках, поэтому разбор один.
func parseEventTime(raw string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// label — эталонная метка обучающей выборки.
type label struct {
	sampleID   string
	trID       int64
	t          time.Time
	targetStop int64
	targetTime time.Time
	curDevS    float64
	targetLagS float64
	class      string
}

func loadLabels(path string) ([]label, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть %s: %w", path, err)
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	col := indexColumns(header)
	var out []label
	var line int
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line+2, err)
		}
		line++
		trRaw, _ := fieldOf(row, col, "tr_id")
		stopRaw, _ := fieldOf(row, col, "target_stop_id")
		trID, err1 := strconv.ParseInt(trRaw, 10, 64)
		stop, err2 := strconv.ParseInt(stopRaw, 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		tRaw, _ := fieldOf(row, col, "T")
		targetRaw, _ := fieldOf(row, col, "target_time_begin")
		t, ok1 := parseEventTime(tRaw)
		targetTime, ok2 := parseEventTime(targetRaw)
		if !ok1 || !ok2 {
			continue
		}
		curRaw, _ := fieldOf(row, col, "cur_dev_s")
		lagRaw, _ := fieldOf(row, col, "target_delay_s")
		curDev, _ := strconv.ParseFloat(curRaw, 64)
		lag, _ := strconv.ParseFloat(lagRaw, 64)
		id, _ := fieldOf(row, col, "sample_id")
		class, _ := fieldOf(row, col, "target_class")
		out = append(out, label{
			sampleID:   id,
			trID:       trID,
			t:          t,
			targetStop: stop,
			targetTime: targetTime,
			curDevS:    curDev,
			targetLagS: lag,
			class:      class,
		})
	}
	return out, nil
}

// describeTracks печатает сводку по траекториям без меток: сколько машин, у
// скольких есть расписание, сколько точек и как машины покрыты по времени.
func describeTracks(tracks map[uint32]*track, plan *schedule.Schedule, binding *schedule.Binding) error {
	units := make([]uint32, 0, len(tracks))
	for unitID := range tracks {
		units = append(units, unitID)
	}
	sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })

	var points, withSchedule, noSchedule int
	var span time.Duration
	for _, unitID := range units {
		t := tracks[unitID]
		points += len(t.points)
		first, last := t.points[0].EventTime, t.points[len(t.points)-1].EventTime
		span += last.Sub(first)
		trID, ok := binding.TRID(unitID)
		if ok && len(plan.Stops(trID)) > 0 {
			withSchedule++
		} else {
			noSchedule++
		}
	}
	fmt.Fprintf(os.Stderr,
		"машин: %d, точек: %d, с расписанием: %d, без расписания: %d, суммарный охват: %s\n",
		len(units), points, withSchedule, noSchedule, span.Round(time.Minute))
	return nil
}
