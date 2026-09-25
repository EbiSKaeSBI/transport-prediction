// Package mapmatch привязывает телеметрию к маршруту: находит ближайшую точку
// ломаной, положение вдоль маршрута, азимут участка и расстояние до цели по
// маршруту.
//
// Ломаная строится прямолинейными сегментами через координаты остановок
// расписания. Точность этой модели измерена на validate: у 9 из 11 размеченных
// транспортных средств медианное расстояние от телеметрии до ломаной составляет
// 8–32 м, то есть дорога восстанавливается хорошо. Исключение — 130072 с
// медианой 3430 м: у него остановки редкие, и отрезок между соседними точками
// проходит по местности, которой нет на дороге.
//
// Отсюда следствие, важное для всех признаков: нахождение вне маршрута —
// нормальное состояние, а не ошибка. В validate/traffic.csv 12 % точек
// 11 размеченных машин лежат дальше 400 м от ломаной: машина стоит вне смены,
// в депо или едет по другому участку. Принудительно притягивать такие точки к
// маршруту нельзя — это выдумает правдоподобное route_progress и
// distance_to_target для точки, маршруту не принадлежащей. Поэтому Match
// возвращает явный OnRoute вместе с расстоянием OffsetM, а признаки, зависящие
// от положения на маршруте, обязаны помечаться недоступными, а не заполняться
// нулём: ноль — правдоподобное значение прогресса в начале маршрута.
package mapmatch

import (
	"math"
	"sort"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
)

const (
	// DefaultSearchRadiusM — расстояние, ближе которого точка считается
	// лежащей на маршруте. Обоснование: медиана расстояния до ломаной по
	// размеченным машинам 8–32 м, 90-й перцентиль по всем машинам — 96 м,
	// а 88 % точек размеченных машин укладываются в 400 м. Порог 400 м
	// отделяет «едет по своей дороге» от «находится в другом месте».
	DefaultSearchRadiusM = 400.0

	// earthRadiusM — радиус Земли для расчёта длины сегмента.
	earthRadiusM = 6371000.0
)

// vertex — вершина ломаной.
type vertex struct {
	lon, lat float64
	// cumM — расстояние от начала маршрута до вершины.
	cumM float64
	// stopID — идентификатор остановки, соответствующей вершине, 0 если
	// вершина синтетическая.
	stopID int64
	// stopTime — плановое время остановки.
	stopTime time.Time
}

// segment — отрезок ломаной.
type segment struct {
	// from, to — индексы вершин.
	from, to int
	// lengthM — длина отрезка.
	lengthM float64
	// bearingDeg — начальный азимут отрезка, градусы, 0 — север, по часовой.
	bearingDeg float64
}

// Route — ломаная маршрута с накопленной длиной.
type Route struct {
	vertices []vertex
	segments []segment
	// totalM — длина маршрута.
	totalM float64
	// from, to — границы временно́го среза, из которого построен маршрут.
	from, to time.Time
}

