package schedule

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 1, 6, 3, 35, 0, 0, time.UTC)

const scheduleHeader = "tt_action_item_id,time_begin,time_fact_begin,order_date,manual_fill,tr_id,geom,building_address\n"

// repoPath поднимает путь до корня репозитория от каталога пакета
// (backend/internal/schedule на три уровня ниже корня).
func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	dir, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("не удалось определить корень репозитория: %v", err)
	}
	return filepath.Join(append([]string{dir}, parts...)...)
}

// fixture собирает расписание для одного транспортного средства.
func fixture(t *testing.T, rows string) *Schedule {
	t.Helper()
	s, err := Load(strings.NewReader(scheduleHeader + rows))
	if err != nil {
		t.Fatalf("не удалось загрузить расписание: %v", err)
	}
	return s
}

func TestLoadOrdersStopsDeterministically(t *testing.T) {
	// Порядок в файле намеренно произвольный, а времена двух строк
	// совпадают: без второго ключа сортировки результат зависел бы от
	// порядка в выгрузке.
	s := fixture(t,
		"3,2026-01-06 03:40:00,,2026-01-06,False,100,POINT (37.5 55.8),b\n"+
			"1,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n"+
			"2,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (37.5 55.8),a2\n",
	)
	stops := s.Stops(100)
	want := []int64{1, 2, 3}
	if len(stops) != len(want) {
		t.Fatalf("остановок %d, ожидалось %d", len(stops), len(want))
	}
	for i, id := range want {
		if stops[i].ActionID != id {
			t.Errorf("остановка %d имеет id %d, ожидался %d", i, stops[i].ActionID, id)
		}
	}
}

func TestLoadCountsAndFlags(t *testing.T) {
	s := fixture(t,
		"1,2026-01-06 03:35:00,2026-01-06 03:36:00,2026-01-06,True,100,POINT (37.5 55.8),a\n"+
			"2,2026-01-06 03:40:00,,2026-01-06,False,100,,b\n",
	)
	if s.StopsCount() != 2 {
		t.Errorf("StopsCount %d, ожидалось 2", s.StopsCount())
	}
	if s.ManualFilledCount() != 1 {
		t.Errorf("ManualFilledCount %d, ожидалась 1", s.ManualFilledCount())
	}
	if s.WithoutGeometryCount() != 1 {
		t.Errorf("WithoutGeometryCount %d, ожидалась 1", s.WithoutGeometryCount())
	}
	first, _ := s.StopByID(1)
	delay, ok := first.Delay()
	if !ok {
		t.Fatal("у первой остановки заполнено фактическое время")
	}
	if delay != time.Minute {
		t.Errorf("Delay %v, ожидалась 1m", delay)
	}
	second, _ := s.StopByID(2)
	if _, ok := second.Delay(); ok {
		t.Error("у остановки без фактического времени Delay обязан сообщить об отсутствии")
	}
}

// Колонка time_fact_begin есть в train, но отсутствует в
// validate/schedule_plan.csv: загрузка обязана работать с обеими выгрузками.
func TestLoadAcceptsScheduleWithoutFactColumn(t *testing.T) {
	const header = "tt_action_item_id,time_begin,order_date,manual_fill,tr_id,geom,building_address\n"
	s, err := Load(strings.NewReader(header +
		"1,2026-01-06 03:35:00,2026-01-06,False,100,POINT (37.5 55.8),a\n"))
	if err != nil {
		t.Fatalf("расписание без колонки факта не должно падать: %v", err)
	}
	stop, _ := s.StopByID(1)
	if stop.HasFact {
		t.Error("HasFact обязан быть ложным без колонки фактического времени")
	}
	if !stop.TimeBegin.Equal(base) {
		t.Errorf("TimeBegin %v, ожидалось %v", stop.TimeBegin, base)
	}
}

