package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
)

// replayResult — итог сверки конвейера с эталонными метками.
type replayResult struct {
	labels   int
	planned  int
	noState  int
	noTarget int
	behind   int

	// targetAgree — меток, чья цель попала в множество наших вариантов.
	targetAgree int
	compared    int
	// ambiguous — расхождения, которые объясняются ничьей в расписании.
	ambiguousTotal     int
	ambiguousExplained int

	// curDev — сверка отставания с эталонным cur_dev_s.
	curDevCompared int
	curDevAgreed   int
	curDevSumAbs   float64
	curDevMaxAbs   float64
	curDevMissing  int
	// curDevFuture — метки, где эталон опирается на факт, наступивший
	// после T. Это утечка в подсказке организаторов, а не ошибка конвейера.
	curDevFuture int

	verbose  bool
	examples []string
}

func (r replayResult) write(w io.Writer) error {
	head := fmt.Sprintf("меток: %d\n"+
		"  кадров построено: %d (без состояния: %d, нет цели: %d, цель позади: %d)\n",
		r.labels, r.planned, r.noState, r.noTarget, r.behind)
	if _, err := io.WriteString(w, head); err != nil {
		return err
	}
	line := fmt.Sprintf("  цель совпала: %d из %d (%.2f%%)\n",
		r.targetAgree, r.compared, percent(r.targetAgree, r.compared))
	if _, err := io.WriteString(w, line); err != nil {
		return err
	}
	if r.ambiguousTotal > 0 {
		line = fmt.Sprintf("  из них с ничьёй в нашем кадре: %d (%d расхождений не объяснено)\n",
			r.ambiguousExplained, r.ambiguousTotal-r.ambiguousExplained)
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	if r.curDevCompared > 0 {
		line = fmt.Sprintf("  cur_dev_s: совпало %d из %d, среднее |Δ| = %.1f с, максимум |Δ| = %.1f с (нет признака: %d)\n",
			r.curDevAgreed, r.curDevCompared, r.curDevSumAbs/float64(r.curDevCompared),
			r.curDevMaxAbs, r.curDevMissing)
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	if r.curDevFuture > 0 {
		// Расхождение объяснено, а не замаскировано: эталонная подсказка
		// использует факт прибытия, который на момент T ещё не наступил.
		line = fmt.Sprintf("  меток с фактом ПОСЛЕ T в эталоне: %d (%.1f%%) — утечка в подсказке организаторов,\n"+
			"    поэтому конвейер осознанно расходится с ней и берёт последнюю остановку с уже известным фактом\n",
			r.curDevFuture, percent(r.curDevFuture, r.compared))
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	for _, example := range r.examples {
		if _, err := fmt.Fprintln(w, "  "+example); err != nil {
			return err
		}
	}
	return nil
}

func percent(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func (r *replayResult) addExample(format string, args ...any) {
	if !r.verbose || len(r.examples) >= 15 {
		return
	}
	r.examples = append(r.examples, fmt.Sprintf(format, args...))
}

// replay сверяет кадры конвейера с эталонными метками.
//
// Сверяются две величины, и они разные по своей природе. Первая — целевая
// остановка: это следствие правила выбора цели, и расхождение здесь означает
// баг в правиле. Вторая — cur_dev_s: это способ, которым обучающая выборка
// измеряла отставание, и расхождение означает, что модель обучена на другом
// определении, чем то, что считает конвейер. Оба расхождения опасны, но
// чинятся в разных местах, поэтому и считаются отдельно.
func replay(plan *schedule.Schedule, binding *schedule.Binding, tracks map[uint32]*track,
	labels []label, out *lineWriter, verbose bool) replayResult {
	result := replayResult{labels: len(labels), verbose: verbose}
	planner := horizon.New()

	byTR := make(map[int64]*track, len(tracks))
	for _, t := range tracks {
		byTR[t.trID] = t
	}

	for _, l := range labels {
		t, ok := byTR[l.trID]
		if !ok {
			result.noState++
			continue
		}
		history := historyUntil(t, l.t)
		state, hasState := stateAt(history, l.t)
		if !hasState {
			result.noState++
			continue
		}
		stops := plan.Window(l.trID, l.t.Add(-15*time.Minute), l.t.Add(40*time.Minute))
		decision := planner.Plan(horizon.Request{
			T:        l.t,
			TRID:     l.trID,
			UnitID:   t.unitID,
			Stops:    stops,
			State:    state,
			HasState: true,
			History:  history,
		})
		if !decision.Create() {
			switch decision.Reason {
			case horizon.ReasonNoTarget:
				result.noTarget++
			case horizon.ReasonTargetBehind:
				result.behind++
			}
			continue
		}
		result.planned++
		frame := decision.Frame
		if out != nil {
			_ = writeFrame(out, frame)
		}

		// Сверка цели. Метка могла выбрать одну из нескольких равноправных
		// остановок, поэтому совпадением считается попадание в множество
		// вариантов, а не строгое равенство с первой.
		result.compared++
		if frame.Ambiguous {
			result.ambiguousTotal++
		}
		if frameHasStop(frame, l.targetStop) {
			result.targetAgree++
			if frame.Ambiguous {
				result.ambiguousExplained++
			}
		} else {
			result.addExample("цель не совпала: метка %d (T=%s), наш выбор %v",
				l.targetStop, l.t.Format(time.RFC3339), stopIDs(frame))
		}

		// Сверка cur_dev_s: подсказка организаторов против значения,
		// посчитанного конвейером. Это главная проверка: в validate эта
		// величина приходит во входе, а в online её считает конвейер, и
		// если определения расходятся, модель обучена на подсказке,
		// которой на площадке не существует.
		if reference, ok := lastPlannedStopWithFact(stops, l.t); ok &&
			reference.TimeFactBegin.After(l.t) {
			// Эталон organizers считает cur_dev_s по последней остановке с
			// плановым временем не позже T, даже если её факт наступит уже
			// после T. Конвейер так делать не может: это утечка.
			result.curDevFuture++
		}
		curDev, ok := frame.Value("cur_dev_s")
		if !ok {
			result.curDevMissing++
			continue
		}
		delta := math.Abs(curDev - l.curDevS)
		result.curDevCompared++
		result.curDevSumAbs += delta
		result.curDevMaxAbs = math.Max(result.curDevMaxAbs, delta)
		if delta <= 1.0 {
			result.curDevAgreed++
		} else if verbose && len(result.examples) < 15 {
			result.addExample("cur_dev_s: метка %.0f, наш %.0f (Δ%.0f с), %s",
				l.curDevS, curDev, delta, l.sampleID)
		}
	}
	return result
}

// lastPlannedStopWithFact — последняя остановка с плановым временем не позже
// t, у которой вообще есть факт. Время факта здесь намеренно не проверяется:
// так считает эталон, и именно это отличие требуется измерить.
func lastPlannedStopWithFact(stops []schedule.Stop, t time.Time) (schedule.Stop, bool) {
	var found schedule.Stop
	ok := false
	for _, stop := range stops {
		if stop.TimeBegin.After(t) || !stop.HasFact {
			continue
		}
		if !ok || stop.TimeBegin.After(found.TimeBegin) {
			found, ok = stop, true
		}
	}
	return found, ok
}

func frameHasStop(f *horizon.Frame, actionID int64) bool {
	for _, arrival := range f.Target {
		if arrival.ActionID == actionID {
			return true
		}
	}
	return false
}

func stopIDs(f *horizon.Frame) []int64 {
	out := make([]int64, 0, len(f.Target))
	for _, arrival := range f.Target {
		out = append(out, arrival.ActionID)
	}
	return out
}

// historyUntil — точки не позже момента t. Отрезок обрезается по времени
// события, а не по времени приёма: в прогнозе не может быть точек, которые
// машина пришлёт позже, даже если файл уже содержит их.
func historyUntil(t *track, until time.Time) []statestore.Point {
	cut := sort.Search(len(t.points), func(i int) bool {
		return t.points[i].EventTime.After(until)
	})
	if cut == 0 {
		return nil
	}
	return t.points[:cut]
}
