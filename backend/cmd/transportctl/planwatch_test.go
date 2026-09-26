package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/pipeline"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

const bindingCSV = "tr_id,unit_id\n1,1166336\n"

// planRow — строка плана в настоящем формате генератора.
func planRow(actionID int64, at time.Time, lon, lat float64) string {
	ts := at.Format("2006-01-02 15:04:05")
	return fmt.Sprintf("%d,%s,%s,False,1,POINT (%.6f %.6f),стоп-%d,\n",
		actionID, ts, ts, lon, lat, actionID)
}

const planHeader = "tt_action_item_id,time_begin,order_date,manual_fill,tr_id," +
	"geom,building_address,time_fact_begin\n"

// testPlan — план с одной остановкой.
func testPlan(actionID int64, at time.Time, lon, lat float64) string {
	return planHeader + planRow(actionID, at, lon, lat)
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// newTestPipeline — конвейер, которому можно сказать ForgetTargets. Полноценный
// сервис тут не нужен: проверяется подмена плана, а не прогноз.
func newTestPipeline(t *testing.T, holder *schedule.Holder) (*pipeline.Pipeline, func()) {
	t.Helper()
	pipe, err := pipeline.New(pipeline.Config{
		Store:  statestore.New(),
		Holder: holder,
		Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("сборка конвейера: %v", err)
	}
	return pipe, func() {}
}

func newLoadedHolder(t *testing.T, planPath, bindingPath string) *schedule.Holder {
	t.Helper()
	sched, err := schedule.LoadFile(planPath)
	if err != nil {
		t.Fatalf("план не загрузился: %v", err)
	}
	binding, err := schedule.LoadBindingFile(bindingPath)
	if err != nil {
		t.Fatalf("привязка не загрузилась: %v", err)
	}
	return schedule.NewHolder(sched, binding)
}

func writePlanFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("запись %s: %v", path, err)
	}
}

func newWatcherFixture(t *testing.T, every time.Duration) (*planWatcher, *schedule.Holder, string, string) {
	t.Helper()
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.csv")
	bindingPath := filepath.Join(dir, "binding.csv")
	at := time.Now().UTC().Add(time.Minute)
	writePlanFile(t, planPath, testPlan(101, at, 37.6, 55.7))
	writePlanFile(t, bindingPath, bindingCSV)
	holder := newLoadedHolder(t, planPath, bindingPath)
	pipe, _ := newTestPipeline(t, holder)
	return newPlanWatcher(planPath, bindingPath, holder, pipe, testLogger(t), every),
		holder, planPath, bindingPath
}

func TestWatcherSwapsPlanOnFileChange(t *testing.T) {
	w, holder, planPath, _ := newWatcherFixture(t, time.Hour)
	at := time.Now().UTC().Add(2 * time.Minute)
	writePlanFile(t, planPath, testPlan(102, at, 37.61, 55.71))

	if w.reloads != 0 {
		t.Fatalf("до проверки перепривязок быть не могло: %d", w.reloads)
	}
	w.step()
	if w.reloads != 1 {
		t.Fatalf("ожидалась одна перепривязка, получили %d", w.reloads)
	}
	stop, ok := holder.Get().StopByID(102)
	if !ok {
		t.Fatal("новый план не подменён: остановки 102 нет")
	}
	if stop.Lon < 37.60 {
		t.Fatalf("координаты нового плана не применились: lon=%.6f", stop.Lon)
	}
	if _, ok := holder.Get().StopByID(101); ok {
		t.Fatal("старый план остался в holder")
	}
	if w.holder.GetBinding() == nil {
		t.Fatal("привязка потерялась при подмене плана")
	}
}

func TestWatcherIgnoresUnchangedFile(t *testing.T) {
	w, _, _, _ := newWatcherFixture(t, time.Hour)
	w.step()
	w.step()
	w.step()
	if w.reloads != 0 {
		t.Fatalf("файл не менялся, перепривязок быть не должно: %d", w.reloads)
	}
}

func TestWatcherKeepsOldPlanOnBrokenFile(t *testing.T) {
	w, holder, planPath, _ := newWatcherFixture(t, time.Hour)
	// обрыв файла: так план выглядит в момент перезаписи генератором
	writePlanFile(t, planPath, planHeader+"101,2026-09-2")
	w.step()
	if w.reloads != 0 {
		t.Fatalf("битый файл не должен был подменять план: %d", w.reloads)
	}
	if _, ok := holder.Get().StopByID(101); !ok {
		t.Fatal("прежний план должен был остаться в работе")
	}
	// и после починки подмена происходит
	writePlanFile(t, planPath, testPlan(103, time.Now().UTC().Add(time.Minute), 37.62, 55.72))
	w.step()
	if w.reloads != 1 {
		t.Fatalf("после починки ожидалась перепривязка, получили %d", w.reloads)
	}
}

func TestWatcherReportsMissingFileOnce(t *testing.T) {
	w, _, planPath, _ := newWatcherFixture(t, time.Hour)
	if err := os.Remove(planPath); err != nil {
		t.Fatalf("удаление плана: %v", err)
	}
	w.step()
	w.step()
	w.step()
	if w.reloads != 0 {
		t.Fatalf("удалённый файл не подменяет план: %d", w.reloads)
	}
	if w.lastErr == "" {
		t.Fatal("исчезновение плана должно быть замечено, а не пройти молча")
	}
}

func TestWatcherRunPicksUpChangeAndStops(t *testing.T) {
	w, holder, planPath, _ := newWatcherFixture(t, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	writePlanFile(t, planPath, testPlan(104, time.Now().UTC().Add(time.Minute), 37.63, 55.73))
	// Ждём подмену в holder, а не роста внутреннего счётчика: счётчик
	// принадлежит горутине наблюдателя, и чтение его из теста было бы гонкой.
	// Проверка заодно честнее — интересует подменённый план, а не счётчик.
	waitFor(t, 2*time.Second, func() bool {
		_, ok := holder.Get().StopByID(104)
		return ok
	})
	if _, ok := holder.Get().StopByID(101); ok {
		t.Fatal("старый план не вытеснен новым")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run не завершился по отмене контекста")
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("условие не выполнилось за отведённое время")
}
