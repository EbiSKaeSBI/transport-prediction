package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/telemetry"
)

// runFeatures строит кадры прогноза по сохранённой телеметрии.
//
// Это тот же путь, что и в serve, только вместо живого сокета историю читает
// файл. Совпадение результатов — это и есть требование о паритете: если replay
// считает иначе, чем сервер, один из них врёт, и определить, какой, можно
// только сравнением.
func runFeatures(args []string) error {
	fs := flag.NewFlagSet("features", flag.ContinueOnError)
	planPath := fs.String("plan", "", "CSV планового графика (обязателен)")
	bindingPath := fs.String("binding", "", "CSV соответствия tr_id и unit_id (обязателен)")
	input := fs.String("input", defaultJSONLPath,
		"телеметрия: JSONL из serve --jsonl либо CSV раздачи (формат определяется по первой строке)")
	out := fs.String("out", "-", "куда писать кадры (�� — stdout)")
	asOf := fs.String("at", "", "момент прогноза RFC3339 (по умолчанию — последняя точка каждой машины)")
	tick := fs.Duration("tick", 0, "шаг прогноза: 0 — только последняя точка")
	framesOut := fs.Bool("frames", false, "выводить признаки построчно, а не только сводку")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *planPath == "" || *bindingPath == "" {
		return fmt.Errorf("нужны --plan и --binding")
	}

	plan, err := schedule.LoadFile(*planPath)
	if err != nil {
		return fmt.Errorf("план %s: %w", *planPath, err)
	}
	binding, err := schedule.LoadBindingFile(*bindingPath)
	if err != nil {
		return fmt.Errorf("привязка %s: %w", *bindingPath, err)
	}

	tracks, err := loadPoints(*input)
	if err != nil {
		return err
	}

	sink, err := openWriter(*out)
	if err != nil {
		return err
	}
	defer sink.Close()

	planner := horizon.New()
	byUnit := make(map[uint32][]statestore.Point, len(tracks))
	units := make([]uint32, 0, len(tracks))
	for unitID, points := range tracks {
		byUnit[unitID] = points
		units = append(units, unitID)
	}
	sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })

	var created, refused int
	refusals := make(map[string]int)
	for _, unitID := range units {
		points := byUnit[unitID]
		trID, ok := binding.TRID(unitID)
		if !ok {
			refusals["нет_привязки"]++
			refused++
			continue
		}
		for _, t := range moments(points, *asOf, *tick) {
			stops := plan.Window(trID, t.Add(-15*time.Minute), t.Add(40*time.Minute))
			state, hasState := stateAt(points, t)
			decision := planner.Plan(horizon.Request{
				T:        t,
				TRID:     trID,
				UnitID:   unitID,
				Stops:    stops,
				State:    state,
				HasState: hasState,
				History:  historyAt(points, t),
			})
			if !decision.Create() {
				refusals[string(decision.Reason)]++
				refused++
				continue
			}
			created++
			if !*framesOut {
				continue
			}
			if err := writeFrame(sink, decision.Frame); err != nil {
				return err
			}
		}
	}

	return writeSummary(created, refused, refusals, units)
}

// moments перечисляет моменты прогноза для одной машины.
func moments(points []statestore.Point, asOf string, tick time.Duration) []time.Time {
	if len(points) == 0 {
		return nil
	}
	last := points[len(points)-1].EventTime
	from := points[0].EventTime
	if asOf != "" {
		if t, err := time.Parse(time.RFC3339, asOf); err == nil {
			from = t
		}
	}
	to := last
	if asOf != "" {
		if t, err := time.Parse(time.RFC3339, asOf); err == nil {
			to = t
		}
	}
	if to.Before(from) {
		return nil
	}
	if tick <= 0 {
		return []time.Time{to}
	}
	var out []time.Time
	for t := from; !t.After(to); t = t.Add(tick) {
		out = append(out, t)
	}
	if len(out) == 0 || out[len(out)-1].Before(to) {
		out = append(out, to)
	}
	return out
}

// stateAt — состояние на момент t: последняя точка не позже t.
func stateAt(points []statestore.Point, t time.Time) (statestore.State, bool) {
	var last statestore.Point
	found := false
	count := 0
	for _, p := range points {
		if p.EventTime.After(t) {
			break
		}
		count++
		if !found || p.EventTime.After(last.EventTime) {
			last, found = p, true
		}
	}
	if !found {
		return statestore.State{}, false
	}
	return statestore.State{
		Point:          last,
		SeenAt:         last.ReceiveTime,
		StalenessS:     t.Sub(last.ReceiveTime).Seconds(),
		PointsInWindow: count,
	}, true
}

// historyAt — окно истории на момент t.
func historyAt(points []statestore.Point, t time.Time) []statestore.Point {
	out := make([]statestore.Point, 0, len(points))
	for _, p := range points {
		if p.EventTime.After(t) {
			break
		}
		out = append(out, p)
	}
	return out
}

