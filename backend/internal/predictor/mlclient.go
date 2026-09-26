package predictor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/latency"
)

const (
	// DefaultTimeout — бюджет одной попытки. Обоснование из замеров: тик
	// конвейера 15 с, кадров на тик приходит по одному на машину, поэтому
	// тратить на модель больше трети секунды незачем.
	DefaultTimeout = 300 * time.Millisecond
	// DefaultRetries — число повторов после первой неудачной попытки.
	DefaultRetries = 2
	// DefaultFailureThreshold — столько неудач подряд, после которых
	// клиент перестаёт стучаться. Десять попыток по 300 мс — это три
	// секунды на каждую машину за тик; столько же ожидаемой реакции на
	// модель мысленно оставить нельзя.
	DefaultFailureThreshold = 10
	// DefaultCooldown — пауза перед пробой после размыкания.
	DefaultCooldown = 30 * time.Second
	// budgetMultiplier — во сколько раз общий бюджет превышает таймаут
	// одной попытки. Трёх попыток по 300 мс он покрывает впритык.
	budgetMultiplier = 3
	// maxBodyBytes — потолок тела ответа. Модель не должна присылать
	// мегабайты, а неограниченное чтение на недоверенном входе это плохая
	// идея.
	maxBodyBytes = 1 << 20
	// errorBodyBytes — сколько текста ошибки попадёт в лог.
	errorBodyBytes = 512
)

// ErrContractMismatch — модель обучена на другом наборе признаков.
var ErrContractMismatch = errors.New("набор признаков модели не совпадает с кадром")

// MLConfig — настройки обращения к сервису модели.
type MLConfig struct {
	// BaseURL — адрес сервиса, например http://localhost:8000.
	BaseURL string
	// Timeout — бюджет одной попытки. При неположительном DefaultTimeout.
	Timeout time.Duration
	// Retries — повторы. При nil берётся DefaultRetries, отрицательное
	// значение запрещает повторы вовсе.
	Retries *int
	// FailureThreshold — неудач подряд до размыкания. При неположительном
	// DefaultFailureThreshold.
	FailureThreshold int
	// Cooldown — пауза перед пробой. При неположительном DefaultCooldown.
	Cooldown time.Duration
	// HTTPClient при nil создаётся с транспортным таймаутом Timeout.
	// Прокси из переменных окружения намеренно не подхватывается: адрес
	// модели задаётся флагом, и подмешивать окружение в адрес значит
	// отправлять кадры не туда, куда сказали.
	HTTPClient *http.Client
	// Now подменяет часы в тестах. При nil time.Now.
	Now func() time.Time
}

// MLClient ходит в сервис модели по HTTP+JSON.
//
// gRPC не используется сознательно. Сервис модели пишет другой разработчик в
// другом репозитории, и общий протобаф-контракт между ними не существует:
// любая правка proto становится синхронным изменением в чужом репозитории.
// HTTP+JSON с полем features оставляет сверку контракта в рантайме, где
// расхождение видно сразу, а не на этапе сборки второй стороны.
type MLClient struct {
	cfg     MLConfig
	http    *http.Client
	timeout time.Duration
	// retries — число повторов, посчитанное один раз в конструкторе.
	retries int

	// mu защищает состояние контракта и автомата отказов. Отдельный
	// мьютекс, а не один широкий: гонять каждый кадр через один лок
	// конвейера нельзя.
	mu          sync.Mutex
	checked     bool
	checkedAt   time.Time
	contractErr error
	failures    int
	openUntil   time.Time
	probing     bool
	version     string
	// calls — успешных обращений, rejected — отклонённых автоматом.
	calls     uint64
	rejected  uint64
	lastErr   error
	startedAt time.Time

	// inference — окно замеров времени обращения к модели. Отдельное от
	// латентности гейтвея: та меряет весь путь «кадр пришёл → прогноз
	// доставлен», эта — только поход в модель. Разница между окнами и
	// есть ответ на вопрос «мы медленные или модель».
	inference *latency.Window

	// noBatch — сервис не умеет /predict/batch. Флаг, а не отсутствие
	// батчинга в конфигурации, потому что узнаётся в рантайме: сервис
	// модели пишется другим разработчиком, и его готовность к батчу
	// нельзя угадать на старте. Без флага каждый тик уходил бы в заведомо
	// мёртвый запрос, чтобы получить тот же 404, — то есть клиент сам
	// создавал бы нагрузку на сервис, который не сможет её обслужить
	// никогда.
	noBatch bool
}

