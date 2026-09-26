package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// Границы риска. Числа взяты не из воздуха: измерения на validate дали
// среднюю абсолютную ошибку baseline 93.46 с и 67.10 с у модели, то есть
// типичная ошибка в четверть минуты. Порог 60 с — примерно половина этой
// ошибки, ниже которого различие «опаздывает на минуту» и «опаздывает на пол
// минуты» неразличимо с точностью самой модели. Порог 120 с вдвое выше и
// означает опоздание, которое уже видно пассажиру.
const (
	// DelayYellowS — нижняя граница жёлтой зоны.
	DelayYellowS = 60.0
	// DelayRedS — нижняя граница красной зоны.
	DelayRedS = 120.0
	// PLateYellow — нижняя граница жёлтой зоны по вероятности.
	PLateYellow = 0.3
	// PLateRed — нижняя граница красной зоны по вероятности.
	PLateRed = 0.6
)

// Risk — уровень риска опоздания.
type Risk string

const (
	// RiskGreen — машина едет в графике либо модель не ждёт опоздания.
	RiskGreen Risk = "green"
	// RiskYellow — опоздание вероятно, но в пределах обычной ошибки.
	RiskYellow Risk = "yellow"
	// RiskRed — опоздание состоялось или почти наверняка.
	RiskRed Risk = "red"
)

// Classify переводит прогноз в уровень риска.
//
// Красная зона объединяет «уже опоздал» и «опоздает с вероятностью выше
// 0.6» намеренно: в обоих случаях оператору нужно что-то делать сейчас, и
// ждать уточнения нельзя. Жёлтая — промежуточная: модель ошибается на
// десятки секунд, и объявлять тревогу там, где типична ошибка самой модели,
// значит приучить дежурного игнорировать тревогу.
func Classify(p predictor.Prediction) Risk {
	switch {
	case p.PredictedDevS >= DelayRedS || p.PLate >= PLateRed:
		return RiskRed
	case p.PredictedDevS >= DelayYellowS || p.PLate >= PLateYellow:
		return RiskYellow
	default:
		return RiskGreen
	}
}

// IncidentStatus — состояние инцидента.
type IncidentStatus string

const (
	// StatusOpen — инцидент открыт и ещё не взят оператором.
	StatusOpen IncidentStatus = "open"
	// StatusAcked — оператор взял инцидент в работу.
	StatusAcked IncidentStatus = "acked"
	// StatusResolved — риск ушёл из красной зоны.
	StatusResolved IncidentStatus = "resolved"
)

// Incident — задержка транспортного средства.
type Incident struct {
	// ID — устойчивый идентификатор. Не счётчик: он выводится из
	// (машина, цель), иначе перезапуск гейтвея отвязал бы инцидент от
	// машины, а повторный приход в красную зону создал бы второй
	// идентификатор на тот же самый простой.
	ID string `json:"id"`
	// UnitID — устройство.
	UnitID uint32 `json:"unit_id"`
	// TRID — номер транспортного средства.
	TRID int64 `json:"tr_id"`
	// TargetStopID — остановка, к которой машина опоздает.
	TargetStopID int64 `json:"target_stop_id"`
	// PrevStopID — плановая остановка перед целью: пара с target задаёт
	// участок маршрута, на котором копится опоздание. 0 — цель первая в
	// плане либо расписание гейтвею неизвестно (не выдумываем сосед).
	PrevStopID int64 `json:"prev_stop_id,omitempty"`
	// Status — состояние.
	Status IncidentStatus `json:"status"`
	// Risk — уровень на момент последнего обновления.
	Risk Risk `json:"risk"`
	// OpenedAt — когда машина впервые попала в красную зону.
	OpenedAt time.Time `json:"opened_at"`
	// UpdatedAt — когда прогноз обновляли в последний раз.
	UpdatedAt time.Time `json:"updated_at"`
	// AckedAt — когда оператор взял инцидент. Нулевое время, если не брал.
	AckedAt *time.Time `json:"acked_at"`
	// AckedBy — кто взял. Пусто, если не брал.
	AckedBy string `json:"acked_by"`
	// ResolvedAt — когда риск ушёл из красной зоны.
	ResolvedAt *time.Time `json:"resolved_at"`
	// PredictedDevS, PLate — значения, давшие красную зону.
	PredictedDevS float64 `json:"predicted_dev_s"`
	// PLate — указатель по той же причине, что в predictionView: у инцидента
	// может не быть вероятности (baseline), и 0% здесь означал бы
	// «опоздания не будет» вместо «вероятность не оценивалась».
	PLate *float64 `json:"p_late"`
	// Reason — предполагаемая причина (правила §5.4 на ML-стороне). Пустая,
	// если прогноз пришёл из baseline/fallback: объяснять нечем.
	Reason string `json:"reason,omitempty"`
	// Stale — прогноз, на котором стоит инцидент, устарел.
	Stale bool `json:"stale"`
	// Source — откуда взят прогноз, положивший инцидент.
	Source predictor.Source `json:"source"`
}

