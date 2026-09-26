// Package statestore ведёт ограниченную историю телеметрии по каждому
// устройству: кольцевой буфер последних точек, последнее известное состояние и
// признак устаревания.
//
// Глубина истории задаётся DefaultCapacity в 90 точек. При медианной
// длительности пакета около 9 с это около 14 минут, при 90-м перцентиле в
// 16 с — около 24 минут, то есть перекрывает окно прогноза 10–15 минут с
// запасом на опоздания пакетов.
//
// Ограничение по числу точек, а не по времени, выбрано осознанно: при
// остановке на остановке транспортное средство шлёт пустые пакеты, и буфер
// на 15 минут при 9-секундном шаге наполнялся бы простоями вместо истории
// движения.
package statestore

import (
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/telemetry"
)

// DefaultCapacity — глубина истории в точках по умолчанию.
const DefaultCapacity = 90

// DefaultStalenessTTL — после какого простоя точка считается устаревшей.
const DefaultStalenessTTL = 2 * time.Minute

// DefaultEvictAfter — после какого простоя устройство выбрасывается из
// хранилища целиком. Должно быть заметно больше DefaultStalenessTTL, чтобы
// устаревшее, но последнее известное состояение переживало скачок связи:
// иначе после кратковременной потери сигнала приборная панель теряла бы
// позицию, которая на карте ещё актуальна.
const DefaultEvictAfter = 30 * time.Minute

// Point — одна телеметрическая точка в виде, пригодном для расчёта
// признаков. Состав полей повторяет TelemetryPoint из proto/ml.proto, чтобы
// выгрузка признаков и online-путь читали одно и то же.
type Point struct {
	// UnitID — идентификатор устройства из handshake.
	UnitID uint32 `json:"unit_id"`
	// EventTime — время события по часам устройства. Точки без него не
	// принимаются: без времени события нельзя ни построить окно, ни
	// отличить опоздание пакета от его отсутствия.
	EventTime time.Time `json:"event_time"`
	// ReceiveTime — локальное время приёма пакета. Разность с EventTime
	// даёт задержку канала, признак quality.lag_s.
	ReceiveTime time.Time `json:"receive_time"`
	// Latitude, Longitude — координаты в градусах.
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	// SpeedKmh — текущая скорость, км/ч: приоритет у Nav00.SpeedAvg.
	SpeedKmh float64 `json:"speed_kmh"`
	// SpeedAvgKmh — средняя скорость из Nav00.
	SpeedAvgKmh float64 `json:"speed_avg_kmh"`
	// SpeedInstantKmh — текущая скорость из Can10.
	SpeedInstantKmh float64 `json:"speed_instant_kmh"`
	// CourseDeg — курс из Nav00, градусы.
	CourseDeg float64 `json:"course_deg"`
	// Satellites — число спутников.
	Satellites uint8 `json:"satellites"`
	// Altitude — высота, м.
	Altitude float64 `json:"altitude"`
	// LocationValid — координаты достоверны.
	LocationValid bool `json:"location_valid"`
	// OdometerKm — пробег, км.
	OdometerKm float64 `json:"odometer_km"`
	// LagS — задержка ReceiveTime минус EventTime, секунды.
	LagS float64 `json:"lag_s"`
}

// State — последнее известное состояние устройства вместе с признаком
// устаревания.
type State struct {
	Point Point `json:"point"`
	// SeenAt — локальное время последнего принятого пакета.
	SeenAt time.Time `json:"seen_at"`
	// StalenessS — сколько секунд прошло с последнего пакета.
	StalenessS float64 `json:"staleness_s"`
	// Stale — признак превышения StalenessTTL.
	Stale bool `json:"stale"`
	// PointsInWindow — глубина накопленной истории.
	PointsInWindow int `json:"points_in_window"`
}

// Stats — накопительные счётчики хранилища.
type Stats struct {
	// Appended — принято точек с корректным временем события.
	Appended int64 `json:"appended"`
	// Rejected — отброшено точек без времени события.
	Rejected int64 `json:"rejected"`
	// Overflows — вытеснений из кольцевого буфера, то есть случаев, когда
	// истории не хватило для расчёта признаков.
	Overflows int64 `json:"overflows"`
	// Units — устройств в хранилище.
	Units int `json:"units"`
	// Points — всего точек во всех буферах.
	Points int `json:"points"`
	// Evicted — устройств выброшено за простой.
	Evicted int64 `json:"evicted"`
}

// Store — потокобезопасное хранилище истории телеметрии. Единицы устройств
// распределены по шардам, чтобы запись одного устройства не блокировала чтение
// остальных.
type Store struct {
	capacity   int
	staleAfter time.Duration
	evictAfter time.Duration
	logger     *slog.Logger
	now        func() time.Time
	shards     []shard

	appended  atomic.Int64
	rejected  atomic.Int64
	overflows atomic.Int64
	evicted   atomic.Int64
}

// Option настраивает Store.
type Option func(*Store)

