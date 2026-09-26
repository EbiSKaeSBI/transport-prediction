// Package pipeline связывает приём NDTP-телеметрии с расчётом прогноза:
// разбирает пакеты, ведёт состояние устройств, сопоставляет устройство с
// расписанием и строит кадры прогноза.
//
// Пакет не знает о расписании, а расписание не знает о протоколе. Связь между
// ними существует только здесь, и потому единственное место, где нужно искать
// пропавшую цель или потерянную привязку.
package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/features"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/telemetry"
)

// Границы окна расписания вокруг момента прогноза. Перед T нужен запас для
// последней пройденной остановки и для расчёта накопленного отставания,
// после T — для цели и для terminal-пары с перерывом.
const (
	// RouteWindowBefore — сколько расписания смотрим назад.
	RouteWindowBefore = 15 * time.Minute
	// RouteWindowAfter — сколько расписания смотрим вперёд. Запас заметно
	// больше горизонта: признак перерыва требует следующей остановки, а
	// выбор цели — только строки до T+15 минут.
	RouteWindowAfter = 40 * time.Minute

	// DefaultGrid — шаг сетки моментов прогноза.
	//
	// Пять минут — не округление на глаз. Модель обучалась на выборке, где
	// T лежит на сетке, и кадр с произвольным T не имеет с ней ничего
	// общего. Сетка нужна ещё и для того, чтобы вызовы модели шли пачками с
	// одинаковым T: батч из тридцати машин с одним временем обучаем, а три
	//дцать разных времён — нет.
	DefaultGrid = 5 * time.Minute
)

// Sink принимает готовые кадры прогноза.
type Sink interface {
	Submit(horizon.Frame)
}

// Config — зависимости конвейера.
type Config struct {
	// Schedule — план-график. При nil конвейер принимает и накапливает
	// телеметрию, но не строит кадры: считает их в Stats.NoPlan.
	Schedule *schedule.Schedule
	// Binding — соответствие unit_id и tr_id. При nil — как при отсутствии
	// Schedule: телеметрия копится, прогноза нет.
	Binding *schedule.Binding
	// Store — накопитель телеметрии. Обязателен: без него конвейеру некуда
	// складывать точки, и он не выполнит даже свою половину задачи.
	Store *statestore.Store
	// Planner — расчёт кадров. При nil создаётся с настройками по умолчанию.
	Planner *horizon.Planner
	// Sink — получатель кадров. При nil кадры только считаются.
	Sink Sink
	// Grid — шаг сетки моментов прогноза. Ноль берётся как DefaultGrid.
	// Момент T округляется вниз до кратного Grid, а кадры считаются только
	// на границе ячейки. Причина в docs/adr/0007.
	Grid time.Duration
	// Observer — разборщик NDTP-пакетов. При nil создаётся без вывода.
	Observer *telemetry.Observer
	// Logger — журнал. При nil используется slog.Default.
	Logger *slog.Logger
}

// Stats — счётчики конвейера, видимые в сводке.
type Stats struct {
	// Observations — принято наблюдений.
	Observations int64
	// Frames — отправлено кадров в Sink.
	Frames int64
	// Deduped — кадров, отброшенных как повтор по той же цели.
	Deduped int64
	// NoBinding — тактиров без единицы в расписании.
	NoBinding int64
	// NoSchedule — тактиров без окна расписания в этот момент.
	NoSchedule int64
	// NoPlan — тактиров, пропущенных из-за отсутствия расписания или
	// привязки. Ненулевое значение означает, что процесс живёт, но не
	// прогнозирует, и это обязано быть видно в сводке.
	NoPlan int64
	// NoStateAtT — тактиров, где на момент T у машины не было ни одного
	// пакета. Обычно это машина, включившаяся посреди ячейки сетки: до
	// границы у неё нет данных, и подставлять более поздние значило бы
	// предсказать по будущему. Отдельный счётчик, потому что молчаливый
	// отказ выглядел бы как «машина спит», а это не так.
	NoStateAtT int64
	// Refused — отказов планировщика с причиной.
	Refused map[string]int64
}

// Pipeline — композитный обработчик NDTP и источник кадров прогноза.
type Pipeline struct {
	cfg      Config
	observer *telemetry.Observer
	planner  *horizon.Planner
	logger   *slog.Logger

	mu sync.Mutex
	// lastTarget — последняя цель, по которой уже отправлен кадр, по
	// устройствам. Один инцидент на пару «машина, остановка»: повторная
	// отправка каждую секунду забила бы очередь прогнозов.
	lastTarget map[uint32]int64
	// lastGrid — начало последней спланированной ячейки сетки. Нулевое
	// значение означает, что не спланировано ещё ничего, и первый тик
	// поэтому считается, даже если процесс поднялся посреди ячейки.
	lastGrid time.Time
	stats    Stats
}

