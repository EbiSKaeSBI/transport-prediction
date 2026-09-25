package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// EventType — тип события в ленте.
type EventType string

const (
	// EventVehicle — обновление карточки машины. Не чаще раза в секунду на
	// машину: подробнее об этом в Publish.
	EventVehicle EventType = "vehicle_update"
	// EventIncident — изменился инцидент: открылся, обновился или закрылся.
	EventIncident EventType = "incident"
	// EventMetrics — срез метрик. Раз в пять секунд, потому что он несёт
	// агрегаты по всему парку и человеку интересен как тренд, а не как
	// мгновенное значение.
	EventMetrics EventType = "metrics"
)

// Event — сообщение ленты.
//
// Данные лежат готовым JSON, а не структурой, по двум причинам. Событие
// формируется один раз и уходит десяткам подписчиков: кодировать его на
// каждого — лишняя работа на горячем пути. И лента обязана пережить
// появление новых типов событий без переписывания hub: с полем Data в JSON
// это происходит добавлением поля в структуру, а не изменением протокола.
type Event struct {
	Type EventType       `json:"type"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// HubConfig — настройки ленты.
type HubConfig struct {
	// Queue — сколько событий буферизуется на подписчика. Заполненная
	// очередь означает, что клиент не успевает читать.
	Queue int
	// DropLimit — сколько пропусков допустимо до отключения клиента.
	// Один пропуск переживает любой, десять подряд означает, что клиент
	// мёртв и занимает память без пользы.
	DropLimit int
	// VehicleInterval — минимальный интервал между событиями по одной
	// машине.
	VehicleInterval time.Duration
	// MetricsInterval — как часто слать метрики.
	MetricsInterval time.Duration
	// MetricsProvider собирает срез метрик. Вызывается по таймеру, а не на
	// каждое событие, иначе сбор метрик совпал бы с пиком трафика.
	MetricsProvider func() any
	// Now подменяет часы в тестах.
	Now func() time.Time
}

// Значения по умолчанию.
const (
	// defaultQueue — очередь на подписчика. Шестьдесят событий при потоке
	// «машина раз в 15 с плюс метрики раз в 5 с» — это около пятнадцати
	// минут буфера на клиента, который замер и вернулся.
	defaultQueue = 64
	// defaultDropLimit — сколько пропусков переживает клиент.
	defaultDropLimit = 10
	// defaultVehicleInterval — не чаще раза в секунду на машину. Тик
	// конвейера 15 с, так что ограничение не трогает живые обновления, а
	// работает, когда из сервиса приходит поток кадров, а не тики.
	defaultVehicleInterval = time.Second
	// defaultMetricsInterval — как часто идут метрики.
	defaultMetricsInterval = 5 * time.Second
)

// Hub — рассылка событий подписчикам.
//
// Рассылка не имеет права блокировать отправителя. Отправитель — конвейер,
// который в этот же момент принимает телеметрию, и ожидание медленного
// браузера в его цикле означало бы, что панель оператора может замедлить
// приём данных с транспорта. Поэтому у каждого подписчика своя ограниченная
// очередь, а переполнение обрабатывается на стороне подписчика.
type Hub struct {
	cfg  HubConfig
	now  func() time.Time
	mu   sync.RWMutex
	byID map[uint64]*subscriber
	// lastID — последний выданный идентификатор подписчика. Идентификаторы
	// не переиспользуются: переиспользование ожившего подписчика с
	// удалённым означало бы, что отписка одного отключит другого.
	lastID uint64

	// lastSent — когда по каждой машине уходило последнее событие. Требуется
	// ограничить поток по одной машине, а не по всем сразу: пять машин в
	// красной зоне дают пять разных карточек, и ограничивать их общим
	// счётчиком значило бы терять чужие обновления.
	lastSent map[uint32]time.Time

	sent    atomic.Uint64
	dropped atomic.Uint64
	refused atomic.Uint64
	// closed защищает от рассылки после закрытия.
	closed atomic.Bool
}

// NewHub создаёт ленту. Нулевые значения заменяются умолчательными.
func NewHub(cfg HubConfig) *Hub {
	if cfg.Queue <= 0 {
		cfg.Queue = defaultQueue
	}
	if cfg.DropLimit <= 0 {
		cfg.DropLimit = defaultDropLimit
	}
	if cfg.VehicleInterval <= 0 {
		cfg.VehicleInterval = defaultVehicleInterval
	}
	if cfg.MetricsInterval <= 0 {
		cfg.MetricsInterval = defaultMetricsInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Hub{
		cfg:      cfg,
		now:      cfg.Now,
		byID:     make(map[uint64]*subscriber),
		lastSent: make(map[uint32]time.Time),
	}
}

// Publish отправляет событие всем подписчикам.
//
// Событие по машине может быть отброшено как слишком частое, и это не ошибка:
// обновление раз в секунду для человека с часовым окном панели неотличимо от
// обновления раз в пятнадцать секунд, а трафик уменьшается в разы.
func (h *Hub) Publish(ev Event) {
	if h.closed.Load() {
		return
	}
	if ev.At.IsZero() {
		ev.At = h.now()
	}
	if ev.Type == EventVehicle {
		unit, ok := vehicleUnitOf(ev.Data)
		if ok && h.tooOften(unit) {
			h.dropped.Add(1)
			return
		}
	}
	h.broadcast(ev)
}

// tooOften проверяет интервал по машине. Проверка и обновление метки
// происходят под одной блокировкой, иначе два события одной машины, пришедших
// одновременно, прошли бы проверку одновременно же и ушли бы оба.
func (h *Hub) tooOften(unit uint32) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	last, seen := h.lastSent[unit]
	if seen && now.Sub(last) < h.cfg.VehicleInterval {
		return true
	}
	h.lastSent[unit] = now
	return false
}

// vehicleUnitOf достаёт unit_id из готового JSON. Разбор ради одного поля
// делается только для событий по машине: остальные типы ограничивать нечем,
// а машин в событии по определению нет.
//
// Если номер достать не удалось, событие проходит без ограничения. Свои
// события всегда несут unit_id, так что это значит чужое или испорченное
// сообщение, а не поток, который заливает панель.
func vehicleUnitOf(data json.RawMessage) (uint32, bool) {
	var probe struct {
		UnitID *uint32 `json:"unit_id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.UnitID == nil {
		return 0, false
	}
	return *probe.UnitID, true
}

