package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/pipeline"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/scheduler"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

func quietServeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// Модель подключена: ответ обязан прийти от неё, а не от baseline. Если
// цепочка соберётся неправильно, признаки будут неверными, и это молча
// уедет в прогнозы на площадке.
func TestPredictorChainUsesModelWhenAddressGiven(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version":       "v1",
				"feature_names": predictor.FeatureContract(),
			})
		case "/predict":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"delta_s": 30.0, "p_late": 0.4, "model_version": "v1",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	frame := serveFrame()
	p := predictorChain(srv.URL, time.Second, quietServeLogger()).Predict(t.Context(), frame)
	if p.Source != predictor.SourceML {
		t.Fatalf("источник %q, ожидалась модель", p.Source)
	}
	if p.DeltaS != 30 {
		t.Errorf("добавка %v, ожидалось 30", p.DeltaS)
	}
	if p.ModelVersion != "v1" {
		t.Errorf("версия модели %q, ожидалась v1", p.ModelVersion)
	}
}

// Без адреса модели прогноз обязан отдавать baseline, а не молчать и не
// падать: сервер поднимают на площадке раньше, чем появится ML-сервис.
func TestPredictorChainFallsBackToBaselineWithoutModel(t *testing.T) {
	p := predictorChain("", 0, quietServeLogger()).Predict(t.Context(), serveFrame())
	if p.Source != predictor.SourceBaseline {
		t.Errorf("источник %q, ожидался baseline", p.Source)
	}
	if p.DeltaS != 0 {
		t.Errorf("добавка baseline %v, ожидался честный ноль", p.DeltaS)
	}
}

// Модель недоступна: первый кадр уходит в baseline, а не обрушивает
// конвейер. Именно этот путь и держит процесс живым при упавшем ML.
func TestPredictorChainSurvivesDeadModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version":       "v1",
				"feature_names": predictor.FeatureContract(),
			})
			return
		}
		http.Error(w, "модель упала", http.StatusInternalServerError)
	}))
	chain := predictorChain(srv.URL, 200*time.Millisecond, quietServeLogger())
	srv.Close()

	p := chain.Predict(t.Context(), serveFrame())
	if p.Source != predictor.SourceBaseline {
		t.Errorf("источник %q при упавшей модели, ожидался baseline", p.Source)
	}
}