// NewMLClient создаёт клиента. Пустая конфигурация допустима: клиент
// откажется работать и объяснит это, а не упадёт при старте.
func NewMLClient(cfg MLConfig) *MLClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = DefaultFailureThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	retries := DefaultRetries
	if cfg.Retries != nil {
		if *cfg.Retries < 0 {
			retries = 0
		} else {
			retries = *cfg.Retries
		}
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &MLClient{
		cfg:       cfg,
		http:      client,
		timeout:   cfg.Timeout,
		retries:   retries,
		startedAt: cfg.Now(),
		inference: latency.New(latency.DefaultWindow),
	}
}

// breakerState — состояние автомата отказов.
type breakerState string

const (
	breakerClosed   breakerState = "closed"
	breakerOpen     breakerState = "open"
	breakerHalfOpen breakerState = "half-open"
)

// Stats — снимок состояния клиента для /metrics и /readyz.
type Stats struct {
	// Breaker — closed, open или half-open.
	Breaker breakerState
	// Calls — успешных обращений.
	Calls uint64
	// Rejected — запросов, отклонённых разомкнутым автоматом.
	Rejected uint64
	// Failures — неудач подряд.
	Failures int
	// OpenUntil — до какого момента клиент не стучится. Нулевое время
	// означает, что автомат замкнут.
	OpenUntil time.Time
	// ContractErr — результат сверки имён признаков.
	ContractErr error
	// LastErr — последняя ошибка обращения.
	LastErr error
	// Version — версия модели по /model/info.
	Version string
	// Uptime — сколько клиент работает.
	Uptime time.Duration
}

// Stats возвращает снимок состояния.
func (c *MLClient) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Breaker:     c.stateLocked(),
		Calls:       c.calls,
		Rejected:    c.rejected,
		Failures:    c.failures,
		OpenUntil:   c.openUntil,
		ContractErr: c.contractErr,
		LastErr:     c.lastErr,
		Version:     c.version,
		Uptime:      c.cfg.Now().Sub(c.startedAt),
	}
}

// CheckContract сверяет имена признаков модели с контрактом кадра. Гейтвей
// зовёт это при старте, чтобы расхождение попало в лог и в /readyz, а не
// обнаружилось на первом прогнозе в проде.
func (c *MLClient) CheckContract(ctx context.Context) error {
	return c.fetchInfoChecked(ctx)
}

// Predict удовлетворяет Predictor и наружу ошибку не отдаёт: отказ модели
// разбирает Fallback. Подробности остаются в Stats.
func (c *MLClient) Predict(ctx context.Context, f horizon.Frame) Prediction {
	p, err := c.predict(ctx, f)
	if err != nil {
		return Prediction{}
	}
	return p
}

// predict — одиночный прогноз модели.
func (c *MLClient) predict(ctx context.Context, f horizon.Frame) (Prediction, error) {
	ps, err := c.call(ctx, 1, func(inner context.Context) ([]Prediction, error) {
		p, err := c.attemptOne(inner, f)
		if err != nil {
			return nil, err
		}
		return []Prediction{p}, nil
	})
	if err != nil {
		return Prediction{}, err
	}
	return ps[0], nil
}

// PredictBatch удовлетворяет Batcher.
//
// Батчем идёт только то, что к моменту отправки лежит в очереди целиком. Если
// кадр один, уходит одиночный POST /predict: держать отдельный путь ради
// одиночного кадра незачем, а заодно это делает клиент совместимым с сервисом,
// который /predict/batch ещё не умеет.
func (c *MLClient) PredictBatch(ctx context.Context, frames []horizon.Frame) ([]Prediction, error) {
	switch len(frames) {
	case 0:
		return nil, nil
	case 1:
		p, err := c.predict(ctx, frames[0])
		if err != nil {
			return nil, err
		}
		return []Prediction{p}, nil
	}
	return c.predictMany(ctx, frames)
}

// predictMany — батчевый прогноз с проверкой контракта, автоматом отказов и
// повторами.
func (c *MLClient) predictMany(ctx context.Context, frames []horizon.Frame) ([]Prediction, error) {
	if c.batchUnsupported() {
		return nil, errNoBatch
	}
	return c.call(ctx, len(frames), func(inner context.Context) ([]Prediction, error) {
		return c.attemptBatch(inner, frames)
	})
}

// fetch — одна попытка: собрать запрос, отправить, разобрать ответ.
type fetch func(context.Context) ([]Prediction, error)