// NewRoute строит маршрут по упорядоченным остановкам. Остановки без
// геометрии пропускаются: отрезок без концов не построить.
//
// Остановки должны идти в порядке следования, то есть по времени
// расписания. Порядок по времени — не формальность: на кольцевом маршруте
// одна и та же точка встречается в разных кругах, и привязка без
// ограничения по времени указала бы на соседний круг.
func NewRoute(stops []schedule.Stop) *Route {
	ordered := make([]schedule.Stop, 0, len(stops))
	for _, stop := range stops {
		if stop.HasGeometry() {
			ordered = append(ordered, stop)
		}
	}
	// Порядок по времени, затем по идентификатору: у соседних остановок
	// времена совпадают на конечных, и без второго ключа маршрут зависел бы
	// от порядка во входном срезе.
	sort.SliceStable(ordered, func(i, j int) bool {
		if c := ordered[i].TimeBegin.Compare(ordered[j].TimeBegin); c != 0 {
			return c < 0
		}
		return ordered[i].ActionID < ordered[j].ActionID
	})

	r := &Route{vertices: make([]vertex, 0, len(ordered))}
	for _, stop := range ordered {
		r.vertices = append(r.vertices, vertex{
			lon:      stop.Lon,
			lat:      stop.Lat,
			stopID:   stop.ActionID,
			stopTime: stop.TimeBegin,
		})
		if len(r.vertices) == 1 {
			r.from = stop.TimeBegin
		}
		r.to = stop.TimeBegin
	}
	for i := 0; i+1 < len(r.vertices); i++ {
		length := haversine(r.vertices[i].lat, r.vertices[i].lon,
			r.vertices[i+1].lat, r.vertices[i+1].lon)
		// Присваивание идёт по индексу среза, а не в локальную копию
		// вершины: вершина — структура, и `b := r.vertices[i]` дал бы
		// копию, в которой cumM изменился бы и сразу потерялся. Ошибка
		// молча обнуляла бы весь маршрут.
		r.vertices[i+1].cumM = r.vertices[i].cumM + length
		r.segments = append(r.segments, segment{
			from:    i,
			to:      i + 1,
			lengthM: length,
			bearingDeg: bearing(r.vertices[i].lat, r.vertices[i].lon,
				r.vertices[i+1].lat, r.vertices[i+1].lon),
		})
	}
	if n := len(r.vertices); n > 0 {
		r.totalM = r.vertices[n-1].cumM
	}
	return r
}

// From — начало временно́го среза, из которого построен маршрут.
func (r *Route) From() time.Time { return r.from }

// To — конец временно́го среза.
func (r *Route) To() time.Time { return r.to }

// Len возвращает число остановок в маршруте.
func (r *Route) Len() int { return len(r.vertices) }

// Segments возвращает число отрезков.
func (r *Route) Segments() int { return len(r.segments) }

// TotalM возвращает длину маршрута в метрах.
func (r *Route) TotalM() float64 { return r.totalM }

// StopIDAt возвращает идентификатор остановки по индексу вершины.
func (r *Route) StopIDAt(index int) (int64, bool) {
	if index < 0 || index >= len(r.vertices) {
		return 0, false
	}
	return r.vertices[index].stopID, true
}

// StopTimeAt возвращает плановое время остановки по индексу вершины.
func (r *Route) StopTimeAt(index int) (time.Time, bool) {
	if index < 0 || index < len(r.vertices) {
		return time.Time{}, false
	}
	return r.vertices[index].stopTime, true
}

// Match — результат привязки точки к маршруту.
type Match struct {
	// OnRoute — точка ближе SearchRadius к ломаной. При OnRoute == false
	// поля положения не имеют смысла и не должны попадать в признаки.
	OnRoute bool `json:"on_route"`
	// OffsetM — расстояние от точки до ломаной.
	OffsetM float64 `json:"offset_m"`
	// AlongM — пройденное расстояние от начала маршрута.
	AlongM float64 `json:"along_m"`
	// Progress — доля пройденного пути в [0, 1].
	Progress float64 `json:"progress"`
	// BearingDeg — азимут участка маршрута в точке привязки.
	BearingDeg float64 `json:"bearing_deg"`
	// HeadingErrorDeg — расхождение между азимутом маршрута и курсом
	// транспортного средства, в диапазоне [-180, 180].
	HeadingErrorDeg float64 `json:"heading_error_deg"`
	// HeadingValid — пригоден ли HeadingErrorDeg. Курс не заполняется, когда
	// машина стоит, и тогда расхождение вычислять не из чего.
	HeadingValid bool `json:"heading_valid"`
	// SegmentIndex — индекс отрезка привязки.
	SegmentIndex int `json:"segment_index"`
	// NearestStopIndex — индекс ближайшей по маршруту остановки.
	NearestStopIndex int `json:"nearest_stop_index"`
	// StopsRemaining — остановок впереди до конца маршрута.
	StopsRemaining int `json:"stops_remaining"`
}

