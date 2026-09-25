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
// HTTP+JSON с полем feature_names оставляет сверку контракта в рантайме, где
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

// predict — прогноз модели с проверкой контракта, автоматом отказов и
// повторами.
func (c *MLClient) predict(ctx context.Context, f horizon.Frame) (Prediction, error) {
	now := c.cfg.Now()
	// Сверка контракта идёт первыми: при несовпадении лучше не делать ни
	// одного запроса прогноза, чем сделать и получить правдоподобную
	// неправду.
	if err := c.ensureContract(ctx, now); err != nil {
		return Prediction{}, err
	}
	if !c.allow(now) {
		c.recordRejected()
		return Prediction{}, errBreakerOpen
	}

	budget := c.timeout * budgetMultiplier
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < budget {
			budget = left
		}
	}
	inner, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	attempts := c.retries + 1
	var lastErr error
	for i := range attempts {
		p, err := c.attempt(inner, f)
		if err == nil {
			c.recordSuccess()
			return p, nil
		}
		lastErr = err
		if !retryable(err) {
			// 4xx — это наша ошибка, а не временная беда модели. Считать
			// её неудачей модели незачем: автомат из-за опечатки в запросе
			// разомкнётся и лечить будет нечем.
			c.recordFailure(err, now)
			return Prediction{}, err
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
			return Prediction{}, fmt.Errorf("ml: %w", err)
		case <-time.After(pause):
		}
	}
	c.recordFailure(lastErr, now)
	return Prediction{}, fmt.Errorf("ml: %w", lastErr)
}

// attempt — одна попытка: собрать запрос, отправить, разобрать ответ.
func (c *MLClient) attempt(ctx context.Context, f horizon.Frame) (Prediction, error) {
	body, err := json.Marshal(predictRequest{
		SampleID:     f.SampleID,
		TRID:         f.TRID,
		UnitID:       f.UnitID,
		T:            f.AsOf,
		TargetStopID: f.PrimaryStopID(),
		HorizonS:     f.HorizonS(),
		Ambiguous:    f.Ambiguous,
		Features:     f.Values,
	})
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
	// Обязательные поля проверяются указателями: нулевая добавка и
	// отсутствующее поле — разные вещи, а без проверки модель, забывшая
	// поле, отвечала бы уверенным «опоздания нет».
	if out.DeltaS == nil || out.PLate == nil {
		return Prediction{}, errors.New("ml: в ответе нет delta_s или p_late")
	}
	if *out.PLate < 0 || *out.PLate > 1 {
		return Prediction{}, fmt.Errorf("ml: p_late = %g вне [0, 1]", *out.PLate)
	}
	p := Prediction{
		SampleID:     f.SampleID,
		UnitID:       f.UnitID,
		TRID:         f.TRID,
		AsOf:         f.AsOf,
		TargetStopID: f.PrimaryStopID(),
		HorizonS:     f.HorizonS(),
		DeltaS:       *out.DeltaS,
		PLate:        *out.PLate,
		ModelVersion: out.ModelVersion,
		Source:       SourceML,
		Missing:      f.Missing(),
	}
	if dev, ok := f.Value("cur_dev_s"); ok {
		p.PredictedDevS = dev + p.DeltaS
	} else {
		// cur_dev_s не измерен, складывать нечего. Оставляем PredictedDevS
		// равной добавке модели и не выдумываем отклонение: неизвестное
		// отклонение, помеченное как измеренное, хуже нуля, помеченного
		// как неизвестный.
		p.PredictedDevS = p.DeltaS
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

// predictResponse — ответ POST /predict.
type predictResponse struct {
	// DeltaS — добавка к cur_dev_s в секундах. Именно её предстоит
	// выучить: метка organizers это predict_cur_dev_s, а cur_dev_s в неё
	// входит, поэтому модель учится на разнице, а не на отклонении.
	DeltaS *float64 `json:"delta_s"`
	// PLate — вероятность опоздания в [0, 1].
	PLate *float64 `json:"p_late"`
	// ModelVersion — версия модели. Пустая допустима: версия нужна для
	// разбора инцидентов, но её отсутствие прогнозу не мешает.
	ModelVersion string `json:"model_version"`
}

// modelInfo — ответ GET /model/info.
type modelInfo struct {
	Version      string   `json:"version"`
	FeatureNames []string `json:"feature_names"`
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
// важен — важно множество. Расхождение перечисляется целиком: по одному имени
// разработчик другой стороны поймёт, что сломалось, только если это
// единственная разница.
func checkNames(got []string) error {
	if len(got) == 0 {
		return fmt.Errorf("%w: модель не объявила ни одного признака", ErrContractMismatch)
	}
	want := slices.Clone(contractNames)
	sort.Strings(want)
	have := slices.Clone(got)
	sort.Strings(have)

	var missing, extra []string
	for _, n := range want {
		if !slices.Contains(have, n) {
			missing = append(missing, n)
		}
	}
	for _, n := range have {
		if !slices.Contains(want, n) {
			extra = append(extra, n)
		}
	}
	// Дубль в списке модели — тоже расхождение: один и тот же признак дважды
	// означает, что модель считает их двумя разными.
	if dup := duplicates(got); len(dup) > 0 {
		extra = append(extra, dup...)
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(ErrContractMismatch.Error())
	if len(missing) > 0 {
		b.WriteString("; нет у модели: " + strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		b.WriteString("; лишние у модели: " + strings.Join(extra, ", "))
	}
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
func retryable(err error) bool {
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
