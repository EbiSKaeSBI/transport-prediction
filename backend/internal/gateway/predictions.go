package gateway

import (
	"sync"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

// DefaultPredictionCap — сколько прогнозов держим. Считано из сетки: при
// тике 15 с и 30 машинах за смену набегает около 7200 прогнозов за 60 минут,
// то есть 2048 покрывает примерно 17 минут парка. Этого хватает, чтобы
// открыть прогноз по машине, ушедшей из сети, и не держать память на горизонте
// смены, который читает отдельный инструмент.
const DefaultPredictionCap = 2048

// Predictions — ограниченное хранилище последних прогнозов.
type Predictions struct {
	byID  map[string]predictor.Prediction
	order []string
	cap   int
	now   func() time.Time

	mu    sync.Mutex
	total uint64
}

// NewPredictions создаёт хранилище. Неположительная вместимость берёт
// DefaultPredictionCap.
func NewPredictions(capacity int) *Predictions {
	if capacity <= 0 {
		capacity = DefaultPredictionCap
	}
	return &Predictions{
		byID: make(map[string]predictor.Prediction, capacity),
		cap:  capacity,
		now:  time.Now,
	}
}

// Put сохраняет прогноз. Ключ — SampleID, который детерминирован по (машина,
// момент времени), поэтому повторный прогноз того же кадра обновляет запись,
// а не плодит новую.
func (s *Predictions) Put(p predictor.Prediction) {
	if p.SampleID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if _, exists := s.byID[p.SampleID]; !exists {
		s.order = append(s.order, p.SampleID)
	}
	s.byID[p.SampleID] = p
	s.evictLocked()
}

// Get возвращает прогноз по идентификатору образца.
func (s *Predictions) Get(sampleID string) (predictor.Prediction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[sampleID]
	return p, ok
}

// Latest возвращает последний прогноз машины. Обход порядка с конца, а не
// отдельный индекс по машинам: индекс стоило бы держать в памяти и чистить на
// каждом вытеснении, а обход нужен только на запрос в карточку, который
// делает человек.
func (s *Predictions) Latest(unitID uint32) (predictor.Prediction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.order) - 1; i >= 0; i-- {
		p, ok := s.byID[s.order[i]]
		if !ok {
			continue
		}
		if p.UnitID == unitID {
			return p, true
		}
	}
	return predictor.Prediction{}, false
}

// Stats — счётчики хранилища для /metrics.
type PredictionStats struct {
	// Stored — сколько прогнозов в хранилище.
	Stored int `json:"stored"`
	// Total — сколько прошло за всё время.
	Total uint64 `json:"total"`
	// Capacity — вместимость.
	Capacity int `json:"capacity"`
}

// Stats возвращает снимок счётчиков.
func (s *Predictions) Stats() PredictionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PredictionStats{Stored: len(s.byID), Total: s.total, Capacity: s.cap}
}

// evictLocked вытесняет самые старые записи до вместимости. Порядок
// добавления достаточно близок к порядку времени: прогнозы кладутся в том же
// тике, в котором посчитаны, а сортировка по времени на каждой вставке
// стоила бы больше, чем вытеснение по порядку.
func (s *Predictions) evictLocked() {
	// Подсчёт проходов, а не срез: order может содержать и уже вытесненные
	// идентификаторы, и их надо пропускать.
	for len(s.byID) > s.cap {
		if len(s.order) == 0 {
			return
		}
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, oldest)
	}
}
