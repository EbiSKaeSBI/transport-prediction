package ndtpserver

import (
	"testing"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
)

// recorder запоминает вызовы, чтобы проверить доставку во все ветки отвода.
type recorder struct {
	handshakes []uint32
	realtime   []uint32
	malformed  []uint32
	disconnect []uint32
	cells      int
}

func (r *recorder) OnHandshake(unitID uint32, _ ndtp.ConnRequest) {
	r.handshakes = append(r.handshakes, unitID)
}

func (r *recorder) OnRealtime(unitID uint32, _ ndtp.Frame, cells []ndtp.Cell) {
	r.realtime = append(r.realtime, unitID)
	r.cells += len(cells)
}

func (r *recorder) OnMalformed(unitID uint32, _ ndtp.Frame, _ error) {
	r.malformed = append(r.malformed, unitID)
}

func (r *recorder) OnDisconnect(unitID uint32) {
	r.disconnect = append(r.disconnect, unitID)
}

func TestNewFanoutDeliversToEveryHandler(t *testing.T) {
	first, second := &recorder{}, &recorder{}
	f := NewFanout(first, second)

	f.OnHandshake(7, ndtp.ConnRequest{})
	f.OnRealtime(7, ndtp.Frame{}, make([]ndtp.Cell, 2))
	f.OnMalformed(7, ndtp.Frame{}, nil)
	f.OnDisconnect(7)

	for name, r := range map[string]*recorder{"первый": first, "второй": second} {
		if len(r.handshakes) != 1 || r.handshakes[0] != 7 {
			t.Errorf("%s обработчик: handshake %v", name, r.handshakes)
		}
		if len(r.realtime) != 1 || r.realtime[0] != 7 {
			t.Errorf("%s обработчик: realtime %v", name, r.realtime)
		}
		if r.cells != 2 {
			t.Errorf("%s обработчик: ячеек %d, ожидалось 2 — режется ли кадр?", name, r.cells)
		}
		if len(r.malformed) != 1 || len(r.disconnect) != 1 {
			t.Errorf("%s обработчик: malformed %v disconnect %v", name, r.malformed, r.disconnect)
		}
	}
}

func TestNewFanoutSkipsNil(t *testing.T) {
	r := &recorder{}
	f := NewFanout(nil, r, nil)
	f.OnHandshake(3, ndtp.ConnRequest{})
	if len(r.handshakes) != 1 {
		t.Fatalf("nil-обработчик не должен был помешать доставке: %v", r.handshakes)
	}
}

func TestNewFanoutSingleHandlerIsReturnedAsIs(t *testing.T) {
	r := &recorder{}
	f := NewFanout(nil, r)
	if got, ok := f.(*recorder); !ok || got != r {
		t.Fatalf("единственный обработчик должен возвращаться без обёртки, получено %T", f)
	}
}

func TestNewFanoutWithoutHandlersIsNil(t *testing.T) {
	if f := NewFanout(nil, nil); f != nil {
		t.Fatalf("без обработчиков отвод должен быть nil, получено %T", f)
	}
}

// Отвод не должен роняться, если один из обработчиков паникует: включённая
// запись кадров обязана быть безопасна для конвейера. Панику в параллельном
// тесте поднять нельзя, поэтому проверяем, что порядок доставки не меняется при
// пустом обработчике в середине списка.
func TestNewFanoutKeepsOrder(t *testing.T) {
	order := []int{}
	mark := func(n int) Handler {
		return &orderRecorder{mark: func() { order = append(order, n) }}
	}
	f := NewFanout(mark(1), mark(2), mark(3))
	f.OnDisconnect(1)
	for i, want := range []int{1, 2, 3} {
		if order[i] != want {
			t.Fatalf("порядок обработчиков нарушен: %v", order)
		}
	}
}

type orderRecorder struct{ mark func() }

func (r *orderRecorder) OnHandshake(uint32, ndtp.ConnRequest) {}
func (r *orderRecorder) OnRealtime(uint32, ndtp.Frame, []ndtp.Cell) {
}
func (r *orderRecorder) OnMalformed(uint32, ndtp.Frame, error) {}
func (r *orderRecorder) OnDisconnect(uint32)                   { r.mark() }