// call — общая обвязка обращения к модели: сверка контракта, автомат отказов,
// бюджет времени и повторы. Один и тот же путь для одиночного кадра и для
// батча: правила отказа и повтора не должны расходиться между ними, иначе
// модель, которую перестали считать отказчивой по одному пути, продолжит
// получать по три попытки по другому.
//
// n — сколько прогнозов ожидается. Проверка количества здесь, а не в
// разборе ответа, потому что обязана защищать оба пути: одиночный ответ без
// delta_s — это не «ноль», это отсутствие ответа.
func (c *MLClient) call(ctx context.Context, n int, f fetch) ([]Prediction, error) {
	now := c.cfg.Now()
	// Сверка контракта идёт первыми: при несовпадении лучше не делать ни
	// одного запроса прогноза, чем сделать и получить правдоподобную
	// неправду.
	if err := c.ensureContract(ctx, now); err != nil {
		return nil, err
	}
	if !c.allow(now) {
		c.recordRejected()
		return nil, errBreakerOpen
	}

	budget := c.timeout * budgetMultiplier
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < budget {
			budget = left
		}
	}
	inner, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Замер идёт по настоящим часам, а не по подменённым cfg.Now: те
	// подменяют время, чтобы автомат отказов считался детерминированно, а
	// здесь измеряется реальный сетевой круг. В замер входят и повторы:
	// если на ответ ушло три попытки, модель была медленной трижды, и
	// скрыть это, оставив в квантилях только последнюю, значило бы
	// отпрятать худший случай.
	started := time.Now()

	attempts := c.retries + 1
	var lastErr error
	for i := range attempts {
		ps, err := f(inner)
		if err == nil && len(ps) != n {
			// Ответ неполный или лишний. Повтор не поможет: сервис ответил
			// и ответил неправильно, те же два запроса дадут то же самое.
			// В автомат отказов это тоже не идёт: размыкание предназначено
			// для недоступности, а не для рассинхронизации контракта, и
			// разомкнутый автомат скрыл бы вторую, куда более важную
			// неисправность. Ошибка попадает в LastErr, дальше Fallback
			// пройдёт эти кадры по одному и увидит, что не так.
			err = fmt.Errorf("ml: %w: получили %d прогнозов вместо %d",
				ErrContractMismatch, len(ps), n)
			c.noteErr(err)
			return nil, err
		}
		if err == nil {
			c.recordSuccess()
			elapsed := time.Since(started)
			c.inference.ObserveDuration(elapsed)
			for i := range ps {
				// У всех кадров батча один и тот же замер: это время одного
				// обращения, а не отдельный ответ на каждый кадр. Приписать
				// каждому кадру его долю от общего времени значило бы
				// изобрести несуществующее измерение.
				ps[i].Latency = elapsed
			}
			return ps, nil
		}
		lastErr = err
		if !retryable(err) {
			// 4xx — это наша ошибка, а не временная беда модели. Считать
			// её неудачей модели незачем: автомат из-за опечатки в запросе
			// разомкнётся и лечить будет нечем.
			c.recordFailure(err, now)
			return nil, err
		}
		if i+1 == attempts {
			break
		}
		// Пауза перед повтором растёт: если сервис падает под нагрузкой,
		// мгновенный повтор добавит к ней ещё один.
		pause := c.timeout * time.Duration(1<<uint(i)) / 2
		select {
		case <-inner.Done():
			c.recordFailure(fmt.Errorf("%w: %w", inner.Err(), err), now)
			return nil, fmt.Errorf("ml: %w", err)
		case <-time.After(pause):
		}
	}
	c.recordFailure(lastErr, now)
	return nil, fmt.Errorf("ml: %w", lastErr)
}

// attemptOne — одна попытка одиночным запросом: собрать кадр, отправить,
// разобрать ответ.
func (c *MLClient) attemptOne(ctx context.Context, f horizon.Frame) (Prediction, error) {
	body, err := json.Marshal(c.request(f))
	if err != nil {
		return Prediction{}, fmt.Errorf("ml: не собрать запрос: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/predict"), bytes.NewReader(body))
	if err != nil {
		return Prediction{}, fmt.Errorf("ml: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Prediction{}, fmt.Errorf("ml: %w", err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return Prediction{}, &statusError{code: resp.StatusCode, body: snippet(resp.Body)}
	}
	var out predictResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&out); err != nil {
		return Prediction{}, fmt.Errorf("ml: не разобрать ответ: %w", err)
	}
	return c.prediction(f, out)
}