// New создаёт конвейер. Обязателен только Store: без него конвейер не
// выполнит даже свою половину задачи — накапливать точки.
//
// Schedule и Binding необязательны намеренно. Режим накопления телеметрии
// (`serve --dry-run`) существует именно для того, чтобы копить точки до
// приезда плана, и он не имеет права требовать план. Опасность «конвейер
// принимает трафик и молча ничего не прогнозирует» снимается не запретом на
// запуск, а тем, что она считается: каждый пропущенный тактир попадает в
// Stats.NoPlan, а процесс предупреждает об этом при старте.
func New(cfg Config) (*Pipeline, error) {
	if cfg.Store == nil {
		return nil, errNil("Store")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Schedule == nil || cfg.Binding == nil {
		logger.Warn("конвейер без расписания: телеметрия принимается, прогноз не строится",
			"schedule", cfg.Schedule == nil, "binding", cfg.Binding == nil)
	}
	if cfg.Grid <= 0 {
		cfg.Grid = DefaultGrid
	}
	planner := cfg.Planner
	if planner == nil {
		planner = horizon.New(horizon.WithFeatureConfig(features.DefaultConfig()))
	}
	observer := cfg.Observer
	if observer == nil {
		observer = telemetry.New(nil, telemetry.WithLogger(logger))
	}
	return &Pipeline{
		cfg:        cfg,
		observer:   observer,
		planner:    planner,
		logger:     logger,
		lastTarget: make(map[uint32]int64),
		stats:      Stats{Refused: make(map[string]int64)},
	}, nil
}

type errNil string

func (e errNil) Error() string {
	return "pipeline: обязательная зависимость " + string(e) + " не задана"
}

// Наблюдение, разобранное из пакета, попадает в накопитель состояния.
func (p *Pipeline) OnHandshake(unitID uint32, req ndtp.ConnRequest) {
	p.observer.OnHandshake(unitID, req)
}

func (p *Pipeline) OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell) {
	p.observer.OnRealtime(unitID, frame, cells)
	observation, ok := p.observer.Latest(unitID)
	if !ok {
		return
	}
	p.cfg.Store.AppendObservation(observation)

	p.mu.Lock()
	p.stats.Observations++
	p.mu.Unlock()
}

func (p *Pipeline) OnMalformed(unitID uint32, frame ndtp.Frame, err error) {
	p.observer.OnMalformed(unitID, frame, err)
}

func (p *Pipeline) OnDisconnect(unitID uint32) {
	p.observer.OnDisconnect(unitID)
	// Привязка сбрасывается вместе с соединением: пока пакетов нет, любая
	// цель для этого устройства была бы выдумана.
	p.mu.Lock()
	delete(p.lastTarget, unitID)
	p.mu.Unlock()
}

// Tick строит кадры прогноза для всех известных устройств.
//
// Тик приходит каждые 15 секунд, а кадры считаются не на каждом тике, а один
// раз на ячейку сетки: T округляется вниз до кратного сетки, и повторные тики
// внутри одной ячейки не делают ничего. Это не оптимизация, а требование
// контракта: модель обучалась на выборке с T на сетке, и кадры с
// произвольным T не имеют с ней ничего общего.
//
// Данные для кадра берутся строго не позже T, а не «как есть на момент
// тика». Иначе кадр, подписанный 08:05, получил бы состояние машины от
// 08:07 — это утечка из будущего, ровно та, которую лечит ADR 0004. Цена
// решения видима: машина, включившаяся посреди ячейки, получит первый
// прогноз только на её границе.
func (p *Pipeline) Tick(ctx context.Context, now time.Time) {
	if err := ctx.Err(); err != nil {
		return
	}
	gridT := now.Truncate(p.cfg.Grid)
	if !p.claimGrid(gridT) {
		return
	}
	for _, unitID := range p.cfg.Store.Units() {
		if err := ctx.Err(); err != nil {
			return
		}
		p.planUnit(ctx, unitID, gridT)
	}
}

// claimGrid отмечает ячейку как спланированную и сообщает, стоит ли её
// считать. Планируется ровно одна ячейка за раз: тик внутри той же ячейки
// делать нечего, а тик назад по времени после перезапуска не должен
// пересчитывать уже посчитанное.
func (p *Pipeline) claimGrid(gridT time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lastGrid.IsZero() && !gridT.After(p.lastGrid) {
		return false
	}
	p.lastGrid = gridT
	return true
}