// Правило выбора цели сверено со всеми размеченными точками.
//
// Результат 147 из 151, и это верхняя граница, достижимая по одному
// расписанию. В 141 из 151 точек минимальное время в окне единственно, и там
// правило безупречно. Оставшиеся 10 — остановки-пары на конечных: обе записи
// имеют одно и то же плановое время, а координаты различаются на десятки
// метров.
// Разметка для них опиралась на фактическое время прибытия, которого в
// validate/schedule_plan.csv нет, поэтому по расписанию выбор неоднозначен
// принципиально. Разрешить 10 ничьих геометрией не удаётся: «ближе к
// предыдущей» даёт 3 из 10, «дальше от предыдущей» — 7 из 10, «ближе к
// следующей» — 2 из 10. Порядок по возрастанию tt_action_item_id даёт
// максимальные 147.
func TestTargetStopMatchesAllLabelledPoints(t *testing.T) {
	plan, err := LoadFile(repoPath(t, "validate", "schedule_plan.csv"))
	if err != nil {
		t.Skipf("нет validate/schedule_plan.csv: %v", err)
	}
	points := readPoints(t, repoPath(t, "validate", "points.csv"))
	if len(points) == 0 {
		t.Skip("нет размеченных точек")
	}

	const wantMatched = 147
	matched, unresolvable, unexplained := 0, 0, 0
	var report strings.Builder
	for _, p := range points {
		target, ok := plan.TargetStop(p.TRID, p.T)
		if !ok {
			unexplained++
			fmt.Fprintf(&report, "\n  %s: кандидатов в окне нет", p.T.Format(time.RFC3339))
			continue
		}
		if target.Stop.ActionID == p.TargetStopID {
			matched++
			continue
		}
		// Расхождение допустимо только при ничьей по минимальному времени.
		// Ambiguous() — ровно этот признак: в окно могло попасть много
		// строк, но мешает только совпадение времени у ближайших.
		if target.Ambiguous() {
			unresolvable++
			continue
		}
		unexplained++
		fmt.Fprintf(&report, "\n  %s: выбрано %d, ожидалось %d, кандидатов %d, ничьих %d",
			p.T.Format(time.RFC3339), target.Stop.ActionID, p.TargetStopID,
			target.Candidates, target.Tied)
	}

	if unexplained != 0 {
		t.Errorf("расхождение в %d точках не объясняется ничьёй по времени:%s", unexplained, report.String())
	}
	if matched != wantMatched {
		t.Errorf("правило совпало в %d из %d точек, ожидалось %d", matched, len(points), wantMatched)
	}
	if unresolvable != len(points)-wantMatched {
		t.Errorf("неразрешимых ничьих %d, ожидалось %d", unresolvable, len(points)-wantMatched)
	}

	// Признак неоднозначности описывает данные точнее, чем счётчик
	// кандидатов: минимальное время разделяют 10 точек из 151. Правило
	// возрастания tt_action_item_id разрешает 6 из них, и только 4 остаются
	// неугаданными — именно они дают 147 из 151.
	ambiguous, resolvedByTieBreak := 0, 0
	for _, p := range points {
		target, ok := plan.TargetStop(p.TRID, p.T)
		if !ok || !target.Ambiguous() {
			continue
		}
		ambiguous++
		if target.Stop.ActionID == p.TargetStopID {
			resolvedByTieBreak++
		}
	}
	if ambiguous != 10 {
		t.Errorf("Ambiguous() в %d точках, ожидалось 10", ambiguous)
	}
	if resolvedByTieBreak != 6 {
		t.Errorf("ничьи, разрешённые правилом возрастания id, %d, ожидалось 6", resolvedByTieBreak)
	}
}

type labelledPoint struct {
	TRID         int64
	T            time.Time
	TargetStopID int64
}

func readPoints(t *testing.T, path string) []labelledPoint {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("нет %s: %v", path, err)
	}
	var out []labelledPoint
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		parsedT, err := ParseTime(fields[2])
		if err != nil {
			t.Fatalf("T в строке %d не разобран: %v", i+1, err)
		}
		trID, err := parseInt(fields[1])
		if err != nil {
			t.Fatalf("tr_id в строке %d не разобран: %v", i+1, err)
		}
		target, err := parseInt(fields[3])
		if err != nil {
			t.Fatalf("target_stop_id в строке %d не разобран: %v", i+1, err)
		}
		out = append(out, labelledPoint{TRID: trID, T: parsedT, TargetStopID: target})
	}
	return out
}

func parseInt(v string) (int64, error) {
	var n int64
	_, err := fmtSscan(v, &n)
	return n, err
}

func fmtSscan(v string, n *int64) (int, error) {
	if v == "" {
		return 0, errEmpty
	}
	neg := false
	i := 0
	if v[0] == '-' {
		neg, i = true, 1
	}
	if i == len(v) {
		return 0, errEmpty
	}
	for ; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, errEmpty
		}
		n2 := *n*10 + int64(v[i]-'0')
		*n = n2
	}
	if neg {
		*n = -*n
	}
	return 1, nil
}

