package latency

import (
	"math"
	"testing"
)

func TestWindowQuantiles(t *testing.T) {
	l := New(4)
	for _, v := range []float64{1, 2, 3, 4, 5, 6, 7, 8} {
		l.Observe(v)
	}
	q := l.Snapshot()
	if q.Count != 4 {
		t.Errorf("в окне %d замеров, ожидалось 4", q.Count)
	}
	if q.Total != 8 {
		t.Errorf("всего замеров %d, ожидалось 8", q.Total)
	}
	// Окно переполнено и хранит последние четыре: 5, 6, 7, 8.
	if q.P50 != 6 || q.P99 != 8 || q.Max != 8 {
		t.Errorf("квантили по окну: p50=%g p99=%g max=%g, ожидалось 6, 8, 8",
			q.P50, q.P99, q.Max)
	}
}

func TestWindowRejectsBrokenMeasurements(t *testing.T) {
	l := New(8)
	l.Observe(1)
	l.Observe(-5)
	l.Observe(nan())
	q := l.Snapshot()
	if q.Count != 1 {
		t.Errorf("в окне %d замеров, ожидался 1: сломанные не считаются", q.Count)
	}
	if !q.Broken {
		t.Error("сломанный замер обязан быть помечен")
	}
	if q.Total != 3 {
		t.Errorf("всего %d, ожидалось 3: сломанные тоже попали в путь замера", q.Total)
	}
}

func TestWindowEmptySnapshot(t *testing.T) {
	q := New(0).Snapshot()
	if q.Count != 0 || q.P50 != 0 {
		t.Errorf("пустое окно дало %+v", q)
	}
}

func nan() float64 { return math.NaN() }
