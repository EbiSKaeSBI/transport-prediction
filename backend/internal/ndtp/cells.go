package ndtp

import (
	"encoding/binary"
	"fmt"
	"math"
)

// CellType — тип ячейки телематики, первый байт её заголовка.
//
// Тип 1 в протоколе отсутствует: нумерация идёт с нуля, а после 0 идёт 2.
type CellType uint8

// Типы ячеек, известные протоколу. Размер каждого задан константой Size* и
// используется для обхода тела пакета.
const (
	// CellNav00 — навигация: координаты, скорость, курс.
	CellNav00 CellType = 0
	// CellIntSensor02 — аналоговые и дискретные входы, одометр, питание.
	CellIntSensor02 CellType = 2
	// CellCrown03 — счётчик пассажиропотока Crown: одометр и состояние дверей.
	CellCrown03 CellType = 3
	// CellIrma04 — счётчик пассажиропотока ИРМА: одометр, двери, признаки.
	CellIrma04 CellType = 4
	// CellKdm05 — климатическая установка: программа, отвал, щётки.
	CellKdm05 CellType = 5
	// CellIdn06 — идентификатор счётчика импульсов.
	CellIdn06 CellType = 6
	// CellIdn07 — короткий идентификатор: одно значение.
	CellIdn07 CellType = 7
	// CellUsi08 — датчик уровня топлива в баке.
	CellUsi08 CellType = 8
	// CellReg09 — регистратор: идентификатор и имя терминала.
	CellReg09 CellType = 9
	// CellCan10 — данные CAN-шины: расход топлива, одометр, скорость.
	CellCan10 CellType = 10
	// CellRfid12 — RFID-метка: ключ из пяти байт.
	CellRfid12 CellType = 12
	// CellPlo13 — уровень жидкости: плотность, температура, объём.
	CellPlo13 CellType = 13
	// CellBms14 — батарея: напряжение ячеек, ток, ошибки.
	CellBms14 CellType = 14
	// CellLls15 — уровнемер топлива с большим набором измерений.
	CellLls15 CellType = 15
	// CellTermo16 — датчик температуры.
	CellTermo16 CellType = 16
	// CellAlcohol1st17 — алкотестер, часть 1: событие и показания прибора.
	CellAlcohol1st17 CellType = 17
	// CellCAN18 — расширенные аналоговые и дискретные входы CAN.
	CellCAN18 CellType = 18
	// CellGSMstations19 — параметры GSM-станций: своя и шесть соседних.
	CellGSMstations19 CellType = 19
	// CellM333CAN20 — флаги CAN-модуля M333.
	CellM333CAN20 CellType = 20
	// CellAlcohol2nd21 — алкотестер, часть 2: паспорт и счётчики прибора.
	CellAlcohol2nd21 CellType = 21
	// CellServerStatistics22 — статистика сервера: очереди и подтверждения.
	CellServerStatistics22 CellType = 22
	// CellTrackerStatistics23 — статистика трекера: соединения и подтверждения.
	CellTrackerStatistics23 CellType = 23
	// CellZipSensorData100 — данные произвольного датчика: 40 байт полезной
	// нагрузки.
	CellZipSensorData100 CellType = 100
)

const (
	// CoordDivider — делитель координат: в градусах значения Nav00 хранятся
	// как целые, делённые на 1e7.
	CoordDivider = 1e7
)

// Биты поля ExtraDop ячейки Nav00.
const (
	// ExtraDopVoiceCall — идёт голосовой вызов.
	ExtraDopVoiceCall uint8 = 1 << 0
	// ExtraDopAlarm — активна тревога.
	ExtraDopAlarm uint8 = 1 << 1
	// ExtraDopSOS — сигнал SOS.
	ExtraDopSOS uint8 = 1 << 2
	// ExtraDopFirstOn — первое включение после подачи питания.
	ExtraDopFirstOn uint8 = 1 << 3
	// ExtraDopBattery — устройство работает от батареи.
	ExtraDopBattery uint8 = 1 << 4
	// ExtraDopLatNorth — широта положительная (северное полушарие).
	ExtraDopLatNorth uint8 = 1 << 5
	// ExtraDopLonEast — долгота положительная (восточное полушарие).
	ExtraDopLonEast uint8 = 1 << 6
	// ExtraDopValid — координаты достоверны.
	ExtraDopValid uint8 = 1 << 7
)

// Размеры payload ячеек в байтах.
//
// Значения измерены на пакетах эмулятора NPL-6.2 и совпадают с порядком полей
// в документации. Длина в потоке не передаётся, поэтому таблица — единственный
// способ корректно пройти тело realtime-пакета.
const (
	// SizeNav00 — размер payload ячейки Nav00.
	SizeNav00          = 26
	SizeIntSensor02    = 26
	SizeCrown03        = 14
	SizeIrma04         = 15
	SizeKdm05          = 6
	SizeIdn06          = 9
	SizeIdn07          = 1
	SizeUsi08          = 6
	SizeReg09          = 40
	SizeCan10          = 37
	SizeRfid12         = 5
	SizePlo13          = 13
	SizeBms14          = 15
	SizeLls15          = 50
	SizeTermo16        = 8
	SizeAlcohol1st17   = 46
	SizeCAN18          = 50
	SizeGSMstations19  = 40
	SizeM333CAN20      = 8
	SizeAlcohol2nd21   = 180
	SizeServerStats22  = 24
	SizeTrackerStats23 = 16
	SizeZipSensor100   = 44
)

var cellNames = map[CellType]string{
	CellNav00:               "G6CellNav00",
	CellIntSensor02:         "G6CellIntSensor02",
	CellCrown03:             "G6CellCrown03",
	CellIrma04:              "G6CellIrma04",
	CellKdm05:               "G6CellKdm05",
	CellIdn06:               "G6CellIdn06",
	CellIdn07:               "G6CellIdn07",
	CellUsi08:               "G6CellUsi08",
	CellReg09:               "G6CellReg09",
	CellCan10:               "G6CellCan10",
	CellRfid12:              "G6CellRfid12",
	CellPlo13:               "G6CellPlo13",
	CellBms14:               "G6CellBms14",
	CellLls15:               "G6CellLls15",
	CellTermo16:             "G6CellTermo16",
	CellAlcohol1st17:        "G6CellAlcohol1st17",
	CellCAN18:               "G6CellCAN18",
	CellGSMstations19:       "G6CellGSMstations19",
	CellM333CAN20:           "G6CellM333CAN20",
	CellAlcohol2nd21:        "G6CellAlcohol2nd21",
	CellServerStatistics22:  "G6CellServerStatistics22",
	CellTrackerStatistics23: "G6CellTrackerStatistics23",
	CellZipSensorData100:    "G6CellZipSensorData100",
}

