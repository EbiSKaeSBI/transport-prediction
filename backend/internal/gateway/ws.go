package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
	"github.com/gorilla/websocket"
)

// Коды закрытия из RFC 6455. Свои числа не выдумываются: клиент обязан
// отличать «мы вас отключили за медлительность» от «сеть моргнула», и для
// этого есть стандартные коды.
const (
	// websocketCloseNormal — штатное закрытие.
	websocketCloseNormal = 1000
	// websocketCloseGoingAway — сервер уходит.
	websocketCloseGoingAway = 1001
	// websocketCloseTooSlow — подписчик не успевает читать.
	websocketCloseTooSlow = 1013
)

// Тайминги WebSocket. Все три числа взяты из поведения браузера, а не из
// обычной сетевой практики: соединение проходит через прокси, которые
// рвут молчащее соединение по своим таймаутам, и без собственного пинга
// панель молча перестаёт обновляться, пока оператор смотрит на застывшую
// картину и считает её актуальной.
const (
	// wsWriteWait — сколько ждать отправки одного события. Медленный
	// клиент обязан отвалиться, а не висеть вечно.
	wsWriteWait = 10 * time.Second
	// wsPongWait — сколько ждать ответа на ping.
	wsPongWait = 60 * time.Second
	// wsPingPeriod — как часто слать ping. Должен быть заметно меньше
	// wsPongWait, иначе соединение успевает умереть молча.
	wsPingPeriod = 25 * time.Second
	// wsMaxMessageBytes — потолок входящего сообщения. Клиент ничего не
	// спрашивает, поэтому читать ему нечего, и большая входящая рамка
	// означает либо ошибку клиента, либо попытку занять память.
	wsMaxMessageBytes = 1 << 16
)

// subscriber — одно WebSocket-соединение.
//
// У каждого своя ограниченная очередь и своя горутина отправки. Очередь
// ограничена намеренно: неограниченная очередь медленного клиента рано или
// поздно съест память процесса, который обязан обслуживать телеметрию.
type subscriber struct {
	id   uint64
	conn *websocket.Conn
	hub  *Hub
	// out — очередь отправки.
	out chan Event
	// done закрывается один раз при завершении.
	done     chan struct{}
	closeOne sync.Once
	// missed — сколько событий пропущено из-за переполнения очереди.
	missed atomic.Uint64
	// closing — защита от приёма событий в закрытое соединение.
	closing atomic.Bool
	// code — код закрытия, который пошлёт writeLoop.
	code atomic.Int64
}

// newSubscriber создаёт подписчика.
func newSubscriber(id uint64, conn *websocket.Conn, hub *Hub) *subscriber {
	return &subscriber{
		id:   id,
		conn: conn,
		hub:  hub,
		out:  make(chan Event, hub.cfg.Queue),
		done: make(chan struct{}),
	}
}

// offer кладёт событие в очередь.
//
// Переполнение обрабатывается так: сначала копятся пропуски, и только когда
// их становится явно много, клиент отключается с кодом «слишком медленный».
// Разовое переполнение переживает любой клиент — это обычное дело при
// переключении вкладки, — а клиент, который не читает вообще, обязан быть
// отключён, иначе он будет висеть в списке подписчиков вечно.
func (s *subscriber) offer(ev Event) {
	if s.closing.Load() {
		return
	}
	select {
	case s.out <- ev:
	default:
		if s.missed.Add(1) > uint64(s.hub.cfg.DropLimit) {
			s.hub.refused.Add(1)
			s.stop(websocketCloseTooSlow)
		}
	}
}

// writeLoop отправляет события и пингует клиента.
//
// Это единственная горутина, которой принадлежит запись в соединение.
// Gorilla допускает ровно одного писателя, и запись из readLoop или из
// обработчика переполнения кончается не гонкой данных, а паникой
// «concurrent write to websocket connection». Поэтому завершение выглядит
// так: остальные только сигнализируют, а код закрытия и сам сокет
// достаются здесь.
func (s *subscriber) writeLoop() {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	defer s.closeSocket()
	for {
		select {
		case <-s.done:
			s.sendClose(int(s.code.Load()))
			return
		case <-ticker.C:
			if err := s.write(websocket.PingMessage, nil); err != nil {
				s.stop(websocketCloseNormal)
				return
			}
		case ev, ok := <-s.out:
			if !ok {
				s.sendClose(websocketCloseNormal)
				return
			}
			if err := s.write(websocket.TextMessage, ev); err != nil {
				s.stop(websocketCloseNormal)
				return
			}
		}
	}
}

