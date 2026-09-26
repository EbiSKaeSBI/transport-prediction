package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

const unitID = 4242

var base = time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)

// frameSink собирает отправленные кадры. Копия строк снимается под мьютексом:
// конвейер вызывается из горутины, и чтение среза без синхронизации было бы
// гонкой в самом тесте.
type frameSink struct {
	mu     sync.Mutex
	frames []horizon.Frame
}

func (f *frameSink) Submit(frame horizon.Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, frame)
}

func (f *frameSink) all() []horizon.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]horizon.Frame(nil), f.frames...)
}

// appendStanding кладёт в накопитель точку, на которой машина стоит.
func appendStanding(t *testing.T, p *Pipeline, at time.Time, lon, lat float64) {
	t.Helper()
	appendStandingAs(t, p, at, unitID, lon, lat)
}

func appendStandingAs(t *testing.T, p *Pipeline, at time.Time, as uint32, lon, lat float64) {
	t.Helper()
	p.cfg.Store.Append(statestore.Point{
		UnitID:        as,
		EventTime:     at,
		ReceiveTime:   at,
		Longitude:     lon,
		Latitude:      lat,
		SpeedKmh:      0,
		LocationValid: true,
		Satellites:    8,
		CourseDeg:     90,
	})
}

// reloadFixtures собирает расписание и привязку из строк CSV, чтобы тесты не
// зависели от файлов в validate: содержимое плана здесь контролируется полностью.
func reloadFixtures(t *testing.T, plan *schedule.Schedule, bind *schedule.Binding, stops []schedule.Stop) error {
	t.Helper()
	var planCSV strings.Builder
	planCSV.WriteString("tt_action_item_id,time_begin,time_fact_begin,order_date,manual_fill,tr_id,geom,building_address\n")
	for _, s := range stops {
		// Пустая геометрия — законный случай в раздаче: строка есть, а
		// координаты не разобрались. Записать (0, 0) нельзя, загрузчик
		// справедливо считает эту пару заглушкой.
		geom := ""
		if s.HasGeometry() {
			geom = fmt.Sprintf("POINT (%f %f)", s.Lon, s.Lat)
		}
		// Факт пишется только когда он есть: validate так и выдаёт расписание
		// без факта, и проверять cur_dev_s на плане без факта бессмысленно.
		fact := ""
		if s.HasFact {
			fact = s.TimeFactBegin.Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(&planCSV, "%d,%s,%s,2026-01-06,%t,%d,%s,тест\n",
			s.ActionID, s.TimeBegin.Format("2006-01-02 15:04:05"), fact,
			s.ManualFill, s.TRID, geom)
	}
	loaded, err := schedule.Load(strings.NewReader(planCSV.String()))
	if err != nil {
		return err
	}
	*plan = *loaded

	bindingCSV := "tr_id,unit_id\n7,4242\n"
	loadedBind, err := schedule.LoadBinding(strings.NewReader(bindingCSV))
	if err != nil {
		return err
	}
	*bind = *loadedBind
	return nil
}

// readGoldenPackets читает сохранённые NDTP-пакеты.
func readGoldenPackets(t *testing.T) []ndtp.Frame {
	t.Helper()
	path := filepath.Join("..", "ndtp", "testdata", "golden", "packets.bin")
	file, err := os.Open(path)
	if err != nil {
		t.Skipf("нет %s: %v", path, err)
	}
	defer file.Close()
	reader := ndtp.NewReader(file)
	var out []ndtp.Frame
	for {
		frame, err := reader.Next()
		if err != nil {
			break
		}
		out = append(out, frame)
	}
	return out
}

// stopAt — остановка маршрута на заданной минуте.
func stopAt(minute int, lon float64) schedule.Stop {
	return schedule.Stop{
		ActionID:  int64(100 + minute),
		TRID:      7,
		TimeBegin: base.Add(time.Duration(minute) * time.Minute),
		Lon:       lon,
		Lat:       55.8,
	}
}

func routeStops() []schedule.Stop {
	stops := make([]schedule.Stop, 0, 21)
	for i := 0; i <= 20; i++ {
		stops = append(stops, stopAt(i, 37.600+0.007*float64(i)))
	}
	return stops
}

// newPipeline собирает конвейер поверх настоящих файлов расписания и
// привязки, если они есть, иначе — на синтетической фикстуре.
func newPipeline(t *testing.T, stops []schedule.Stop, sink Sink) *Pipeline {
	t.Helper()
	plan := &schedule.Schedule{}
	bind := &schedule.Binding{}
	if err := reloadFixtures(t, plan, bind, stops); err != nil {
		t.Fatalf("не удалось собрать расписание: %v", err)
	}
	store := statestore.New(
		statestore.WithClock(func() time.Time { return base.Add(30 * time.Minute) }),
	)
	p, err := New(Config{Schedule: plan, Binding: bind, Store: store, Sink: sink})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	cfg := Config{Schedule: &schedule.Schedule{}, Binding: &schedule.Binding{}}
	if _, err := New(cfg); err == nil {
		t.Error("без Store конвейер обязан не собираться: накапливать точки некуда")
	}
}

// Режим накопления телеметрии: расписания ещё нет, но конвейер обязан работать
// и честно считать, что не прогнозирует.
func TestNewAllowsPlanlessDryRun(t *testing.T) {
	sink := &frameSink{}
	quiet := slog.New(slog.DiscardHandler)
	// Часы фиксированы на базу теста: реальное «сейчас» отстоит от фикстуры на
	// месяцы, и наблюдение пришло бы отброшенным как устаревшее.
	store := statestore.New(
		statestore.WithClock(func() time.Time { return base.Add(30 * time.Minute) }),
	)
	p, err := New(Config{Store: store, Sink: sink, Logger: quiet})
	if err != nil {
		t.Fatalf("конвейер без расписания обязан собираться: %v", err)
	}

	now := base
	appendStanding(t, p, now, 37.6005, 55.8005)
	p.Tick(context.Background(), now)

	st := p.Stats()
	if st.NoPlan == 0 {
		t.Error("пропуск тактиров без расписания обязан попадать в NoPlan, иначе отказ молчалив")
	}
	if st.Frames != 0 {
		t.Errorf("кадров без расписания %d, ожидался 0", st.Frames)
	}
	if got := len(sink.all()); got != 0 {
		t.Errorf("в Sink ушло %d кадров без расписания, ожидался 0", got)
	}
}

// Сквозной путь: точка в накопителе → привязка к расписанию → кадр прогноза.
func TestTickProducesFrameForBoundUnit(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)

	now := base
	appendStanding(t, p, now, routeStops()[1].Lon, routeStops()[1].Lat)
	p.Tick(context.Background(), now)

	frames := sink.all()
	if len(frames) != 1 {
		t.Fatalf("кадров %d, ожидался 1", len(frames))
	}
	if frames[0].UnitID != unitID {
		t.Errorf("unit %d, ожидался %d", frames[0].UnitID, unitID)
	}
	if got := frames[0].PrimaryStopID(); got != 111 {
		t.Errorf("цель %d, ожидалась остановка минуты 11", got)
	}
	if got := frames[0].HorizonS(); got < 601 || got > 900 {
		t.Errorf("горизонт %.0f с вне (600, 900]", got)
	}
	if frames[0].SampleID == "" {
		t.Error("sample_id обязан быть заполнен")
	}
	if got := p.Stats().Frames; got != 1 {
		t.Errorf("счётчик кадров %d, ожидался 1", got)
	}
}