var cellSizes = map[CellType]int{
	CellNav00:               SizeNav00,
	CellIntSensor02:         SizeIntSensor02,
	CellCrown03:             SizeCrown03,
	CellIrma04:              SizeIrma04,
	CellKdm05:               SizeKdm05,
	CellIdn06:               SizeIdn06,
	CellIdn07:               SizeIdn07,
	CellUsi08:               SizeUsi08,
	CellReg09:               SizeReg09,
	CellCan10:               SizeCan10,
	CellRfid12:              SizeRfid12,
	CellPlo13:               SizePlo13,
	CellBms14:               SizeBms14,
	CellLls15:               SizeLls15,
	CellTermo16:             SizeTermo16,
	CellAlcohol1st17:        SizeAlcohol1st17,
	CellCAN18:               SizeCAN18,
	CellGSMstations19:       SizeGSMstations19,
	CellM333CAN20:           SizeM333CAN20,
	CellAlcohol2nd21:        SizeAlcohol2nd21,
	CellServerStatistics22:  SizeServerStats22,
	CellTrackerStatistics23: SizeTrackerStats23,
	CellZipSensorData100:    SizeZipSensor100,
}

// String возвращает имя типа ячейки, например "G6CellNav00". Для неизвестных
// типов возвращается "CellType(N)".
func (t CellType) String() string {
	if name, ok := cellNames[t]; ok {
		return name
	}
	return fmt.Sprintf("CellType(%d)", uint8(t))
}

// Size возвращает размер payload ячейки в байтах. Второе значение false
// означает, что тип не зарегистрирован в протоколе.
func (t CellType) Size() (int, bool) {
	size, ok := cellSizes[t]
	return size, ok
}

// Payload — разобранное содержимое ячейки. Реализуется структурами для всех
// типов, зарегистрированных в протоколе.
type Payload interface {
	// Cell возвращает тип ячейки.
	Cell() CellType
}

// Cell — разобранная ячейка вместе с её порядковым номером.
type Cell struct {
	// Number — второй байт заголовка ячейки (instance number).
	Number uint8
	// Payload — разобранные данные ячейки.
	Payload Payload
}

// Nav00 — ячейка навигационных данных: координаты, время, скорость, курс.
type Nav00 struct {
	// Timestamp — время события, секунды с эпохи Unix.
	Timestamp uint32
	// LongitudeRaw — долгота в 1e-7 градуса без знака.
	LongitudeRaw uint32
	// LatitudeRaw — широта в 1e-7 градуса без знака.
	LatitudeRaw uint32
	// ExtraDop — флаги достоверности и знаков, см. ExtraDopValid.
	ExtraDop uint8
	// BatVoltage — напряжение питания в 20 мВ.
	BatVoltage uint8
	// SpeedAvg — средняя скорость, км/ч.
	SpeedAvg uint16
	// SpeedMax — максимальная скорость, км/ч.
	SpeedMax uint16
	// Course — курс в градусах.
	Course uint16
	// Track — путь с начала поездки.
	Track uint16
	// Altitude — высота над уровнем моря, м.
	Altitude uint16
	// Nsat — число видимых спутников.
	Nsat uint8
	// Pdop — геометрический фактор точности.
	Pdop uint8
}

// Cell реализует Payload.
func (Nav00) Cell() CellType { return CellNav00 }

// Latitude возвращает широту в градусах с учётом знака из ExtraDopLatNorth.
func (c Nav00) Latitude() float64 {
	value := float64(c.LatitudeRaw) / CoordDivider
	if c.ExtraDop&ExtraDopLatNorth == 0 {
		return -value
	}
	return value
}

// Longitude возвращает долготу в градусах с учётом знака из ExtraDopLonEast.
func (c Nav00) Longitude() float64 {
	value := float64(c.LongitudeRaw) / CoordDivider
	if c.ExtraDop&ExtraDopLonEast == 0 {
		return -value
	}
	return value
}

// LocationValid сообщает, достоверны ли координаты.
func (c Nav00) LocationValid() bool { return c.ExtraDop&ExtraDopValid != 0 }

// VoiceCall сообщает, идёт ли голосовой вызов.
func (c Nav00) VoiceCall() bool { return c.ExtraDop&ExtraDopVoiceCall != 0 }

// Alarm сообщает, активна ли тревога.
func (c Nav00) Alarm() bool { return c.ExtraDop&ExtraDopAlarm != 0 }

// SOS сообщает, установлен ли сигнал SOS.
func (c Nav00) SOS() bool { return c.ExtraDop&ExtraDopSOS != 0 }

// FirstPowerOn сообщает, было ли это первое включение после подачи питания.
func (c Nav00) FirstPowerOn() bool { return c.ExtraDop&ExtraDopFirstOn != 0 }

// OnBattery сообщает, работает ли устройство от батареи.
func (c Nav00) OnBattery() bool { return c.ExtraDop&ExtraDopBattery != 0 }

// BatteryMillivolts возвращает напряжение питания в милливольтах.
func (c Nav00) BatteryMillivolts() int {
	return int(c.BatVoltage) * 20
}

// IntSensor02 — ячейка аналоговых и дискретных входов с одометром.
type IntSensor02 struct {
	// AnIn — четыре аналоговых входа.
	AnIn [4]uint16
	// DiIn — состояния дискретных входов.
	DiIn uint8
	// DiOut — состояния дискретных выходов.
	DiOut uint8
	// DiCounter — счётчики дискретных входов.
	DiCounter [4]uint16
	// Odometer — одометр в сотых долях километра.
	Odometer uint32
	// Csq — уровень сигнала GSM.
	Csq uint8
	// GprsState — состояние GPRS.
	GprsState uint8
	// AccelEnergy — накопленная энергия по модулю ускорения.
	AccelEnergy uint8
	// ExtVolt — внешнее напряжение со знаком.
	ExtVolt int8
}

// Cell реализует Payload.
func (IntSensor02) Cell() CellType { return CellIntSensor02 }

// Usi08 — ячейка датчика уровня топлива в баке.
type Usi08 struct {
	// DetStatus — статус датчика.
	DetStatus uint8
	// LevelMM — уровень в миллиметрах.
	LevelMM uint16
	// LevelLiters — объём в литрах.
	LevelLiters uint16
	// Temperature — температура, °C.
	Temperature uint8
}

// Cell реализует Payload.
func (Usi08) Cell() CellType { return CellUsi08 }

// Can10 — ячейка данных CAN-шины: расход топлива, одометр, скорость.
type Can10 struct {
	// SecFlagStatus — статус модуля, 0xFFFFFFFF означает отсутствие модуля.
	SecFlagStatus uint32
	// AllTimeEngine — наработка двигателя в сотых долях часа.
	AllTimeEngine uint32
	// AllTrack — общий пробег в сотых долях километра.
	AllTrack uint32
	// AllFuelConsum — суммарный расход топлива.
	AllFuelConsum uint32
	// FuelLevel — уровень топлива; старший бит задаёт шкалу, см. FuelIsPercent.
	FuelLevel uint16
	// SpeedTurnEngine — обороты двигателя.
	SpeedTurnEngine uint16
	// TEngine — температура двигателя.
	TEngine int16
	// Speed — текущая скорость, км/ч.
	Speed uint8
	// PressureAxis — давление по осям.
	PressureAxis [5]uint16
	// FlagAlarm — флаги тревог.
	FlagAlarm uint32
}

// Cell реализует Payload.
func (Can10) Cell() CellType { return CellCan10 }

// FuelIsPercent сообщает, задан ли уровень топлива в процентах, а не в
// абсолютных единицах.
func (c Can10) FuelIsPercent() bool { return c.FuelLevel&0x8000 != 0 }