// Grid возвращает начало последней спланированной ячейки.
func (p *Pipeline) Grid() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastGrid
}

// Run тикает с заданным интервалом до отмены контекста.
func (p *Pipeline) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			p.Tick(ctx, at)
		}
	}
}

// planUnit строит кадр для одного устройства на момент t.
func (p *Pipeline) planUnit(ctx context.Context, unitID uint32, t time.Time) {
	if p.cfg.Schedule == nil || p.cfg.Binding == nil {
		p.mu.Lock()
		p.stats.NoPlan++
		p.mu.Unlock()
		return
	}
	trID, ok := p.cfg.Binding.TRID(unitID)
	if !ok {
		p.mu.Lock()
		p.stats.NoBinding++
		p.mu.Unlock()
		return
	}
	// Состояние и история берутся на момент t, а не «последние известные».
	// Тик может прийти через несколько секунд после начала ячейки, и к тому
	// моменту в хранилище есть пакеты с временем события после t: подставить
	// их в кадр с этим t значит предсказать по данным из будущего.
	state, hasState := p.cfg.Store.StateAsOf(unitID, t)
	if !hasState {
		p.mu.Lock()
		p.stats.NoStateAtT++
		p.mu.Unlock()
		return
	}
	window := p.cfg.Schedule.Window(trID, t.Add(-RouteWindowBefore), t.Add(RouteWindowAfter))
	if len(window) == 0 {
		p.mu.Lock()
		p.stats.NoSchedule++
		p.mu.Unlock()
		return
	}

	decision := p.planner.Plan(horizon.Request{
		T:        t,
		TRID:     trID,
		UnitID:   unitID,
		Stops:    window,
		State:    state,
		HasState: hasState,
		History:  p.cfg.Store.Window(unitID),
	})
	if err := ctx.Err(); err != nil {
		return
	}
	if !decision.Create() {
		p.mu.Lock()
		p.stats.Refused[decision.Reason]++
		p.mu.Unlock()
		p.logger.Debug("кадр не построен",
			"unit", unitID, "tr_id", trID, "reason", decision.Reason)
		return
	}

	frame := *decision.Frame
	if !p.claim(unitID, frame.PrimaryStopID()) {
		p.mu.Lock()
		p.stats.Deduped++
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.stats.Frames++
	p.mu.Unlock()

	p.logger.Debug("кадр прогноза",
		"unit", unitID, "tr_id", trID,
		"sample", frame.SampleID,
		"target", frame.PrimaryStopID(),
		"variants", len(frame.Target),
		"horizon_s", frame.HorizonS(),
		"ambiguous", frame.Ambiguous)
	if p.cfg.Sink != nil {
		p.cfg.Sink.Submit(frame)
	}
}

// claim регистрирует цель как уже обработанную и сообщает, можно ли её
// отправлять. Повторная отправка запрещена до смены цели: инцидент — это
// пара «машина, остановка», а не состояние.
func (p *Pipeline) claim(unitID uint32, targetStopID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if previous, ok := p.lastTarget[unitID]; ok && previous == targetStopID {
		return false
	}
	p.lastTarget[unitID] = targetStopID
	return true
}

// Forget снимает отметку о цели, разрешая повторную отправку. Нужен в replay,
// где один и тот же кадр может встретиться в двух независимых отрезках.
func (p *Pipeline) Forget(unitID uint32) {
	p.mu.Lock()
	delete(p.lastTarget, unitID)
	p.mu.Unlock()
}

// Stats возвращает снимок счётчиков.
func (p *Pipeline) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := Stats{
		Observations: p.stats.Observations,
		Frames:       p.stats.Frames,
		Deduped:      p.stats.Deduped,
		NoBinding:    p.stats.NoBinding,
		NoSchedule:   p.stats.NoSchedule,
		NoPlan:       p.stats.NoPlan,
		NoStateAtT:   p.stats.NoStateAtT,
		Refused:      make(map[string]int64, len(p.stats.Refused)),
	}
	for reason, n := range p.stats.Refused {
		out.Refused[reason] = n
	}
	return out
}

// Observer возвращает разборщик пакетов: он же пишет JSONL при включённом
// выводе, и серверу нужен доступ к нему для журналирования.
func (p *Pipeline) Observer() *telemetry.Observer { return p.observer }