// Тики внутри одной ячейки сетки не плодят кадров. Пять тиков за пять секунд —
// это один момент прогноза, а не пять: модель обучена на выборке с T на
// сетке, и кадры с произвольным T не имеют с ней ничего общего.
func TestTicksWithinOneCellProduceSingleFrame(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	now := base
	appendStanding(t, p, now, routeStops()[1].Lon, routeStops()[1].Lat)

	for i := 0; i < 5; i++ {
		p.Tick(context.Background(), now.Add(time.Duration(i)*time.Second))
	}
	if got := len(sink.all()); got != 1 {
		t.Errorf("кадров %d за пять тиков внутри ячейки, ожидался 1", got)
	}
	// Отбрасывание сделала сетка, а не проверка цели: важно, потому что
	// Deduped показывается в сводке, и два разных отказа не должны
	// выглядеть в ней одинаково.
	if got := p.Stats().Deduped; got != 0 {
		t.Errorf("отброшено повторов %d, ожидался 0: тики отсекла сетка", got)
	}
}

// T округляется вниз до кратного сетки, и кадр считается по данным, которые
// были не позже этого T. Оба свойства — контракт, а не деталь реализации.
func TestTickFloorsTToGridAndIgnoresLaterPoints(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	// Точка в 08:04:30 — до границы ячейки.
	appendStanding(t, p, base.Add(4*time.Minute+30*time.Second),
		routeStops()[1].Lon, routeStops()[1].Lat)
	// Точка в 08:06:00 — уже после неё, и в кадр попасть не должна.
	appendStanding(t, p, base.Add(6*time.Minute),
		routeStops()[9].Lon, routeStops()[9].Lat)
	p.Tick(context.Background(), base.Add(6*time.Minute))

	frames := sink.all()
	if len(frames) != 1 {
		t.Fatalf("кадров %d, ожидался 1", len(frames))
	}
	if want := base.Add(5 * time.Minute); !frames[0].AsOf.Equal(want) {
		t.Errorf("T кадра %v, ожидалось %v: момент обязан лежать на сетке",
			frames[0].AsOf, want)
	}
	if frames[0].SampleID != "7_"+"1767686700" {
		t.Errorf("sample_id %q не соответствует округлённому T", frames[0].SampleID)
	}
	// Машина стояла у первой остановки до границы и переехала к девятой
	// после. Кадр про первый адрес: иначе это предсказание по будущему.
	dist := frames[0].Features.DistanceToTargetM
	if dist == nil {
		t.Fatal("признак расстояния не посчитан")
	}
	if *dist < 1000 {
		t.Errorf("расстояние до цели %g м: взята точка из будущего", *dist)
	}
}