// DefaultIncidentCap — сколько инцидентов держим. 1024 с запасом покрывает
// парк из 30 машин, даже если каждая в красной зоне несколько смен подряд.
const DefaultIncidentCap = 1024

// Incidents — ограниченное хранилище инцидентов.
//
// Инциденты живут в памяти, и это осознанный предел: гейтвей предназначен для
// показа текущей ситуации и разбора последних смен, а историю за годы
// изменения схемы хранить в том же процессе нельзя — она всё равно станет
// нечитаемой. Что теряется, указано в ADR 0007, чтобы потеря была записана,
// а не обнаружена.
type Incidents struct {
	// byID — все инциденты по их идентификаторам, включая закрытые.
	byID map[string]*Incident
	// byKey — текущий эпизод по паре (машина, цель). Нужен отдельно от
	// byID, потому что идентификатор эпизода несёт порядковый номер ради
	// истории повторных приходов в красную зону, а искать текущий эпизод
	// надо по паре, а не по номеру: номера ещё нет, пока эпизод не начался.
	// Смешивать эти два пространства в одной карте означало бы, что поиск
	// никогда не находит существующий инцидент и каждый прогноз в красной
	// зоне создаёт новый вместо обновления старого.
	byKey map[string]*Incident
	// byUnit — текущий незакрытый инцидент каждой машины. Инцидент живёт на
	// конкретную остановку, а прогноз смотрит вперёд: когда цель уехала на
	// следующую, прежний инцидент безнадзорно висит открытым, пока машина
	// снова не выйдет из красной зоны по той же цели. Одна вечно опаздывающая
	// ТС тогда оставляет в рельсе по карточке на каждую пройденную остановку,
	// и оператор теряет среди них ту, что про текущую цель. Ключ по машине
	// позволяет закрыть прежний эпизод в момент смены цели.
	byUnit map[uint32]*Incident
	// order — идентификаторы в порядке открытия. По нему и считаются
	// свежие, и вытесняется самый старый.
	order []string
	cap   int
	now   func() time.Time

	mu   sync.Mutex
	seq  uint64
	acks uint64
	// prevStop — плановая остановка перед целью (участок, на котором копится
	// опоздание). Инжектируется гейтвеем после New: без расписания поля
	// просто нет (0), хранилище не обязано его знать.
	prevStop func(trID, target int64) int64
}

// NewIncidents создаёт хранилище. Неположительный вместимость берёт
// DefaultIncidentCap.
func NewIncidents(capacity int) *Incidents {
	if capacity <= 0 {
		capacity = DefaultIncidentCap
	}
	return &Incidents{
		byID:   make(map[string]*Incident, capacity),
		byKey:  make(map[string]*Incident, capacity),
		byUnit: make(map[uint32]*Incident),
		cap:    capacity,
		now:    time.Now,
	}
}

// SetScheduleLookup подключает планового соседа цели: инциденты получают
// prev_stop_id (участок «откуда опаздывают»). Вызывается гейтвеем один раз
// после New; без него карточка честно живёт без участка.
func (s *Incidents) SetScheduleLookup(fn func(trID, target int64) int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prevStop = fn
}

// prevStopOf — плановая остановка перед целью прогноза; 0, когда соседа
// взять неоткуда (нет расписания или цель первая).
func (s *Incidents) prevStopOf(p predictor.Prediction) int64 {
	if s.prevStop == nil {
		return 0
	}
	return s.prevStop(p.TRID, p.TargetStopID)
}

