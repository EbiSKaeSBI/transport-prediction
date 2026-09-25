package telemetry

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func goldenFrames(t *testing.T) ([]byte, []ndtp.Frame) {
	t.Helper()
	path := filepath.Join("..", "ndtp", "testdata", "golden", "packets.bin")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не прочитать golden-файл: %v", err)
	}
	reader := ndtp.NewReader(bytes.NewReader(data))
	var frames []ndtp.Frame
	for {
		frame, err := reader.Next()
		if err != nil {
			break
		}
		frames = append(frames, frame)
	}
	if len(frames) == 0 {
		t.Fatal("golden-файл не содержит кадров")
	}
	return data, frames
}

func TestBuildFromGoldenPacket(t *testing.T) {
	_, frames := goldenFrames(t)
	frame := frames[0]
	cells, err := ndtp.ParseCells(frame.Body)
	if err != nil {
		t.Fatalf("разбор ячеек: %v", err)
	}
	nav, ok := cells[0].Nav00()
	if !ok {
		t.Fatal("первая ячейка golden-пакета не Nav00")
	}

	observation := Build(1166336, cells, time.Unix(int64(nav.Timestamp), 0).UTC())

	if observation.UnitID != 1166336 {
		t.Errorf("UnitID %d, ожидалось 1166336", observation.UnitID)
	}
	if !observation.HasPosition {
		t.Fatal("HasPosition=false, хотя Nav00.location_valid=true")
	}
	if !observation.LocationValid {
		t.Error("LocationValid=false")
	}
	if observation.UnixSeconds != nav.Timestamp {
		t.Errorf("UnixSeconds %d, ожидалось %d", observation.UnixSeconds, nav.Timestamp)
	}
	wantEvent := time.Unix(int64(nav.Timestamp), 0).UTC()
	if !observation.EventTime.Equal(wantEvent) {
		t.Errorf("EventTime %v, ожидалось %v", observation.EventTime, wantEvent)
	}
	if observation.Latitude < 55.0 || observation.Latitude > 56.5 {
		t.Errorf("широта %.7f вне ожидаемого диапазона", observation.Latitude)
	}
	if observation.Longitude < 37.0 || observation.Longitude > 38.0 {
		t.Errorf("долгота %.7f вне ожидаемого диапазона", observation.Longitude)
	}
	if observation.Speed <= 0 {
		t.Errorf("Speed %.1f, ожидалось > 0", observation.Speed)
	}
	if !observation.HasCan10 {
		t.Error("HasCan10=false, в пакете есть Can10")
	}
	if !observation.HasUsi08 {
		t.Error("HasUsi08=false, в пакете есть Usi08")
	}
	if observation.TotalKm <= 0 {
		t.Errorf("TotalKm %.2f, ожидалось > 0", observation.TotalKm)
	}
	if len(observation.Cells) != 5 {
		t.Errorf("Cells %d, ожидалось 5", len(observation.Cells))
	}
}

func TestObservationJSONRoundTrip(t *testing.T) {
	_, frames := goldenFrames(t)
	frame := frames[0]
	cells, _ := ndtp.ParseCells(frame.Body)
	observation := Build(1166336, cells, time.Now())

	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatalf("маршализация: %v", err)
	}
	var back Observation
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("десериализация: %v", err)
	}
	if back.UnitID != observation.UnitID {
		t.Errorf("UnitID %d != %d", back.UnitID, observation.UnitID)
	}
	if math.Abs(back.Latitude-observation.Latitude) > 1e-12 {
		t.Errorf("широта не совпала: %v != %v", back.Latitude, observation.Latitude)
	}
	if len(back.Cells) != len(observation.Cells) {
		t.Errorf("Cells %d != %d", len(back.Cells), len(observation.Cells))
	}
}

func TestObserverWritesJSONL(t *testing.T) {
	_, frames := goldenFrames(t)
	var buffer bytes.Buffer
	observer := New(&buffer, WithLogger(quietLogger()))

	frame := frames[0]
	cells, _ := ndtp.ParseCells(frame.Body)
	observer.OnRealtime(1166336, frame, cells)

	output := buffer.String()
	if !strings.HasSuffix(output, "\n") {
		t.Error("каждая запись должна заканчиваться переводом строки")
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		t.Fatalf("строк %d, ожидалась 1", len(lines))
	}
	var decoded Observation
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("строка не разбирается как JSON: %v", err)
	}
	if decoded.UnitID != 1166336 {
		t.Errorf("UnitID %d, ожидалось 1166336", decoded.UnitID)
	}
}