var errEmpty = errStr("пустое число")

type errStr string

func (e errStr) Error() string { return string(e) }

// Границы окна выбора цели: слева строго, справа включительно. Сдвиг любой из
// них на секунду меняет ответ.
func TestTargetStopWindowBoundaries(t *testing.T) {
	const rows = "" +
		"1,2026-01-06 03:45:00,,2026-01-06,False,100,POINT (37.5 55.8),lower\n" +
		"2,2026-01-06 03:50:00,,2026-01-06,False,100,POINT (37.5 55.8),edge\n" +
		"3,2026-01-06 03:50:01,,2026-01-06,False,100,POINT (37.5 55.8),upper\n"
	s := fixture(t, rows)

	// T = 03:35, окно (03:45, 03:50]: нижняя граница исключена, верхняя
	// включена.
	target, ok := s.TargetStop(100, base)
	if !ok {
		t.Fatal("цель должна быть найдена")
	}
	if target.Stop.ActionID != 2 {
		t.Errorf("выбрана остановка %d, ожидалась 2 (границы окна трактуются верно)", target.Stop.ActionID)
	}
	if target.Candidates != 1 {
		t.Errorf("Candidates %d, ожидался 1", target.Candidates)
	}
	if target.Tied != 1 {
		t.Errorf("Tied %d, ожидался 1", target.Tied)
	}

	// Ровно на 10 минут позже — вне окна.
	if _, ok := s.TargetStop(100, base.Add(MinHorizon)); ok {
		t.Error("остановка ровно на T+10min не должна попадать в окно")
	}
	// Ровно на 15 минут позже относительно нового T — внутри окна.
	late := base.Add(MaxHorizon)
	edge := late.Add(MaxHorizon)
	shifted := fixture(t, "5,"+edge.Format("2006-01-02 15:04:05")+
		",,2026-01-06,False,100,POINT (37.5 55.8),edge\n")
	if target, ok := shifted.TargetStop(100, late); !ok {
		t.Error("остановка ровно на T+15min обязана попадать в окно")
	} else if !target.Stop.TimeBegin.Equal(edge) {
		t.Errorf("выбрано время %v, ожидалось %v", target.Stop.TimeBegin, edge)
	}
}

// Неоднозначность выбора — норма, а не дефект: её число должно доходить до
// слоя прогноза.
func TestTargetStopReportsCandidateCount(t *testing.T) {
	const rows = "" +
		"1,2026-01-06 03:46:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n" +
		"2,2026-01-06 03:47:00,,2026-01-06,False,100,POINT (37.5 55.8),b\n" +
		"3,2026-01-06 03:48:00,,2026-01-06,False,100,POINT (37.5 55.8),c\n"
	target, ok := fixture(t, rows).TargetStop(100, base)
	if !ok {
		t.Fatal("цель должна быть найдена")
	}
	if target.Candidates != 3 {
		t.Errorf("Candidates %d, ожидалось 3", target.Candidates)
	}
	// Выбирается ближайшая по времени.
	if target.Stop.ActionID != 1 {
		t.Errorf("выбрана остановка %d, ожидалась 1", target.Stop.ActionID)
	}
	// Три кандидата в окне, но время у ближайшего единственно: цель
	// определена однозначно. Это и есть различие между размером окна и
	// неоднозначностью.
	if target.Tied != 1 || target.Ambiguous() {
		t.Errorf("Tied %d, Ambiguous %v; ожидались 1 и false", target.Tied, target.Ambiguous())
	}
}