// IncidentID выводит устойчивый идентификатор из машины и цели. Хэш, а не
// счётчик: перезапуск процесса не должен ни потерять связь инцидента с
// машиной, ни выдать ей второй номер на тот же простой.
func IncidentID(unitID uint32, targetStopID int64) string {
	sum := sha256.Sum256([]byte(strconv.FormatUint(uint64(unitID), 10) +
		"\x00" + strconv.FormatInt(targetStopID, 10)))
	return hex.EncodeToString(sum[:8])
}

// IncidentEvent — что именно сделал прогноз с инцидентами. Отдельный тип
// нужен подписчикам WebSocket: им важно не «есть ли инцидент», а что
// изменилось, иначе рассылка превратится в поток одинаковых сообщений.
type IncidentEvent struct {
	// Incident — изменившийся инцидент. Nil, если ничего не изменилось.
	Incident *Incident
	// Opened — инцидент только что открыт.
	Opened bool
	// Updated — открытый инцидент получил новые цифры.
	Updated bool
	// Resolved — инцидент только что закрыт, потому что риск ушёл из
	// красной зоны.
	Resolved bool
	// Superseded — инцидент прежней цели той же машины, закрытый из-за
	// перехода прогноза на следующую остановку. Отдельное поле, а не флаг в
	// Incident: за один прогноз меняются два инцидента, и подписчик ленты
	// должен получить оба, иначе закрытый навсегда останется висеть в панели.
	Superseded *Incident
}

// Update приводит хранилище в соответствие с прогнозом и возвращает
// изменившийся инцидент вместе с признаком, что он появился.
//
// Правило одно и оно: инцидент открывается в красной зоне и закрывается,
// когда риск из неё выходит. Жёлтая зона инцидента не создаёт — она и
// существует затем, чтобы оператор видел тревожный прогноз до того, как он
// стал инцидентом.
func (s *Incidents) Update(p predictor.Prediction) IncidentEvent {
	risk := Classify(p)
	now := s.now()
	id := IncidentID(p.UnitID, p.TargetStopID)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Прежняя цель той же машины. Смена цели — это и есть «простой закончился,
	// поездка пошла к следующей остановке»: прежний инцидент закрывается
	// сам, без причины в самом инциденте, потому что машина уехала. Раньше он
	// ждал выхода из красной зоны по своей цели, а её больше не существует.
	suppressed := s.supersedeLocked(p.UnitID, id, now)

	inc, ok := s.byKey[id]
	if ok && inc.Status == StatusResolved {
		// Машина снова в красной зоне после закрытия. Это новый эпизод:
		// закрытый инцидент и последующий простой разные события, и
		// склеивать их одним идентификатором нельзя, иначе в истории
		// сольётся причина со следствием.
		delete(s.byKey, id)
		ok = false
	}

	if ok {
		// Эпизод идёт: цифры обновляются под текущим прогнозом, иначе в
		// карточке осталась бы та оценка, на которой инцидент открылся.
		inc.PredictedDevS = p.PredictedDevS
		inc.PLate = plateOrNil(p)
		// Причина перезаписывается только когда её принесли: пустая строка
		// от fallback не должна стирать последнее известное объяснение —
		// инцидент в этот момент всё ещё красные секунды, и оператору
		// полезнее старая версия «почему», чем пустое поле.
		if p.Reason != "" {
			inc.Reason = p.Reason
		}
		inc.Stale = p.Stale
		inc.Source = p.Source
		inc.Risk = risk
		inc.UpdatedAt = now
		if risk != RiskRed {
			// Вышли из красной зоны. Закрытый инцидент остаётся видимым
			// в resolved: разбор «кто и когда признал» после смены не
			// должен зависеть от памяти оператора.
			inc.Status = StatusResolved
			resolved := now
			inc.ResolvedAt = &resolved
			if cur, ok := s.byUnit[p.UnitID]; ok && cur.ID == inc.ID {
				delete(s.byUnit, p.UnitID)
			}
			return IncidentEvent{Incident: copyOf(inc), Resolved: true, Superseded: suppressed}
		}
		s.byUnit[p.UnitID] = inc
		return IncidentEvent{Incident: copyOf(inc), Updated: true, Superseded: suppressed}
	}

	if risk != RiskRed {
		// Жёлтая зона инцидента не создаёт: она существует затем, чтобы
		// оператор увидел тревожный прогноз до того, как он стал простоям.
		// Прежняя цель при этом закрывается — иначе её карточка осталась бы
		// висеть до конца смены.
		return IncidentEvent{Superseded: suppressed}
	}

	s.seq++
	inc = &Incident{
		ID:            newIncidentID(s.seq, p),
		UnitID:        p.UnitID,
		TRID:          p.TRID,
		TargetStopID:  p.TargetStopID,
		PrevStopID:    s.prevStopOf(p),
		Status:        StatusOpen,
		Risk:          risk,
		OpenedAt:      now,
		UpdatedAt:     now,
		PredictedDevS: p.PredictedDevS,
		PLate:         plateOrNil(p),
		Reason:        p.Reason,
		Stale:         p.Stale,
		Source:        p.Source,
	}
	s.byID[inc.ID] = inc
	s.byKey[IncidentID(inc.UnitID, inc.TargetStopID)] = inc
	s.byUnit[inc.UnitID] = inc
	s.order = append(s.order, inc.ID)
	s.evictLocked()
	// Копия, а не сам указатель: инцидент живёт в хранилище и следующий
	// прогноз перепишет его поля, пока вызывающий смотрит в этот ответ.
	// Get и List тоже отдают копии, и Update не должен быть исключением.
	return IncidentEvent{Incident: copyOf(inc), Opened: true, Superseded: suppressed}
}