// attemptBatch — одна попытка батчем.
func (c *MLClient) attemptBatch(ctx context.Context, frames []horizon.Frame) ([]Prediction, error) {
	reqs := make([]predictRequest, len(frames))
	// Порядок кадров запоминается по sample_id, а не по позиции: ответ
	// сопоставляется по имени кадра. Сервис вправе вернуть ответы в любом
	// порядке — например, отсортировав по времени обработки, — и при
	// сопоставлении по индексу прогноз одной машины приклеился бы к другой.
	// На городской сетке это неверное время ожидания пассажира, у которого
	// под рукой нет способа это перепроверить, поэтому цена ошибки тут
	// выше, чем у любой другой ошибки разбора ответа.
	index := make(map[string]int, len(frames))
	for i, f := range frames {
		reqs[i] = c.request(f)
		if _, dup := index[f.SampleID]; dup {
			return nil, fmt.Errorf("ml: %w: кадр %q повторился в батче",
				ErrContractMismatch, f.SampleID)
		}
		index[f.SampleID] = i
	}

	body, err := json.Marshal(batchRequest{Frames: reqs})
	if err != nil {
		return nil, fmt.Errorf("ml: не собрать батч: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/predict/batch"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ml: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ml: %w", err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		code := resp.StatusCode
		// Отсутствие самого эндпоинта — не беда, а недостающая
		// оптимизация: одиночный путь отвечает, и клиент продолжит
		// работать. Помечаем один раз и больше не спрашиваем.
		if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
			c.markBatchUnsupported()
		}
		return nil, &statusError{code: code, body: snippet(resp.Body)}
	}
	var out batchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("ml: не разобрать батч: %w", err)
	}
	return c.mapBatch(frames, index, out)
}

// mapBatch раскладывает ответы батча по кадрам.
//
// Несовпадение по составу — отказ, а не попытка догадаться: лишний или
// недостающий ответ означает, что сервис посчитал не то, о чём его просили, и
// любая догадка здесь превратится в чужое время ожидания. Fallback после
// отказа пройдёт эти кадры по одному, где расхождение видно сразу.
func (c *MLClient) mapBatch(frames []horizon.Frame, index map[string]int, out batchResponse) ([]Prediction, error) {
	slots := make([]*Prediction, len(frames))
	for _, r := range out.Predictions {
		i, ok := index[r.SampleID]
		if !ok {
			return nil, fmt.Errorf("ml: %w: в ответе батча неизвестный кадр %q",
				ErrContractMismatch, r.SampleID)
		}
		if slots[i] != nil {
			return nil, fmt.Errorf("ml: %w: кадр %q в ответе батча продублирован",
				ErrContractMismatch, r.SampleID)
		}
		p, err := c.prediction(frames[i], r)
		if err != nil {
			return nil, err
		}
		slots[i] = &p
	}
	ps := make([]Prediction, len(frames))
	for i, p := range slots {
		if p == nil {
			return nil, fmt.Errorf("ml: %w: в ответе батча нет кадра %q",
				ErrContractMismatch, frames[i].SampleID)
		}
		ps[i] = *p
	}
	return ps, nil
}

// request — тело одного кадра. Имена признаков едут вместе со значениями:
// модель обязана разбирать кадр по именам, а не по позициям, иначе
// перестановка признаков в Go станет молчаливой порчей данных.
func (c *MLClient) request(f horizon.Frame) predictRequest {
	return predictRequest{
		SampleID:     f.SampleID,
		TRID:         f.TRID,
		UnitID:       f.UnitID,
		T:            f.AsOf,
		TargetStopID: f.PrimaryStopID(),
		HorizonS:     f.HorizonS(),
		Ambiguous:    f.Ambiguous,
		Features:     f.Values,
	}
}