// Ничья считается только по минимальному времени: совпадение времени у
// более поздних строк цель не размывает. Именно это наблюдение позволило
// отделить 10 реально неразрешимых точек от 141 точки с многокандидатным
// окном.
func TestTargetStopTieOnlyOnEarliestTime(t *testing.T) {
	// Две строки в одно время плюс более поздняя: неоднозначность есть.
	const tied = "" +
		"1,2026-01-06 03:46:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n" +
		"2,2026-01-06 03:46:00,,2026-01-06,False,100,POINT (37.6 55.8),b\n" +
		"3,2026-01-06 03:47:00,,2026-01-06,False,100,POINT (37.5 55.8),c\n"
	target, ok := fixture(t, tied).TargetStop(100, base)
	if !ok {
		t.Fatal("цель должна быть найдена")
	}
	if target.Tied != 2 || !target.Ambiguous() {
		t.Errorf("Tied %d, Ambiguous %v; ожидались 2 и true", target.Tied, target.Ambiguous())
	}
	// При равенстве времени выбор детерминирован: меньший ActionID.
	if target.Stop.ActionID != 1 {
		t.Errorf("выбрана остановка %d, ожидалась 1", target.Stop.ActionID)
	}
	if target.Candidates != 3 {
		t.Errorf("Candidates %d, ожидалось 3", target.Candidates)
	}

	// Совпадение времени у двух более поздних строк цель не размывает.
	const laterTie = "" +
		"1,2026-01-06 03:46:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n" +
		"2,2026-01-06 03:47:00,,2026-01-06,False,100,POINT (37.5 55.8),b\n" +
		"3,2026-01-06 03:47:00,,2026-01-06,False,100,POINT (37.6 55.8),c\n"
	target, ok = fixture(t, laterTie).TargetStop(100, base)
	if !ok {
		t.Fatal("цель должна быть найдена")
	}
	if target.Tied != 1 || target.Ambiguous() {
		t.Errorf("Tied %d, Ambiguous %v; ожидались 1 и false", target.Tied, target.Ambiguous())
	}
	if target.Stop.ActionID != 1 {
		t.Errorf("выбрана остановка %d, ожидалась 1", target.Stop.ActionID)
	}
}

func TestNextStopAfter(t *testing.T) {
	s := fixture(t,
		"1,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n"+
			"2,2026-01-06 03:40:00,,2026-01-06,False,100,POINT (37.5 55.8),b\n",
	)
	stop, ok := s.NextStopAfter(100, base)
	if !ok || stop.ActionID != 1 {
		t.Errorf("на T должна вернуться остановка с TimeBegin == T, получено %v, ok=%v", stop.ActionID, ok)
	}
	stop, ok = s.NextStopAfter(100, base.Add(time.Nanosecond))
	if !ok || stop.ActionID != 2 {
		t.Errorf("чуть позже T должна вернуться следующая остановка, получено %v", stop.ActionID)
	}
	if _, ok := s.NextStopAfter(100, base.Add(time.Hour)); ok {
		t.Error("за концом расписания цели быть не должно")
	}
}

func TestWindowIsHalfOpen(t *testing.T) {
	s := fixture(t,
		"1,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n"+
			"2,2026-01-06 03:40:00,,2026-01-06,False,100,POINT (37.5 55.8),b\n"+
			"3,2026-01-06 03:45:00,,2026-01-06,False,100,POINT (37.5 55.8),c\n",
	)
	window := s.Window(100, base, base.Add(5*time.Minute))
	// [03:35, 03:40) содержит только первую остановку: вторая лежит ровно
	// на правой границе и в полуинтервал не входит.
	if len(window) != 1 {
		t.Fatalf("в окне %d остановок, ожидалась 1", len(window))
	}
	if window[0].ActionID != 1 {
		t.Errorf("окно [%v, %v) вернуло остановку %d, ожидалась 1",
			base, base.Add(5*time.Minute), window[0].ActionID)
	}
}

// Пропуск обязательной колонки — ошибка формата, а не пустое расписание:
// иначе «машина не найдена» была бы единственным симптомом.
func TestLoadRejectsMissingColumn(t *testing.T) {
	_, err := Load(strings.NewReader("tt_action_item_id,tr_id,geom\n1,100,POINT (37.5 55.8)\n"))
	if err == nil {
		t.Fatal("расписание без колонки time_begin обязано отклоняться")
	}
	if !strings.Contains(err.Error(), colTimeBegin) {
		t.Errorf("ошибка должна называть отсутствующую колонку, получено: %v", err)
	}
}

func TestLoadRejectsDuplicateActionID(t *testing.T) {
	_, err := Load(strings.NewReader(scheduleHeader +
		"1,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n" +
		"1,2026-01-06 03:40:00,,2026-01-06,False,100,POINT (37.5 55.8),b\n"))
	if err == nil {
		t.Fatal("повтор tt_action_item_id обязан отклоняться")
	}
}

func TestLoadRejectsGarbageNumber(t *testing.T) {
	_, err := Load(strings.NewReader(scheduleHeader +
		"abc,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (37.5 55.8),a\n"))
	if err == nil {
		t.Fatal("нечисловой идентификатор обязан отклоняться")
	}
}

