// Package schedule читает план-график движения и отвечает на вопросы о нём,
// нужные прогнозу: где машина должна оказаться, что у неё впереди и как далеко
// до цели по маршруту.
//
// Ключ хранения — tr_id, а не unit_id: в расписании транспортное средство
// identified по своему номеру, и связь с физическим устройством живёт в
// отдельном типе Binding. Смешивать эти идентификаторы в одном поле означало
// бы разрушить соответствие в момент первой же выгрузки признаков.
package schedule

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Границы горизонта прогноза. Значение 10–15 минут задано условием задачи и
// одновременно является правилом выбора целевой остановки.
const (
	// MinHorizon — нижняя граница: остановки на (T+MinHorizon, T+MaxHorizon].
	MinHorizon = 10 * time.Minute
	// MaxHorizon — верхняя граница окна выбора цели.
	MaxHorizon = 15 * time.Minute
)

// timeLayout разбирает оба встречающихся вида отметки времени: с наносекундной
// дробной частью (train/schedule.csv) и без неё (validate/schedule_plan.csv,
// колонка T в points.csv). В Go хвостовые девятки в макете означают
// необязательную дробную часть, поэтому одна константа покрывает оба случая.
const timeLayout = "2006-01-02 15:04:05.999999999"

// Обязательные колонки расписания.
const (
	colActionID   = "tt_action_item_id"
	colTimeBegin  = "time_begin"
	colTRID       = "tr_id"
	colGeom       = "geom"
	colOrderDate  = "order_date"
	colManualFill = "manual_fill"
	colAddress    = "building_address"
	colTimeFact   = "time_fact_begin"
)

// Stop — одна остановка план-графика.
type Stop struct {
	// ActionID — идентификатор строки расписания, он же target_stop_id в
	// размеченной выборке. Глобально уникален.
	ActionID int64 `json:"action_id"`
	// TRID — номер транспортного средства.
	TRID int64 `json:"tr_id"`
	// TimeBegin — плановое время прибытия по расписанию. Это и есть
	// предсказываемая величина: фактическое время известно лишь постфактум.
	TimeBegin time.Time `json:"time_begin"`
	// TimeFactBegin — фактическое время прибытия. Заполняется в train, но
	// отсутствует в validate/schedule_plan.csv, поэтому его наличие
	// отражает HasFact.
	TimeFactBegin time.Time `json:"time_fact_begin"`
	// HasFact — заполнено ли TimeFactBegin.
	HasFact bool `json:"has_fact"`
	// OrderDate — дата рейса в исходном формате.
	OrderDate string `json:"order_date"`
	// ManualFill — отметка о ручном заполнении строки. Такие строки
	// unreliable: их плановое время могли внести вручную, поэтому признак
	// должен доходить до модели, а не отбрасываться молча.
	ManualFill bool `json:"manual_fill"`
	// Lon, Lat — координаты остановки, разобранные из WKT.
	Lon float64 `json:"lon"`
	Lat float64 `json:"lat"`
	// Address — адрес остановки для показа в интерфейсе.
	Address string `json:"address"`
}

// Target — результат выбора целевой остановки.
type Target struct {
	// Stop — выбранная остановка: ближайшая по времени в окне горизонта.
	Stop Stop `json:"stop"`
	// Candidates — число строк расписания, попавших в окно. Это размер
	// окна по времени, а не мера неоднозначности: в размеченной выборке
	// в окно попадало несколько строк почти в каждом случае, и правило
	// при этом однозначно.
	Candidates int `json:"candidates"`
	// Tied — число строк, разделяющих минимальное время в окне. Именно
	// это настоящая неоднозначность: такие строки геометрией не
	// различаются, и выбирается первая по tt_action_item_id. В
	// размеченной выборке Tied > 1 в 10 из 151 случаев.
	Tied int `json:"tied"`
}