// prediction — разбор одного ответа модели. Общий для одиночного запроса и
// для каждого элемента батча: если бы правила проверки ответа жили в двух
// местах, они разошлись бы при первой же правке одного из них.
func (c *MLClient) prediction(f horizon.Frame, out predictResponse) (Prediction, error) {
	// Обязательные поля проверяются указателями: нулевой прогноз и
	// отсутствующее поле — разные вещи, а без проверки модель, забывшая
	// поле, отвечала бы уверенным «опоздания нет».
	if out.DelayS == nil {
		return Prediction{}, errors.New("ml: в ответе нет delay_s")
	}
	p := Prediction{
		SampleID:      f.SampleID,
		UnitID:        f.UnitID,
		TRID:          f.TRID,
		AsOf:          f.AsOf,
		TargetStopID:  f.PrimaryStopID(),
		HorizonS:      f.HorizonS(),
		PredictedDevS: *out.DelayS,
		ModelVersion:  out.ModelVersion,
		Source:        SourceML,
		Missing:       f.Missing(),
	}
	// Добавка выводится из ответа: дельту модель вернула внутри delay_s,
	// вычитаем измеренное отклонение. Если cur_dev_s не измерен, вычитать
	// нечего и DeltaS остаётся нулём — итог в PredictedDevS при этом
	// честный, а неполноту кадра видно по Missing.
	if dev, ok := f.Value("cur_dev_s"); ok {
		p.CurDevS = &dev
		p.DeltaS = *out.DelayS - dev
	}
	if out.PLate != nil {
		if *out.PLate < 0 || *out.PLate > 1 {
			return Prediction{}, fmt.Errorf("ml: p_late = %g вне [0, 1]", *out.PLate)
		}
		p.PLate = *out.PLate
		p.HasPLate = true
	}
	// Причина едет текстом от сервиса; обрезаем по руне, а не по байту:
	// строки русские, и обрезка посреди UTF-8 превратила бы объяснение в
	// кракозябры на дашборде.
	if r := []rune(out.Reason); len(r) > 160 {
		p.Reason = string(r[:160])
	} else {
		p.Reason = out.Reason
	}
	return p, nil
}

// predictRequest — тело POST /predict. Имена признаков едут вместе со
// значениями: модель обязана разбирать кадр по именам, а не по позициям,
// иначе перестановка признаков в Go станет молчаливой порчей данных.
type predictRequest struct {
	SampleID     string              `json:"sample_id"`
	TRID         int64               `json:"tr_id"`
	UnitID       uint32              `json:"unit_id"`
	T            time.Time           `json:"t"`
	TargetStopID int64               `json:"target_stop_id"`
	HorizonS     float64             `json:"horizon_s"`
	Ambiguous    bool                `json:"target_ambiguous"`
	Features     map[string]*float64 `json:"features"`
}

// MarshalJSON пишет запрос ПЛОСКИМ словарём: имена признаков — ключами
// верхнего уровня (контракт §4.5, `extra='allow'` на стороне FastAPI:
// extras и есть фичи). Вложенный «features» сервер валидации не проходит,
// и это молча проверялось только собственными фейками каждой стороны.
// null отправляется ЯВНЫМ null, а не выкидыванием ключа: присутствие имени — то,
// что сверяет сервер, а CatBoost понимает null как пропуск (так же, как
// учился на null-колонках). Только cur_dev_s — исключение: без него delta
// не собрать (ADR-0007), поэтому при null ключ отсутствует и запрос
// честно уходит на 422 → baseline.
func (r predictRequest) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(r.Features)+7)
	for name, v := range r.Features {
		if v != nil {
			out[name] = *v
		} else if name != "cur_dev_s" {
			out[name] = nil
		}
	}
	out["sample_id"] = r.SampleID
	out["tr_id"] = r.TRID
	out["unit_id"] = r.UnitID
	out["t"] = r.T
	out["target_stop_id"] = r.TargetStopID
	out["target_ambiguous"] = r.Ambiguous
	out["horizon_s"] = r.HorizonS
	if v := r.Features["cur_dev_s"]; v != nil {
		out["cur_dev_s"] = *v
	} else {
		delete(out, "cur_dev_s")
	}
	return json.Marshal(out)
}

// UnmarshalJSON читает плоский dict обратно — зеркало MarshalJSON. Нужен
// не продакшену (сервер модели — python), а мокам: фейк обязан видеть
// запрос ровно в той форме, в какой его видит FastAPI, а не в виде
// внутреннего структа клиента.
func (r *predictRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	meta := []struct {
		key string
		dst any
	}{
		{"sample_id", &r.SampleID},
		{"tr_id", &r.TRID},
		{"unit_id", &r.UnitID},
		{"t", &r.T},
		{"target_stop_id", &r.TargetStopID},
		{"target_ambiguous", &r.Ambiguous},
	}
	for _, m := range meta {
		if v, ok := raw[m.key]; ok {
			if err := json.Unmarshal(v, m.dst); err != nil {
				return fmt.Errorf("ml: поле %q: %w", m.key, err)
			}
			delete(raw, m.key)
		}
	}
	// horizon_s и cur_dev_s — одновременно и выделенные поля, и признаки
	// контракта: забираем их в Features, а горизонт ещё и в поле.
	if v, ok := raw["horizon_s"]; ok {
		var h float64
		if err := json.Unmarshal(v, &h); err != nil {
			return fmt.Errorf("ml: поле %q: %w", "horizon_s", err)
		}
		r.HorizonS = h
	}
	r.Features = make(map[string]*float64, len(raw))
	for name, v := range raw {
		if string(v) == "null" {
			r.Features[name] = nil
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			return fmt.Errorf("ml: фича %q: %w", name, err)
		}
		r.Features[name] = &f
	}
	return nil
}