// write отправляет одно сообщение с таймаутом. Таймаут обязателен: TCP может
// месяцами не сообщать об отвалившемся клиенте, и писать в такое соединение
// без срока — значит копить в буфере ядра до отказа.
func (s *subscriber) write(kind int, payload any) error {
	if err := s.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
		return fmt.Errorf("ws: %w", err)
	}
	if payload == nil {
		return s.conn.WriteMessage(kind, nil)
	}
	if raw, ok := payload.([]byte); ok {
		return s.conn.WriteMessage(kind, raw)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ws: сообщение не разобрано: %w", err)
	}
	return s.conn.WriteMessage(kind, data)
}

// readLoop читает входящие сообщения, чтобы замечать закрытие.
//
// Содержимое входящих сообщений не используется: у протокола нет команд, и
// читать всё равно нужно, иначе разрыв соединения обнаружится только по
// таймауту записи, а это медленно и ненадёжно.
func (s *subscriber) readLoop() {
	defer s.stop(websocketCloseNormal)
	s.conn.SetReadLimit(wsMaxMessageBytes)
	if err := s.conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		return
	}
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		if _, _, err := s.conn.ReadMessage(); err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				// Клиент попросил закрыться — это его право, а не ошибка.
				return
			}
			return
		}
	}
}

// stop просит подписчика завершиться с кодом. В сокет при этом ничего не
// пишется: единственный писатель — writeLoop, и он отреагирует на сигнал сам.
func (s *subscriber) stop(code int) {
	s.closeOne.Do(func() {
		s.closing.Store(true)
		// Код записывается до закрытия done, поэтому writeLoop, выйдя по
		// нему, всегда видит верный.
		s.code.Store(int64(code))
		close(s.done)
	})
}

// sendClose шлёт кадр закрытия. Вызывается только из writeLoop.
func (s *subscriber) sendClose(code int) {
	if s.conn == nil {
		return
	}
	// Ошибка ожидаема, если соединение уже мертво, и глотается: вызывающий
	// ничего не может с этим сделать, а регистрировать её в счётчике отказов
	// бессмысленно.
	_ = s.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	_ = s.conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, closeReason(code)))
}

// closeSocket закрывает соединение. Вызывается только из writeLoop, и
// повторное закрытие безвредно.
func (s *subscriber) closeSocket() {
	if s.conn == nil {
		// Сокет у подписчика необязателен: подписчик существует и между
		// подключением и обслуживанием, где события ещё считаются, а
		// закрывать нечего.
		return
	}
	_ = s.conn.Close()
}

// closeReason даёт клиенту читаемую причину закрытия.
func closeReason(code int) string {
	switch code {
	case websocketCloseTooSlow:
		return "подписчик не успевает читать поток"
	case websocketCloseGoingAway:
		return "сервер завершает работу"
	default:
		return "соединение закрыто"
	}
}

// upgrader поднимает соединение.
//
// CheckOrigin разрешает всё, и это не небрежность: гейтвей обслуживает
// приборную панель и раздаёт ею же данные, то есть потребитель и источник
// — одна и та же система. Если позже панель переедет на другой домен,
// проверку придётся добавить, и это будет изменение с последствиями, а не
// молчаливое ослабление ужесточения.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// stream обслуживает GET /ws/stream.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade уже отправил ответ с ошибкой, если мог.
		s.log.Warn("ws: не удалось поднять соединение", slog.String("err", err.Error()))
		return
	}
	h := s.Hub()
	if h == nil {
		// Ленты нет: соединение закрываем сразу, иначе клиент будет ждать
		// события, которого не придёт никогда.
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocketCloseGoingAway,
				"лента не настроена"))
		_ = conn.Close()
		return
	}
	sub := newSubscriber(h.nextID(), conn, h)
	h.subscribe(sub)

	// Снимок при подключении обязателен: без него панель минуту-другую
	// показывала бы пустые карточки, хотя данные уже есть.
	for _, ev := range s.snapshot() {
		sub.offer(ev)
	}

	go sub.writeLoop()
	go sub.readLoop()
	<-sub.done
	h.unsubscribe(sub.id)
}