// Ambiguous сообщает, что ближайшее время в окне разделяют несколько
// остановок. Такую точку нельзя обучать на единственном ответе: расписание
// само по себе не говорит, какая из остановок имелась в виду.
func (t Target) Ambiguous() bool { return t.Tied > 1 }

// Schedule — план-график в памяти, с индексами по транспортному средству и
// по идентификатору остановки. Тип неизменяем после загрузки и безопасен для
// конкурентного чтения.
type Schedule struct {
	// vehicles — остановки, отсортированные по (TimeBegin, ActionID).
	vehicles map[int64][]Stop
	// byID — все остановки по идентификатору строки.
	byID map[int64]Stop
	// order — номера транспортных средств по возрастанию.
	order []int64
	// stops — общее число строк, включая строки без геометрии.
	stops int
	// rows — число строк, прочитанных из источника.
	rows int
	// withoutGeometry — строки с пустой или нечитаемой геометрией.
	withoutGeometry int
	// manualFilled — строки с ручным заполнением.
	manualFilled int
	// outsideRegion — остановки с координатами вне ожидаемого региона.
	outsideRegion int
	// region — границы ожидаемого региона.
	region Region
}

// Region — прямоугольник ожидаемых координат. Он нужен не для красоты, а
// чтобы поймать перестановку колонок геометрии: пара POINT (55.80 37.43)
// удовлетворяет проверке диапазонов, потому что обе координаты в пределах
// Земли, и при этом помещает остановку в 600 км от города. Диапазонная
// проверка такой случай не видит, а неверная привязка портит все признаки
// молча.
type Region struct {
	MinLon float64
	MinLat float64
	MaxLon float64
	MaxLat float64
}

// Contains сообщает, лежит ли точка внутри региона.
func (r Region) Contains(lon, lat float64) bool {
	return lon >= r.MinLon && lon <= r.MaxLon && lat >= r.MinLat && lat <= r.MaxLat
}

// DefaultRegion — границы региона, покрывающего город и область обслуживания
// парка из задания. Значения намеренно шире точки данных, а не равны ей:
// задача — поймать перестановку, а не отсечь окраинный рейс.
var DefaultRegion = Region{MinLon: 36.0, MinLat: 55.0, MaxLon: 38.5, MaxLat: 56.5}

// LoadFile читает расписание из файла.
func LoadFile(path string) (*Schedule, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("schedule: %w", err)
	}
	defer file.Close()
	return Load(file)
}

// Load читает расписание из потока CSV.
//
// Строки без геометрии не отбрасываются: на них нельзя построить маршрут, но
// они остаются значимыми для выбора цели по времени. Их число отражается в
// Schedule.WithoutGeometry.
func Load(r io.Reader) (*Schedule, error) {
	reader := csv.NewReader(r)
	reader.ReuseRecord = true
	// В geom и address есть запятые внутри кавычек; FieldsPerRecord = -1
	// снимает требование одинаковой длины строк, что важнее подсказки,
	// потому что пропуск колонки в конце строки встречается в выгрузках.
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("schedule: не удалось прочитать заголовок: %w", err)
	}
	columns, err := indexColumns(header)
	if err != nil {
		return nil, err
	}

	s := &Schedule{
		vehicles: make(map[int64][]Stop),
		byID:     make(map[int64]Stop),
		region:   DefaultRegion,
	}
	for row := 2; ; row++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("schedule: строка %d: %w", row, err)
		}
		s.rows++
		stop, ok, err := parseStop(record, columns, row)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, exists := s.byID[stop.ActionID]; exists {
			return nil, fmt.Errorf("schedule: строка %d: повтор tt_action_item_id %d", row, stop.ActionID)
		}
		s.byID[stop.ActionID] = stop
		s.vehicles[stop.TRID] = append(s.vehicles[stop.TRID], stop)
		s.stops++
		if stop.ManualFill {
			s.manualFilled++
		}
		if !stop.HasGeometry() {
			s.withoutGeometry++
		} else if !s.region.Contains(stop.Lon, stop.Lat) {
			// Строка не отбрасывается: она остаётся значимой для выбора
			// цели по времени. Но попадание в счётчик делает перестановку
			// координат видимой в отчёте, а не обнаруживаемой лишь по
			// испорченным признакам через месяц.
			s.outsideRegion++
		}
	}

	for trID, stops := range s.vehicles {
		// Порядок (TimeBegin, ActionID) — полный и детерминированный: в
		// 534 группах у одного транспортного средства плановые времена
		// совпадают, и без второго ключа выборка строк зависела бы от
		// порядка в файле.
		slices.SortFunc(stops, func(a, b Stop) int {
			if c := a.TimeBegin.Compare(b.TimeBegin); c != 0 {
				return c
			}
			return cmpInt64(a.ActionID, b.ActionID)
		})
		s.vehicles[trID] = stops
		s.order = append(s.order, trID)
	}
	slices.Sort(s.order)
	return s, nil
}