// predictResponse — один ответ сервиса модели (одиночный /predict или
// элемент батча). Ключевое расхождение, которое этот разбор закрывает:
// сервис по ADR-0007 складывает прогноз сам и отдаёт ГОТОВЫЙ delay_s, а
// не сырую добавку; ранняя версия клиента ждала delta_s и молча считала
// каждый ответ ошибкой (все прогнозы уходили в baseline).
type predictResponse struct {
	// SampleID — кадр, к которому относится ответ. В одиночном ответе поле
	// необязательно: там запрос и ответ один, и кадр известен на стороне
	// клиента. В батче оно обязательно и служит ключом сопоставления —
	// без него порядок ответов пришлось бы угадывать.
	SampleID string `json:"sample_id,omitempty"`
	// DelayS — итоговое предсказанное отклонение в секундах. По ADR-0007
	// сервис модели складывает cur_dev_s и выученную добавку сам и наружу
	// отдаёт уже сумму; сырой дельты в проволке нет.
	DelayS *float64 `json:"delay_s"`
	// PLate — вероятность опоздания в [0, 1]. Может прийти null:
	// классификатор подключается флагом --late-model (v4, §5.1), без него
	// сервис честно отвечает null, и risk считается только по секундам.
	PLate *float64 `json:"p_late"`
	// Reason — предполагаемая причина прогноза (правила §5.4 на ML-стороне).
	// Null/пусто допустимы: у fallback-ответа объяснения нет.
	Reason string `json:"reason"`
	// ModelVersion — версия модели. Пустая допустима: версия нужна для
	// разбора инцидентов, но её отсутствие прогнозу не мешает.
	ModelVersion string `json:"model_version"`
}

// batchRequest — тело POST /predict/batch.
//
// Форма кадра здесь ровно та же, что и в одиночном predictRequest. Отдельная
// схема для батча была бы вторым местом, где живёт описание признаков, и
// рано или поздно две схемы разошлись бы: в одном наборе появился бы признак,
// а в другом нет, и расхождение поймал бы только model/info, уже после того
// как прогнозы поехали бы мимо.
type batchRequest struct {
	Frames []predictRequest `json:"frames"`
}

// batchResponse — ответ POST /predict/batch.
type batchResponse struct {
	Predictions []predictResponse `json:"predictions"`
}

// modelInfo — ответ GET /model/info.
type modelInfo struct {
	Version      string   `json:"version"`
	FeatureNames []string `json:"features"`
}

// ModelInfo — публичный вид ответа GET /model/info для панели дашборда.
// От внутреннего modelInfo отличается набором: тому нужны имена признаков
// для сверки контракта, а панели — паспорт обучения (версия, дата, метрики
// качества). Разделение намеренное: сверка имён здесь не выполняется и не
// должна — паспорт интересен именно когда контракт расходится, и прятать
// его из-за расхождения значило бы оставить панель слепой в тот момент,
// когда на неё смотрят.
type ModelInfo struct {
	Version      string             `json:"version"`
	Target       string             `json:"target,omitempty"`
	TrainedAt    string             `json:"trained_at,omitempty"`
	MAE          map[string]float64 `json:"mae,omitempty"`
	FeatureCount int                `json:"feature_count"`
	// Late — паспорт парного P(late)-классификатора (v4); nil, когда голова
	// не подключена: «нет модели вероятностей» и «вероятность нулевая» —
	// разные вещи, и панель обязана различать их так же, как проволка.
	Late *LateInfo `json:"late,omitempty"`
}

// LateInfo — то из паспорта классификатора, что смотрит диспетчер.
type LateInfo struct {
	Version      string   `json:"version"`
	ThresholdS   float64  `json:"threshold_s"`
	AUC          *float64 `json:"auc_holdout"`
	PositiveRate *float64 `json:"positive_rate_holdout"`
}