func (h *Hub) broadcast(ev Event) {
	h.mu.RLock()
	subs := make([]*subscriber, 0, len(h.byID))
	for _, sub := range h.byID {
		subs = append(subs, sub)
	}
	h.mu.RUnlock()

	for _, sub := range subs {
		sub.offer(ev)
	}
	h.sent.Add(1)
}

// subscribe добавляет подписчика. Подписчик создан заранее и запускает свои
// горутины, чтобы подключение не ждало их.
func (h *Hub) subscribe(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byID[sub.id] = sub
}

// unsubscribe убирает подписчика.
//
// Отметку о последней отправке по машинам (lastSent) он не трогает: она
// принадлежит машине, а не подписчику, и снимать её уходом одного читателя
// значило бы сбрасывать ограничение для всех остальных. Расти ей не на что —
// ключов не больше, чем машин в парке.
func (h *Hub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.byID, id)
}

// Run шлёт метрики по таймеру, пока ctx жив. Отдельная горутина, а не
// обязанность планировщика: метрики нужны и когда конвейер молчит, иначе на
// панели не было бы видно, что он молчит.
func (h *Hub) Run(ctx context.Context) {
	if h.cfg.MetricsProvider == nil {
		return
	}
	ticker := time.NewTicker(h.cfg.MetricsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.Close()
			return
		case <-ticker.C:
			payload, err := json.Marshal(h.cfg.MetricsProvider())
			if err != nil {
				continue
			}
			h.broadcast(Event{Type: EventMetrics, At: h.now(), Data: payload})
		}
	}
}

// HubStats — счётчики ленты для /metrics.
type HubStats struct {
	// Clients — сколько подписчиков сейчас подключено.
	Clients int `json:"clients"`
	// Sent — сколько событий разослано.
	Sent uint64 `json:"sent"`
	// Dropped — сколько событий отброшено как слишком частые.
	Dropped uint64 `json:"dropped"`
	// Refused — сколько подписчиков отключено за неспособность читать.
	Refused uint64 `json:"refused"`
}

// Stats возвращает снимок счётчиков.
func (h *Hub) Stats() HubStats {
	h.mu.RLock()
	n := len(h.byID)
	h.mu.RUnlock()
	return HubStats{
		Clients: n,
		Sent:    h.sent.Load(),
		Dropped: h.dropped.Load(),
		Refused: h.refused.Load(),
	}
}

// Close отключает всех подписчиков. Повторный вызов безопасен.
func (h *Hub) Close() {
	if h.closed.Swap(true) {
		return
	}
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.byID))
	for _, sub := range h.byID {
		subs = append(subs, sub)
	}
	h.byID = make(map[uint64]*subscriber)
	h.lastSent = make(map[uint32]time.Time)
	h.mu.Unlock()

	for _, sub := range subs {
		sub.stop(websocketCloseGoingAway)
	}
}