// columns — индексы обязательных колонок в записи CSV.
type columns struct {
	actionID   int
	trID       int
	timeBegin  int
	timeFact   int
	orderDate  int
	manualFill int
	geom       int
	address    int
}

// indexColumns сопоставляет имена колонок их позициям. Отсутствие
// обязательной колонки — ошибка формата, а не повод молча пропустить файл:
// иначе расписание загрузится пустым и следующая ошибка будет выглядеть как
// «машина не найдена».
func indexColumns(header []string) (columns, error) {
	position := make(map[string]int, len(header))
	for i, name := range header {
		position[strings.TrimSpace(name)] = i
	}
	required := []string{colActionID, colTimeBegin, colTRID, colGeom}
	for _, name := range required {
		if _, ok := position[name]; !ok {
			return columns{}, fmt.Errorf("schedule: в заголовке нет обязательной колонки %q", name)
		}
	}
	optional := func(name string) int {
		if i, ok := position[name]; ok {
			return i
		}
		return -1
	}
	return columns{
		actionID:   position[colActionID],
		trID:       position[colTRID],
		timeBegin:  position[colTimeBegin],
		timeFact:   optional(colTimeFact),
		orderDate:  optional(colOrderDate),
		manualFill: optional(colManualFill),
		geom:       position[colGeom],
		address:    optional(colAddress),
	}, nil
}

// field возвращает значение колонки либо пустую строку, если колонки нет.
func (c columns) field(record []string, index int) string {
	if index < 0 || index >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[index])
}

// parseStop разбирает строку расписания. Второй результат равен false для
// полностью пустых строк, третий — ошибкой разбора.
func parseStop(record []string, col columns, row int) (Stop, bool, error) {
	actionRaw := col.field(record, col.actionID)
	trRaw := col.field(record, col.trID)
	timeRaw := col.field(record, col.timeBegin)
	if actionRaw == "" && trRaw == "" && timeRaw == "" {
		return Stop{}, false, nil
	}

	actionID, err := strconv.ParseInt(actionRaw, 10, 64)
	if err != nil {
		return Stop{}, false, fmt.Errorf("schedule: строка %d: неверный %s %q: %w", row, colActionID, actionRaw, err)
	}
	trID, err := strconv.ParseInt(trRaw, 10, 64)
	if err != nil {
		return Stop{}, false, fmt.Errorf("schedule: строка %d: неверный %s %q: %w", row, colTRID, trRaw, err)
	}
	timeBegin, err := ParseTime(timeRaw)
	if err != nil {
		return Stop{}, false, fmt.Errorf("schedule: строка %d: неверный %s %q: %w", row, colTimeBegin, timeRaw, err)
	}

	stop := Stop{
		ActionID:  actionID,
		TRID:      trID,
		TimeBegin: timeBegin,
		OrderDate: col.field(record, col.orderDate),
		Address:   col.field(record, col.address),
	}
	stop.ManualFill = parseBool(col.field(record, col.manualFill))
	if factRaw := col.field(record, col.timeFact); factRaw != "" {
		fact, err := ParseTime(factRaw)
		if err != nil {
			return Stop{}, false, fmt.Errorf("schedule: строка %d: неверный %s %q: %w", row, colTimeFact, factRaw, err)
		}
		stop.TimeFactBegin = fact
		stop.HasFact = true
	}
	if geomRaw := col.field(record, col.geom); geomRaw != "" {
		lon, lat, err := ParsePointWKT(geomRaw)
		if err != nil {
			return Stop{}, false, fmt.Errorf("schedule: строка %d: %w", row, err)
		}
		stop.Lon, stop.Lat = lon, lat
	} else {
		stop.Lon, stop.Lat = 0, 0
	}
	return stop, true, nil
}

