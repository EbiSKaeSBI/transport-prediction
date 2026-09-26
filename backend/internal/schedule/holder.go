package schedule

import "sync"

// Holder — план-график и привязка, которые можно поменять на лету.
//
// План-график перестал быть неизменяемым: живой прогон перепривязывает его к
// текущему положению машин, и делать это перезапуском конвейера означало бы
// ронять накопленную телеметрию и разрывать поток на минуты. Holder поэтому
// отдаёт текущую пару по ссылке под RWMutex, а менять её можно в любой
// момент.
//
// Одной пары «план + привязка» намеренно: привязка tr_id → unit_id смысла
// вне своего плана не имеет, и промежуточное состояние, где план новый, а
// привязка старая, — это ровно тот случай, где прогноз уехал бы на чужое ТС.
//
// Нулевой Holder — валидный и пустой: Get() вернёт nil, и вызывающий код
// сохранит прежнее поведение «плана нет».
type Holder struct {
	mu      sync.RWMutex
	sched   *Schedule
	binding *Binding
}

// NewHolder — holder сразу с планом, как при старте конвейера.
func NewHolder(sched *Schedule, binding *Binding) *Holder {
	return &Holder{sched: sched, binding: binding}
}

// Get — текущий план-график, nil если его нет.
func (h *Holder) Get() *Schedule {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sched
}

// GetBinding — текущая привязка, nil если её нет.
func (h *Holder) GetBinding() *Binding {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.binding
}

// HasPlan — есть ли план вообще. Дёшево и не создаёт ложного впечатления, что
// привязка есть без плана: пара всегда ставится и снимается целиком.
func (h *Holder) HasPlan() bool {
	return h.Get() != nil && h.GetBinding() != nil
}

// Swap заменить пару целиком.
//
// «Менялось ли что-то» решает вызывающий (он следит за файлом и знает его
// содержимое), здесь только атомарная подмена. План без привязки или наоборот
// не ставится: половина пары — это ровно то состояние, в котором прогноз уехал
// бы на чужое ТС.
func (h *Holder) Swap(sched *Schedule, binding *Binding) {
	if h == nil || sched == nil || binding == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sched, h.binding = sched, binding
}
