package predictor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
)

// DefaultStaleTTL — сколько прогноз модели может лежать в кэше и ещё считаться
// пригодным для показа. Пять минут — примерно два интервала планировщика по
// 300 с: за это время телеметрия накапливается, но машина не уехала
// настолько далеко, чтобы прежний ответ вводил в заблуждение.
const DefaultStaleTTL = 5 * time.Minute

// maxCached — сколько машин держим в кэше устаревших прогнозов. Ограничение
// нужно, чтобы вышедшие из смены машины не держали память до перезапуска.
const maxCached = 4096

// Fallback выбирает, чем ответить, когда модель недоступна.
//
// Цепочка ровно трёх ступеней, и порядок выбран по тому, что дороже всего
// обойтись неверным ответом:
//
//  1. модель — единственный источник, который что-то добавляет к cur_dev_s;
//  2. последний удачный прогноз этой машины, помеченный устаревшим: отвечает
//     на вопрос «что было известно минуту назад», что для оператора полезнее
//     нуля, и при этом честно показывает свою давность;
//  3. baseline — cur_dev_s без изменений, то есть единственный ответ,
//     который не может быть неправильным по построению.
//
// Четвёртой ступени нет намеренно. Средняя абсолютная ошибка baseline на
// validate — 93.46 с, и выдумывать между ним и нулём «сглаженное» значение
// значило бы прятать в ответе то, чего в нём нет. Пусть худший ответ на
// вопрос «на сколько машина опоздает» звучит как «на сколько уже отстаёт».
type Fallback struct {
	// aware позволяет отличить модель, умеющую сообщить об ошибке, от
	// заглушки: молча вернувший пустой прогноз предиктор — это неудача, а
	// не успех, и такой случай обязан уходить в деградацию.
	aware failureAware
	base  Predictor
	ttl   time.Duration
	now   func() time.Time

	// cache — последний успешный прогноз модели по каждой машине.
	cache map[uint32]Prediction
	mu    sync.Mutex
	// count — сколько прогнозов обслужено каждым способом. Доли нужны для
	// /metrics: доля деградации важнее абсолютного числа прогнозов,
	// потому что абсолютное растёт вместе с потоком машин. Счётчики
	// атомарные и лежат вне мьютекса кэша: прогнозы идут пачками, и
	// обновление статистики не должно ждать эвакуации кэша.
	nTotal atomic.Uint64
	nModel atomic.Uint64
	nCache atomic.Uint64
	nBase  atomic.Uint64
}

// failureAware — предиктор, способный сообщить, что не справился.
type failureAware interface {
	// predict возвращает ошибку вместо пустого прогноза.
	predict(ctx context.Context, f horizon.Frame) (Prediction, error)
}

// NewFallback собирает цепочку деградации. primary — модель, base — то, что
// ответит всегда. nil в любом из них заменяется рабочей заглушкой, чтобы
// отсутствие модели было штатной ситуацией, а не паникой на старте.
func NewFallback(primary, base Predictor, ttl time.Duration) *Fallback {
	if ttl <= 0 {
		ttl = DefaultStaleTTL
	}
	f := &Fallback{
		base:  base,
		ttl:   ttl,
		now:   time.Now,
		cache: make(map[uint32]Prediction),
	}
	if f.base == nil {
		f.base = BaselinePredictor{}
	}
	f.aware = asAware(primary)
	if f.aware == nil {
		// Модели нет вовсе. Подставлять baseline первичным нельзя: он
		// ответил бы успехом, Fallback счёл бы это работой модели, и
		// статистика деградации врала бы ровно тогда, когда важнее всего
		// сказать правду. Поэтому первичным становится предиктор, который
		// честно отказывает, а ответ даёт base.
		f.aware = unavailable{}
	}
	return f
}

// unavailable — модель не настроена. Отказ, а не успех с нулём.
type unavailable struct{}

func (unavailable) Predict(context.Context, horizon.Frame) Prediction {
	return Prediction{}
}

func (unavailable) predict(context.Context, horizon.Frame) (Prediction, error) {
	return Prediction{}, errNoModel
}

// errNoModel — сервис модели не сконфигурирован.
var errNoModel = errors.New("predictor: модель не настроена")

func asAware(p Predictor) failureAware {
	a, _ := p.(failureAware)
	return a
}

// Predict обслуживает кадр по всей цепочке и всегда даёт ответ.
func (f *Fallback) Predict(ctx context.Context, frame horizon.Frame) Prediction {
	f.nTotal.Add(1)

	if p, err := f.aware.predict(ctx, frame); err == nil {
		f.nModel.Add(1)
		f.remember(p)
		return p
	}
	if cached, ok := f.fromCache(frame.UnitID); ok {
		f.nCache.Add(1)
		// Ответ переписывается на копии: Source описывает, откуда пришёл
		// именно этот ответ, а в кэше лежит честный прогноз модели. Флаг
		// устаревания — свойство данных, а не способа ответа, поэтому он
		// остаётся отдельным полем.
		cached.Source = SourceCached
		cached.Stale = true
		return cached
	}
	p := f.base.Predict(context.WithoutCancel(ctx), frame)
	if p.Source == "" {
		p.Source = SourceBaseline
	}
	f.nBase.Add(1)
	return p
}