// ParseTime разбирает отметку времени в формате выгрузки. Пустая строка даёт
// нулевое время и ошибку: молча трактовать отсутствие времени как epoch нельзя,
// это сдвинуло бы всю временную ось.
func ParseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("пустое значение времени")
	}
	parsed, err := time.Parse(timeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("не разобрано время %q: %w", value, err)
	}
	// Время в выгрузке приходит без указания зоны и относится к московскому
	// времени, как и весь остальной контекст задачи. Приведение к UTC
	// выполняется явно, чтобы при сравнении с временем приёма пакетов не
	// возникало скрытого смещения в три часа.
	return parsed.In(time.UTC), nil
}

// parseBool разбирает булево поле выгрузки.
func parseBool(value string) bool {
	switch strings.ToLower(value) {
	case "true", "1", "t", "yes":
		return true
	default:
		return false
	}
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Vehicles возвращает номера транспортных средств по возрастанию.
func (s *Schedule) Vehicles() []int64 {
	return slices.Clone(s.order)
}

// Stops возвращает все остановки транспортного средства в порядке
// (TimeBegin, ActionID). Возвращаемый слайс принадлежит расписанию;
// изменять его нельзя.
func (s *Schedule) Stops(trID int64) []Stop {
	return s.vehicles[trID]
}

// StopByID возвращает остановку по идентификатору строки расписания.
func (s *Schedule) StopByID(actionID int64) (Stop, bool) {
	stop, ok := s.byID[actionID]
	return stop, ok
}

// StopsCount возвращает число прочитанных остановок.
func (s *Schedule) StopsCount() int { return s.stops }

// RowsCount возвращает число строк в источнике, включая пустые и те, что
// не удалось превратить в остановку.
func (s *Schedule) RowsCount() int { return s.rows }

// WithoutGeometryCount возвращает число остановок без пригодной геометрии.
func (s *Schedule) WithoutGeometryCount() int { return s.withoutGeometry }

// ManualFilledCount возвращает число строк с ручным заполнением.
func (s *Schedule) ManualFilledCount() int { return s.manualFilled }

// OutsideRegionCount возвращает число остановок с координатами вне региона
// Schedule. Ненулевое значение означает либо перестановку координат в
// геометрии, либо выгрузку по другому региону.
func (s *Schedule) OutsideRegionCount() int { return s.outsideRegion }

// Region возвращает регион, относительно которого проверялись координаты.
func (s *Schedule) Region() Region { return s.region }

// SetRegion задаёт регион проверки координат.
func (s *Schedule) SetRegion(region Region) { s.region = region }

// seekFirstStop возвращает индекс первой остановки с TimeBegin не раньше t.
//
// Используется sort.Search, а не slices.BinarySearchFunc, потому что
// плановые времена совпадают у 534 групп строк: двоичный поиск при равных
// элементах возвращает не обязательно первый из них, и граница окна зависела
// бы от длины среза.
func seekFirstStop(stops []Stop, t time.Time) int {
	return sort.Search(len(stops), func(i int) bool {
		return !stops[i].TimeBegin.Before(t)
	})
}

// Window возвращает остановки транспортного средства с TimeBegin в полуинтервале
// [from, to).
func (s *Schedule) Window(trID int64, from, to time.Time) []Stop {
	stops := s.vehicles[trID]
	start := seekFirstStop(stops, from)
	end := seekFirstStop(stops, to)
	if start >= end {
		return nil
	}
	return slices.Clone(stops[start:end])
}

// NextStopAfter возвращает первую остановку транспортного средства с
// TimeBegin не раньше t. Это ближайшая цель впереди по расписанию.
func (s *Schedule) NextStopAfter(trID int64, t time.Time) (Stop, bool) {
	stops := s.vehicles[trID]
	idx := seekFirstStop(stops, t)
	if idx >= len(stops) {
		return Stop{}, false
	}
	return stops[idx], true
}

// TargetStop выбирает целевую остановку по правилу, проверенному на
// размеченной выборке: ближайшая по времени остановка в окне (T+MinHorizon,
// T+MaxHorizon]. Правило подтверждено на всех 151 размеченных точках.
//
// Возвращаемое число кандидатов показывает неоднозначность выбора: в 141 из
// 151 случаев в окне было больше одной строки, и это норма, а не дефект.
func (s *Schedule) TargetStop(trID int64, t time.Time) (Target, bool) {
	return TargetStop(s.Window(trID, t.Add(MinHorizon), t.Add(MaxHorizon).Add(time.Nanosecond)), t)
}

// TargetStop выбирает цель из заранее полученного окна остановок. Вынесено
// отдельно, чтобы правило выбора можно было применять и к окну, построенному
// иначе, не дублируя логику.
//
// Входной слайс не изменяется: вызывающий код вправе передать результат
// Schedule.Window и продолжить им пользоваться.
func TargetStop(candidates []Stop, t time.Time) (Target, bool) {
	from := t.Add(MinHorizon)
	to := t.Add(MaxHorizon)
	// Окно выбора: исключительно слева, включительно справа. Именно так оно
	// построено в размеченной выборке, и сдвиг границы на секунду меняет
	// целевую остановку.
	selected := make([]Stop, 0, len(candidates))
	for _, stop := range candidates {
		if stop.TimeBegin.After(from) && !stop.TimeBegin.After(to) {
			selected = append(selected, stop)
		}
	}
	if len(selected) == 0 {
		return Target{}, false
	}
	slices.SortFunc(selected, func(a, b Stop) int {
		if c := a.TimeBegin.Compare(b.TimeBegin); c != 0 {
			return c
		}
		return cmpInt64(a.ActionID, b.ActionID)
	})
	// Неоднозначность — это совпадение времени, а не размер окна. Считаются
	// только строки на минимальном времени: более поздние кандидаты не мешают
	// выбрать цель и не делают правило неоднозначным.
	tied := 1
	for _, stop := range selected[1:] {
		if stop.TimeBegin.Equal(selected[0].TimeBegin) {
			tied++
		} else {
			break
		}
	}
	return Target{Stop: selected[0], Candidates: len(selected), Tied: tied}, true
}

// RouteBetween возвращает остановки маршрута в порядке следования на
// полуинтервале [from, to). Остановки без геометрии пропускаются.
func (s *Schedule) RouteBetween(trID int64, from, to time.Time) []Stop {
	window := s.Window(trID, from, to)
	out := make([]Stop, 0, len(window))
	for _, stop := range window {
		if stop.HasGeometry() {
			out = append(out, stop)
		}
	}
	return out
}

// HasGeometry сообщает, есть ли у остановки пригодные координаты.
func (s Stop) HasGeometry() bool {
	// 0,0 — не точка над Москвой, а отсутствие данных: в выгрузке такая
	// пара не встречается, и трактовка её как реальной координаты уводила
	// бы привязку к маршруту на полпути к Гринвичу.
	return s.Lon != 0 || s.Lat != 0
}

// Delay возвращает задержку фактического прибытия относительно планового.
// Второй результат равен false, если фактическое время не заполнено.
func (s Stop) Delay() (time.Duration, bool) {
	if !s.HasFact {
		return 0, false
	}
	return s.TimeFactBegin.Sub(s.TimeBegin), true
}
