package statestore

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// base — опорный момент, чтобы тесты не зависели от локального времени.
var base = time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)

func point(unitID uint32, seq int) Point {
	// Время приёма совпадает с временем события: отдельный сценарий со
	// сдвигом разбирает AsOf.
	event := base.Add(time.Duration(seq) * 10 * time.Second)
	return Point{
		UnitID:        unitID,
		EventTime:     event,
		ReceiveTime:   event,
		Latitude:      55.6 + float64(seq)*0.001,
		Longitude:     37.6 + float64(seq)*0.001,
		SpeedKmh:      30,
		LocationValid: true,
	}
}

func newStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	return New(opts...)
}

// Значения по умолчанию задаются в New, а не в нулевом Server, иначе
// неявный забытый вызов WithCapacity тихо урезал бы историю до одной точки.
func TestStoreDefaults(t *testing.T) {
	store := newStore(t)
	if store.capacity != DefaultCapacity {
		t.Errorf("capacity %d, ожидалось %d", store.capacity, DefaultCapacity)
	}
	if store.staleAfter != DefaultStalenessTTL {
		t.Errorf("staleAfter %v, ожидалось %v", store.staleAfter, DefaultStalenessTTL)
	}
	if store.evictAfter != DefaultEvictAfter {
		t.Errorf("evictAfter %v, ожидалось %v", store.evictAfter, DefaultEvictAfter)
	}
	if store.now == nil || store.logger == nil || len(store.shards) == 0 {
		t.Error("New обязан задать clock, логгер и шарды")
	}
	// Неположительные значения не должны оставлять хранилище без истории.
	zero := newStore(t, WithCapacity(0), WithClock(nil))
	if zero.capacity != DefaultCapacity {
		t.Errorf("capacity %d при WithCapacity(0), ожидалось %d", zero.capacity, DefaultCapacity)
	}
	zero.Append(point(1, 0))
	zero.Append(point(1, 1))
	if got := len(zero.Window(1)); got != 2 {
		t.Errorf("окно содержит %d точек, ожидалось 2: ёмкость схлопнулась", got)
	}
}

// Глубина истории ограничена, и вытеснение видно в счётчиках: без него
// признаки по 15-минутному окну молча считались бы по усечённой истории.
func TestStoreBoundsHistory(t *testing.T) {
	store := newStore(t, WithCapacity(3))
	for seq := range 5 {
		store.Append(point(1, seq))
	}

	window := store.Window(1)
	if len(window) != 3 {
		t.Fatalf("в окне %d точек, ожидалось 3", len(window))
	}
	// Вытеснены две самые ранние точки.
	if want := base.Add(20 * time.Second); !window[0].EventTime.Equal(want) {
		t.Errorf("первая точка окна %v, ожидалась %v", window[0].EventTime, want)
	}
	if want := base.Add(40 * time.Second); !window[2].EventTime.Equal(want) {
		t.Errorf("последняя точка окна %v, ожидалась %v", window[2].EventTime, want)
	}

	stats := store.Stats()
	if stats.Overflows != 2 {
		t.Errorf("Overflows %d, ожидалось 2", stats.Overflows)
	}
	if stats.Appended != 5 {
		t.Errorf("Appended %d, ожидалось 5", stats.Appended)
	}
	if stats.Points != 3 {
		t.Errorf("Points %d, ожидалось 3", stats.Points)
	}
}

// Окно обязано быть в порядке времени события, даже если пакеты приходили
// не по порядку: иначе расчёт скорости участка видел бы отрицательные
// интервалы.
func TestStoreOrdersWindowByEventTime(t *testing.T) {
	store := newStore(t, WithCapacity(10))
	store.Append(point(1, 2))
	store.Append(point(1, 0))
	store.Append(point(1, 1))

	window := store.Window(1)
	for i, p := range window {
		if want := base.Add(time.Duration(i) * 10 * time.Second); !p.EventTime.Equal(want) {
			t.Errorf("точка %d имеет время %v, ожидалось %v", i, p.EventTime, want)
		}
	}
}

// Точка без времени события не может повлиять ни на один признак движения,
// поэтому она отбрасывается на входе, а не копится в буфере.
func TestStoreRejectsPointWithoutEventTime(t *testing.T) {
	store := newStore(t)
	broken := point(1, 0)
	broken.EventTime = time.Time{}
	store.Append(broken)

	if _, ok := store.Last(1); ok {
		t.Error("устройство не должно появиться после точки без времени события")
	}
	if stats := store.Stats(); stats.Rejected != 1 {
		t.Errorf("Rejected %d, ожидалась 1", stats.Rejected)
	}
}