type frameJSON struct {
	SampleID   string             `json:"sample_id"`
	UnitID     uint32             `json:"unit_id"`
	TRID       int64              `json:"tr_id"`
	T          string             `json:"t"`
	Target     int64              `json:"target_stop_id"`
	HorizonS   float64            `json:"horizon_s"`
	Ambiguous  bool               `json:"ambiguous"`
	Variants   int                `json:"variants"`
	Values     map[string]float64 `json:"values"`
	StalenessS float64            `json:"staleness_s"`
	Points     int32              `json:"points_in_window"`
	LagS       float64            `json:"lag_s"`
}

func writeFrame(w *lineWriter, f *horizon.Frame) error {
	payload := frameJSON{
		SampleID:   f.SampleID,
		UnitID:     f.UnitID,
		TRID:       f.TRID,
		T:          f.AsOf.Format(time.RFC3339),
		Target:     f.PrimaryStopID(),
		HorizonS:   f.HorizonS(),
		Ambiguous:  f.Ambiguous,
		Variants:   len(f.Target),
		Values:     f.Values,
		StalenessS: f.Quality.StalenessS,
		Points:     f.Quality.PointsInWindow,
		LagS:       f.Quality.LagS,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return w.Write(encoded)
}

func writeSummary(created, refused int, reasons map[string]int, units []uint32) error {
	// Сводка идёт в stderr, чтобы не мешать машинному чтению кадров из
	// stdout: потребитель не должен разбирать строки, чтобы узнать счётчики.
	fmt.Fprintf(os.Stderr, "машин: %d, кадров: %d, отказов: %d\n", len(units), created, refused)
	keys := make([]string, 0, len(reasons))
	for reason := range reasons {
		keys = append(keys, reason)
	}
	sort.Strings(keys)
	for _, reason := range keys {
		fmt.Fprintf(os.Stderr, "  %s: %d\n", reason, reasons[reason])
	}
	return nil
}

// lineWriter буферизует вывод: построчная запись в stdout без буфера упирается
// в тысячи системных вызовов на больших файлах.
type lineWriter struct {
	writer *bufio.Writer
	file   *os.File
}

func openWriter(path string) (*lineWriter, error) {
	if path == "-" {
		return &lineWriter{writer: bufio.NewWriter(os.Stdout)}, nil
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось создать %s: %w", path, err)
	}
	return &lineWriter{writer: bufio.NewWriter(file), file: file}, nil
}

func (w *lineWriter) Write(data []byte) error {
	if _, err := w.writer.Write(data); err != nil {
		return err
	}
	return w.writer.WriteByte('\n')
}

func (w *lineWriter) Close() error {
	if err := w.writer.Flush(); err != nil {
		return err
	}
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// loadPoints читает телеметрию в любом из двух форматов, которые встречаются
// в проекте: JSONL, который пишет serve --jsonl, и CSV раздачи.
//
// Автоопределение нужно потому, что требование «посчитать признаки по
// сохранённым данным» одинаково для обоих файлов, а спрашивать пользователя
// формат значит заставить его знать внутреннее устройство конвейера. Признак
// разбирается по первой строке: CSV начинается с заголовка, JSONL — с
// открывающей фигурной скобки.
func loadPoints(path string) (map[uint32][]statestore.Point, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть %s: %w", path, err)
	}
	defer file.Close()

	head := make([]byte, 512)
	n, err := file.Read(head)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("%s: файл пуст: считать нечего, а «0 признаков» "+
			"выглядело бы как успешный расчёт", path)
	}
	head = head[:n]
	if bytes.HasPrefix(bytes.TrimSpace(head), []byte("{")) {
		// Вернуться к началу: тот же дескриптор использует loadTraffic.
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		file.Close()
		return loadJSONLTracks(path)
	}
	tracks, err := loadTraffic(path)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32][]statestore.Point, len(tracks))
	for unitID, t := range tracks {
		out[unitID] = t.points
	}
	return out, nil
}

// loadJSONLTracks раскладывает наблюдения, записанные serve --jsonl.
func loadJSONLTracks(path string) (map[uint32][]statestore.Point, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть %s: %w", path, err)
	}
	defer file.Close()

	out := make(map[uint32][]statestore.Point)
	scanner := bufio.NewScanner(file)
	// Строка наблюдения невелика, но packets.jsonl рядом с ней может быть
	// разобран как не-JSONL; увеличенный буфер снимает вопрос «а почему
	// не читается».
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var line int
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var observation telemetry.Observation
		if err := json.Unmarshal(raw, &observation); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		// Наблюдение без Nav00 не имеет ни координат, ни времени события.
		// Оно не может повлиять ни на один признак движения, поэтому в
		// историю не попадает — ровно так же, как в store.AppendObservation.
		if observation.EventTime.IsZero() {
			continue
		}
		point := statestore.FromObservation(observation)
		out[observation.UnitID] = append(out[observation.UnitID], point)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for unitID := range out {
		points := out[unitID]
		sort.Slice(points, func(i, j int) bool {
			return points[i].EventTime.Before(points[j].EventTime)
		})
		out[unitID] = points
	}
	return out, nil
}
