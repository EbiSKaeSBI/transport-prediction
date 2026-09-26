package gateway

import (
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
)

func TestRiskBoundaries(t *testing.T) {
	// Границы проверяются точно, а не «где-то рядом»: сдвиг порога на
	// секунду меняет, при каком прогнозе дежурный увидит тревогу, а это
	// ровно то решение, которое здесь записано числами.
	cases := []struct {
		name  string
		dev   float64
		plate float64
		want  Risk
	}{
		{"в графике", 0, 0.0, RiskGreen},
		{"чуть опоздает", 59.9, 0.0, RiskGreen},
		{"ровно жёлтый порог по времени", 60, 0.0, RiskYellow},
		{"жёлтый порог по вероятности", 0, 0.3, RiskYellow},
		{"ровно красный порог по времени", 120, 0.0, RiskRed},
		{"красный порог по вероятности", 0, 0.6, RiskRed},
		{"опоздание с малой вероятностью", 200, 0.1, RiskRed},
		{"раньше с большой вероятностью", 0, 0.7, RiskRed},
		{"раньше с пограничной вероятностью", -300, 0.3, RiskYellow},
		{"раньше и не ждём", -300, 0.1, RiskGreen},
		{"ровно на пороге снизу", 59.999, 0.299, RiskGreen},
	}
	for _, c := range cases {
		got := Classify(predictor.Prediction{PredictedDevS: c.dev, PLate: c.plate})
		if got != c.want {
			t.Errorf("%s: dev=%g plate=%g дал %q, ожидался %q",
				c.name, c.dev, c.plate, got, c.want)
		}
	}
}

func TestIncidentOpensOnlyOnRed(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }

	// Зелёный и жёлтый инцидента не создают: жёлтая зона существует затем,
	// чтобы оператор увидел тревожный прогноз ДО того, как он стал простоем.
	// Ни зелёная, ни жёлтая зона инцидента не создают: жёлтая существует
	// затем, чтобы оператор увидел тревожный прогноз ДО того, как он стал
	// простоем.
	for _, p := range []predictor.Prediction{
		predictionAt(4242, 0, 0.1),
		predictionAt(4242, 90, 0.1),
		predictionAt(4242, 0, 0.4),
	} {
		if ev := s.Update(p); ev.Incident != nil {
			t.Fatalf("риск %q создал инцидент, хотя создаёт только красный",
				Classify(p))
		}
	}
	if st := s.Stats(); st.Total != 0 {
		t.Errorf("инцидентов %d, ожидался 0", st.Total)
	}

	ev := s.Update(predictionAt(4242, 200, 0.9))
	if !ev.Opened || ev.Incident == nil {
		t.Fatalf("красный прогноз обязан открыть инцидент: %+v", ev)
	}
	if st := s.Stats(); st.Open != 1 {
		t.Errorf("открытых %d, ожидался 1", st.Open)
	}
}

// Повторный красный прогноз той же машины обязан обновлять тот же инцидент.
// Иначе на каждый тик появлялся бы новый, и список инцидентов превращался бы
// в поток дублей одного простоя.
func TestRepeatedRedUpdatesSameIncident(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }

	first := s.Update(predictionAt(4242, 200, 0.9))
	second := s.Update(predictionAt(4242, 260, 0.95))
	third := s.Update(predictionAt(4242, 300, 0.99))

	if second.Opened {
		t.Error("второй красный прогноз не должен открывать новый инцидент")
	}
	if !third.Updated {
		t.Error("третий прогноз должен обновлять инцидент")
	}
	if second.Incident.ID != first.Incident.ID {
		t.Errorf("идентификатор поменялся: %q -> %q",
			first.Incident.ID, second.Incident.ID)
	}
	if st := s.Stats(); st.Total != 1 {
		t.Errorf("инцидентов %d, ожидался 1", st.Total)
	}
	// Значения обязаны обновляться: в карточке иначе осталась бы оценка, на
	// которой инцидент открылся.
	if got := second.Incident.PredictedDevS; got != 260 {
		t.Errorf("отклонение в карточке %g, ожидалось 260", got)
	}
}