// Ключевое свойство replay: в момент T история должна совпадать с тем, что
// наблюдатель видел вживую. Оно проверено по каждой точке, потому что сдвиг
// на одну точку в окне тихо меняет все производные признаки.
func TestStoreAsOfMatchesArrivalOrder(t *testing.T) {
	const capacity = 8
	store := newStore(t, WithCapacity(capacity))

	// Точки поступают с опозданием: пакет с меткой из прошлого приходит
	// после более свежих.
	delays := []time.Duration{0, 30 * time.Second, 5 * time.Second, 40 * time.Second}
	seqs := []int{0, 1, 2, 3}
	var received []Point
	for i, seq := range seqs {
		p := point(1, seq)
		p.ReceiveTime = p.EventTime.Add(delays[i])
		store.Append(p)
		received = append(received, p)

		at := p.ReceiveTime
		want := make([]Point, 0, len(received))
		for _, q := range received {
			if !q.ReceiveTime.After(at) {
				want = append(want, q)
			}
		}
		got := store.AsOf(1, at)
		if len(got) != len(want) {
			t.Fatalf("AsOf(%v) вернул %d точек, ожидалось %d", at, len(got), len(want))
		}
		for j := range want {
			if !got[j].EventTime.Equal(want[j].EventTime) {
				t.Errorf("AsOf(%v) точка %d: %v, ожидалась %v", at, j, got[j].EventTime, want[j].EventTime)
			}
		}
	}

	// Пакет, пришедший позже отметки T, в окно T не попадает, даже если его
	// метка времени события в прошлом.
	early := point(1, 9)
	early.ReceiveTime = base.Add(time.Second)
	store.Append(early)
	at := base.Add(30 * time.Second)
	for _, p := range store.AsOf(1, at) {
		if !p.EventTime.Before(base.Add(35*time.Second)) && p.ReceiveTime.After(at) {
			t.Errorf("точка с временем приёма после %v попала в окно: %v", at, p)
		}
	}
}

// После разрыва связи last-known-state переживает порог устаревания, но не
// порог вытеснения: приборная панель не должна терять позицию из-за короткой
// потери сигнала, но и не должна копить мёртвые устройства.
func TestStoreStalenessAndEviction(t *testing.T) {
	now := base
	store := newStore(t,
		WithStalenessTTL(time.Minute),
		WithEvictAfter(5*time.Minute),
		WithClock(func() time.Time { return now }),
	)
	store.Append(point(1, 0))

	if state, _ := store.State(1); state.Stale {
		t.Error("только что записанная точка не должна считаться устаревшей")
	}

	now = base.Add(90 * time.Second)
	state, ok := store.State(1)
	if !ok {
		t.Fatal("состояние пропало до порога вытеснения")
	}
	if !state.Stale {
		t.Error("через 90 с при TTL в 1 минуту состояние должно считаться устаревшим")
	}
	if state.PointsInWindow != 1 {
		t.Errorf("PointsInWindow %d, ожидалась 1", state.PointsInWindow)
	}
	if store.Evict() != 0 {
		t.Error("до порога вытеснения устройство должно оставаться в хранилище")
	}

	now = base.Add(10 * time.Minute)
	if store.Evict() != 1 {
		t.Error("после порога вытеснения устройство должно быть выброшено")
	}
	if _, ok := store.State(1); ok {
		t.Error("выброшенное устройство не должно возвращать состояние")
	}
	if stats := store.Stats(); stats.Evicted != 1 {
		t.Errorf("Evicted %d, ожидалась 1", stats.Evicted)
	}
}

// Хранилище обслуживается горутинами соединений одновременно: гонка здесь
// означала бы повреждённую историю при внешне корректных симптомах.
func TestStoreConcurrentAccess(t *testing.T) {
	const units = 200
	const perUnit = 50
	store := newStore(t, WithCapacity(perUnit))

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			// Разные воркеры пишут пересекающиеся наборы устройств.
			unitID := uint32(worker*units/8 + 1)
			for seq := range perUnit {
				store.Append(point(unitID, seq))
			}
		}(worker)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				for _, unitID := range store.Units() {
					store.Window(unitID)
					store.State(unitID)
					store.AsOf(unitID, base.Add(time.Minute))
				}
				store.Stats()
			}
		}()
	}
	wg.Wait()

	stats := store.Stats()
	if want := int64(8 * perUnit); stats.Appended != want {
		t.Errorf("Appended %d, ожидалось %d", stats.Appended, want)
	}
	if stats.Units != 8 {
		t.Errorf("Units %d, ожидалось 8", stats.Units)
	}
	if stats.Overflows != 0 {
		t.Errorf("Overflows %d, ожидалось 0 при заполнении ровно до ёмкости", stats.Overflows)
	}
}

// Units обязан быть отсортирован: по нему строятся выгрузки и дашборд, а
// случайный порядок делает диффы между прогонами бессмысленными.
func TestStoreUnitsSorted(t *testing.T) {
	store := newStore(t)
	for _, unitID := range []uint32{901, 5, 64, 700, 3} {
		store.Append(point(unitID, 0))
	}
	got := store.Units()
	want := []uint32{3, 5, 64, 700, 901}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Units %v, ожидалось %v", got, want)
	}
}