// ModelInfo сходит в сервис за паспортом модели. В отличие от fetchInfo
// имена не проверяет (см. ModelInfo) и не кэширует результат: это редкий
// запрос панели, а не горячий путь, и кэш показал бы версию, пережившую
// /model/reload.
func (c *MLClient) ModelInfo(ctx context.Context) (ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/model/info"), nil)
	if err != nil {
		return ModelInfo{}, fmt.Errorf("ml: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ModelInfo{}, fmt.Errorf("ml: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return ModelInfo{}, &statusError{code: resp.StatusCode, body: snippet(resp.Body)}
	}
	var info ModelInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&info); err != nil {
		return ModelInfo{}, fmt.Errorf("ml: не разобрать /model/info: %w", err)
	}
	return info, nil
}

// fetchInfo сходит в сервис за объявлением модели. Проверка контракта из неё
// убрана намеренно: сверка общего состояния клиента из этой функции не
// вытекает и приводит к блокировкам, которых здесь не нужно.
func (c *MLClient) fetchInfo(ctx context.Context) (modelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/model/info"), nil)
	if err != nil {
		return modelInfo{}, fmt.Errorf("ml: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return modelInfo{}, fmt.Errorf("ml: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return modelInfo{}, &statusError{code: resp.StatusCode, body: snippet(resp.Body)}
	}
	var info modelInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&info); err != nil {
		return modelInfo{}, fmt.Errorf("ml: не разобрать /model/info: %w", err)
	}
	if err := checkNames(info.FeatureNames); err != nil {
		return info, err
	}
	return info, nil
}

// checkNames сверяет объявленные моделью имена с контрактом кадра. Порядок не
// важен — важно множество.
//
// Сверка односторонняя, и это ровно то, что сервис уже документирует на своей
// стороне (predictor/serve.py, module docstring): запрос может быть надмножеством
// того, что нужно модели, поэтому модель, обученная на подмножестве признаков
// кадра, законна и «недостающих» имён ошибкой не считается. Обратное неверно:
// признака, которого в кадре нет, в рантайме не появится, и сервис ответит
// 422 на каждый кадр — молчаливый отказ прогнозировать хуже явной ошибки
// контракта при старте.
//
// Расхождением остаётся лишний у модели признак и дубль: дубль означает, что
// модель считает одно и то же имя двумя разными, и согласиться с этим нельзя.
func checkNames(got []string) error {
	if len(got) == 0 {
		return fmt.Errorf("%w: модель не объявила ни одного признака", ErrContractMismatch)
	}
	want := slices.Clone(contractNames)
	sort.Strings(want)
	have := slices.Clone(got)
	sort.Strings(have)

	var extra []string
	for _, n := range have {
		if !slices.Contains(want, n) {
			extra = append(extra, n)
		}
	}
	if dup := duplicates(got); len(dup) > 0 {
		extra = append(extra, dup...)
	}
	if len(extra) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(ErrContractMismatch.Error())
	b.WriteString("; лишние у модели: " + strings.Join(extra, ", "))
	b.WriteString("; в кадре есть только: " + strings.Join(want, ", "))
	return errors.New(b.String())
}

func duplicates(names []string) []string {
	seen := make(map[string]int, len(names))
	for _, n := range names {
		seen[n]++
	}
	var out []string
	for n, c := range seen {
		if c > 1 {
			out = append(out, n+" (×"+strconv.Itoa(c)+")")
		}
	}
	sort.Strings(out)
	return out
}

// errBreakerOpen — автомат разомкнут, запрос не отправлен.
var errBreakerOpen = errors.New("ml: клиент разомкнут после серии неудач")

// errNoBatch — сервис модели не умеет батч. Не беда: одиночный путь работает,
// батчинг просто выключен.
var errNoBatch = errors.New("ml: сервис не поддерживает /predict/batch")

// ensureContract сверяет имена признаков, не чаще чем раз в cooldown: иначе
// каждый кадр при недоступном сервисе шёл бы ещё и за /model/info, удваивая
// давление на модель именно тогда, когда ей тяжело.
func (c *MLClient) ensureContract(ctx context.Context, now time.Time) error {
	c.mu.Lock()
	if c.checked && c.contractErr == nil {
		c.mu.Unlock()
		return nil
	}
	if c.checked && now.Sub(c.checkedAt) < c.cfg.Cooldown {
		err := c.contractErr
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	return c.fetchInfoChecked(ctx)
}

// fetchInfoChecked ходит за /model/info и запоминает результат сверки вместе
// с версией модели. Синхронизация вынесена наружу, чтобы и стартовая проверка,
// и ленивая перед первым прогнозом смотрели в одно и то же состояние.
func (c *MLClient) fetchInfoChecked(ctx context.Context) error {
	info, err := c.fetchInfo(ctx)
	now := c.cfg.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = true
	c.checkedAt = now
	c.contractErr = err
	if err == nil {
		c.version = info.Version
	}
	return err
}

func (c *MLClient) recordRejected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejected++
}