func TestIncidentResolvesWhenRiskFalls(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }

	opened := s.Update(predictionAt(4242, 200, 0.9))
	ev := s.Update(predictionAt(4242, 10, 0.1))
	if !ev.Resolved {
		t.Fatalf("выход из красной зоны обязан закрыть инцидент: %+v", ev)
	}
	if ev.Incident.Status != StatusResolved {
		t.Errorf("статус %q, ожидался resolved", ev.Incident.Status)
	}
	if ev.Incident.ResolvedAt == nil {
		t.Error("время закрытия не проставлено")
	}
	if st := s.Stats(); st.Resolved != 1 || st.Open != 0 {
		t.Errorf("счётчики %+v, ожидался 1 закрытый и 0 открытых", st)
	}
	// Закрытый инцидент обязан остаться видимым: разбор «кто и когда
	// признал» после смены не должен зависеть от памяти оператора.
	if _, err := s.Get(opened.Incident.ID); err != nil {
		t.Errorf("закрытый инцидент пропал из хранилища: %v", err)
	}
}

// Возврат в красную зону после закрытия — новый эпизод с новым
// идентификатором. Склеивать их одним идентификатором нельзя: в истории
// сольётся причина и следствие.
func TestRedAfterResolveIsNewEpisode(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }

	first := s.Update(predictionAt(4242, 200, 0.9))
	s.Update(predictionAt(4242, 10, 0.1))
	second := s.Update(predictionAt(4242, 220, 0.9))

	if !second.Opened {
		t.Fatal("возврат в красную зону обязан открыть инцидент")
	}
	if second.Incident.ID == first.Incident.ID {
		t.Error("новый эпизод получил тот же идентификатор")
	}
	if st := s.Stats(); st.Total != 2 || st.Open != 1 {
		t.Errorf("счётчики %+v, ожидалось 2 всего и 1 открытый", st)
	}
}

func TestAckLifecycle(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }
	inc := s.Update(predictionAt(4242, 200, 0.9)).Incident

	acked, err := s.Ack(inc.ID, "дежурный Иванов")
	if err != nil {
		t.Fatalf("подтверждение не прошло: %v", err)
	}
	if acked.Status != StatusAcked {
		t.Errorf("статус %q, ожидался acked", acked.Status)
	}
	if acked.AckedBy != "дежурный Иванов" {
		t.Errorf("кто подтвердил: %q", acked.AckedBy)
	}
	if acked.AckedAt == nil {
		t.Error("время подтверждения не проставлено")
	}

	// Смена оператора обязана иметь возможность подтвердить приём повторно:
	// двойной клик в панели не должен выглядеть как поломка.
	again, err := s.Ack(inc.ID, "дежурный Петров")
	if err != nil {
		t.Fatalf("повторное подтверждение не прошло: %v", err)
	}
	if again.AckedBy != "дежурный Иванов" {
		t.Errorf("повторное подтверждение переписало автора на %q", again.AckedBy)
	}
	if st := s.Stats(); st.Acked != 1 {
		t.Errorf("взятых в работу %d, ожидался 1: повтор не должен считаться", st.Acked)
	}
}

func TestAckErrors(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }
	inc := s.Update(predictionAt(4242, 200, 0.9)).Incident

	if _, err := s.Ack("нет-такого", "кого-то"); err == nil {
		t.Error("подтверждение несуществующего инцидента обязано падать")
	}
	s.Update(predictionAt(4242, 10, 0.1)) // закрываем
	if _, err := s.Ack(inc.ID, "кого-то"); err == nil {
		t.Error("подтверждение закрытого инцидента обязано падать")
	}
}