// Кадр из конвейера обязан дойти до модели через журналирующий приёмник.
// Раньше frameLogger был конечным звеном и кадры никуда не уходили, поэтому
// потерять звено молча очень легко: процесс работает, а прогнозов нет.
func TestFrameLoggerForwardsToScheduler(t *testing.T) {
	var calls atomic64
	sched := scheduler.New(scheduler.Config{
		Predictor: countingPredictor{calls: &calls},
		Queue:     4,
		Logger:    quietServeLogger(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()

	logger := &frameLogger{inner: sched, logger: quietServeLogger()}
	logger.Submit(serveFrame())
	waitScheduler(t, &calls, 1)
	cancel()
	<-done
}

// frameLogger без внутреннего приёмника — это --dry-run: кадр обязан
// просто исчезнуть в журнале, без паники и без очереди.
func TestFrameLoggerWithoutInnerDoesNotPanic(t *testing.T) {
	logger := &frameLogger{logger: quietServeLogger()}
	logger.Submit(serveFrame())
}

type countingPredictor struct{ calls *atomic64 }

func (c countingPredictor) Predict(_ context.Context, f horizon.Frame) predictor.Prediction {
	c.calls.add(1)
	return predictor.Prediction{SampleID: f.SampleID, UnitID: f.UnitID, Source: predictor.SourceML}
}

func waitScheduler(t *testing.T, calls *atomic64, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls.get() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("модель позвали %d раз, ожидалось %d", calls.get(), want)
}

// atomic64 — счётчик позванных моделей из горуточного теста.
type atomic64 struct{ n atomic.Int64 }

func (a *atomic64) add(n int64) { a.n.Add(n) }
func (a *atomic64) get() int64  { return a.n.Load() }

// serveFrame — кадр, достаточный для вызова модели: целевую остановку
// планировщик уже выбрал, модели нужен сам кадр.
func serveFrame() horizon.Frame {
	var f horizon.Frame
	f.SampleID = "7_1767686700"
	f.UnitID = 4242
	f.TRID = 7
	f.AsOf = time.Date(2026, 1, 6, 8, 5, 0, 0, time.UTC)
	f.Ambiguous = false
	f.Target = []horizon.Arrival{{
		ActionID: 111,
		Planned:  f.AsOf.Add(11 * time.Minute),
		HorizonS: 660,
	}}
	f.Values = map[string]*float64{
		"cur_dev_s":     ptrF(60),
		"plan_travel_s": ptrF(660),
	}
	return f
}

func ptrF(v float64) *float64 { return &v }

// Сквозная проверка сборки: телеметрия → конвейер → планировщик → модель →
// гейтвей → HTTP. Потеря любого звена здесь выглядит как «процесс работает,
// прогнозов нет», и тесты по кускам этого не показывают: собирать их должен
// ровно этот код.
func TestBuildServiceDeliversPredictionToGateway(t *testing.T) {
	var modelCalls atomic64
	mlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version":       "v1",
				"feature_names": predictor.FeatureContract(),
			})
		case "/predict":
			modelCalls.add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"delta_s": 42.0, "p_late": 0.75, "model_version": "v1",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mlSrv.Close()

	now := time.Now().Truncate(time.Second)
	plan := writePlanCSV(t, now)
	binding := writeBindingCSV(t)

	addr := freeAddr(t)
	store := statestore.New()
	svc, err := buildService(serviceOptions{
		Pipeline: pipeline.Config{
			Store:    store,
			Schedule: plan,
			Binding:  binding,
			// Секундная сетка: в тесте не пять минут ждать первую границу.
			Grid:   time.Second,
			Logger: quietServeLogger(),
		},
		Predictor: predictorChain(mlSrv.URL, 2*time.Second, quietServeLogger()),
		Queue:     8,
		HTTPAddr:  addr,
		Logger:    quietServeLogger(),
	})
	if err != nil {
		t.Fatalf("buildService: %v", err)
	}
	if svc.Pipeline == nil || svc.Scheduler == nil || svc.Gateway == nil || svc.Hub == nil {
		t.Fatal("сборка вернула пустые компоненты")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go svc.Scheduler.Run(ctx)
	go svc.Hub.Run(ctx)
	go func() { _ = svc.Gateway.ListenAndServe(ctx) }()
	waitHTTPReady(t, "http://"+addr+"/healthz")

	// Точка в накопитель — это ровно то, что делает разборщик NDTP-пакета.
	store.Append(statestore.Point{
		UnitID: 4242, EventTime: now, ReceiveTime: now,
		Longitude: 37.6000, Latitude: 55.8, SpeedKmh: 20,
		LocationValid: true, Satellites: 9, CourseDeg: 90,
	})
	svc.Pipeline.Tick(ctx, now)

	waitModelCalls(t, &modelCalls, 1)

	// Прогноз виден в карточке машины: именно её читает панель.
	cards := waitBody(t, "http://"+addr+"/api/v1/vehicles")
	if !strings.Contains(cards, "4242") {
		t.Errorf("в карточках нет нашей машины: %s", cards)
	}
	// И точечно по идентификатору кадра: T секундной сетки равен моменту
	// точки, поэтому идентификатор вычисляется, а не подглядывается.
	sampleID := fmt.Sprintf("1_%d", now.Unix())
	one := waitBody(t, "http://"+addr+"/api/v1/predictions/"+sampleID)
	// Добавка модели 42 секунды обязана дойти до клиента: иначе по всему
	// звену прошёл нулевой baseline, и потери не видно.
	if !strings.Contains(one, `"delta_s": 42`) {
		t.Errorf("прогноз %s без добавки модели: %s", sampleID, one)
	}
	if got := svc.Scheduler.Stats().Predicted; got != 1 {
		t.Errorf("Predicted %d, ожидался 1", got)
	}
	if got := svc.Scheduler.Stats().Dropped; got != 0 {
		t.Errorf("Dropped %d, ожидался 0: очередь должна была принять единственный кадр", got)
	}
}

// План под текущий момент: остановки обязаны лежать в окне (T+10, T+15].
// Время берётся у часов теста, а не из фикстуры января 2026 года, иначе
// цель была бы вне окна и кадр не построился бы вовсе.
func writePlanCSV(t *testing.T, now time.Time) *schedule.Schedule {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.csv")
	var b strings.Builder
	b.WriteString("tt_action_item_id,time_begin,order_date,manual_fill,tr_id,geom,building_address\n")
	// Время в выгрузке без зоны, и разбор читает его как UTC. Форматировать
	// надо по UTC, а не по зоне машины: на машине в MSK локальное время на
	// три часа впереди, и остановки уехали бы за горизонтом вместе с целью.
	const stamp = "2006-01-02 15:04:05"
	for i := range 5 {
		at := now.Add(time.Duration(11+i) * time.Minute).UTC()
		lon := 37.600 + 0.007*float64(i)
		fmt.Fprintf(&b, "%d,%s,%s,False,1,POINT (%f %f),стоп-%d\n",
			100+i, at.Format(stamp), at.Format(stamp), lon, 55.8, i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("план: %v", err)
	}
	plan, err := schedule.LoadFile(path)
	if err != nil {
		t.Fatalf("план: %v", err)
	}
	return plan
}

func writeBindingCSV(t *testing.T) *schedule.Binding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "traffic.csv")
	if err := os.WriteFile(path, []byte("tr_id,unit_id\n1,4242\n"), 0o644); err != nil {
		t.Fatalf("привязка: %v", err)
	}
	binding, err := schedule.LoadBindingFile(path)
	if err != nil {
		t.Fatalf("привязка: %v", err)
	}
	return binding
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("свободный адрес: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("освобождение адреса: %v", err)
	}
	return addr
}

func waitHTTPReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s не ответил за пять секунд", url)
	return
}

func waitModelCalls(t *testing.T, calls *atomic64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls.get() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("модель позвали %d раз, ожидалось %d", calls.get(), want)
}

func waitBody(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			last = string(raw)
			if resp.StatusCode == http.StatusOK && len(last) > 2 {
				return last
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s не ответил прогнозами: %s", url, last)
	return ""
}
