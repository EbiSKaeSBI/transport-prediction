package statestore

import (
	"slices"
	"time"
)

// ring — кольцевой буфер последних точек одного устройства.
//
// Хранится в порядке поступления, а не по времени события: телеметрия может
// приходить с перестановками и опозданием, и сортировка при записи означала
// бы сдвиг всего буфера на каждой точке. Порядок по времени события
// восстанавливается при чтении, где стоимость сортировки несопоставима с её
// пользой.
type ring struct {
	// points — заранее выделенный буфер слотов, изначально заполненный нулями.
	points []Point
	// head — индекс слота, который получит следующую точку.
	head int
	// count — число занятых слотов.
	count int
}

// newRing создаёт пустой буфер на capacity точек. Нулевая или отрицательная
// ёмкость приводится к DefaultCapacity, потому что буфер нулевой длины
// означал бы потерю истории без видимой ошибки.
func newRing(capacity int) *ring {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &ring{points: make([]Point, capacity)}
}

// push добавляет точку, вытесняя самую старую при заполнении. Возвращает
// true, если вытеснение произошло, то есть глубины истории уже не хватает
// для расчёта признаков.
func (r *ring) push(p Point) bool {
	r.points[r.head] = p
	r.head = (r.head + 1) % len(r.points)
	if r.count < len(r.points) {
		r.count++
		return false
	}
	return true
}

// len возвращает число накопленных точек.
func (r *ring) len() int { return r.count }

// last возвращает последнюю поступившую точку.
func (r *ring) last() (Point, bool) {
	if r.count == 0 {
		return Point{}, false
	}
	return r.points[(r.head-1+len(r.points))%len(r.points)], true
}

// all возвращает все точки в порядке поступления.
func (r *ring) all() []Point {
	if r.count == 0 {
		return nil
	}
	out := make([]Point, 0, r.count)
	for i := range r.count {
		out = append(out, r.points[(r.head-r.count+i+len(r.points))%len(r.points)])
	}
	return out
}

// since возвращает точки с временем приёма не позже limit в порядке
// поступления.
//
// Граница задаётся временем приёма, а не временем события, потому что отвечает
// на вопрос «что наблюдатель уже знал в момент limit». Ограничение по
// EventTime отвечало бы на другой вопрос и незаметно подменяло бы наблюдаемое
// состояние, из-за чего offline-replay разошёлся бы с online.
func (r *ring) since(limit time.Time) []Point {
	points := r.all()
	if points == nil {
		return nil
	}
	out := points[:0:0]
	for _, p := range points {
		if !p.ReceiveTime.After(limit) {
			out = append(out, p)
		}
	}
	return out
}

// byEventTime возвращает точки в порядке поступления, отсортированные по
// времени события. Точки с одинаковым временем сохраняют порядок поступления,
// что делает результат воспроизводимым между прогонами.
func (r *ring) byEventTime() []Point {
	points := r.all()
	if points == nil {
		return nil
	}
	slices.SortStableFunc(points, func(a, b Point) int {
		return a.EventTime.Compare(b.EventTime)
	})
	return points
}