func TestIncidentsArePerVehicle(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }

	a := s.Update(predictionAt(4242, 200, 0.9))
	b := s.Update(predictionAt(7777, 200, 0.9))
	if a.Incident.ID == b.Incident.ID {
		t.Error("у разных машин один идентификатор инцидента")
	}
	if st := s.Stats(); st.Open != 2 {
		t.Errorf("открытых %d, ожидалось 2", st.Open)
	}

	// Закрытие одной машины не трогает инцидент другой.
	s.Update(predictionAt(4242, 10, 0.1))
	if st := s.Stats(); st.Open != 1 {
		t.Errorf("открытых %d после закрытия одной машины, ожидался 1", st.Open)
	}
}

func TestIncidentStoreIsBounded(t *testing.T) {
	capacity := 8
	s := NewIncidents(capacity)
	s.now = func() time.Time { return base }
	// Разные машины: идентификаторы не совпадают, каждый открывает своё.
	for i := range 200 {
		p := predictionAt(uint32(i+1), 200, 0.9)
		s.Update(p)
	}
	if st := s.Stats(); st.Total > capacity {
		t.Errorf("в хранилище %d инцидентов при потолке %d", st.Total, capacity)
	}
}

// Закрытый инцидент вытесняется первым. Вместимость подобрана так, чтобы
// места хватало трём открытым: иначе вытеснять больше нечего и проверять
// будет нечего, а именно порядок вытеснения тут и проверяется.
func TestEvictionPrefersResolved(t *testing.T) {
	s := NewIncidents(5)
	s.now = func() time.Time { return base }

	openIDs := make([]string, 0, 3)
	for i := range 3 {
		openIDs = append(openIDs, s.Update(predictionAt(uint32(i+1), 200, 0.9)).Incident.ID)
	}
	resolved := s.Update(predictionAt(9, 200, 0.9)).Incident
	s.Update(predictionAt(9, 10, 0.1)) // закрыт

	// Два новых: первый заполняет хранилище до потолка, второй вытесняет
	// единственного закрытого — и на этом останавливается.
	for i := range 2 {
		s.Update(predictionAt(uint32(100+i), 200, 0.9))
	}
	if _, err := s.Get(resolved.ID); err == nil {
		t.Error("закрытый инцидент должен был быть вытеснен первым")
	}
	for _, id := range openIDs {
		if _, err := s.Get(id); err != nil {
			t.Errorf("открытый инцидент %s вытеснен раньше закрытого", id)
		}
	}
}

// Когда открытых больше, чем вместимость, вытеснять приходится их, и это
// осознанный размен: неограниченный рост хуже, чем потерять самый старый
// активный простой. Тест фиксирует поведение, а не оправдывает его.
func TestEvictionFallsBackToOldestWhenAllOpen(t *testing.T) {
	s := NewIncidents(3)
	s.now = func() time.Time { return base }

	ids := make([]string, 0, 6)
	for i := range 6 {
		ids = append(ids, s.Update(predictionAt(uint32(i+1), 200, 0.9)).Incident.ID)
	}
	if st := s.Stats(); st.Total > 3 {
		t.Errorf("в хранилище %d инцидентов при потолке 3", st.Total)
	}
	if _, err := s.Get(ids[0]); err == nil {
		t.Error("самый старый инцидент обязан быть вытеснен: места больше нет")
	}
	// Последние три на месте.
	for _, id := range ids[3:] {
		if _, err := s.Get(id); err != nil {
			t.Errorf("свежий инцидент %s вытеснен: %v", id, err)
		}
	}
}