// MatchPoint привязывает точку к маршруту. Курс не участвует в поиске
// ближайшего сегмента: у остановки машина стоит, курс при этом произвольный,
// и участие курса в геометрии уводило бы привязку с дороги.
func (r *Route) MatchPoint(lon, lat, headingDeg float64, headingKnown bool, radiusM float64) Match {
	match := Match{OffsetM: math.Inf(1), SegmentIndex: -1, NearestStopIndex: -1}
	if len(r.segments) == 0 {
		return match
	}

	bestDist := math.Inf(1)
	bestSeg := -1
	bestT := 0.0
	for i := range r.segments {
		dist, t := segmentProjection(lat, lon, r.segments[i], r.vertices)
		if dist < bestDist {
			bestDist, bestSeg, bestT = dist, i, t
		}
	}
	if bestSeg < 0 {
		return match
	}

	seg := r.segments[bestSeg]
	from := r.vertices[seg.from]
	along := from.cumM + seg.lengthM*bestT

	match.OffsetM = bestDist
	match.AlongM = along
	match.BearingDeg = seg.bearingDeg
	match.SegmentIndex = bestSeg
	if r.totalM > 0 {
		match.Progress = clamp(along/r.totalM, 0, 1)
	}
	if radiusM <= 0 {
		radiusM = DefaultSearchRadiusM
	}
	match.OnRoute = bestDist <= radiusM

	// Ближайшая по маршруту остановка, а не ближайшая по прямой: в точке
	// привязки это вершина, между которой машина находится.
	if t := bestT; t < 0.5 {
		match.NearestStopIndex = seg.from
	} else {
		match.NearestStopIndex = seg.to
	}
	match.StopsRemaining = len(r.vertices) - 1 - match.NearestStopIndex

	if headingKnown {
		match.HeadingErrorDeg = AngleDiffDeg(seg.bearingDeg, headingDeg)
		match.HeadingValid = true
	}
	return match
}

// segmentProjection возвращает расстояние от точки до отрезка и долю
// проекции вдоль него в [0, 1].
func segmentProjection(lat, lon float64, seg segment, vertices []vertex) (dist, t float64) {
	a, b := vertices[seg.from], vertices[seg.to]
	// Работа в равновершинной проекции: на масштабе города погрешность
	// такого перехода — единицы метров, что несопоставимо с радиусом
	// привязки в сотни метров.
	scale := 111320 * math.Cos((a.lat+b.lat)*math.Pi/360)
	mx, my := scale, 110540.0

	dx := (b.lon - a.lon) * mx
	dy := (b.lat - a.lat) * my
	px := (lon - a.lon) * mx
	py := (lat - a.lat) * my

	lengthSq := dx*dx + dy*dy
	if lengthSq <= 0 {
		// Вырожденный отрезок: обе вершины совпадают, точек проекции нет
		// и единственная осмысленная величина — расстояние до вершины.
		return haversine(lat, lon, a.lat, a.lon), 0
	}
	t = clamp((px*dx+py*dy)/lengthSq, 0, 1)
	return math.Hypot(px-t*dx, py-t*dy), t
}

// AlongToStop возвращает расстояние по маршруту от начала до указанной
// остановки. Это и есть distance_to_target_m с точностью до положения
// телеметрии.
func (r *Route) AlongToStop(actionID int64) (float64, bool) {
	for i := range r.vertices {
		if r.vertices[i].stopID == actionID {
			return r.vertices[i].cumM, true
		}
	}
	return 0, false
}

