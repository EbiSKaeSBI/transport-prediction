package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsServer поднимает гейтвей с лентой на тестовом сервере.
func wsServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	s := testServer(t)
	s.cfg.Hub = NewHub(HubConfig{
		Queue:           8,
		DropLimit:       3,
		VehicleInterval: time.Millisecond,
		MetricsInterval: 20 * time.Millisecond,
		MetricsProvider: func() any {
			return map[string]any{"vehicles": 1}
		},
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(s.cfg.Hub.Close)
	return srv, s
}

// dialWS подключается к ленте.
func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("подключение не удалось: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readEvent читает одно событие с предельным ожиданием.
func readEvent(t *testing.T, conn *websocket.Conn, d time.Duration) Event {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	var ev Event
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("событие не прочитано: %v", err)
	}
	return ev
}

// waitClients ждёт, пока подписчиков станет ровно столько-то.
func waitClients(t *testing.T, h *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Stats().Clients == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("подписчиков %d, ожидалось %d", h.Stats().Clients, want)
}

// При подключении подписчик обязан получить снимок, иначе панель минуту-другую
// показывала бы пустые карточки, хотя данные уже есть.
func TestStreamSendsSnapshotOnConnect(t *testing.T) {
	srv, s := wsServer(t)
	s.Observe(predictionAt(4242, 200, 0.9))
	conn := dialWS(t, srv)

	// Снимок приходит сразу, и в нём есть открытый инцидент по нашей машине.
	ev := readEvent(t, conn, 2*time.Second)
	if ev.Type != EventIncident {
		t.Fatalf("первым пришло %q, ожидался снимок инцидентов", ev.Type)
	}
	var payload incidentEvent
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatalf("снимок не разобран: %v", err)
	}
	if len(payload.Incidents) != 1 || payload.Incidents[0].UnitID != 4242 {
		t.Errorf("в снимке %+v, ожидался инцидент по машине 4242", payload.Incidents)
	}
	if payload.Stats.Open != 1 {
		t.Errorf("в снимке открыто %d инцидентов, ожидался 1", payload.Stats.Open)
	}
	// Вторым событием идёт снимок прогнозов.
	next := readEvent(t, conn, 2*time.Second)
	if next.Type != EventVehicle {
		t.Errorf("вторым пришло %q, ожидался снимок машин", next.Type)
	}
}

// Открытие инцидента обязано уйти в ленту немедленно: это и есть то, ради
// чего панель подключена.
func TestIncidentIsBroadcastImmediately(t *testing.T) {
	srv, s := wsServer(t)
	conn := dialWS(t, srv)
	waitClients(t, s.Hub(), 1)
	// Вычитываем снимок.
	readEvent(t, conn, 2*time.Second)
	readEvent(t, conn, 2*time.Second)

	s.Observe(predictionAt(4242, 300, 0.95))

	// Карточка и инцидент уходят одним тиком, и какой из них первым —
	// не закреплено. Ждём инцидент, попутно убеждаясь, что карточка тоже
	// доехала: молчащая лента при живой панели выглядит как «всё спокойно».
	sawCard := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ev := readEvent(t, conn, 500*time.Millisecond)
		switch ev.Type {
		case EventVehicle:
			sawCard = true
		case EventIncident:
			var payload incidentEvent
			if err := json.Unmarshal(ev.Data, &payload); err != nil {
				t.Fatalf("инцидент не разобран: %v", err)
			}
			if payload.Incident == nil {
				t.Fatal("в событии инцидента нет самого инцидента")
			}
			if payload.Incident.PredictedDevS != 300 {
				t.Errorf("отклонение %g, ожидалось 300", payload.Incident.PredictedDevS)
			}
			if payload.Stats.Open != 1 {
				t.Errorf("открыто %d, ожидался 1", payload.Stats.Open)
			}
			if !sawCard {
				t.Error("инцидент дошёл, а карточка машины — нет")
			}
			return
		}
	}
	t.Fatal("инцидент не дошёл за две секунды")
}

func TestVehicleEventsAreThrottled(t *testing.T) {
	hub := NewHub(HubConfig{
		Queue: 4, DropLimit: 100,
		VehicleInterval: time.Hour, // заведомо больше интервала теста
		Now:             func() time.Time { return base },
	})
	sub := newSubscriber(1, nil, hub)
	hub.subscribe(sub)

	for range 10 {
		hub.Publish(Event{Type: EventVehicle, Data: json.RawMessage(`{"unit_id":4242}`)})
	}
	if n := len(sub.out); n != 1 {
		t.Errorf("в очередь легло %d событий по одной машине, ожидалось 1", n)
	}
	if hub.Stats().Dropped != 9 {
		t.Errorf("отброшено %d, ожидалось 9", hub.Stats().Dropped)
	}
}

// Ограничение по машинам независимо: пять машин в красной зоне дают пять
// разных карточек, и общий счётчик терял бы чужие обновления.
func TestThrottleIsPerVehicle(t *testing.T) {
	hub := NewHub(HubConfig{
		Queue: 8, DropLimit: 100,
		VehicleInterval: time.Hour,
		Now:             func() time.Time { return base },
	})
	sub := newSubscriber(1, nil, hub)
	hub.subscribe(sub)

	for i := range 5 {
		hub.Publish(Event{Type: EventVehicle,
			Data: json.RawMessage(`{"unit_id":` + itoa(i+1) + `}`)})
	}
	if n := len(sub.out); n != 5 {
		t.Errorf("в очередь легло %d событий по пяти машинам, ожидалось 5", n)
	}
}