// supersedeLocked закрывает текущий инцидент машины, если он держится на
// другую цель, чем текущий прогноз. Возвращает копию закрытого (nil, если
// закрывать нечего), чтобы вызывающий отдал его подписчикам.
//
// Закрытие здесь не выдумывает для инцидента новой причины: уехавшая машина
// — это разрешение ситуации, а не отмена тревоги, и ResolvedAt остаётся
// временем, когда машина реально уехала.
func (s *Incidents) supersedeLocked(unitID uint32, currentKey string, now time.Time) *Incident {
	cur, ok := s.byUnit[unitID]
	if !ok {
		return nil
	}
	if IncidentID(cur.UnitID, cur.TargetStopID) == currentKey {
		return nil
	}
	delete(s.byUnit, unitID)
	if cur.Status == StatusResolved {
		return nil
	}
	cur.Status = StatusResolved
	resolved := now
	cur.ResolvedAt = &resolved
	cur.UpdatedAt = now
	delete(s.byKey, IncidentID(cur.UnitID, cur.TargetStopID))
	return copyOf(cur)
}

// copyOf возвращает копию инцидента. Копия нужна во всех ответах наружу:
// вызывающий держит её, пока инцидент в хранилище продолжают обновлять, и
// без копии карточка в WebSocket тикала бы вслед за чужим прогнозом.
func copyOf(inc *Incident) *Incident {
	if inc == nil {
		return nil
	}
	out := *inc
	if inc.AckedAt != nil {
		t := *inc.AckedAt
		out.AckedAt = &t
	}
	if inc.ResolvedAt != nil {
		t := *inc.ResolvedAt
		out.ResolvedAt = &t
	}
	return &out
}

// newIncidentID делает идентификатор для нового эпизода. Постоянная часть
// идентификатора из машины и цели плюс порядковый номер эпизода: номер
// нужен именно для повторного прихода в красную зону.
func newIncidentID(seq uint64, p predictor.Prediction) string {
	return IncidentID(p.UnitID, p.TargetStopID) + "-" +
		strconv.FormatUint(seq, 10)
}

// Ack берёт инцидент оператором в работу. Уже взятый инцидент подтверждается
// повторно без ошибки: двойной клик в приборной панели не должен выглядеть
// как поломка, а смена оператора обязана иметь возможность подтвердить приём.
func (s *Incidents) Ack(id, by string) (Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inc, ok := s.byID[id]
	if !ok {
		return Incident{}, errIncidentNotFound
	}
	if inc.Status == StatusResolved {
		// Закрытый инцидент подтверждать нечего: он уже в истории.
		return *inc, errIncidentResolved
	}
	now := s.now()
	if inc.Status == StatusOpen {
		acked := now
		inc.AckedAt = &acked
		inc.AckedBy = by
		inc.Status = StatusAcked
		inc.UpdatedAt = now
		s.acks++
	}
	return *inc, nil
}

// Get возвращает инцидент по идентификатору.
func (s *Incidents) Get(id string) (Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inc, ok := s.byID[id]
	if !ok {
		return Incident{}, errIncidentNotFound
	}
	return *inc, nil
}