func TestParsePointWKT(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		lon, lat float64
		wantErr  bool
	}{
		{name: "обычный", value: "POINT (37.43070705 55.8040083)", lon: 37.43070705, lat: 55.8040083},
		{name: "с пробелами", value: "POINT(37.43 55.80)", lon: 37.43, lat: 55.80},
		{name: "нижний регистр", value: "point (37.43 55.80)", lon: 37.43, lat: 55.80},
		{name: "вне диапазона широты", value: "POINT (37.43 95.80)", wantErr: true},
		{name: "ноль-заглушка", value: "POINT (0 0)", wantErr: true},
		{name: "не point", value: "LINESTRING (37.43 55.80, 37.44 55.81)", wantErr: true},
		{name: "одна координата", value: "POINT (37.43)", wantErr: true},
		{name: "без скобок", value: "37.43 55.80", wantErr: true},
		{name: "пусто", value: "   ", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lon, lat, err := ParsePointWKT(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q разобран как (%.5f, %.5f), ожидалась ошибка", tc.value, lon, lat)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q не разобран: %v", tc.value, err)
			}
			if math.Abs(lon-tc.lon) > 1e-9 || math.Abs(lat-tc.lat) > 1e-9 {
				t.Errorf("%q разобран как (%.9f, %.9f), ожидалось (%.9f, %.9f)",
					tc.value, lon, lat, tc.lon, tc.lat)
			}
		})
	}
}

// Перестановка координат удовлетворяет проверке диапазонов, поэтому её ловит
// региональный контроль на загрузке, а не разбор WKT.
func TestRegionGuardCatchesTransposedCoordinates(t *testing.T) {
	transposed := fixture(t, "1,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (55.80 37.43),a\n")
	if transposed.OutsideRegionCount() != 1 {
		t.Errorf("переставленные координаты не отмечены: OutsideRegionCount %d, ожидалась 1",
			transposed.OutsideRegionCount())
	}

	correct := fixture(t, "1,2026-01-06 03:35:00,,2026-01-06,False,100,POINT (37.43 55.80),a\n")
	if correct.OutsideRegionCount() != 0 {
		t.Errorf("корректные координаты отмечены как посторонние: %d", correct.OutsideRegionCount())
	}
	if correct.Region() != DefaultRegion {
		t.Error("по умолчанию должна применяться DefaultRegion")
	}
}

func TestParseTimeRejectsEmpty(t *testing.T) {
	if _, err := ParseTime("  "); err == nil {
		t.Fatal("пустое время обязано отклоняться, а не читаться как epoch")
	}
	if _, err := ParseTime("06.01.2026 03:35"); err == nil {
		t.Fatal("неверный формат даты обязан отклоняться")
	}
	// Обе формы встречаются в выгрузках и обе обязаны разбираться.
	for _, value := range []string{"2026-01-06 03:35:00", "2026-01-06 03:35:00.000000000",
		"2026-01-06 03:35:00.123456"} {
		if _, err := ParseTime(value); err != nil {
			t.Errorf("%q не разобран: %v", value, err)
		}
	}
}

func TestLoadBinding(t *testing.T) {
	const trafficHeader = "packet_id,tr_id,unit_id,event_time\n"
	binding, err := LoadBinding(strings.NewReader(trafficHeader +
		"1,100,664030,2026-01-06 12:30:31\n" +
		"2,100,664030,2026-01-06 12:30:41\n" +
		"3,101,664031,2026-01-06 12:30:51\n" +
		",102,,2026-01-06 12:31:01\n"))
	if err != nil {
		t.Fatalf("привязка не загрузилась: %v", err)
	}
	if binding.Len() != 2 {
		t.Errorf("Len %d, ожидалось 2", binding.Len())
	}
	unitID, ok := binding.UnitID(101)
	if !ok || unitID != 664031 {
		t.Errorf("UnitID(101) = %d, %v; ожидалось 664031", unitID, ok)
	}
	trID, ok := binding.TRID(664030)
	if !ok || trID != 100 {
		t.Errorf("TRID(664030) = %d, %v; ожидалось 100", trID, ok)
	}
	if binding.MissingCount() != 1 {
		t.Errorf("MissingCount %d, ожидалась 1", binding.MissingCount())
	}
	if binding.ConflictsCount() != 0 {
		t.Errorf("ConflictsCount %d, ожидался 0", binding.ConflictsCount())
	}
	if got := binding.Vehicles(); len(got) != 2 || got[0] != 100 || got[1] != 101 {
		t.Errorf("Vehicles %v, ожидалось [100 101]", got)
	}
}

