// Package telemetry нормализует декодированные ячейки NDTP в наблюдения
// о транспортном средстве и ведёт последнее известное состояние по каждому
// устройству. Наблюдения пригодны как вход для расчёта признаков.
//
// Пакеты без Nav00 не отбрасываются: из них берутся доступные поля, но признак
// HasPosition остаётся ложным.
package telemetry

import (
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
)

// CellSummary кратко описывает одну ячейку в пакете.
type CellSummary struct {
	Type   string `json:"type"`
	Number uint8  `json:"number"`
	Size   int    `json:"size"`
}

// Observation — нормализованное состояние транспортного средства на момент
// приёма realtime-пакета. Поля, которых нет в пакете, остаются нулевыми, их
// наличие отражает флаги Has*.
//
// EventTime берётся из Nav00 и остаётся нулевым, если Nav00 не было в пакете.
type Observation struct {
	// UnitID — идентификатор устройства из handshake.
	UnitID uint32 `json:"unit_id"`
	// ReceivedAt — локальное время приёма пакета.
	ReceivedAt time.Time `json:"received_at"`
	// EventTime — время события по часам устройства, нулевое без Nav00.
	EventTime time.Time `json:"event_time"`
	// UnixSeconds — время события в секундах с эпохи Unix, 0 без Nav00.
	UnixSeconds uint32 `json:"unix_seconds"`
	// Latitude — широта в градусах.
	Latitude float64 `json:"lat"`
	// Longitude — долгота в градусах.
	Longitude float64 `json:"lon"`
	// Speed — скорость в км/ч: приоритет у Nav00.SpeedAvg, запасной вариант
	// — Can10.Speed.
	Speed float64 `json:"speed_kmh"`
	// SpeedAvg — средняя скорость из Nav00.
	SpeedAvg float64 `json:"speed_avg_kmh"`
	// SpeedInstant — текущая скорость из Can10.
	SpeedInstant float64 `json:"speed_instant_kmh"`
	// LocationValid — координаты достоверны.
	LocationValid bool `json:"location_valid"`
	// Course — курс из Nav00, градусы.
	Course uint16 `json:"course"`
	// Altitude — высота из Nav00, м.
	Altitude uint16 `json:"altitude"`
	// Satellites — число спутников из Nav00.
	Satellites uint8 `json:"satellites"`
	// BatteryMV — напряжение питания из Nav00, мВ.
	BatteryMV int `json:"battery_mv"`
	// Odometer — одометр в сотых долях километра: из IntSensor02, при его
	// отсутствии из Can10.AllTrack.
	Odometer uint32 `json:"odometer"`
	// TotalKm — пробег из Can10, км.
	TotalKm float64 `json:"total_km"`
	// FuelValue — уровень топлива из Can10 без бита шкалы.
	FuelValue uint16 `json:"fuel_value"`
	// FuelIsPercent — уровень топлива задан в процентах.
	FuelIsPercent bool `json:"fuel_is_percent"`
	// EngineTemp — температура двигателя из Can10.
	EngineTemp int16 `json:"engine_temp"`
	// FuelLevelMM — уровень топлива из Usi08, мм.
	FuelLevelMM uint16 `json:"fuel_level_mm"`
	// Temperature — температура из Usi08, °C.
	Temperature uint8 `json:"usi_temperature"`
	// HasPosition — в пакете была ячейка Nav00 с достоверными координатами.
	HasPosition bool `json:"has_position"`
	// HasSpeed — определена ненулевая скорость.
	HasSpeed bool `json:"has_speed"`
	// HasCan10 — в пакете была ячейка Can10.
	HasCan10 bool `json:"has_can10"`
	// HasUsi08 — в пакете была ячейка Usi08.
	HasUsi08 bool `json:"has_usi08"`
	// Cells — состав ячеек пакета.
	Cells []CellSummary `json:"cells"`
}

