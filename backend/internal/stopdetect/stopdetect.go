// Package stopdetect определяет прибытия на остановки и простои по окну
// телеметрии.
//
// Ключевой вывод из данных, определивший конструкцию пакета: остановка не
// определяется временем. В validate/traffic.csv 40.8 % пакетов имеют скорость
// ниже 0.5 км/ч, но 68.9 % неподвижных серий короче 10 секунд — это светофоры.
// Порог только по времени пометил бы остановкой две трети светофоров и
// размёг бы признак простоя, который как раз и говорит о задержке на
// остановке. Поэтому остановкой считается неподвижность, подтверждённая
// близостью к остановке расписания.
package stopdetect

import (
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

// Значения по умолчанию, откалиброванные на validate/traffic.csv.
const (
	// DefaultSpeedThresholdKmh — граница неподвижности. В 40.8 % пакетов
	// скорость ниже неё; при 1.0 км/ч доля падает примерно вдвое и часть
	// реальных простоев на светофорах уходит в движение.
	DefaultSpeedThresholdKmh = 0.5

	// DefaultMinDwell — минимальная длительность простоя, за которой
	// неподвижность считается прибытием на остановку. Серии короче 10 с
	// составляют 68.9 % всех неподвижных серий и почти целиком приходятся
	// на светофоры. Значение 15 с оставляет после порога около 15 % серий,
	// из которых остаются настоящие стоянки.
	DefaultMinDwell = 15 * time.Second

	// DefaultStopRadiusM — радиус, в котором неподвижность относится к
	// остановке расписания. Медианное расстояние между соседними
	// остановками маршрута — 347 м, поэтому 150 м не накрывает соседнюю
	// остановку, но с запасом перекрывает шум GPS.
	DefaultStopRadiusM = 150.0
)

// earthRadiusM — радиус Земли для расчёта расстояния по большому кругу.
const earthRadiusM = 6371000.0

// Visit — одно пребывание на остановке.
type Visit struct {
	// StopID — идентификатор строки расписания, если остановка
	// опознана. При Matched == false остаётся нулём.
	StopID int64 `json:"stop_id"`
	// Matched — найдена ли остановка расписания в пределах радиуса.
	Matched bool `json:"matched"`
	// MatchDistM — расстояние от центра простоя до опознанной остановки.
	MatchDistM float64 `json:"match_dist_m"`
	// Arrive — время первого неподвижного пакета. Приходится по времени
	// события: время приёма смазано задержкой канала, которая в этих
	// данных достигает десятков секунд.
	Arrive time.Time `json:"arrive"`
	// Depart — время первого пакета в движении после простоя. Нулевое,
	// если простой ещё не завершён.
	Depart time.Time `json:"depart"`
	// DwellS — длительность простоя в секундах.
	DwellS float64 `json:"dwell_s"`
	// Open — простой продолжается на момент расчёта.
	Open bool `json:"open"`
	// Truncated — простой не помещается в окно и его длительность
	// занижена. Признак обязателен: молча обрезанный простой выглядел бы
	// как короткий, и модель сочла бы машину у остановки, когда она
	// простояла там полчаса.
	Truncated bool `json:"truncated"`
	// Points — сколько пакетов подтвердили простой.
	Points int `json:"points"`
	// Latitude, Longitude — центр простоя.
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
}

// Result — сводка по окну.
type Result struct {
	// Current — текущий простой, если машина стоит. IsDwelling == false,
	// когда машина в движении.
	Current Visit `json:"current"`
	// IsDwelling — стоит ли машина прямо сейчас.
	IsDwelling bool `json:"is_dwelling"`
	// Last — последний завершённый простой.
	Last Visit `json:"last"`
	// HasLast — был ли завершённый простой в окне.
	HasLast bool `json:"has_last"`
	// StopsServed — число завершённых простоев, опознанных как остановки
	// расписания.
	StopsServed int `json:"stops_served"`
	// MovingSince — время начала текущего движения. Нулевое, если машина
	// стоит.
	MovingSince time.Time `json:"moving_since"`
	// UsedPoints — точек в окне, на которых что-либо считано. Точки без
	// достоверных координат пропущены.
	UsedPoints int `json:"used_points"`
	// SkippedPoints — точек, отброшенных из-за непригодных координат.
	SkippedPoints int `json:"skipped_points"`
	// Visits — все простые окна в хронологическом порядке, включая те, что
	// не дотянули до минимальной длительности. Нужен слою признаков: по
	// одному лишь Last нельзя посчитать накопленное отставание, которое
	// определяется по последовательности опознанных остановок.
	Visits []Visit `json:"visits"`
}

// Option настраивает Detector.
type Option func(*Detector)

// WithSpeedThreshold задаёт границу неподвижности в км/ч.
func WithSpeedThreshold(kmh float64) Option {
	return func(d *Detector) { d.speedThreshold = kmh }
}

// WithMinDwell задаёт минимальную длительность простоя.
func WithMinDwell(dwell time.Duration) Option {
	return func(d *Detector) { d.minDwell = dwell }
}

// WithStopRadius задаёт радиус опознания остановки в метрах.
func WithStopRadius(m float64) Option {
	return func(d *Detector) { d.radiusM = m }
}

// WithLogger задаёт логгер.
func WithLogger(logger *slog.Logger) Option {
	return func(d *Detector) { d.logger = logger }
}

// Detector считает простои по окну телеметрии.
type Detector struct {
	speedThreshold float64
	minDwell       time.Duration
	radiusM        float64
	logger         *slog.Logger
}

// New создаёт Detector.
func New(opts ...Option) *Detector {
	d := &Detector{
		speedThreshold: DefaultSpeedThresholdKmh,
		minDwell:       DefaultMinDwell,
		radiusM:        DefaultStopRadiusM,
		logger:         slog.Default(),
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.speedThreshold <= 0 {
		d.speedThreshold = DefaultSpeedThresholdKmh
	}
	if d.minDwell <= 0 {
		d.minDwell = DefaultMinDwell
	}
	if d.radiusM <= 0 {
		d.radiusM = DefaultStopRadiusM
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	return d
}

// Detect разбирает окно телеметрии и возвращает простои.
//
// points передаётся в порядке времени события; порядок приёма роли не играет,
// потому что dwell считается по времени события, а задержка канала в этих
// данных достигает десятков секунд и сделала бы границы простоя
// невоспроизводимыми.
//
// stops — остановки маршрута рядом с окном; они нужны, чтобы отличить
// остановку от светофора. Порядок не важен, внутри будет отсортирован.
//
// Функция чистая: результат зависит только от аргументов. Это и есть условие
// совпадения offline-replay с online — состояние детектора не переносится
// между вызовами.
func (d *Detector) Detect(points []statestore.Point, stops []schedule.Stop) Result {
	usable := make([]statestore.Point, 0, len(points))
	result := Result{}
	for _, p := range points {
		// Без достоверных координат простой не к чему привязать, а
		// скорость без координат не подтверждает ничего: Nav00 может
		// отсутствовать в пакете, и это не движение.
		if !p.LocationValid {
			result.SkippedPoints++
			continue
		}
		usable = append(usable, p)
	}
	result.UsedPoints = len(usable)
	if len(usable) == 0 {
		return result
	}

	visits := d.segments(usable)
	ordered := stopsWithGeometry(stops)
	for i := range visits {
		d.match(&visits[i], ordered)
	}
	result.Visits = visits

	for i := range visits {
		visit := &visits[i]
		if visit.Open {
			result.Current = *visit
			result.IsDwelling = true
			continue
		}
		// Считаются только те завершённые простои, которые опознаны как
		// остановки расписания. Долгий светофор — это простой без
		// остановки, и включать его в счётчик обслуженных остановок значило
		// бы завышать их число на каждом загруженном перекрёстке.
		if visit.Matched {
			result.StopsServed++
		}
	}
	if last := lastCompleted(visits); last != nil {
		result.Last = *last
		result.HasLast = true
	}
	if !result.IsDwelling {
		result.MovingSince = d.movingSince(usable, visits)
	}
	if d.logger != nil {
		d.logger.Debug("простои посчитаны",
			"used", result.UsedPoints,
			"skipped", result.SkippedPoints,
			"served", result.StopsServed,
			"dwelling", result.IsDwelling)
	}
	return result
}

// lastCompleted возвращает последний завершённый простой.
func lastCompleted(visits []Visit) *Visit {
	for i := len(visits) - 1; i >= 0; i-- {
		if !visits[i].Open {
			return &visits[i]
		}
	}
	return nil
}

// segments разбивает точки на неподвижные серии и превращает их в Visit.
func (d *Detector) segments(points []statestore.Point) []Visit {
	// Первая классификация: неподвижна ли точка.
	stationary := make([]bool, len(points))
	for i, p := range points {
		stationary[i] = p.SpeedKmh < d.speedThreshold
	}

	var visits []Visit
	i := 0
	for i < len(points) {
		if !stationary[i] {
			i++
			continue
		}
		j := i
		for j+1 < len(points) && stationary[j+1] {
			j++
		}
		// Серия [i, j] неподвижна. Начало простоя — время первого
		// неподвижного пакета; конец — время первого пакета в движении
		// после серии, потому что именно в нём машина трогается.
		arrive := points[i].EventTime
		open := j == len(points)-1
		depart := time.Time{}
		if !open {
			depart = points[j+1].EventTime
		}
		visit := Visit{
			Arrive:    arrive,
			Depart:    depart,
			Open:      open,
			Points:    j - i + 1,
			Latitude:  centroidLat(points[i : j+1]),
			Longitude: centroidLon(points[i : j+1]),
		}
		if open {
			visit.DwellS = points[j].EventTime.Sub(arrive).Seconds()
		} else {
			visit.DwellS = depart.Sub(arrive).Seconds()
		}
		// Короткая неподвижность — светофор, а не остановка.
		if time.Duration(visit.DwellS*float64(time.Second)) >= d.minDwell {
			visits = append(visits, visit)
		}
		i = j + 1
	}

	// Обрезка окна. Если простой упирается в правую границу окна, его
	// длительность занижена, и это обязано быть видно.
	if n := len(visits); n > 0 && visits[n-1].Open && len(points) > 0 {
		visits[n-1].Truncated = true
	}
	return visits
}

// match сопоставляет простой с ближайшей остановкой маршрута.
func (d *Detector) match(visit *Visit, stops []schedule.Stop) {
	best := math.Inf(1)
	for _, stop := range stops {
		dist := haversine(visit.Latitude, visit.Longitude, stop.Lat, stop.Lon)
		if dist < best {
			best = dist
			visit.StopID = stop.ActionID
		}
	}
	if math.IsInf(best, 1) {
		return
	}
	visit.MatchDistM = best
	visit.Matched = best <= d.radiusM
}

// stopsWithGeometry возвращает остановки с пригодными координатами, отсортированные
// по идентификатору: порядок во входных данных влиял бы на выбор при равных
// расстояниях, а расстояния до десятков метров различаются на метры.
func stopsWithGeometry(stops []schedule.Stop) []schedule.Stop {
	out := make([]schedule.Stop, 0, len(stops))
	for _, stop := range stops {
		if stop.HasGeometry() {
			out = append(out, stop)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ActionID < out[j].ActionID })
	return out
}

// movingSince возвращает время, с которого машина движется без остановки.
//
// Это отправление последнего простоя: первый пакет в движении после него и
// есть начало текущего движения. Требовать точку строго после отправления
// было бы ошибкой — тогда на последней точке окна машина, только что
// тронувшаяся, выглядела бы как стоящая.
func (d *Detector) movingSince(points []statestore.Point, visits []Visit) time.Time {
	if len(points) == 0 {
		return time.Time{}
	}
	if len(visits) == 0 {
		// Простоев не было вовсе: машина движется с начала окна.
		return points[0].EventTime
	}
	last := visits[len(visits)-1]
	if last.Open {
		// Последний простой открыт, значит машина стоит. Этот путь
		// недостижим: вызывается только при !IsDwelling.
		return time.Time{}
	}
	return last.Depart
}

func centroidLat(points []statestore.Point) float64 {
	if len(points) == 0 {
		return 0
	}
	// Среднее по широте достаточно: окно в 15 минут не даёт заметного
	// искажения на масштабе города, а тригонометрическое усреднение
	// потребовало бы разворачивать векторы ради погрешности в сантиметры.
	var sum float64
	for _, p := range points {
		sum += p.Latitude
	}
	return sum / float64(len(points))
}

func centroidLon(points []statestore.Point) float64 {
	if len(points) == 0 {
		return 0
	}
	var sum float64
	for _, p := range points {
		sum += p.Longitude
	}
	return sum / float64(len(points))
}

// haversine возвращает расстояние между двумя точками по большому кругу,
// метры. Формула выбрана из-за устойчивости на малых расстояниях, где
// упрощённая плоская метрика на широте 56° даёт ошибку в десятки метров —
// сопоставимо с радиусом опознания остановки.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(a)))
}