// noteErr запоминает ошибку, не считая её неудачей модели.
//
// Так помечаются расхождения контракта: модель ответила, и ответ неверен —
// это не то же самое, что модель не ответила, и автомат отказов, задуманный на
// недоступность, разомкнулся бы на том, что лечится правкой контракта, а не
// ожиданием. При этом молчать об ошибке нельзя: она уедет в LastErr, /readyz
// и в лог, иначе рассинхронизация обнаружится по косвенным признакам —
// неверным временем ожидания на линии.
func (c *MLClient) noteErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err
}

// Inference отдаёт окно замеров обращения к модели: сколько занимал поход в
// ml-core без учёта остального пути. Гейтвей забирает снимок в /metrics.
func (c *MLClient) Inference() latency.Quantiles {
	return c.inference.Snapshot()
}

// batchUnsupported — сервис уже сказал, что /predict/batch у него нет.
func (c *MLClient) batchUnsupported() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.noBatch
}

func (c *MLClient) markBatchUnsupported() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noBatch = true
}

// allow решает, можно ли стучаться. После серии неудач клиент молчит до
// конца cooldown, затем выпускает ровно одну пробу: если сервис вернулся,
// дальше работает как обычно, если нет — молчание продлевается.
func (c *MLClient) allow(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openUntil.IsZero() {
		return true
	}
	if now.Before(c.openUntil) {
		return false
	}
	if c.probing {
		// Проба уже идёт; вторая не нужна.
		return false
	}
	c.probing = true
	return true
}

func (c *MLClient) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.failures = 0
	c.probing = false
	c.openUntil = time.Time{}
	c.lastErr = nil
}

// recordFailure считает неудачу. Проба, неудавшаяся после cooldown, немедленно
// размыкает автомат заново: сервис не поднялся.
func (c *MLClient) recordFailure(err error, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err
	if c.probing {
		c.openUntil = now.Add(c.cfg.Cooldown)
		c.probing = false
		c.failures = c.cfg.FailureThreshold
		return
	}
	c.failures++
	if c.failures >= c.cfg.FailureThreshold {
		c.openUntil = now.Add(c.cfg.Cooldown)
		c.probing = false
	}
}

func (c *MLClient) stateLocked() breakerState {
	if c.openUntil.IsZero() {
		return breakerClosed
	}
	if c.probing {
		return breakerHalfOpen
	}
	if c.cfg.Now().Before(c.openUntil) {
		return breakerOpen
	}
	return breakerClosed
}

// statusError — неуспешный ответ сервиса модели.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return "ml: ответ " + strconv.Itoa(e.code) + ": " + e.body
}

// retryable отличает «попробуй ещё раз» от «не трать попытки». Повтор имеет
// смысл на сетевых сбоях, 429 и 5xx. 4xx означает, что запрос неправильный:
// повтор лишь добавит нагрузку на сервис, который и так отвечает ошибкой.
// Расхождение контракта повтор не исправит тоже: сервис ответит так же
// ровно, а ещё две попытки лишь задержат переход к поштучному пути.
func retryable(err error) bool {
	if errors.Is(err, ErrContractMismatch) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return true
}

// snippet возвращает начало тела ответа: оно попадёт в лог и в /readyz, и
// неожиданный HTML от прокси там полезнее пустоты.
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, errorBodyBytes))
	return strings.TrimSpace(string(b))
}

// drain вычитывает и закрывает тело. Без этого соединение не вернётся в
// пул, и каждый прогноз оставит после себя один в TIME_WAIT.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyBytes))
	resp.Body.Close()
}

// url собирает адрес. Без настроенного адреса клиент обязан быть честно
// сломанным, а не стучаться в относительный путь, который net/http не
// примет, и не ходить в localhost наугад.
func (c *MLClient) url(path string) string {
	base := strings.TrimRight(c.cfg.BaseURL, "/")
	if base == "" {
		return "http://ml-unconfigured.invalid" + path
	}
	return base + path
}