// Stats — накопительные счётчики наблюдателя.
type Stats struct {
	// Packets — принято realtime-пакетов.
	Packets int64 `json:"packets"`
	// Observations — построено наблюдений, равно Packets при отсутствии сбоев.
	Observations int64 `json:"observations"`
	// NoPosition — наблюдений без Nav00.
	NoPosition int64 `json:"no_position"`
	// Malformed — пакетов с неразобранными ячейками.
	Malformed int64 `json:"malformed"`
	// Stale — наблюдений с расхождением времени больше допуска.
	Stale int64 `json:"stale"`
	// Units — устройств с известным последним состоянием.
	Units int `json:"units"`
}

// Observer реализует ndtpserver.Handler: принимает ячейки, строит
// Observation, хранит последнее состояние по устройствам и пишет наблюдения
// в JSONL-поток. Поток nil отключает вывод. Все методы безопасны для
// конкурентного вызова.
type Observer struct {
	out     io.Writer
	encoder *json.Encoder
	logger  *slog.Logger
	now     func() time.Time
	maxSkew time.Duration

	mu     sync.RWMutex
	latest map[uint32]Observation

	packets      atomic.Int64
	observations atomic.Int64
	noPosition   atomic.Int64
	malformed    atomic.Int64
	staleEvents  atomic.Int64
}

// Option настраивает Observer.
type Option func(*Observer)

// WithLogger задаёт логгер. По умолчанию используется slog.Default.
func WithLogger(logger *slog.Logger) Option {
	return func(o *Observer) { o.logger = logger }
}

// WithClock подменяет источник времени, что удобно в тестах.
func WithClock(now func() time.Time) Option {
	return func(o *Observer) { o.now = now }
}

// WithMaxSkew задаёт допустимое расхождение между EventTime пакета и
// локальным временем. Значения вне границ помечаются как устаревшие.
// Неположительное значение отключает проверку.
func WithMaxSkew(skew time.Duration) Option {
	return func(o *Observer) { o.maxSkew = skew }
}

// New создаёт Observer. Если out не nil, каждое наблюдение дописывается
// в поток одной строкой JSON.
func New(out io.Writer, opts ...Option) *Observer {
	o := &Observer{
		out:     out,
		logger:  slog.Default(),
		now:     time.Now,
		maxSkew: 5 * time.Minute,
		latest:  make(map[uint32]Observation),
	}
	if out != nil {
		o.encoder = json.NewEncoder(out)
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// OnHandshake регистрирует устройство после успешного handshake.
func (o *Observer) OnHandshake(unitID uint32, req ndtp.ConnRequest) {
	o.logger.Info("устройство подключено",
		"unit", unitID,
		"version", req.Version(),
		"max_packet", req.MaxPacketSize)
}

// OnRealtime строит наблюдение из realtime-пакета.
func (o *Observer) OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell) {
	o.packets.Add(1)
	observation := Build(unitID, cells, o.now())

	o.mu.Lock()
	o.latest[unitID] = observation
	o.mu.Unlock()

	o.observations.Add(1)
	if !observation.HasPosition {
		o.noPosition.Add(1)
	}
	if o.IsStale(observation) {
		o.staleEvents.Add(1)
		o.logger.Warn("время события сильно отличается от локального",
			"unit", unitID,
			"event_time", observation.EventTime,
			"unix", observation.UnixSeconds)
	}
	o.write(observation)
}

// OnMalformed фиксирует пакет, который не удалось разобрать.
func (o *Observer) OnMalformed(unitID uint32, frame ndtp.Frame, err error) {
	o.malformed.Add(1)
	o.logger.Warn("пакет с неразобранными ячейками отброшен",
		"unit", unitID, "err", err, "bytes", len(frame.Raw))
}

// OnDisconnect снимает устройство с учёта активных соединений.
func (o *Observer) OnDisconnect(unitID uint32) {
	o.logger.Info("устройство отключено", "unit", unitID)
}

// Latest возвращает последнее наблюдение по устройству.
func (o *Observer) Latest(unitID uint32) (Observation, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	observation, ok := o.latest[unitID]
	return observation, ok
}

// Units возвращает отсортированный список известных устройств.
func (o *Observer) Units() []uint32 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	units := make([]uint32, 0, len(o.latest))
	for unitID := range o.latest {
		units = append(units, unitID)
	}
	sort.Slice(units, func(i, j int) bool { return units[i] < units[j] })
	return units
}