// Медленный клиент обязан быть отключён, а не висеть в списке подписчиков
// вечно. Одиночное переполнение при этом переживает: вкладка в фоне
// перестаёт читать на секунду — это обычное дело.
func TestSlowClientIsDropped(t *testing.T) {
	hub := NewHub(HubConfig{Queue: 2, DropLimit: 3,
		VehicleInterval: time.Millisecond})
	sub := newSubscriber(1, nil, hub)
	hub.subscribe(sub)

	for range 10 {
		hub.Publish(Event{Type: EventIncident, Data: json.RawMessage(`{}`)})
	}
	if !sub.closing.Load() {
		t.Error("переполненный подписчик не отключён")
	}
	if hub.Stats().Refused != 1 {
		t.Errorf("отказов %d, ожидался 1", hub.Stats().Refused)
	}
}

// Одно-два пропуска подписчик переживает: счётчик пропусков растёт, но
// отключения нет.
func TestOccasionalStallKeepsClient(t *testing.T) {
	hub := NewHub(HubConfig{Queue: 4, DropLimit: 3,
		VehicleInterval: time.Millisecond})
	sub := newSubscriber(1, nil, hub)
	hub.subscribe(sub)

	for range 6 { // 4 в очередь, 2 пропущено
		hub.Publish(Event{Type: EventIncident, Data: json.RawMessage(`{}`)})
	}
	if sub.closing.Load() {
		t.Error("подписчик отключён за два пропуска, а лимит 3")
	}
	if sub.missed.Load() != 2 {
		t.Errorf("пропусков %d, ожидалось 2", sub.missed.Load())
	}
}

func TestMetricsArriveOnTimer(t *testing.T) {
	hub := NewHub(HubConfig{
		Queue: 8, DropLimit: 10,
		VehicleInterval: time.Millisecond,
		MetricsInterval: 10 * time.Millisecond,
		MetricsProvider: func() any { return map[string]any{"ok": true} },
	})
	sub := newSubscriber(1, nil, hub)
	hub.subscribe(sub)

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go hub.Run(runCtx)

	select {
	case ev := <-sub.out:
		if ev.Type != EventMetrics {
			t.Errorf("пришло %q, ожидались метрики", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("метрики не пришли за две секунды")
	}
}

// Публикация не должна блокировать отправителя: конвейер в этот же момент
// принимает телеметрию, и ожидание медленного браузера в его цикле означало
// бы, что панель может замедлить приём данных.
func TestPublishNeverBlocks(t *testing.T) {
	hub := NewHub(HubConfig{Queue: 1, DropLimit: 2, VehicleInterval: time.Millisecond})
	for range 20 {
		sub := newSubscriber(1, nil, hub)
		hub.subscribe(sub)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 500 {
			hub.Publish(Event{Type: EventIncident, Data: json.RawMessage(`{}`)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("рассылка заблокирована на переполненных подписчиках")
	}
}

func TestCloseDisconnectsEveryone(t *testing.T) {
	hub := NewHub(HubConfig{Queue: 4, DropLimit: 10, VehicleInterval: time.Millisecond})
	subs := make([]*subscriber, 0, 3)
	for i := range 3 {
		// Идентификаторы разные: подписчики живут в карте по ключу, и три
		// одинаковых схлопнулись бы в одного.
		sub := newSubscriber(uint64(i+1), nil, hub)
		hub.subscribe(sub)
		subs = append(subs, sub)
	}
	hub.Close()
	for i, sub := range subs {
		if !sub.closing.Load() {
			t.Errorf("подписчик %d не отключён при закрытии ленты", i)
		}
	}
	if hub.Stats().Clients != 0 {
		t.Errorf("после закрытия подписчиков %d", hub.Stats().Clients)
	}
	// Повторное закрытие безопасно, а публикация после него — нет.
	hub.Close()
	hub.Publish(Event{Type: EventIncident})
	if hub.Stats().Sent != 0 {
		t.Error("после закрытия лента приняла событие")
	}
}

// Подписка без ленты обязана закрываться сразу: иначе клиент будет ждать
// события, которое не придёт никогда, и панель покажет вечную заглушку.
func TestStreamWithoutHubClosesImmediately(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("апгрейд не удался: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	if err == nil {
		t.Fatal("соединение без ленты не закрыто")
	}
	if !errors.As(err, &closeErr) {
		t.Fatalf("ожидался код закрытия, получено: %v", err)
	}
	if closeErr.Code != websocketCloseGoingAway {
		t.Errorf("код закрытия %d, ожидался %d", closeErr.Code, websocketCloseGoingAway)
	}
}

// Метрики ленты обязаны попадать в /metrics: молчащий поток событий при
// живой ленте выглядит как «панель работает», а не как «панель отвалилась».
func TestHubStatsReachMetrics(t *testing.T) {
	srv, s := wsServer(t)
	conn := dialWS(t, srv)
	waitClients(t, s.Hub(), 1)
	_ = conn

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "transport_gateway_ws_clients") {
		t.Errorf("в метриках нет числа подписчиков:\n%s", rec.Body)
	}
}