// InferenceWindow — предиктор, умеющий отдать окно замеров времени обращения
// к модели. Отдельный интерфейс по той же причине, что и Batcher: /metrics
// интересует эта способность, а не весь предиктор.
type InferenceWindow interface {
	// Inference возвращает квантили по окну замеров.
	Inference() latency.Quantiles
}

// Inference отдаёт окно замеров модели, если она в цепочке есть.
//
// Проброс нужен потому, что настроенная цепочка — это Fallback, а модель под
// ним: спрашивать про окно у цепочки правильно, спрашивать у клиента —
// значило бы знать, как именно цепочка собрана. У цепочки без модели окно
// пустое, и это честное «замеров не было», а не «модель отвечала мгновенно».
func (f *Fallback) Inference() latency.Quantiles {
	if w, ok := f.aware.(InferenceWindow); ok {
		return w.Inference()
	}
	return latency.Quantiles{}
}

// PredictBatch обслуживает пачку кадров по той же цепочке, что и Predict, но
// с одной попыткой вместо пачки.
//
// Отказ батча — не отказ модели. Планировщик отдал несколько кадров, чтобы
// сэкономить один сетевой круг, и если сервис не умеет батч или ответил
// невеждом, те же кадры надо обслужить по одному: терять их нельзя, потому
// что каждый из них ждёт в очереди по-настоящему. Поэтому путь отказа не
// сворачивается в отказ, а разворачивается обратно в поштучный Predict —
// где кэш и baseline уже отработаны и где счётчики считаются сами.
func (f *Fallback) PredictBatch(ctx context.Context, frames []horizon.Frame) ([]Prediction, error) {
	if len(frames) == 0 {
		return nil, nil
	}
	if b, ok := f.aware.(Batcher); ok {
		ps, err := b.PredictBatch(ctx, frames)
		if err == nil && len(ps) == len(frames) {
			// Успех пачкой: счётчики заводятся сразу на всё число кадров,
			// потому что ниже по одному они уже не считаются.
			f.nTotal.Add(uint64(len(frames)))
			f.nModel.Add(uint64(len(frames)))
			for _, p := range ps {
				f.remember(p)
			}
			return ps, nil
		}
	}
	ps := make([]Prediction, len(frames))
	for i, frame := range frames {
		ps[i] = f.Predict(ctx, frame)
	}
	return ps, nil
}

// remember кладёт успешный прогноз в кэш.
func (f *Fallback) remember(p Prediction) {
	if p.SampleID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.cache[p.UnitID]; exists {
		f.cache[p.UnitID] = p
		return
	}
	if len(f.cache) >= maxCached {
		f.evictLocked()
	}
	f.cache[p.UnitID] = p
}

// evictLocked выбрасывает запись с самым старым кадром. Перебор линейный, но
// он случается только когда кэш заполнен, а сравнение дат дешевле любого
// обращения к сети; очередь на вытеснение усложнила бы код без выигрыша.
func (f *Fallback) evictLocked() {
	oldest, found := uint32(0), false
	var oldestAt time.Time
	for id, p := range f.cache {
		if !found || p.AsOf.Before(oldestAt) {
			oldest, oldestAt, found = id, p.AsOf, true
		}
	}
	if found {
		delete(f.cache, oldest)
	}
}

// fromCache отдаёт последний прогноз машины, если он не старше ttl. Ответ
// сохраняет собственный SampleID и AsOf старого кадра: подставить к нему
// идентификатор текущего кадра нельзя, иначе клиент решит, что ответ
// получен для только что пришедшего кадра, а инцидент получит чужое время.
func (f *Fallback) fromCache(unit uint32) (Prediction, bool) {
	f.mu.Lock()
	p, ok := f.cache[unit]
	f.mu.Unlock()
	if !ok {
		return Prediction{}, false
	}
	if f.now().Sub(p.AsOf) > f.ttl {
		return Prediction{}, false
	}
	return p, true
}

// StatsFallback — счётчики деградации для /metrics.
type StatsFallback struct {
	// Total — сколько кадров обслужено.
	Total uint64
	// FromModel, FromCache, FromBase — сколько обслужено моделью, из
	// кэша устаревших прогнозов и baseline.
	FromModel uint64
	FromCache uint64
	FromBase  uint64
}

// Degraded возвращает, сколько прогнозов пришлось не на модель. Это доля,
// которую обязан видеть оператор: рост означает, что модель молчит.
func (s StatsFallback) Degraded() uint64 { return s.FromCache + s.FromBase }

// Stats возвращает снимок счётчиков.
func (f *Fallback) Stats() StatsFallback {
	return StatsFallback{
		Total:     f.nTotal.Load(),
		FromModel: f.nModel.Load(),
		FromCache: f.nCache.Load(),
		FromBase:  f.nBase.Load(),
	}
}

// Cached возвращает прогноз из кэша без проверки возраста. Нужен тестам и
// диагностике: посмотреть, что в кэше лежит, должно быть можно без
// притворства, что это пригодный для ответа прогноз.
func (f *Fallback) Cached(unit uint32) (Prediction, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.cache[unit]
	return p, ok
}