// Snapshot возвращает накопленное состояние всех устройств.
func (o *Observer) Snapshot() map[uint32]Observation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[uint32]Observation, len(o.latest))
	for unitID, observation := range o.latest {
		out[unitID] = observation
	}
	return out
}

// Stats возвращает накопительные счётчики.
func (o *Observer) Stats() Stats {
	o.mu.RLock()
	units := len(o.latest)
	o.mu.RUnlock()
	return Stats{
		Packets:      o.packets.Load(),
		Observations: o.observations.Load(),
		NoPosition:   o.noPosition.Load(),
		Malformed:    o.malformed.Load(),
		Stale:        o.staleEvents.Load(),
		Units:        units,
	}
}

// IsStale сообщает, что событие пакета слишком далеко от локального времени.
func (o *Observer) IsStale(observation Observation) bool {
	if o.maxSkew <= 0 || observation.EventTime.IsZero() {
		return false
	}
	skew := o.now().Sub(observation.EventTime)
	if skew < 0 {
		skew = -skew
	}
	return skew > o.maxSkew
}

func (o *Observer) write(observation Observation) {
	if o.encoder == nil {
		return
	}
	if err := o.encoder.Encode(observation); err != nil {
		o.logger.Warn("не удалось записать наблюдение", "err", err)
	}
}

// Build собирает Observation из разобранных ячеек пакета. receivedAt —
// локальное время приёма пакета. Ячейки без известного соответствия полям,
// например Termo16, учитываются только в составе Cells.
func Build(unitID uint32, cells []ndtp.Cell, receivedAt time.Time) Observation {
	observation := Observation{
		UnitID:     unitID,
		ReceivedAt: receivedAt,
		Cells:      make([]CellSummary, 0, len(cells)),
	}
	for _, cell := range cells {
		observation.Cells = append(observation.Cells, CellSummary{
			Type:   cell.Type().String(),
			Number: cell.Number,
			Size:   cell.PayloadCellSize(),
		})
		switch payload := cell.Payload.(type) {
		case ndtp.Nav00:
			applyNav00(&observation, payload)
		case ndtp.Can10:
			applyCan10(&observation, payload)
		case ndtp.IntSensor02:
			if observation.Odometer == 0 {
				observation.Odometer = payload.Odometer
			}
		case ndtp.Usi08:
			observation.FuelLevelMM = payload.LevelMM
			observation.Temperature = payload.Temperature
			observation.HasUsi08 = true
		}
	}
	observation.Speed = resolveSpeed(observation)
	observation.HasSpeed = observation.Speed > 0
	if observation.UnixSeconds > 0 {
		observation.EventTime = time.Unix(int64(observation.UnixSeconds), 0).UTC()
	}
	return observation
}

func applyNav00(observation *Observation, nav ndtp.Nav00) {
	observation.UnixSeconds = nav.Timestamp
	observation.Latitude = nav.Latitude()
	observation.Longitude = nav.Longitude()
	observation.LocationValid = nav.LocationValid()
	observation.SpeedAvg = float64(nav.SpeedAvg)
	observation.Course = nav.Course
	observation.Altitude = nav.Altitude
	observation.Satellites = nav.Nsat
	observation.BatteryMV = nav.BatteryMillivolts()
	observation.HasPosition = observation.LocationValid
}

func applyCan10(observation *Observation, can ndtp.Can10) {
	observation.Odometer = can.AllTrack
	observation.TotalKm = can.TotalKm()
	observation.FuelValue = can.FuelValue()
	observation.FuelIsPercent = can.FuelIsPercent()
	observation.EngineTemp = can.TEngine
	observation.SpeedInstant = float64(can.Speed)
	observation.HasCan10 = true
}

func resolveSpeed(observation Observation) float64 {
	if observation.SpeedAvg > 0 {
		return observation.SpeedAvg
	}
	return observation.SpeedInstant
}