// FuelValue возвращает уровень топлива без старшего бита шкалы.
func (c Can10) FuelValue() uint16 { return c.FuelLevel & 0x7FFF }

// EngineHours возвращает наработку двигателя в часах.
func (c Can10) EngineHours() float64 {
	return float64(c.AllTimeEngine) / 100
}

// TotalKm возвращает общий пробег в километрах.
func (c Can10) TotalKm() float64 {
	return float64(c.AllTrack) / 100
}

// CANModuleMissing сообщает, что модуль CAN не отвечает.
func (c Can10) CANModuleMissing() bool {
	return c.SecFlagStatus == 0xFFFFFFFF
}

// Termo16 — ячейка датчика температуры.
type Termo16 struct {
	// Status — статус датчика, 0 означает штатную работу.
	Status uint32
	// Temp — температура со знаком.
	Temp int32
}

// Cell реализует Payload.
func (Termo16) Cell() CellType { return CellTermo16 }

// Connected сообщает, что датчик подключён и отвечает.
func (c Termo16) Connected() bool { return c.Status == 0 }

// Lls15 — ячейка уровнемера топлива с большим набором измерений.
type Lls15 struct {
	// Status — статус датчика.
	Status uint16
	// MainFloatLevel — основной поплавок.
	MainFloatLevel uint32
	// TemperatureAverage — средняя температура.
	TemperatureAverage uint32
	// PercentOfVolume — заполнение в процентах объёма.
	PercentOfVolume uint32
	// TotalVolume — полный объём.
	TotalVolume uint32
	// Weight — масса.
	Weight uint32
	// Density — плотность.
	Density uint32
	// NetStandardVolume — нетто-объём при стандартных условиях.
	NetStandardVolume uint32
	// LevelOfWater — уровень воды.
	LevelOfWater uint32
	// Pressure — давление.
	Pressure uint32
	// VaporTemperatureAverage — средняя температура паровой фазы.
	VaporTemperatureAverage uint32
	// VaporWeight — масса паровой фазы.
	VaporWeight uint32
	// LiquidPhaseWeight — масса жидкой фазы.
	LiquidPhaseWeight uint32
}

// Cell реализует Payload.
func (Lls15) Cell() CellType { return CellLls15 }

// Crown03 — ячейка счётчика пассажиропотока Crown.
type Crown03 struct {
	// Odometer — одометр.
	Odometer uint32
	// Zone — номер зоны.
	Zone uint16
	// DoorIn — состояния четырёх входных дверей.
	DoorIn [4]uint8
	// DoorOut — состояния четырёх выходных дверей.
	DoorOut [4]uint8
}

// Cell реализует Payload.
func (Crown03) Cell() CellType { return CellCrown03 }

// Irma04 — ячейка счётчика пассажиропотока ИРМА.
type Irma04 struct {
	// Odometer — одометр.
	Odometer uint32
	// Zone — номер зоны.
	Zone uint16
	// DoorIn — состояния четырёх входных дверей.
	DoorIn [4]uint8
	// DoorOut — состояния четырёх выходных дверей.
	DoorOut [4]uint8
	// Present — наличие пассажира у каждой из четырёх дверей.
	Present [4]bool
	// Closed — закрытость каждой из четырёх дверей.
	Closed [4]bool
}

// Cell реализует Payload.
func (Irma04) Cell() CellType { return CellIrma04 }

// Kdm05 — ячейка климатической установки.
type Kdm05 struct {
	// PgmEnable — признак активности программы.
	PgmEnable uint8
	// PgmWidth — ширина отвала.
	PgmWidth uint8
	// PgmDensity — плотность реагента.
	PgmDensity uint16
	// PloughState — состояние отвала.
	PloughState uint8
	// BrushState — состояние щёток.
	BrushState uint8
}

// Cell реализует Payload.
func (Kdm05) Cell() CellType { return CellKdm05 }

// Idn06 — ячейка идентификатора счётчика импульсов.
type Idn06 struct {
	// NumImpulseMin — минимальное число импульсов.
	NumImpulseMin uint16
	// NumImpulseMax — максимальное число импульсов.
	NumImpulseMax uint16
	// Time — интервал счёта.
	Time uint8
	// NumOverflow — число переполнений счётчика.
	NumOverflow uint8
	// NumImpulse — накопленное число импульсов.
	NumImpulse uint16
	// PresImpulse — текущее значение импульса.
	PresImpulse uint8
}

// Cell реализует Payload.
func (Idn06) Cell() CellType { return CellIdn06 }

// Idn07 — ячейка короткого идентификатора.
type Idn07 struct {
	// Value — значение идентификатора.
	Value uint8
}

// Cell реализует Payload.
func (Idn07) Cell() CellType { return CellIdn07 }

// Reg09 — ячейка регистратора с именем терминала.
type Reg09 struct {
	// ID — идентификатор регистратора со знаком.
	ID int64
	// Name — имя терминала, заполненное нулями.
	Name [32]byte
}

// Cell реализует Payload.
func (Reg09) Cell() CellType { return CellReg09 }

// NameString возвращает имя терминала без нулевого хвоста.
func (c Reg09) NameString() string {
	end := len(c.Name)
	for end > 0 && c.Name[end-1] == 0 {
		end--
	}
	return string(c.Name[:end])
}

// Rfid12 — ячейка RFID-метки.
type Rfid12 struct {
	// Key — ключ метки.
	Key [5]byte
}

// Cell реализует Payload.
func (Rfid12) Cell() CellType { return CellRfid12 }

// Plo13 — ячейка измерителя уровня жидкости.
type Plo13 struct {
	// Density — плотность жидкости.
	Density float32
	// Temperature — температура, °C.
	Temperature float32
	// Level — уровень заполнения.
	Level float32
	// LevelUnit — код единицы измерения уровня.
	LevelUnit uint8
}

// Cell реализует Payload.
func (Plo13) Cell() CellType { return CellPlo13 }

// Bms14 — ячейка батареи.
type Bms14 struct {
	// MaxTemperature — максимальная температура.
	MaxTemperature uint16
	// MinCellVoltage — минимальное напряжение ячейки.
	MinCellVoltage uint16
	// MaxCellVoltage — максимальное напряжение ячейки.
	MaxCellVoltage uint16
	// Voltage — напряжение батареи.
	Voltage uint32
	// CodeError0 — ошибка 0.
	CodeError0 bool
	// CodeError1 — ошибка 1.
	CodeError1 bool
	// CodeError2 — ошибка 2.
	CodeError2 bool
	// CodeError3 — ошибка 3.
	CodeError3 bool
	// CodeError — код ошибки из четырёх бит.
	CodeError uint8
	// Current — ток со знаком, сотые доли ампера.
	Current int32
}

// Cell реализует Payload.
func (Bms14) Cell() CellType { return CellBms14 }

// CAN18 — ячейка расширенных аналоговых и дискретных входов CAN.
type CAN18 struct {
	// AnIn — восемь аналоговых входов.
	AnIn [8]uint16
	// DiIn — состояния дискретных входов.
	DiIn uint8
	// DiOut — состояния дискретных выходов.
	DiOut uint8
	// DiCounter — счётчики восьми дискретных входов.
	DiCounter [8]uint32
}

