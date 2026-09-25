package schedule

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Колонки выгрузки телеметрии, нужные для привязки.
const (
	trafficTRID   = "tr_id"
	trafficUnitID = "unit_id"
)

// Binding отображает номер транспортного средства в идентификатор физического
// устройства.
//
// Связь в данных взаимно однозначная: в телеметрии каждому tr_id соответствует
// ровно один unit_id, и наоборот. Именно поэтому возможно строить признаки по
// расписанию прямо в потоке приёма, без справочной таблицы и без ожидания
// прихода телеметрии по всем машинам парка.
type Binding struct {
	// byTR — tr_id → unit_id.
	byTR map[int64]uint32
	// byUnit — unit_id → tr_id.
	byUnit map[uint32]int64
	// trIDs — номера транспортных средств по возрастанию.
	trIDs []int64
	// conflicts — число tr_id, встретившихся с несколькими unit_id.
	conflicts int
	// missing — число строк без unit_id.
	missing int
}

// LoadBindingFile читает привязку из файла телеметрии.
func LoadBindingFile(path string) (*Binding, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("schedule: %w", err)
	}
	defer file.Close()
	return LoadBinding(file)
}

// LoadBinding читает привязку из потока CSV с телеметрией.
//
// Файл телеметрии велик: в train это около 288 тысяч строк, поэтому привязка
// строится одним проходом с отбрасыванием ненужных колонок, а не через
// промежуточную структуру строк.
func LoadBinding(r io.Reader) (*Binding, error) {
	reader := csv.NewReader(r)
	reader.ReuseRecord = true
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("schedule: не удалось прочитать заголовок телеметрии: %w", err)
	}
	trIndex, unitIndex, err := indexTrafficColumns(header)
	if err != nil {
		return nil, err
	}

	b := &Binding{
		byTR:   make(map[int64]uint32),
		byUnit: make(map[uint32]int64),
	}
	for row := 2; ; row++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("schedule: телеметрия, строка %d: %w", row, err)
		}
		trRaw := field(record, trIndex)
		unitRaw := field(record, unitIndex)
		if trRaw == "" || unitRaw == "" {
			b.missing++
			continue
		}
		trID, err := strconv.ParseInt(trRaw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("schedule: телеметрия, строка %d: неверный tr_id %q: %w", row, trRaw, err)
		}
		unitID, err := strconv.ParseUint(unitRaw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("schedule: телеметрия, строка %d: неверный unit_id %q: %w", row, unitRaw, err)
		}
		known, exists := b.byTR[trID]
		if exists && known != uint32(unitID) {
			// Противоречие в исходных данных. Молча выбрать одно из двух
			// означало бы привязать телеметрию машины к чужому расписанию,
			// а это молча портит все признаки этой машины. Поэтому факт
			// фиксируется и выгружается в отчёт, а привязка остаётся
			// первого увиденного значения.
			b.conflicts++
			continue
		}
		b.byTR[trID] = uint32(unitID)
		b.byUnit[uint32(unitID)] = trID
	}

	b.trIDs = make([]int64, 0, len(b.byTR))
	for trID := range b.byTR {
		b.trIDs = append(b.trIDs, trID)
	}
	slices.Sort(b.trIDs)
	return b, nil
}

// indexTrafficColumns находит tr_id и unit_id в заголовке телеметрии.
func indexTrafficColumns(header []string) (trIndex, unitIndex int, err error) {
	position := make(map[string]int, len(header))
	for i, name := range header {
		position[strings.TrimSpace(name)] = i
	}
	trIndex, ok := position[trafficTRID]
	if !ok {
		return 0, 0, fmt.Errorf("schedule: в телеметрии нет колонки %q", trafficTRID)
	}
	unitIndex, ok = position[trafficUnitID]
	if !ok {
		return 0, 0, fmt.Errorf("schedule: в телеметрии нет колонки %q", trafficUnitID)
	}
	return trIndex, unitIndex, nil
}

// field возвращает обрезанное значение колонки либо пустую строку.
func field(record []string, index int) string {
	if index < 0 || index >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[index])
}

// UnitID возвращает идентификатор устройства по номеру транспортного средства.
func (b *Binding) UnitID(trID int64) (uint32, bool) {
	unitID, ok := b.byTR[trID]
	return unitID, ok
}

// TRID возвращает номер транспортного средства по идентификатору устройства.
func (b *Binding) TRID(unitID uint32) (int64, bool) {
	trID, ok := b.byUnit[unitID]
	return trID, ok
}

// Vehicles возвращает номера транспортных средств по возрастанию.
func (b *Binding) Vehicles() []int64 {
	return slices.Clone(b.trIDs)
}

// Len возвращает число связанных пар.
func (b *Binding) Len() int { return len(b.byTR) }

// ConflictsCount возвращает число транспортных средств, для которых в
// телеметрии встретились разные unit_id. Ненулевое значение означает дефект
// исходных данных и требует разбора до использования привязки.
func (b *Binding) ConflictsCount() int { return b.conflicts }

// MissingCount возвращает число строк телеметрии без unit_id или tr_id.
func (b *Binding) MissingCount() int { return b.missing }