// StateAsOf не должен видеть телеметрию, пришедшую после границы. Это ровно
// тот случай, из-за которого метод и написан: кадр, подписанный T, не должен
// считаться по данным, которых не было в T.
func TestStateAsOfIgnoresFuturePoints(t *testing.T) {
	store := New()
	base := time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)
	for i, at := range []time.Duration{0, 9 * time.Second, 18 * time.Second, 27 * time.Second} {
		store.Append(Point{
			UnitID: 1, EventTime: base.Add(at), ReceiveTime: base.Add(at + time.Second),
			SpeedKmh: float64(10 * (i + 1)),
		})
	}
	// Граница в середине: пакет от 18 с виден, а 27 с — уже нет.
	state, ok := store.StateAsOf(1, base.Add(20*time.Second))
	if !ok {
		t.Fatal("состояние на момент не найдено")
	}
	if got := state.Point.SpeedKmh; got != 30 {
		t.Errorf("скорость %g, ожидалось 30: точка из будущего просочилась", got)
	}
	if state.PointsInWindow != 3 {
		t.Errorf("в окне %d точек, ожидалось 3", state.PointsInWindow)
	}
	// Устаревание считается от границы, а не от текущего момента: отвечает
	// на вопрос «насколько машина молчала к T».
	if got := state.StalenessS; got < 0 || got > 3 {
		t.Errorf("устаревание %g с, ожидалось 0..3", got)
	}
}

// Точки без приёма к моменту не считаются, даже если время события подходит:
// в T машина ещё не знала, что этот пакет придёт.
func TestStateAsOfRespectsReceiveTime(t *testing.T) {
	store := New()
	base := time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)
	store.Append(Point{
		UnitID: 1, EventTime: base, ReceiveTime: base.Add(30 * time.Second),
		SpeedKmh: 99,
	})
	if _, ok := store.StateAsOf(1, base.Add(10*time.Second)); ok {
		t.Error("точка, пришедшая после границы, попала в срез")
	}
	if _, ok := store.StateAsOf(1, base.Add(time.Minute)); !ok {
		t.Error("точка не попала в срез, хотя пришла вовремя")
	}
}

// Машина, включившаяся посреди ячейки, не имеет состояния на момент начала
// этой ячейки. Это должно быть «нет данных», а не состояние из будущего:
// до первого кадра лучше, чем кадр с подменённым T.
func TestStateAsOfHasNothingBeforeFirstPoint(t *testing.T) {
	store := New()
	base := time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)
	store.Append(Point{
		UnitID: 1, EventTime: base.Add(2 * time.Minute), ReceiveTime: base.Add(2 * time.Minute),
	})
	if _, ok := store.StateAsOf(1, base); ok {
		t.Error("состояние нашлось до первой точки")
	}
	if _, ok := store.StateAsOf(9, base); ok {
		t.Error("состояние нашлось у неизвестной машины")
	}
}

// При одинаковом времени события побеждает более поздний пакет: сортировка
// устойчивая, и порядок прихода сохраняется.
func TestStateAsOfPrefersLatestOnEqualEventTime(t *testing.T) {
	store := New()
	base := time.Date(2026, 1, 6, 8, 0, 0, 0, time.UTC)
	store.Append(Point{UnitID: 1, EventTime: base, ReceiveTime: base, SpeedKmh: 10})
	store.Append(Point{
		UnitID: 1, EventTime: base, ReceiveTime: base.Add(time.Second), SpeedKmh: 20,
	})
	state, ok := store.StateAsOf(1, base.Add(time.Minute))
	if !ok {
		t.Fatal("состояние не найдено")
	}
	if got := state.Point.SpeedKmh; got != 20 {
		t.Errorf("скорость %g, ожидалось 20", got)
	}
}

// Пакет с меткой события из будущего, пришедший вовремя, в состояние на
// момент T не попадает. Это перекос часов устройства, а не редкий крайний
// случай: такие пакеты приходят от каждого второго терминала, и если такой
// пакет станет состоянием, кадр получит скорость и координаты из будущего.
func TestStateAsOfSkipsFutureEventTime(t *testing.T) {
	at := time.Date(2026, 1, 6, 8, 5, 0, 0, time.UTC)
	s := New()
	s.Append(Point{
		UnitID: 1, EventTime: at.Add(-2 * time.Minute), ReceiveTime: at.Add(-2 * time.Minute),
		Longitude: 37.6, Latitude: 55.8, SpeedKmh: 12, LocationValid: true,
	})
	// Событие на две минуты позже T, но пакет принят до T.
	s.Append(Point{
		UnitID: 1, EventTime: at.Add(2 * time.Minute), ReceiveTime: at.Add(-time.Minute),
		Longitude: 37.9, Latitude: 55.8, SpeedKmh: 90, LocationValid: true,
	})

	state, ok := s.StateAsOf(1, at)
	if !ok {
		t.Fatal("состояние на момент T не найдено")
	}
	if state.Point.SpeedKmh != 12 {
		t.Errorf("скорость состояния %g, ожидалось 12: взят пакет с меткой из будущего",
			state.Point.SpeedKmh)
	}
	if got := state.PointsInWindow; got != 1 {
		t.Errorf("точек в окне %d, ожидалась 1", got)
	}
}