// Cell реализует Payload.
func (CAN18) Cell() CellType { return CellCAN18 }

// GSMStation — параметры одной базовой станции GSM.
type GSMStation struct {
	// LAC — код области.
	LAC uint16
	// CID — идентификатор соты.
	CID uint16
	// RSSI — уровень принимаемого сигнала.
	RSSI uint8
}

// GSMstations19 — ячейка параметров GSM-станций.
type GSMstations19 struct {
	// MCC — код оператора, три цифры.
	MCC uint16
	// MNC — код сети внутри оператора.
	MNC uint8
	// LAC — код области текущей станции.
	LAC uint16
	// CID — идентификатор соты текущей станции.
	CID uint16
	// RSSI — уровень сигнала текущей станции.
	RSSI uint8
	// TimeAdv — параметр времени.
	TimeAdv uint16
	// Neighbors — параметры шести соседних станций.
	Neighbors [6]GSMStation
}

// Cell реализует Payload.
func (GSMstations19) Cell() CellType { return CellGSMstations19 }

// M333CAN20 — ячейка флагов CAN-модуля M333.
type M333CAN20 struct {
	// FlagHigh — старшие флаги.
	FlagHigh uint32
	// FlagLow — младшие флаги.
	FlagLow uint32
}

// Cell реализует Payload.
func (M333CAN20) Cell() CellType { return CellM333CAN20 }

// ServerStatistics22 — ячейка статистики сервера.
type ServerStatistics22 struct {
	// IDMax — максимальный идентификатор.
	IDMax uint32
	// IDMin — минимальный идентификатор.
	IDMin uint32
	// TmOldest — время самой старой записи.
	TmOldest uint32
	// TmOldestUnack — время самой старой неподтверждённой записи.
	TmOldestUnack uint32
	// CntUnack — число неподтверждённых записей.
	CntUnack uint32
	// CntUnackLosted — число потерянных неподтверждённых записей.
	CntUnackLosted uint32
}

// Cell реализует Payload.
func (ServerStatistics22) Cell() CellType { return CellServerStatistics22 }

// TrackerStatistics23 — ячейка статистики трекера.
type TrackerStatistics23 struct {
	// CntAck — число подтверждений.
	CntAck uint32
	// CntAckRealtime — число подтверждений realtime-пакетов.
	CntAckRealtime uint32
	// CntNoack — число неподтверждённых пакетов.
	CntNoack uint32
	// CntConnect — число соединений.
	CntConnect uint32
}

// Cell реализует Payload.
func (TrackerStatistics23) Cell() CellType { return CellTrackerStatistics23 }

// ZipSensorData100 — ячейка данных произвольного датчика.
type ZipSensorData100 struct {
	// IDSensor — идентификатор датчика.
	IDSensor uint8
	// Flag — флаги нагрузки.
	Flag uint8
	// LenData — заявленная длина нагрузки.
	LenData uint16
	// Data — нагрузка, 40 байт.
	Data [40]byte
}

// Cell реализует Payload.
func (ZipSensorData100) Cell() CellType { return CellZipSensorData100 }

// Alcohol1st17 — ячейка алкотестера, часть 1.
type Alcohol1st17 struct {
	// Status — статус прибора.
	Status uint16
	// AlcoEvent — событие алкогольного теста.
	AlcoEvent uint16
	// AlcoholVolume — показание алкометра.
	AlcoholVolume uint16
	// StartTime — время начала теста.
	StartTime uint32
	// EndTime — время завершения теста.
	EndTime uint32
	// RecordCounter — номер записи.
	RecordCounter uint16
	// Temperature — температура.
	Temperature uint16
	// ReadAlcoEvent — прочитанное событие теста.
	ReadAlcoEvent uint16
	// ReadAlcoholVolume — прочитанное показание алкометра.
	ReadAlcoholVolume uint16
	// ReadStartTime — прочитанное время начала.
	ReadStartTime uint32
	// ReadEndTime — прочитанное время завершения.
	ReadEndTime uint32
	// CurrentTime — текущее время прибора.
	CurrentTime uint32
	// DeviceStatus — состояние прибора.
	DeviceStatus uint16
	// CurrentVolume — текущее показание.
	CurrentVolume uint16
	// CurrentSection — текущая секция.
	CurrentSection uint16
	// Reserved16 — зарезервированное поле.
	Reserved16 uint16
	// Reserved32 — зарезервированное поле.
	Reserved32 uint32
}

// Cell реализует Payload.
func (Alcohol1st17) Cell() CellType { return CellAlcohol1st17 }

// Alcohol2nd21 — ячейка алкотестера, часть 2.
type Alcohol2nd21 struct {
	// Status — статус прибора.
	Status uint16
	// RecordCounter — номер записи.
	RecordCounter uint16
	// DeviceStatusSection — секция состояния прибора.
	DeviceStatusSection uint16
	// CurrentTime — текущее время прибора.
	CurrentTime uint32
	// CurrentVolume — текущее показание.
	CurrentVolume uint16
	// AlcoEventSection — секция события теста.
	AlcoEventSection uint16
	// SensorSerialNumber — серийный номер датчика.
	SensorSerialNumber [8]byte
	// CounterStartValue — значение счётчика на старте.
	CounterStartValue uint32
	// CounterStopValue — значение счётчика на остановке.
	CounterStopValue uint32
	// StartTime — время начала теста.
	StartTime uint32
	// EndTime — время завершения теста.
	EndTime uint32
	// FinalTemperature — итоговая температура.
	FinalTemperature uint16
	// Temperature — температура.
	Temperature uint16
	// Reserved16 — зарезервированное поле.
	Reserved16 uint16
	// ReadAlcoEventSection — прочитанная секция события.
	ReadAlcoEventSection uint16
	// ReadSensorSerialNumber — прочитанный серийный номер.
	ReadSensorSerialNumber [8]byte
	// ReadCounterStartValue — прочитанное значение счётчика на старте.
	ReadCounterStartValue uint32
	// ReadCounterStopValue — прочитанное значение счётчика на остановке.
	ReadCounterStopValue uint32
	// ReadStartTime — прочитанное время начала.
	ReadStartTime uint32
	// ReadEndTime — прочитанное время завершения.
	ReadEndTime uint32
	// ReadFinalTemperature — прочитанная итоговая температура.
	ReadFinalTemperature uint16
	// ProductType — тип изделия.
	ProductType uint32
	// ProductCode — код изделия.
	ProductCode [20]byte
	// OrganizationCode — код организации.
	OrganizationCode [18]byte
	// InstallDateTime — дата и время установки.
	InstallDateTime [16]byte
	// ErrorDescription — описание ошибки.
	ErrorDescription [50]byte
}

// Cell реализует Payload.
func (Alcohol2nd21) Cell() CellType { return CellAlcohol2nd21 }

type cursor struct {
	buf []byte
	off int
}

func (c *cursor) remaining() int { return len(c.buf) - c.off }

func (c *cursor) take(n int) ([]byte, error) {
	if c.remaining() < n {
		return nil, fmt.Errorf("%w: нужно %d байт, осталось %d", ErrEmptyPayload, n, c.remaining())
	}
	out := c.buf[c.off : c.off+n]
	c.off += n
	return out, nil
}