func TestListFiltersByStatus(t *testing.T) {
	s := NewIncidents(0)
	s.now = func() time.Time { return base }
	s.Update(predictionAt(1, 200, 0.9))
	acked := s.Update(predictionAt(2, 200, 0.9)).Incident
	s.Update(predictionAt(3, 200, 0.9))
	s.Update(predictionAt(4, 10, 0.1)) // создаст? нет — зелёный
	s.Ack(acked.ID, "оператор")

	if got := len(s.List("", 0)); got != 3 {
		t.Errorf("без фильтра %d, ожидалось 3 незакрытых", got)
	}
	if got := len(s.List(StatusOpen, 0)); got != 2 {
		t.Errorf("открытых %d, ожидалось 2", got)
	}
	if got := len(s.List(StatusAcked, 0)); got != 1 {
		t.Errorf("взятых в работу %d, ожидался 1", got)
	}
	if got := len(s.List(StatusOpen, 2)); got != 2 {
		t.Errorf("с лимитом 2 вернулось %d", got)
	}
}

func TestListReturnsNewestFirst(t *testing.T) {
	s := NewIncidents(0)
	now := base
	s.now = func() time.Time { return now }
	for i := range 4 {
		s.Update(predictionAt(uint32(i+1), 200, 0.9))
		now = now.Add(time.Minute)
	}
	list := s.List("", 0)
	if len(list) != 4 {
		t.Fatalf("инцидентов %d, ожидалось 4", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].OpenedAt.After(list[i-1].OpenedAt) {
			t.Errorf("список не отсортирован свежими первыми: %v", list)
			break
		}
	}
}

func TestIncidentIDIsStable(t *testing.T) {
	// Идентификатор обязан пережить перезапуск процесса: он выводится из
	// пары (машина, цель), а не из счётчика. Иначе после перезапуска тот же
	// простой получил бы другой номер и разорвался бы с историей.
	first := IncidentID(4242, 114)
	second := IncidentID(4242, 114)
	if first != second {
		t.Errorf("идентификатор неустойчив: %q != %q", first, second)
	}
	if IncidentID(4242, 115) == first {
		t.Error("разные цели получили один идентификатор")
	}
	if IncidentID(7777, 114) == first {
		t.Error("разные машины получили один идентификатор")
	}
}

func TestPredictionStoreIsBounded(t *testing.T) {
	s := NewPredictions(8)
	for i := range 100 {
		p := predictionAt(uint32(i+1), float64(i), 0.1)
		p.SampleID = "sample_" + itoa(i)
		s.Put(p)
	}
	if st := s.Stats(); st.Stored > 8 {
		t.Errorf("в хранилище %d прогнозов при потолке %d", st.Stored, 8)
	}
	if _, ok := s.Get("sample_99"); !ok {
		t.Error("последний прогноз обязан остаться в хранилище")
	}
	if _, ok := s.Get("sample_0"); ok {
		t.Error("первый прогноз должен был быть вытеснен")
	}
}

func TestPredictionStoreUpdatesSameSample(t *testing.T) {
	s := NewPredictions(0)
	p := predictionAt(4242, 100, 0.1)
	p.SampleID = "7_1767686400"
	s.Put(p)
	s.Put(predictionAt(4242, 150, 0.2)) // тот же SampleID
	got, _ := s.Get("7_1767686400")
	if got.PredictedDevS != 150 {
		t.Errorf("прогноз не обновился: %g", got.PredictedDevS)
	}
	if st := s.Stats(); st.Stored != 1 {
		t.Errorf("прогнозов %d, ожидался 1: тот же sample_id обязан обновлять", st.Stored)
	}
}

func TestLatestPerVehicle(t *testing.T) {
	s := NewPredictions(0)
	for i := range 3 {
		p := predictionAt(4242, float64(100+i), 0.1)
		p.SampleID = "a_" + itoa(i)
		s.Put(p)
	}
	other := predictionAt(7777, 5, 0.1)
	other.SampleID = "b_0"
	s.Put(other)

	// Последний прогноз именно этой машины, а не последний вообще.
	got, ok := s.Latest(4242)
	if !ok || got.SampleID != "a_2" {
		t.Errorf("последний прогноз %q, ожидался a_2", got.SampleID)
	}
	if _, ok := s.Latest(9999); ok {
		t.Error("для несуществующей машины не должно быть прогноза")
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