// vehicleEvent — карточка машины в ленте.
//
// Идентификатор машины лежит рядом с прогнозом, а не внутри него: по нему
// лента решает, можно ли отправлять событие (чаще раза в секунду на машину
// нельзя), и по нему же панель ищет карточку. Внутри predictionView его нет
// намеренно — прогноз принадлежит модели, и её схема не обязана знать про
// транспорт.
type vehicleEvent struct {
	UnitID     uint32         `json:"unit_id"`
	TRID       int64          `json:"tr_id"`
	TargetStop int64          `json:"target_stop_id"`
	Prediction predictionView `json:"prediction"`
}

// incidentEvent — изменение инцидента в ленте.
//
// Счётчики едут вместе с инцидентом, а не отдельным событием: панель
// показывает «открыто 3» в шапке, и без счётчиков рядом с событием эта цифра
// расходилась бы с карточками на экране.
type incidentEvent struct {
	// Incident — изменившийся инцидент. nil у снимка, который несёт только
	// текущее состояние.
	Incident *Incident `json:"incident"`
	// Incidents — все незакрытые инциденты. Заполняется только снимком:
	// подписчик, который подключился, обязан увидеть всё открытое, а не
	// только то, что изменится после него.
	Incidents []Incident    `json:"incidents,omitempty"`
	Stats     IncidentStats `json:"stats"`
}

// publishVehicle отправляет карточку машины в ленту.
func (s *Server) publishVehicle(p predictor.Prediction) {
	h := s.Hub()
	if h == nil {
		return
	}
	data, err := json.Marshal(vehicleEvent{
		UnitID: p.UnitID, TRID: p.TRID, TargetStop: p.TargetStopID,
		Prediction: viewOf(p),
	})
	if err != nil {
		return
	}
	h.Publish(Event{Type: EventVehicle, At: s.now(), Data: data})
}

// publishIncident отправляет изменившийся инцидент.
func (s *Server) publishIncident(inc Incident) {
	h := s.Hub()
	if h == nil {
		return
	}
	data, err := json.Marshal(incidentEvent{
		Incident: &inc, Stats: s.incidents.Stats(),
	})
	if err != nil {
		return
	}
	h.Publish(Event{Type: EventIncident, At: s.now(), Data: data})
}

// snapshot собирает начальное состояние для нового подписчика.
func (s *Server) snapshot() []Event {
	now := s.now()
	var out []Event
	incidents, err := json.Marshal(incidentEvent{
		Incidents: s.incidents.List("", 0),
		Stats:     s.incidents.Stats(),
	})
	if err == nil {
		out = append(out, Event{Type: EventIncident, At: now, Data: incidents})
	}
	// Снимок прогнозов по всем машинам, которые о них знают. Машин без
	// прогноза в снимок не попадают: карточка без прогноза полезна ровно
	// настолько, насколько полезна телеметрия без прогноза, то есть
	// показывается пустой меткой.
	//
	// Снимок берётся из хранилища, а не из наличия предиктора: прогнозы,
	// доставленные до перезапуска конфигурации, принадлежат панели по
	// праву, и прятать их из-за отсутствия предиктора значило бы врать.
	var all []vehicleEvent
	for _, unit := range s.knownUnits() {
		if p, ok := s.preds.Latest(unit); ok {
			all = append(all, vehicleEvent{
				UnitID: unit, TRID: p.TRID, TargetStop: p.TargetStopID,
				Prediction: viewOf(p),
			})
		}
	}
	payload, err := json.Marshal(map[string]any{
		"vehicles": all,
		"count":    len(all),
	})
	if err != nil {
		return out
	}
	return append(out, Event{Type: EventVehicle, At: now, Data: payload})
}

// knownUnits перечисляет машины, о которых гейтвей что-то знает: они есть
// либо в телеметрии, либо в расписании.
func (s *Server) knownUnits() []uint32 {
	seen := make(map[uint32]struct{})
	var out []uint32
	add := func(unit uint32) {
		if _, ok := seen[unit]; ok {
			return
		}
		seen[unit] = struct{}{}
		out = append(out, unit)
	}
	if s.cfg.Store != nil {
		for _, unit := range s.cfg.Store.Units() {
			add(unit)
		}
	}
	if s.cfg.Schedule != nil {
		for _, tr := range s.cfg.Schedule.Vehicles() {
			if s.cfg.Binding != nil {
				if unit, ok := s.cfg.Binding.UnitID(tr); ok {
					add(unit)
				}
			}
		}
	}
	return out
}

// nextID выдаёт подписчику идентификатор.
func (h *Hub) nextID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastID++
	return h.lastID
}