// List возвращает инциденты в выбранном статусе, новые первыми. При пустом
// статусе отдаются незакрытые.
func (s *Incidents) List(status IncidentStatus, limit int) []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Incident, 0, len(s.order))
	// Идём по order с конца: там самые новые, и лимит обычно мал.
	for i := len(s.order) - 1; i >= 0; i-- {
		inc, ok := s.byID[s.order[i]]
		if !ok {
			continue
		}
		if status == "" && inc.Status == StatusResolved {
			continue
		}
		if status != "" && inc.Status != status {
			continue
		}
		out = append(out, *inc)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].OpenedAt.After(out[j].OpenedAt)
	})
	return out
}

// IncidentCounters — накопительные счётчики, которые нельзя вывести из
// текущего содержимого хранилища: инциденты вытесняются, а счётчик
// подтверждений обязан помнить и вытесненные.
type IncidentCounters struct {
	// Created — сколько инцидентов открыто за всё время работы.
	Created uint64 `json:"created"`
	// Acked — сколько раз инцидент брали в работу.
	Acked uint64 `json:"acked"`
}

// Counters возвращает накопительные счётчики. Отдельный метод, потому что seq
// и acks живут под мьютексом и читать их напрямую из /metrics означало бы
// гонку данных, которая проявилась бы только под нагрузкой.
func (s *Incidents) Counters() IncidentCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return IncidentCounters{Created: s.seq, Acked: s.acks}
}

// Stats — счётчики инцидентов для /metrics.
type IncidentStats struct {
	// Total — сколько инцидентов в хранилище, включая закрытые.
	Total int `json:"total"`
	// Open — сколько сейчас открыто.
	Open int `json:"open"`
	// Acked — сколько взято в работу.
	Acked int `json:"acked"`
	// Resolved — сколько закрыто.
	Resolved int `json:"resolved"`
}

// Stats возвращает снимок счётчиков.
func (s *Incidents) Stats() IncidentStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st IncidentStats
	st.Total = len(s.byID)
	for _, inc := range s.byID {
		switch inc.Status {
		case StatusOpen:
			st.Open++
		case StatusAcked:
			st.Acked++
		case StatusResolved:
			st.Resolved++
		}
	}
	return st
}

// evictLocked вытесняет самый старый закрытый инцидент, а если закрытых нет —
// самый старый вообще. Открытый инцидент вытеснять нельзя: он про активный
// простой, и потерять его молча хуже, чем потерять архивную запись.
func (s *Incidents) evictLocked() {
	if len(s.byID) <= s.cap {
		return
	}
	for _, id := range s.order {
		inc, ok := s.byID[id]
		if !ok {
			continue
		}
		if inc.Status == StatusResolved {
			s.removeLocked(id)
			return
		}
	}
	for _, id := range s.order {
		if _, ok := s.byID[id]; ok {
			s.removeLocked(id)
			return
		}
	}
}

// removeLocked убирает инцидент из всех трёх карт. Убрать только из byID
// нельзя: в byKey осталась бы ссылка на удалённый инцидент, и следующий
// прогноз обновил бы то, чего уже нет. byUnit тоже обязателен — иначе
// вытесненный (уже удалённый из byID) инцидент остался бы текущим для
// машины, и следующая смена цели опубликовала бы Superseded для несуществующей
// записи: в дашборде появилось бы закрытие инцидента, которого там нет, а
// Ack по нему отвечал бы 404. Сверка по ID обязательна и здесь: на одну машину
// в byUnit может висеть уже более новый инцидент, и его трогать нельзя.
func (s *Incidents) removeLocked(id string) {
	inc, ok := s.byID[id]
	if !ok {
		return
	}
	delete(s.byID, id)
	key := IncidentID(inc.UnitID, inc.TargetStopID)
	if cur, ok := s.byKey[key]; ok && cur.ID == inc.ID {
		delete(s.byKey, key)
	}
	if cur, ok := s.byUnit[inc.UnitID]; ok && cur.ID == inc.ID {
		delete(s.byUnit, inc.UnitID)
	}
}

// Ошибки хранилища инцидентов. Раздельные, потому что по ним различают
// «инцидента нет» и «инцидент закрыт»: клиенту в первом случае положено 404,
// во втором — 409, и подставлять 404 на оба значит скрыть от оператора, что
// он подтверждает уже завершённый простой.
var (
	errIncidentNotFound = errors.New("инцидент не найден")
	errIncidentResolved = errors.New("инцидент уже закрыт")
)
