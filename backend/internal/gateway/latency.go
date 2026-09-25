package gateway

import (
	"math"
	"sort"
	"sync"
	"time"
)

// DefaultLatencyWindow — сколько замеров удерживаем. 1024 замера при
// тике 15 с и трёх прогнозах в секунду покрывает примерно шесть минут, чего
// хватает, чтобы увидеть и внезапную деградацию, и разовый выброс, не
// размазывая его по часовому окну, где он уже не виден.
const DefaultLatencyWindow = 1024

// Latency — окно замеров с квантилями.
//
// Хранится кольцо фиксированного размера, а не растущий слайс: замеры идут
// постоянно, и неограниченный накопленный список рано или поздно съел бы
// память процесса, который обязан работать месяцами без перезапуска.
// Растущий слайс к тому же заставлял бы сортировать всё окно на каждый запрос
// /metrics.
type Latency struct {
	// window — кольцо замеров.
	window []float64
	// next — куда писать следующий замер.
	next int
	// filled — сколько слотов занято.
	filled int

	// count — всего замеров за всё время. Нужен, чтобы по переполненному
	// окну нельзя было принять квантили за точные.
	count uint64
	// broken — встретился замер, который нельзя было учесть. Живёт под тем
	// же мьютексом, что и кольцо: отдельный тип ради одного флага стоил бы
	// дороже, чем один лишний bool.
	broken bool
	mu     sync.Mutex
}

// NewLatency создаёт окно. Неположительный размер берёт DefaultLatencyWindow.
func NewLatency(size int) *Latency {
	if size <= 0 {
		size = DefaultLatencyWindow
	}
	return &Latency{window: make([]float64, size)}
}

// Observe добавляет замер в секундах. Отрицательные значения и NaN
// отбрасываются: такой замер говорит о баге в измерении, а не о медленном
// ответе, и попадание в квантили исказило бы их для всех.
func (l *Latency) Observe(seconds float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
	if seconds < 0 || math.IsNaN(seconds) {
		l.broken = true
		return
	}
	l.window[l.next] = seconds
	l.next = (l.next + 1) % len(l.window)
	if l.filled < len(l.window) {
		l.filled++
	}
}

// ObserveDuration добавляет замер из time.Duration.
func (l *Latency) ObserveDuration(d time.Duration) {
	l.Observe(d.Seconds())
}

// Quantiles — сводка по окну.
type Quantiles struct {
	// Count — сколько замеров попало в окно.
	Count int `json:"count"`
	// Total — сколько замеров прошло за всё время жизни окна. Больше Count,
	// когда окно переполнено.
	Total uint64 `json:"total"`
	// P50, P95, P99 — квантили в секундах по заполненной части окна.
	P50 float64 `json:"p50_s"`
	P95 float64 `json:"p95_s"`
	P99 float64 `json:"p99_s"`
	// Max — максимум по окну.
	Max float64 `json:"max_s"`
	// Broken — встретился ли замер, который нельзя было учесть.
	Broken bool `json:"broken"`
}

// Snapshot считает квантили. Сортируется копия: сама перестановка окна
// испортила бы порядок замеров, а следующая выборка считалась бы по мусору.
func (l *Latency) Snapshot() Quantiles {
	l.mu.Lock()
	values := make([]float64, l.filled)
	copy(values, l.window[:l.filled])
	q := Quantiles{Count: len(values), Total: l.count, Broken: l.broken}
	l.mu.Unlock()

	if len(values) == 0 {
		return q
	}
	sort.Float64s(values)
	q.P50 = quantile(values, 0.50)
	q.P95 = quantile(values, 0.95)
	q.P99 = quantile(values, 0.99)
	q.Max = values[len(values)-1]
	return q
}

// quantile возвращает квантиль уже отсортированного среза методом ближайшего
// ранга. Интерполяция дала бы дробные значения между соседними замерами,
// которых в измерении не существует: замер — это то, что реально наблюдали.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