// Машина, включившаяся посреди ячейки, не имеет состояния на момент начала
// ячейки. Подставлять более поздние данные значило бы предсказать по
// будущему, поэтому кадра не будет вовсе — и это должно быть видно в
// счётчике, а не выглядеть как «машина спит».
func TestUnitJoiningMidCellGetsNoFrame(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	appendStanding(t, p, base.Add(2*time.Minute), routeStops()[1].Lon, routeStops()[1].Lat)
	p.Tick(context.Background(), base.Add(2*time.Minute))

	if got := len(sink.all()); got != 0 {
		t.Errorf("кадров %d для машины без данных на момент T, ожидался 0", got)
	}
	if got := p.Stats().NoStateAtT; got != 1 {
		t.Errorf("NoStateAtT %d, ожидался 1", got)
	}
	// На следующей границе кадр появляется.
	appendStanding(t, p, base.Add(3*time.Minute), routeStops()[1].Lon, routeStops()[1].Lat)
	p.Tick(context.Background(), base.Add(5*time.Minute))
	if got := len(sink.all()); got != 1 {
		t.Errorf("кадров %d на следующей границе, ожидался 1", got)
	}
}

// Новая ячейка обязана породить новый кадр, даже если машина никуда не
// ехала: момент прогноза другой, и кадр без этого остался бы на пять минут.
func TestTickEmitsAgainWhenTargetChanges(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	appendStanding(t, p, base, routeStops()[1].Lon, routeStops()[1].Lat)
	p.Tick(context.Background(), base)

	// На следующей границе целью станет другая остановка окна.
	appendStanding(t, p, base.Add(5*time.Minute), routeStops()[3].Lon, routeStops()[3].Lat)
	p.Tick(context.Background(), base.Add(5*time.Minute))

	frames := sink.all()
	if len(frames) != 2 {
		t.Fatalf("кадров %d, ожидалось 2: смена цели обязана порождать новый кадр", len(frames))
	}
	if frames[0].PrimaryStopID() == frames[1].PrimaryStopID() {
		t.Errorf("цель не сменилась: оба кадра %d", frames[0].PrimaryStopID())
	}
}