func (c *cursor) u8() (uint8, error) {
	b, err := c.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *cursor) u16() (uint16, error) {
	b, err := c.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (c *cursor) u32() (uint32, error) {
	b, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (c *cursor) i8() (int8, error) {
	v, err := c.u8()
	return int8(v), err
}

func (c *cursor) i16() (int16, error) {
	v, err := c.u16()
	return int16(v), err
}

func (c *cursor) i32() (int32, error) {
	v, err := c.u32()
	return int32(v), err
}

func (c *cursor) u64() (uint64, error) {
	b, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (c *cursor) i64() (int64, error) {
	v, err := c.u64()
	return int64(v), err
}

func (c *cursor) f32() (float32, error) {
	b, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(b)), nil
}

// array заполняет dst байтами из потока; dst должен иметь точный размер.
func (c *cursor) array(dst []byte) error {
	b, err := c.take(len(dst))
	if err != nil {
		return err
	}
	copy(dst, b)
	return nil
}

func parseNav00(b []byte) (Nav00, error) {
	c := &cursor{buf: b}
	var out Nav00
	var err error
	if out.Timestamp, err = c.u32(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.LongitudeRaw, err = c.u32(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.LatitudeRaw, err = c.u32(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.ExtraDop, err = c.u8(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.BatVoltage, err = c.u8(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.SpeedAvg, err = c.u16(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.SpeedMax, err = c.u16(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.Course, err = c.u16(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.Track, err = c.u16(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.Altitude, err = c.u16(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.Nsat, err = c.u8(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	if out.Pdop, err = c.u8(); err != nil {
		return out, wrapCellErr(CellNav00, err)
	}
	return out, nil
}

func parseIntSensor02(b []byte) (IntSensor02, error) {
	c := &cursor{buf: b}
	var out IntSensor02
	var err error
	for i := range out.AnIn {
		if out.AnIn[i], err = c.u16(); err != nil {
			return out, wrapCellErr(CellIntSensor02, err)
		}
	}
	if out.DiIn, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIntSensor02, err)
	}
	if out.DiOut, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIntSensor02, err)
	}
	for i := range out.DiCounter {
		if out.DiCounter[i], err = c.u16(); err != nil {
			return out, wrapCellErr(CellIntSensor02, err)
		}
	}
	if out.Odometer, err = c.u32(); err != nil {
		return out, wrapCellErr(CellIntSensor02, err)
	}
	if out.Csq, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIntSensor02, err)
	}
	if out.GprsState, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIntSensor02, err)
	}
	if out.AccelEnergy, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIntSensor02, err)
	}
	if out.ExtVolt, err = c.i8(); err != nil {
		return out, wrapCellErr(CellIntSensor02, err)
	}
	return out, nil
}

func parseUsi08(b []byte) (Usi08, error) {
	c := &cursor{buf: b}
	var out Usi08
	var err error
	if out.DetStatus, err = c.u8(); err != nil {
		return out, wrapCellErr(CellUsi08, err)
	}
	if out.LevelMM, err = c.u16(); err != nil {
		return out, wrapCellErr(CellUsi08, err)
	}
	if out.LevelLiters, err = c.u16(); err != nil {
		return out, wrapCellErr(CellUsi08, err)
	}
	if out.Temperature, err = c.u8(); err != nil {
		return out, wrapCellErr(CellUsi08, err)
	}
	return out, nil
}

func parseCan10(b []byte) (Can10, error) {
	c := &cursor{buf: b}
	var out Can10
	var err error
	if out.SecFlagStatus, err = c.u32(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	if out.AllTimeEngine, err = c.u32(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	if out.AllTrack, err = c.u32(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	if out.AllFuelConsum, err = c.u32(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	if out.FuelLevel, err = c.u16(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	if out.SpeedTurnEngine, err = c.u16(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	if out.TEngine, err = c.i16(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	if out.Speed, err = c.u8(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	for i := range out.PressureAxis {
		if out.PressureAxis[i], err = c.u16(); err != nil {
			return out, wrapCellErr(CellCan10, err)
		}
	}
	if out.FlagAlarm, err = c.u32(); err != nil {
		return out, wrapCellErr(CellCan10, err)
	}
	return out, nil
}

func parseTermo16(b []byte) (Termo16, error) {
	c := &cursor{buf: b}
	var out Termo16
	var err error
	if out.Status, err = c.u32(); err != nil {
		return out, wrapCellErr(CellTermo16, err)
	}
	if out.Temp, err = c.i32(); err != nil {
		return out, wrapCellErr(CellTermo16, err)
	}
	return out, nil
}

func parseLls15(b []byte) (Lls15, error) {
	c := &cursor{buf: b}
	var out Lls15
	var err error
	if out.Status, err = c.u16(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.MainFloatLevel, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.TemperatureAverage, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.PercentOfVolume, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.TotalVolume, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.Weight, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.Density, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.NetStandardVolume, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.LevelOfWater, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.Pressure, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.VaporTemperatureAverage, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.VaporWeight, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	if out.LiquidPhaseWeight, err = c.u32(); err != nil {
		return out, wrapCellErr(CellLls15, err)
	}
	return out, nil
}

func parseCrown03(b []byte) (Crown03, error) {
	c := &cursor{buf: b}
	var out Crown03
	var err error
	if out.Odometer, err = c.u32(); err != nil {
		return out, wrapCellErr(CellCrown03, err)
	}
	if out.Zone, err = c.u16(); err != nil {
		return out, wrapCellErr(CellCrown03, err)
	}
	if err = c.array(out.DoorIn[:]); err != nil {
		return out, wrapCellErr(CellCrown03, err)
	}
	if err = c.array(out.DoorOut[:]); err != nil {
		return out, wrapCellErr(CellCrown03, err)
	}
	return out, nil
}

func parseIrma04(b []byte) (Irma04, error) {
	c := &cursor{buf: b}
	var out Irma04
	var err error
	if out.Odometer, err = c.u32(); err != nil {
		return out, wrapCellErr(CellIrma04, err)
	}
	if out.Zone, err = c.u16(); err != nil {
		return out, wrapCellErr(CellIrma04, err)
	}
	if err = c.array(out.DoorIn[:]); err != nil {
		return out, wrapCellErr(CellIrma04, err)
	}
	if err = c.array(out.DoorOut[:]); err != nil {
		return out, wrapCellErr(CellIrma04, err)
	}
	flags, err := c.u8()
	if err != nil {
		return out, wrapCellErr(CellIrma04, err)
	}
	for i := range out.Present {
		out.Present[i] = flags&(1<<uint(i)) != 0
		out.Closed[i] = flags&(1<<uint(i+4)) != 0
	}
	return out, nil
}

func parseKdm05(b []byte) (Kdm05, error) {
	c := &cursor{buf: b}
	var out Kdm05
	var err error
	if out.PgmEnable, err = c.u8(); err != nil {
		return out, wrapCellErr(CellKdm05, err)
	}
	if out.PgmWidth, err = c.u8(); err != nil {
		return out, wrapCellErr(CellKdm05, err)
	}
	if out.PgmDensity, err = c.u16(); err != nil {
		return out, wrapCellErr(CellKdm05, err)
	}
	if out.PloughState, err = c.u8(); err != nil {
		return out, wrapCellErr(CellKdm05, err)
	}
	if out.BrushState, err = c.u8(); err != nil {
		return out, wrapCellErr(CellKdm05, err)
	}
	return out, nil
}

func parseIdn06(b []byte) (Idn06, error) {
	c := &cursor{buf: b}
	var out Idn06
	var err error
	if out.NumImpulseMin, err = c.u16(); err != nil {
		return out, wrapCellErr(CellIdn06, err)
	}
	if out.NumImpulseMax, err = c.u16(); err != nil {
		return out, wrapCellErr(CellIdn06, err)
	}
	if out.Time, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIdn06, err)
	}
	if out.NumOverflow, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIdn06, err)
	}
	if out.NumImpulse, err = c.u16(); err != nil {
		return out, wrapCellErr(CellIdn06, err)
	}
	if out.PresImpulse, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIdn06, err)
	}
	return out, nil
}

func parseIdn07(b []byte) (Idn07, error) {
	c := &cursor{buf: b}
	var out Idn07
	var err error
	if out.Value, err = c.u8(); err != nil {
		return out, wrapCellErr(CellIdn07, err)
	}
	return out, nil
}

func parseReg09(b []byte) (Reg09, error) {
	c := &cursor{buf: b}
	var out Reg09
	var err error
	if out.ID, err = c.i64(); err != nil {
		return out, wrapCellErr(CellReg09, err)
	}
	if err = c.array(out.Name[:]); err != nil {
		return out, wrapCellErr(CellReg09, err)
	}
	return out, nil
}

func parseRfid12(b []byte) (Rfid12, error) {
	c := &cursor{buf: b}
	var out Rfid12
	if err := c.array(out.Key[:]); err != nil {
		return out, wrapCellErr(CellRfid12, err)
	}
	return out, nil
}

func parsePlo13(b []byte) (Plo13, error) {
	c := &cursor{buf: b}
	var out Plo13
	var err error
	if out.Density, err = c.f32(); err != nil {
		return out, wrapCellErr(CellPlo13, err)
	}
	if out.Temperature, err = c.f32(); err != nil {
		return out, wrapCellErr(CellPlo13, err)
	}
	if out.Level, err = c.f32(); err != nil {
		return out, wrapCellErr(CellPlo13, err)
	}
	if out.LevelUnit, err = c.u8(); err != nil {
		return out, wrapCellErr(CellPlo13, err)
	}
	return out, nil
}

func parseBms14(b []byte) (Bms14, error) {
	c := &cursor{buf: b}
	var out Bms14
	var err error
	if out.MaxTemperature, err = c.u16(); err != nil {
		return out, wrapCellErr(CellBms14, err)
	}
	if out.MinCellVoltage, err = c.u16(); err != nil {
		return out, wrapCellErr(CellBms14, err)
	}
	if out.MaxCellVoltage, err = c.u16(); err != nil {
		return out, wrapCellErr(CellBms14, err)
	}
	if out.Voltage, err = c.u32(); err != nil {
		return out, wrapCellErr(CellBms14, err)
	}
	flags, err := c.u8()
	if err != nil {
		return out, wrapCellErr(CellBms14, err)
	}
	out.CodeError0 = flags&0x01 != 0
	out.CodeError1 = flags&0x02 != 0
	out.CodeError2 = flags&0x04 != 0
	out.CodeError3 = flags&0x08 != 0
	out.CodeError = (flags >> 4) & 0x0F
	if out.Current, err = c.i32(); err != nil {
		return out, wrapCellErr(CellBms14, err)
	}
	return out, nil
}

func parseCAN18(b []byte) (CAN18, error) {
	c := &cursor{buf: b}
	var out CAN18
	var err error
	for i := range out.AnIn {
		if out.AnIn[i], err = c.u16(); err != nil {
			return out, wrapCellErr(CellCAN18, err)
		}
	}
	if out.DiIn, err = c.u8(); err != nil {
		return out, wrapCellErr(CellCAN18, err)
	}
	if out.DiOut, err = c.u8(); err != nil {
		return out, wrapCellErr(CellCAN18, err)
	}
	for i := range out.DiCounter {
		if out.DiCounter[i], err = c.u32(); err != nil {
			return out, wrapCellErr(CellCAN18, err)
		}
	}
	return out, nil
}

func parseGSMstations19(b []byte) (GSMstations19, error) {
	c := &cursor{buf: b}
	var out GSMstations19
	var err error
	if out.MCC, err = c.u16(); err != nil {
		return out, wrapCellErr(CellGSMstations19, err)
	}
	if out.MNC, err = c.u8(); err != nil {
		return out, wrapCellErr(CellGSMstations19, err)
	}
	if out.LAC, err = c.u16(); err != nil {
		return out, wrapCellErr(CellGSMstations19, err)
	}
	if out.CID, err = c.u16(); err != nil {
		return out, wrapCellErr(CellGSMstations19, err)
	}
	if out.RSSI, err = c.u8(); err != nil {
		return out, wrapCellErr(CellGSMstations19, err)
	}
	if out.TimeAdv, err = c.u16(); err != nil {
		return out, wrapCellErr(CellGSMstations19, err)
	}
	for i := range out.Neighbors {
		if out.Neighbors[i].LAC, err = c.u16(); err != nil {
			return out, wrapCellErr(CellGSMstations19, err)
		}
		if out.Neighbors[i].CID, err = c.u16(); err != nil {
			return out, wrapCellErr(CellGSMstations19, err)
		}
		if out.Neighbors[i].RSSI, err = c.u8(); err != nil {
			return out, wrapCellErr(CellGSMstations19, err)
		}
	}
	return out, nil
}

func parseM333CAN20(b []byte) (M333CAN20, error) {
	c := &cursor{buf: b}
	var out M333CAN20
	var err error
	if out.FlagHigh, err = c.u32(); err != nil {
		return out, wrapCellErr(CellM333CAN20, err)
	}
	if out.FlagLow, err = c.u32(); err != nil {
		return out, wrapCellErr(CellM333CAN20, err)
	}
	return out, nil
}

func parseServerStatistics22(b []byte) (ServerStatistics22, error) {
	c := &cursor{buf: b}
	var out ServerStatistics22
	var err error
	for _, dst := range []*uint32{
		&out.IDMax, &out.IDMin, &out.TmOldest,
		&out.TmOldestUnack, &out.CntUnack, &out.CntUnackLosted,
	} {
		if *dst, err = c.u32(); err != nil {
			return out, wrapCellErr(CellServerStatistics22, err)
		}
	}
	return out, nil
}

func parseTrackerStatistics23(b []byte) (TrackerStatistics23, error) {
	c := &cursor{buf: b}
	var out TrackerStatistics23
	var err error
	for _, dst := range []*uint32{&out.CntAck, &out.CntAckRealtime, &out.CntNoack, &out.CntConnect} {
		if *dst, err = c.u32(); err != nil {
			return out, wrapCellErr(CellTrackerStatistics23, err)
		}
	}
	return out, nil
}

func parseZipSensorData100(b []byte) (ZipSensorData100, error) {
	c := &cursor{buf: b}
	var out ZipSensorData100
	var err error
	if out.IDSensor, err = c.u8(); err != nil {
		return out, wrapCellErr(CellZipSensorData100, err)
	}
	if out.Flag, err = c.u8(); err != nil {
		return out, wrapCellErr(CellZipSensorData100, err)
	}
	if out.LenData, err = c.u16(); err != nil {
		return out, wrapCellErr(CellZipSensorData100, err)
	}
	if err = c.array(out.Data[:]); err != nil {
		return out, wrapCellErr(CellZipSensorData100, err)
	}
	return out, nil
}

func parseAlcohol1st17(b []byte) (Alcohol1st17, error) {
	c := &cursor{buf: b}
	var out Alcohol1st17
	var err error
	if out.Status, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.AlcoEvent, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.AlcoholVolume, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.StartTime, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.EndTime, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.RecordCounter, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.Temperature, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.ReadAlcoEvent, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.ReadAlcoholVolume, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.ReadStartTime, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.ReadEndTime, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.CurrentTime, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.DeviceStatus, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.CurrentVolume, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.CurrentSection, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.Reserved16, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	if out.Reserved32, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol1st17, err)
	}
	return out, nil
}

func parseAlcohol2nd21(b []byte) (Alcohol2nd21, error) {
	c := &cursor{buf: b}
	var out Alcohol2nd21
	var err error
	if out.Status, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.RecordCounter, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.DeviceStatusSection, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.CurrentTime, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.CurrentVolume, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.AlcoEventSection, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if err = c.array(out.SensorSerialNumber[:]); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	for _, dst := range []*uint32{&out.CounterStartValue, &out.CounterStopValue, &out.StartTime, &out.EndTime} {
		if *dst, err = c.u32(); err != nil {
			return out, wrapCellErr(CellAlcohol2nd21, err)
		}
	}
	if out.FinalTemperature, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.Temperature, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.Reserved16, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.ReadAlcoEventSection, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if err = c.array(out.ReadSensorSerialNumber[:]); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	for _, dst := range []*uint32{&out.ReadCounterStartValue, &out.ReadCounterStopValue, &out.ReadStartTime, &out.ReadEndTime} {
		if *dst, err = c.u32(); err != nil {
			return out, wrapCellErr(CellAlcohol2nd21, err)
		}
	}
	if out.ReadFinalTemperature, err = c.u16(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	if out.ProductType, err = c.u32(); err != nil {
		return out, wrapCellErr(CellAlcohol2nd21, err)
	}
	for _, dst := range [][]byte{
		out.ProductCode[:], out.OrganizationCode[:],
		out.InstallDateTime[:], out.ErrorDescription[:],
	} {
		if err = c.array(dst); err != nil {
			return out, wrapCellErr(CellAlcohol2nd21, err)
		}
	}
	return out, nil
}

var payloadParsers = map[CellType]func([]byte) (Payload, error){
	CellNav00:               func(b []byte) (Payload, error) { return parseNav00(b) },
	CellIntSensor02:         func(b []byte) (Payload, error) { return parseIntSensor02(b) },
	CellCrown03:             func(b []byte) (Payload, error) { return parseCrown03(b) },
	CellIrma04:              func(b []byte) (Payload, error) { return parseIrma04(b) },
	CellKdm05:               func(b []byte) (Payload, error) { return parseKdm05(b) },
	CellIdn06:               func(b []byte) (Payload, error) { return parseIdn06(b) },
	CellIdn07:               func(b []byte) (Payload, error) { return parseIdn07(b) },
	CellUsi08:               func(b []byte) (Payload, error) { return parseUsi08(b) },
	CellReg09:               func(b []byte) (Payload, error) { return parseReg09(b) },
	CellCan10:               func(b []byte) (Payload, error) { return parseCan10(b) },
	CellRfid12:              func(b []byte) (Payload, error) { return parseRfid12(b) },
	CellPlo13:               func(b []byte) (Payload, error) { return parsePlo13(b) },
	CellBms14:               func(b []byte) (Payload, error) { return parseBms14(b) },
	CellLls15:               func(b []byte) (Payload, error) { return parseLls15(b) },
	CellTermo16:             func(b []byte) (Payload, error) { return parseTermo16(b) },
	CellAlcohol1st17:        func(b []byte) (Payload, error) { return parseAlcohol1st17(b) },
	CellCAN18:               func(b []byte) (Payload, error) { return parseCAN18(b) },
	CellGSMstations19:       func(b []byte) (Payload, error) { return parseGSMstations19(b) },
	CellM333CAN20:           func(b []byte) (Payload, error) { return parseM333CAN20(b) },
	CellAlcohol2nd21:        func(b []byte) (Payload, error) { return parseAlcohol2nd21(b) },
	CellServerStatistics22:  func(b []byte) (Payload, error) { return parseServerStatistics22(b) },
	CellTrackerStatistics23: func(b []byte) (Payload, error) { return parseTrackerStatistics23(b) },
	CellZipSensorData100:    func(b []byte) (Payload, error) { return parseZipSensorData100(b) },
}

func wrapCellErr(cellType CellType, err error) error {
	return fmt.Errorf("ndtp: ячейка %s: %w", cellType, err)
}

// CellHeader — расположение одной ячейки в теле пакета без её разбора.
//
// Применяется, когда нужны только границы ячеек, например для диагностики:
// разбор payload не выполняется и неизвестные типы не считаются ошибкой.
type CellHeader struct {
	// Type — тип ячейки.
	Type CellType
	// Number — порядковый номер экземпляра ячейки.
	Number uint8
	// Size — размер payload в байтах.
	Size int
	// Offset — смещение ячейки от начала тела пакета.
	Offset int
}

// ScanCells проходит тело пакета и возвращает границы всех ячеек без разбора
// payload. Ошибка возникает, если тип неизвестен или тело обрывается.
func ScanCells(body []byte) ([]CellHeader, error) {
	var headers []CellHeader
	for off := 0; off < len(body); {
		if len(body)-off < 2 {
			return nil, fmt.Errorf("%w: осталось %d байт, нужен минимум type+number",
				ErrEmptyPayload, len(body)-off)
		}
		cellType := CellType(body[off])
		number := body[off+1]
		size, ok := cellType.Size()
		if !ok {
			return nil, fmt.Errorf("ndtp: неизвестный тип ячейки %d (number=%d) на смещении %d",
				uint8(cellType), number, off)
		}
		if len(body)-off-2 < size {
			return nil, fmt.Errorf("ndtp: ячейка %s (number=%d) требует %d байт, доступно %d",
				cellType, number, size, len(body)-off-2)
		}
		headers = append(headers, CellHeader{
			Type:   cellType,
			Number: number,
			Size:   size,
			Offset: off,
		})
		off += 2 + size
	}
	return headers, nil
}

func parseCellAt(body []byte, off int) (Cell, int, error) {
	if len(body)-off < 2 {
		return Cell{}, 0, fmt.Errorf("%w: осталось %d байт, нужен минимум type+number",
			ErrEmptyPayload, len(body)-off)
	}
	cellType := CellType(body[off])
	number := body[off+1]
	size, ok := cellType.Size()
	if !ok {
		return Cell{}, 0, fmt.Errorf("ndtp: неподдерживаемый тип ячейки %s (number=%d)", cellType, number)
	}
	if len(body)-off-2 < size {
		return Cell{}, 0, fmt.Errorf("ndtp: ячейка %s (number=%d) требует %d байт, доступно %d",
			cellType, number, size, len(body)-off-2)
	}
	parse, ok := payloadParsers[cellType]
	if !ok {
		return Cell{}, 0, fmt.Errorf("ndtp: нет парсера для ячейки %s", cellType)
	}
	decoded, err := parse(body[off+2 : off+2+size])
	if err != nil {
		return Cell{}, 0, err
	}
	return Cell{Number: number, Payload: decoded}, 2 + size, nil
}

// ParseCell разбирает ровно одну ячейку и требует, чтобы она занимала весь
// переданный буфер. Для разбора тела пакета целиком используйте ParseCells.
func ParseCell(body []byte) (Cell, error) {
	cell, consumed, err := parseCellAt(body, 0)
	if err != nil {
		return Cell{}, err
	}
	if consumed != len(body) {
		return Cell{}, fmt.Errorf("%w: ячейка занимает %d байт, в теле %d",
			ErrEmptyPayload, consumed, len(body))
	}
	return cell, nil
}

// ParseCells разбирает тело realtime-пакета в последовательность ячеек.
//
// Границы ячеек определяются по типу через таблицу размеров, поэтому порядок
// ячеек не фиксирован. Разбор строгий: неизвестный тип ячейки, тип без
// декодера, нехватка байтов или лишние байты в конце возвращаются как ошибка.
func ParseCells(body []byte) ([]Cell, error) {
	var cells []Cell
	for off := 0; off < len(body); {
		cell, consumed, err := parseCellAt(body, off)
		if err != nil {
			return nil, err
		}
		cells = append(cells, cell)
		off += consumed
	}
	return cells, nil
}

// Type возвращает тип ячейки.
func (c Cell) Type() CellType { return c.Payload.Cell() }

// PayloadCellSize возвращает размер payload ячейки в байтах.
func (c Cell) PayloadCellSize() int {
	size, _ := c.Type().Size()
	return size
}

// Nav00 возвращает содержимое ячейки как Nav00. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Nav00() (Nav00, bool) {
	value, ok := c.Payload.(Nav00)
	return value, ok
}

// IntSensor02 возвращает содержимое ячейки как IntSensor02. Второе значение
// false означает, что ячейка другого типа.
func (c Cell) IntSensor02() (IntSensor02, bool) {
	value, ok := c.Payload.(IntSensor02)
	return value, ok
}

// Usi08 возвращает содержимое ячейки как Usi08. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Usi08() (Usi08, bool) {
	value, ok := c.Payload.(Usi08)
	return value, ok
}

// Can10 возвращает содержимое ячейки как Can10. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Can10() (Can10, bool) {
	value, ok := c.Payload.(Can10)
	return value, ok
}

// Termo16 возвращает содержимое ячейки как Termo16. Второе значение false
// означает, что ячейка другого типа.
func (c Cell) Termo16() (Termo16, bool) {
	value, ok := c.Payload.(Termo16)
	return value, ok
}

// Lls15 возвращает содержимое ячейки как Lls15. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Lls15() (Lls15, bool) {
	value, ok := c.Payload.(Lls15)
	return value, ok
}

// Crown03 возвращает содержимое ячейки как Crown03. Второе значение false
// означает, что ячейка другого типа.
func (c Cell) Crown03() (Crown03, bool) {
	value, ok := c.Payload.(Crown03)
	return value, ok
}

// Irma04 возвращает содержимое ячейки как Irma04. Второе значение false
// означает, что ячейка другого типа.
func (c Cell) Irma04() (Irma04, bool) {
	value, ok := c.Payload.(Irma04)
	return value, ok
}

// Kdm05 возвращает содержимое ячейки как Kdm05. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Kdm05() (Kdm05, bool) {
	value, ok := c.Payload.(Kdm05)
	return value, ok
}