func TestObserverLatestAndUnits(t *testing.T) {
	_, frames := goldenFrames(t)
	observer := New(nil, WithLogger(quietLogger()))

	frame := frames[0]
	cells, _ := ndtp.ParseCells(frame.Body)
	observer.OnRealtime(100, frame, cells)
	observer.OnRealtime(300, frame, cells)
	observer.OnRealtime(200, frames[len(frames)-1], mustCells(t, frames[len(frames)-1]))

	units := observer.Units()
	if len(units) != 3 {
		t.Fatalf("устройств %d, ожидалось 3", len(units))
	}
	for i := 1; i < len(units); i++ {
		if units[i-1] > units[i] {
			t.Errorf("Units не отсортированы: %v", units)
			break
		}
	}
	latest, ok := observer.Latest(200)
	if !ok {
		t.Fatal("Latest(200) не найдено")
	}
	if latest.UnitID != 200 {
		t.Errorf("UnitID %d, ожидалось 200", latest.UnitID)
	}
	if _, ok := observer.Latest(999); ok {
		t.Error("Latest(999) не должно существовать")
	}
	if snapshot := observer.Snapshot(); len(snapshot) != 3 {
		t.Errorf("Snapshot %d, ожидалось 3", len(snapshot))
	}
}

func mustCells(t *testing.T, frame ndtp.Frame) []ndtp.Cell {
	t.Helper()
	cells, err := ndtp.ParseCells(frame.Body)
	if err != nil {
		t.Fatalf("разбор ячеек: %v", err)
	}
	return cells
}

func TestObserverStatsCounters(t *testing.T) {
	_, frames := goldenFrames(t)
	observer := New(nil, WithLogger(quietLogger()))

	observer.OnRealtime(1, frames[0], mustCells(t, frames[0]))
	observer.OnMalformed(1, frames[0], io.ErrUnexpectedEOF)

	stats := observer.Stats()
	if stats.Packets != 1 {
		t.Errorf("Packets %d, ожидалось 1", stats.Packets)
	}
	if stats.Observations != 1 {
		t.Errorf("Observations %d, ожидалось 1", stats.Observations)
	}
	if stats.Malformed != 1 {
		t.Errorf("Malformed %d, ожидалось 1", stats.Malformed)
	}
	if stats.Units != 1 {
		t.Errorf("Units %d, ожидалось 1", stats.Units)
	}
}

func TestIsStale(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	observer := New(nil,
		WithLogger(quietLogger()),
		WithClock(func() time.Time { return base }),
		WithMaxSkew(2*time.Minute))

	fresh := Observation{EventTime: base}
	if observer.IsStale(fresh) {
		t.Error("свежее событие не должно считаться устаревшим")
	}
	old := Observation{EventTime: base.Add(-10 * time.Minute)}
	if !observer.IsStale(old) {
		t.Error("событие 10 минут назад должно считаться устаревшим")
	}
	zero := Observation{}
	if observer.IsStale(zero) {
		t.Error("нулевое время не должно считаться устаревшим")
	}
}

func TestIsStaleDisabled(t *testing.T) {
	observer := New(nil, WithLogger(quietLogger()), WithMaxSkew(0))
	observation := Observation{EventTime: time.Unix(0, 0)}
	if observer.IsStale(observation) {
		t.Error("при maxSkew<=0 проверка должна быть выключена")
	}
}

func TestSpeedFallsBackToInstant(t *testing.T) {
	observation := Observation{SpeedAvg: 0, SpeedInstant: 42}
	if got := resolveSpeed(observation); got != 42 {
		t.Errorf("resolveSpeed = %v, ожидалось 42", got)
	}
	observation = Observation{SpeedAvg: 15, SpeedInstant: 42}
	if got := resolveSpeed(observation); got != 15 {
		t.Errorf("resolveSpeed = %v, ожидалось 15 (приоритет у SpeedAvg)", got)
	}
}

func TestBuildWithoutNav00(t *testing.T) {
	_, frames := goldenFrames(t)
	frame := frames[0]
	all, err := ndtp.ParseCells(frame.Body)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	var onlyCan []ndtp.Cell
	for _, cell := range all {
		if cell.Type() == ndtp.CellCan10 {
			onlyCan = append(onlyCan, cell)
		}
	}
	observation := Build(7, onlyCan, time.Now())
	if observation.HasPosition {
		t.Error("без Nav00 HasPosition должен быть false")
	}
	if !observation.HasCan10 {
		t.Error("HasCan10 должен быть true")
	}
	if observation.Speed <= 0 {
		t.Errorf("Speed %.1f должно браться из Can10", observation.Speed)
	}
	if !observation.EventTime.IsZero() {
		t.Errorf("без Nav00 EventTime должно быть нулевым, получено %v", observation.EventTime)
	}
}

func TestObserverConcurrentUse(t *testing.T) {
	_, frames := goldenFrames(t)
	observer := New(nil, WithLogger(quietLogger()))
	cells := mustCells(t, frames[0])

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(unit uint32) {
			defer wg.Done()
			for i := range 200 {
				observer.OnRealtime(unit+uint32(i%3), frames[0], cells)
				observer.Latest(unit)
				observer.Units()
				observer.Stats()
			}
		}(uint32(worker) * 100)
	}
	wg.Wait()

	if stats := observer.Stats(); stats.Observations != 1600 {
		t.Errorf("Observations %d, ожидалось 1600", stats.Observations)
	}
}