// PlannedTimeAtAlong возвращает плановое время остановки, ближайшей к
// пройденному расстоянию alongM, с линейной интерполяцией по времени между
// соседними остановками.
//
// Это опорная величина для признака «отклонение от расписания»: зная, в
// какой точке маршрута машина находится, можно сравнить T с плановым временем
// в этой точке. Вне маршрута возвращается ближайший конец, чтобы за пределами
// смены не появлялось бесконечных остатков.
func (r *Route) PlannedTimeAtAlong(alongM float64) (time.Time, bool) {
	if len(r.vertices) == 0 {
		return time.Time{}, false
	}
	if len(r.vertices) == 1 || alongM <= 0 {
		return r.vertices[0].stopTime, true
	}
	for i := 0; i+1 < len(r.vertices); i++ {
		a, b := r.vertices[i], r.vertices[i+1]
		if alongM > b.cumM {
			continue
		}
		span := b.cumM - a.cumM
		if span <= 0 {
			// Вырожденный сегмент: две остановки в одной точке. Время
			// между ними всё равно известно, поэтому отдаём конец сегмента.
			return b.stopTime, true
		}
		frac := (alongM - a.cumM) / span
		if frac > 1 {
			frac = 1
		}
		return a.stopTime.Add(time.Duration(frac * float64(b.stopTime.Sub(a.stopTime)))), true
	}
	return r.vertices[len(r.vertices)-1].stopTime, true
}

// StopIndexAtAlong возвращает индекс вершины, к которой относится
// накопленное расстояние alongM: последняя вершина с cumM <= alongM.
func (r *Route) StopIndexAtAlong(alongM float64) (int, bool) {
	if len(r.vertices) == 0 {
		return 0, false
	}
	index := 0
	for i := range r.vertices {
		if r.vertices[i].cumM <= alongM {
			index = i
		}
	}
	return index, true
}

// DistanceToStop возвращает расстояние по маршруту от положения, найденного
// функцией MatchPoint, до остановки actionID.
//
// Знак зависит от направления движения и намеренно возвращается вместе с
// величиной: цель позади машины (после разворота на конечной) и цель впереди —
// это разные ситуации, и сведение их к модулю расстояния потеряло бы
// существенный признак.
func (r *Route) DistanceToStop(match Match, actionID int64) (metres float64, ahead bool, ok bool) {
	target, found := r.AlongToStop(actionID)
	if !found {
		return 0, false, false
	}
	delta := target - match.AlongM
	return math.Abs(delta), delta >= 0, true
}

// StopsInRange возвращает остановки маршрута, чьё накопленное расстояние
// попадает в (from, to].
func (r *Route) StopsInRange(from, to float64) []schedule.Stop {
	out := make([]schedule.Stop, 0)
	for i := range r.vertices {
		if r.vertices[i].cumM > from && r.vertices[i].cumM <= to {
			out = append(out, schedule.Stop{
				ActionID:  r.vertices[i].stopID,
				TimeBegin: r.vertices[i].stopTime,
				Lon:       r.vertices[i].lon,
				Lat:       r.vertices[i].lat,
			})
		}
	}
	return out
}

// AngleDiffDeg возвращает разность углов a - b, приведённую к диапазону
// [-180, 180]. Без приведения расхождение курса давало бы скачок с 179 на
// -179 градусов, и среднее по нему было бы бессмысленным.
func AngleDiffDeg(a, b float64) float64 {
	diff := math.Mod(a-b, 360)
	if diff > 180 {
		diff -= 360
	}
	if diff < -180 {
		diff += 360
	}
	return diff
}

// haversine возвращает расстояние между двумя точками по большому кругу,
// метры.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(a)))
}

// bearing возвращает начальный азимут от a к b в градусах, 0 — север, по
// часовой стрелке.
func bearing(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	y := math.Sin((lon2-lon1)*rad) * math.Cos(lat2*rad)
	x := math.Cos(lat1*rad)*math.Sin(lat2*rad) -
		math.Sin(lat1*rad)*math.Cos(lat2*rad)*math.Cos((lon2-lon1)*rad)
	deg := math.Atan2(y, x) / rad
	if deg < 0 {
		deg += 360
	}
	return deg
}

func clamp(v, low, high float64) float64 {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