// Противоречие в данных не должно молча выбирать одну из сторон: факт
// фиксируется, привязка остаётся первого увиденного значения.
func TestLoadBindingReportsConflict(t *testing.T) {
	const trafficHeader = "packet_id,tr_id,unit_id,event_time\n"
	binding, err := LoadBinding(strings.NewReader(trafficHeader +
		"1,100,664030,2026-01-06 12:30:31\n" +
		"2,100,999999,2026-01-06 12:30:41\n"))
	if err != nil {
		t.Fatalf("привязка не загрузилась: %v", err)
	}
	if binding.ConflictsCount() != 1 {
		t.Errorf("ConflictsCount %d, ожидалась 1", binding.ConflictsCount())
	}
	if unitID, _ := binding.UnitID(100); unitID != 664030 {
		t.Errorf("UnitID(100) = %d, ожидалось первое увиденное 664030", unitID)
	}
}

func TestLoadBindingRejectsMissingColumn(t *testing.T) {
	_, err := LoadBinding(strings.NewReader("packet_id,tr_id,event_time\n1,100,2026-01-06 12:30:31\n"))
	if err == nil {
		t.Fatal("отсутствие unit_id обязано отклоняться")
	}
}

// Привязка на реальных данных парка обязана быть взаимно однозначной: иначе
// признаки одной машины считались бы по чужому расписанию.
func TestBindingOnTrainTraffic(t *testing.T) {
	path := repoPath(t, "train", "traffic.csv")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("нет %s: %v", path, err)
	}
	binding, err := LoadBindingFile(path)
	if err != nil {
		t.Fatalf("привязка не загрузилась: %v", err)
	}
	if binding.ConflictsCount() != 0 {
		t.Errorf("ConflictsCount %d: связь tr_id ↔ unit_id должна быть однозначной", binding.ConflictsCount())
	}
	if binding.Len() < 30 {
		t.Errorf("привязано %d машин, ожидалось не меньше 30", binding.Len())
	}
	seen := make(map[uint32]int64, binding.Len())
	for _, trID := range binding.Vehicles() {
		unitID, _ := binding.UnitID(trID)
		if other, dup := seen[unitID]; dup {
			t.Errorf("unit_id %d принадлежит и tr_id %d, и tr_id %d", unitID, other, trID)
		}
		seen[unitID] = trID
	}
}

// Полная загрузка train-расписания: проверяются объём, отсутствие потерянных
// строк и геометрии.
func TestLoadTrainSchedule(t *testing.T) {
	path := repoPath(t, "train", "schedule.csv")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("нет %s: %v", path, err)
	}
	s, err := LoadFile(path)
	if err != nil {
		t.Fatalf("расписание не загрузилось: %v", err)
	}
	if s.StopsCount() != 16674 {
		t.Errorf("остановок %d, ожидалось 16674", s.StopsCount())
	}
	if s.StopsCount() != s.RowsCount() {
		t.Errorf("остановок %d при %d строках: часть строк потеряна",
			s.StopsCount(), s.RowsCount())
	}
	if s.WithoutGeometryCount() != 0 {
		t.Errorf("без геометрии %d остановок, ожидался 0", s.WithoutGeometryCount())
	}
	if s.ManualFilledCount() != 2970 {
		t.Errorf("с ручным заполнением %d, ожидалось 2970", s.ManualFilledCount())
	}
	if got := len(s.Vehicles()); got != 39 {
		t.Errorf("транспортных средств %d, ожидалось 39", got)
	}
	// Все координаты в московском регионе: подмена порядка широты и долготы
	// прошла бы проверку «число в диапазоне», но не эту.
	for _, trID := range s.Vehicles() {
		for _, stop := range s.Stops(trID) {
			if stop.Lat < 55.0 || stop.Lat > 56.5 || stop.Lon < 36.0 || stop.Lon > 38.5 {
				t.Fatalf("остановка %d вне региона: (%.5f, %.5f)", stop.ActionID, stop.Lat, stop.Lon)
			}
		}
	}
}