// WithCapacity задаёт глубину истории в точках. Неположительное значение
// означает DefaultCapacity.
func WithCapacity(n int) Option {
	return func(s *Store) { s.capacity = n }
}

// WithStalenessTTL задаёт порог устаревания последнего пакета.
func WithStalenessTTL(d time.Duration) Option {
	return func(s *Store) { s.staleAfter = d }
}

// WithEvictAfter задаёт порог простоя, после которого устройство выбрасывается
// из хранилища вместе с историей. Неположительное значение отключает вытеснение.
func WithEvictAfter(d time.Duration) Option {
	return func(s *Store) { s.evictAfter = d }
}

// WithLogger задаёт логгер. По умолчанию используется slog.Default.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Store) { s.logger = logger }
}

// WithClock подменяет источник времени, что удобно в тестах и в replay.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// defaultShardCount — число шардов. Значение взято с запасом относительно
// числа транспортных средств: 30 устройств на парке разъезжаются по шардам
// почти поровну, а блокировки на несколько сотен единиц не конкурируют.
const defaultShardCount = 64

// New создаёт Store.
func New(opts ...Option) *Store {
	s := &Store{
		capacity:   DefaultCapacity,
		staleAfter: DefaultStalenessTTL,
		evictAfter: DefaultEvictAfter,
		logger:     slog.Default(),
		now:        time.Now,
		shards:     make([]shard, defaultShardCount),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.capacity <= 0 {
		s.capacity = DefaultCapacity
	}
	// Опция с nil обязана оставлять значение по умолчанию, а не подменять
	// вызов на nil: иначе New успешно вернёт хранилище, которое упадёт
	// позже и совсем в другом месте, в Append или State.
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	for i := range s.shards {
		s.shards[i].units = make(map[uint32]*vehicle)
	}
	return s
}

// shard — группа устройств под одной блокировкой.
type shard struct {
	mu    sync.RWMutex
	units map[uint32]*vehicle
}

// vehicle — история одного устройства.
type vehicle struct {
	buf *ring
	// last — последняя поступившая точка, она же last-known-state.
	last Point
	// seenAt — локальное время последней записи.
	seenAt time.Time
}

func (s *Store) shardFor(unitID uint32) *shard {
	return &s.shards[unitID%uint32(len(s.shards))]
}

// Append добавляет точку в историю устройства. Точка без времени события
// отбрасывается и учитывается в Stats.Rejected: хранить её нельзя, потому
// что окно признаков строится по времени события.
func (s *Store) Append(p Point) {
	if p.EventTime.IsZero() {
		s.rejected.Add(1)
		return
	}
	if p.UnitID == 0 {
		// Устройство без идентификатора невозможно привязать ни к
		// расписанию, ни к предыдущим точкам.
		s.rejected.Add(1)
		return
	}
	p.LagS = p.ReceiveTime.Sub(p.EventTime).Seconds()

	sh := s.shardFor(p.UnitID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	v, ok := sh.units[p.UnitID]
	if !ok {
		v = &vehicle{buf: newRing(s.capacity)}
		sh.units[p.UnitID] = v
	}
	if v.buf.push(p) {
		s.overflows.Add(1)
	}
	v.last = p
	v.seenAt = s.now()
	s.appended.Add(1)
}

// AppendObservation принимает нормализованное наблюдение и переносит его в
// историю. Наблюдение без Nav00 не попадает в буфер: у него нет ни координат,
// ни времени события, и оно не может повлиять ни на один признак движения.
func (s *Store) AppendObservation(observation telemetry.Observation) {
	if observation.EventTime.IsZero() {
		s.rejected.Add(1)
		return
	}
	s.Append(FromObservation(observation))
}

// FromObservation переносит наблюдение в точку истории.
func FromObservation(observation telemetry.Observation) Point {
	return Point{
		UnitID:          observation.UnitID,
		EventTime:       observation.EventTime,
		ReceiveTime:     observation.ReceivedAt,
		Latitude:        observation.Latitude,
		Longitude:       observation.Longitude,
		SpeedKmh:        observation.Speed,
		SpeedAvgKmh:     observation.SpeedAvg,
		SpeedInstantKmh: observation.SpeedInstant,
		CourseDeg:       float64(observation.Course),
		Satellites:      observation.Satellites,
		Altitude:        float64(observation.Altitude),
		LocationValid:   observation.LocationValid,
		OdometerKm:      observation.TotalKm,
	}
}

// Window возвращает накопленную историю устройства в порядке времени события.
// Возвращаемая копия не разделяется с хранилищем и безопасно переживает
// последующие записи.
func (s *Store) Window(unitID uint32) []Point {
	sh := s.shardFor(unitID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.units[unitID]
	if !ok {
		return nil
	}
	return v.buf.byEventTime()
}

// AsOf возвращает то, что хранилище знало об устройстве в момент at: точки,
// принятые не позже at, в порядке времени события.
//
// Это граница, на которой offline-replay обязан совпасть с online. Если
// ограничиваться EventTime, то опоздавший пакет с меткой из прошлого окажется
// в окне при offline-проигрывании и не окажется при приёме вживую, и признаки
// для одних и тех же данных разойдутся.
func (s *Store) AsOf(unitID uint32, at time.Time) []Point {
	sh := s.shardFor(unitID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.units[unitID]
	if !ok {
		return nil
	}
	return s.sortedSince(v, at)
}

// sortedSince заполняет критерий сортировки по времени события, сохраняя
// порядок поступления при равных метках.
func (s *Store) sortedSince(v *vehicle, at time.Time) []Point {
	points := v.buf.since(at)
	if len(points) < 2 {
		return points
	}
	slices.SortStableFunc(points, func(a, b Point) int {
		return a.EventTime.Compare(b.EventTime)
	})
	return points
}

// Last возвращает последнее известное состояние устройства.
func (s *Store) Last(unitID uint32) (Point, bool) {
	sh := s.shardFor(unitID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.units[unitID]
	if !ok {
		return Point{}, false
	}
	return v.last, true
}

// State возвращает последнее известное состояние с признаком устаревания.
func (s *Store) State(unitID uint32) (State, bool) {
	sh := s.shardFor(unitID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.units[unitID]
	if !ok {
		return State{}, false
	}
	staleness := s.now().Sub(v.seenAt)
	return State{
		Point:          v.last,
		SeenAt:         v.seenAt,
		StalenessS:     staleness.Seconds(),
		Stale:          staleness > s.staleAfter,
		PointsInWindow: v.buf.len(),
	}, true
}

// StateAsOf возвращает состояние машины таким, каким оно было в момент at.
//
// Отдельный метод, а не аргумент у State, потому что это другой вопрос.
// State отвечает на «что машина делает сейчас», и для планирования с T на
// сетке он даёт неправильный ответ: он смотрит на телеметрию, пришедшую уже
// после T, и кадр, подписанный T, получает данные из будущего. Это тот самый
// класс утечки, который лечит ADR 0004, поэтому граница проходит по at с
// обеих сторон — по времени события и по времени приёма.
//
// Устаревание здесь считается от at, а не от текущего момента: отвечает на
// вопрос «насколько машина была молча к T», и только этот вопрос имеет
// смысл для кадра с этим T.
func (s *Store) StateAsOf(unitID uint32, at time.Time) (State, bool) {
	sh := s.shardFor(unitID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.units[unitID]
	if !ok {
		return State{}, false
	}
	// Точки не позже at по обеим меткам: пакет с меткой события из будущего,
	// пришедший вовремя, в кадр с этим T не попадает. Пропуск такого пакета
	// обязателен и для паритета: offline stateAt обрывает историю по
	// EventTime > t, и «последняя по приёму» против «последняя по событию»
	// разошлись бы ровно на часах с перекосом.
	points := v.buf.since(at)
	kept := points[:0:0]
	for _, p := range points {
		if !p.EventTime.After(at) {
			kept = append(kept, p)
		}
	}
	points = kept
	if len(points) == 0 {
		return State{}, false
	}
	if len(points) > 1 {
		slices.SortStableFunc(points, func(a, b Point) int {
			return a.EventTime.Compare(b.EventTime)
		})
	}
	// Состояние — точка с наибольшим временем события: машина ехала дальше
	// всех, даже если пакет пришёл позже более раннего по времени события.
	last := points[len(points)-1]
	staleness := at.Sub(last.ReceiveTime)
	return State{
		Point:          last,
		SeenAt:         last.ReceiveTime,
		StalenessS:     staleness.Seconds(),
		Stale:          staleness > s.staleAfter,
		PointsInWindow: len(points),
	}, true
}

// Units возвращает отсортированный список устройств с историей.
func (s *Store) Units() []uint32 {
	units := make([]uint32, 0, 16)
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for unitID := range sh.units {
			units = append(units, unitID)
		}
		sh.mu.RUnlock()
	}
	slices.Sort(units)
	return units
}

// Evict удаляет устройства, не видевшие пакет дольше порога. Возвращает
// число выброшенных устройств. При отрицательном или нулевом пороге ничего не
// выбрасывается.
func (s *Store) Evict() int {
	if s.evictAfter <= 0 {
		return 0
	}
	now := s.now()
	removed := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for unitID, v := range sh.units {
			if now.Sub(v.seenAt) > s.evictAfter {
				delete(sh.units, unitID)
				removed++
			}
		}
		sh.mu.Unlock()
	}
	if removed > 0 {
		s.evicted.Add(int64(removed))
	}
	return removed
}

// Stats возвращает накопленные счётчики и текущую наполненность.
func (s *Store) Stats() Stats {
	points := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for _, v := range sh.units {
			points += v.buf.len()
		}
		sh.mu.RUnlock()
	}
	units := len(s.Units())
	return Stats{
		Appended:  s.appended.Load(),
		Rejected:  s.rejected.Load(),
		Overflows: s.overflows.Load(),
		Units:     units,
		Points:    points,
		Evicted:   s.evicted.Load(),
	}
}