// Idn06 возвращает содержимое ячейки как Idn06. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Idn06() (Idn06, bool) {
	value, ok := c.Payload.(Idn06)
	return value, ok
}

// Idn07 возвращает содержимое ячейки как Idn07. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Idn07() (Idn07, bool) {
	value, ok := c.Payload.(Idn07)
	return value, ok
}

// Reg09 возвращает содержимое ячейки как Reg09. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Reg09() (Reg09, bool) {
	value, ok := c.Payload.(Reg09)
	return value, ok
}

// Rfid12 возвращает содержимое ячейки как Rfid12. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Rfid12() (Rfid12, bool) {
	value, ok := c.Payload.(Rfid12)
	return value, ok
}

// Plo13 возвращает содержимое ячейки как Plo13. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Plo13() (Plo13, bool) {
	value, ok := c.Payload.(Plo13)
	return value, ok
}

// Bms14 возвращает содержимое ячейки как Bms14. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) Bms14() (Bms14, bool) {
	value, ok := c.Payload.(Bms14)
	return value, ok
}

// CAN18 возвращает содержимое ячейки как CAN18. Второе значение false означает,
// что ячейка другого типа.
func (c Cell) CAN18() (CAN18, bool) {
	value, ok := c.Payload.(CAN18)
	return value, ok
}

