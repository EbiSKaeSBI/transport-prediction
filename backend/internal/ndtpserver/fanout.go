package ndtpserver

import "github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"

// Fanout раздаёт кадры нескольким обработчикам по очереди.
//
// Зачем. У NDTP-приёмника один вход и несколько потребителей: конвейер
// прогноза, наблюдатель телеметрии и — когда включена запись — сборщик кадров
// для последующей генерации плана. Заводить под второго потребителя второй
// порт нельзя: эмулятор шлёт на один адрес, и на тот же адрес, что и сервер,
// второй слушатель не встанет. Отвод внутри процесса решает это без второго
// сокета и без перенастройки источника.
//
// Обработчики вызываются последовательно, в порядке аргументов, и под тем же
// мьютексом, что и обычный вызов: порядок обработки кадра не должен зависеть от
// того, кто подписан. Ошибка одного обработчика не отменяет остальные —
// иначе включённая запись могла бы остановить прогноз.
type Fanout struct {
	handlers []Handler
}

// NewFanout собирает отвод из непустых обработчиков. Если остался ровно один,
// он возвращается как есть: лишний слой вызовов в горячем пути не нужен.
func NewFanout(handlers ...Handler) Handler {
	live := make([]Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			live = append(live, h)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	default:
		return &Fanout{handlers: live}
	}
}

func (f *Fanout) OnHandshake(unitID uint32, req ndtp.ConnRequest) {
	for _, h := range f.handlers {
		h.OnHandshake(unitID, req)
	}
}

func (f *Fanout) OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell) {
	for _, h := range f.handlers {
		h.OnRealtime(unitID, frame, cells)
	}
}

func (f *Fanout) OnMalformed(unitID uint32, frame ndtp.Frame, err error) {
	for _, h := range f.handlers {
		h.OnMalformed(unitID, frame, err)
	}
}

func (f *Fanout) OnDisconnect(unitID uint32) {
	for _, h := range f.handlers {
		h.OnDisconnect(unitID)
	}
}