// Устройство без единицы в расписании не должно ронять конвейер и не должно
// получать кадр: предсказывать ему нечего.
func TestTickSkipsUnboundUnit(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	now := base
	appendStandingAs(t, p, now, 999999, routeStops()[1].Lon, routeStops()[1].Lat)
	p.Tick(context.Background(), now)

	if got := len(sink.all()); got != 0 {
		t.Errorf("кадров %d для несвязанного устройства, ожидался 0", got)
	}
	if got := p.Stats().NoBinding; got != 1 {
		t.Errorf("NoBinding %d, ожидался 1", got)
	}
}

// Отмена контекста обязана останавливать тик: иначе shutdown ждал бы конца
// окна расписания.
func TestTickRespectsCancelledContext(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	appendStanding(t, p, base, routeStops()[1].Lon, routeStops()[1].Lat)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Tick(ctx, base)
	if got := len(sink.all()); got != 0 {
		t.Errorf("кадров %d после отмены контекста, ожидался 0", got)
	}
}

// Разрыв соединения снимает отметку о цели: пока пакетов нет, повторный
// кадр той же цели после восстановления связи отправляться не должен.
//
// Проверка идёт напрямую через planUnit, а не через Tick. Через Tick она
// недостижима: момент T двигается вместе с ячейкой, а цель сдвигается на
// следующую остановку, так что отметка о цели не совпадает никогда и
// защита не срабатывает ни при какой сетке. Сама защита нужна для
// повторного планирования одного и того же момента, и вот это она и
// проверяет.
func TestDisconnectForgetsTarget(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	appendStanding(t, p, base, routeStops()[1].Lon, routeStops()[1].Lat)

	p.planUnit(t.Context(), unitID, base)
	if got := len(sink.all()); got != 1 {
		t.Fatalf("кадров %d, ожидался 1", got)
	}
	p.planUnit(t.Context(), unitID, base)
	if got := p.Stats().Deduped; got != 1 {
		t.Errorf("отброшено повторов %d, ожидался 1", got)
	}
	if got := len(sink.all()); got != 1 {
		t.Errorf("кадров %d при повторе того же момента, ожидался 1", got)
	}

	// После разрыва отметка снята, и тот же момент планируется заново.
	p.OnDisconnect(unitID)
	p.planUnit(t.Context(), unitID, base)
	if got := len(sink.all()); got != 2 {
		t.Errorf("кадров %d после разрыва и восстановления, ожидалось 2", got)
	}
}

// Настоящий NDTP-пакет обязан дойти до накопителя состояния: конвейер не
// должен требовать отдельного вызова Append.
func TestRealtimeFrameReachesStore(t *testing.T) {
	packets := readGoldenPackets(t)
	if len(packets) == 0 {
		t.Skip("нет сохранённых NDTP-пакетов")
	}
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)

	var delivered int
	for _, frame := range packets {
		cells, err := ndtp.ParseCells(frame.Body)
		if err != nil {
			continue
		}
		p.OnRealtime(unitID, frame, cells)
		delivered++
	}
	if delivered == 0 {
		t.Skip("ни один сохранённый пакет не разобрался как телеметрия")
	}
	if got := p.Stats().Observations; got == 0 {
		t.Error("счётчик наблюдений остался нулевым: пакеты не дошли до накопителя")
	}
	if got := p.Observer().Units(); len(got) == 0 {
		t.Error("разборщик не увидел ни одного устройства")
	}
}

// Run тикает до отмены контекста и не зависает на нулевом интервале.
func TestRunStopsOnCancel(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	appendStanding(t, p, base, routeStops()[1].Lon, routeStops()[1].Lat)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx, time.Millisecond)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run не завершился после отмены контекста")
	}

	// Нулевой интервал обязан выходить сразу, а не крутить пустой цикл.
	p.Run(context.Background(), 0)
}

func TestStatsSnapshotIsIndependent(t *testing.T) {
	sink := &frameSink{}
	p := newPipeline(t, routeStops(), sink)
	snap := p.Stats()
	snap.Refused["мусор"] = 1
	if _, ok := p.Stats().Refused["мусор"]; ok {
		t.Error("снимок статистики делится с живым счётчиком")
	}
}