// GSMstations19 возвращает содержимое ячейки как GSMstations19. Второе значение
// false означает, что ячейка другого типа.
func (c Cell) GSMstations19() (GSMstations19, bool) {
	value, ok := c.Payload.(GSMstations19)
	return value, ok
}

// M333CAN20 возвращает содержимое ячейки как M333CAN20. Второе значение false
// означает, что ячейка другого типа.
func (c Cell) M333CAN20() (M333CAN20, bool) {
	value, ok := c.Payload.(M333CAN20)
	return value, ok
}

// ServerStatistics22 возвращает содержимое ячейки как ServerStatistics22.
// Второе значение false означает, что ячейка другого типа.
func (c Cell) ServerStatistics22() (ServerStatistics22, bool) {
	value, ok := c.Payload.(ServerStatistics22)
	return value, ok
}

// TrackerStatistics23 возвращает содержимое ячейки как TrackerStatistics23.
// Второе значение false означает, что ячейка другого типа.
func (c Cell) TrackerStatistics23() (TrackerStatistics23, bool) {
	value, ok := c.Payload.(TrackerStatistics23)
	return value, ok
}

// ZipSensorData100 возвращает содержимое ячейки как ZipSensorData100. Второе
// значение false означает, что ячейка другого типа.
func (c Cell) ZipSensorData100() (ZipSensorData100, bool) {
	value, ok := c.Payload.(ZipSensorData100)
	return value, ok
}

// Alcohol1st17 возвращает содержимое ячейки как Alcohol1st17. Второе значение
// false означает, что ячейка другого типа.
func (c Cell) Alcohol1st17() (Alcohol1st17, bool) {
	value, ok := c.Payload.(Alcohol1st17)
	return value, ok
}

// Alcohol2nd21 возвращает содержимое ячейки как Alcohol2nd21. Второе значение
// false означает, что ячейка другого типа.
func (c Cell) Alcohol2nd21() (Alcohol2nd21, bool) {
	value, ok := c.Payload.(Alcohol2nd21)
	return value, ok
}
