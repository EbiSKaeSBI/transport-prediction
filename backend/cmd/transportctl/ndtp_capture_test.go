package main

import (
	"bufio"
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
)

// navCell — ячейка для счётчиков captureHandler: важен только тип, поэтому
// содержимое Nav00 в тесте не заполняется.
func navCell(number uint8) ndtp.Cell {
	return ndtp.Cell{Number: number, Payload: ndtp.Nav00{}}
}

func TestCaptureHandlerConcurrentUnits(t *testing.T) {
	var bin, meta bytes.Buffer
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &captureHandler{
		bin:    bufio.NewWriter(&bin),
		meta:   bufio.NewWriter(&meta),
		units:  map[uint32]int{},
		byType: map[string]int{},
		limit:  0,
		cancel: cancel,
	}

	const units, framesPerUnit, cellsPerFrame = 4, 50, 2
	var wg sync.WaitGroup
	for unit := uint32(0); unit < units; unit++ {
		wg.Add(1)
		go func(unitID uint32) {
			defer wg.Done()
			for i := 0; i < framesPerUnit; i++ {
				cells := make([]ndtp.Cell, 0, cellsPerFrame)
				for c := 0; c < cellsPerFrame; c++ {
					cells = append(cells, navCell(uint8(c)))
				}
				handler.OnRealtime(unitID, ndtp.Frame{Raw: []byte{0x01, 0x02}}, cells)
			}
		}(unit + 1166336)
	}
	wg.Wait()
	// Буферы смотрим после Flush: записи в файлы буферизуются, и без сброса
	// в счётчиках видно только то, что уже уехало в буфер ядра.
	if err := handler.bin.Flush(); err != nil {
		t.Fatalf("сброс bin: %v", err)
	}
	if err := handler.meta.Flush(); err != nil {
		t.Fatalf("сброс meta: %v", err)
	}

	want := units * framesPerUnit
	if handler.seq != want {
		t.Errorf("seq %d, ожидалось %d", handler.seq, want)
	}
	if got := handler.byType[ndtp.CellNav00.String()]; got != want*cellsPerFrame {
		t.Errorf("Nav00 %d, ожидалось %d", got, want*cellsPerFrame)
	}
	if got := bin.Len(); got != want*2 {
		t.Errorf("в bin записано %d байт, ожидалось %d", got, want*2)
	}
	// seq в JSONL должен идти подряд, без дыр и повторов: рекорды собираются
	// под мьютексом, поэтому порядок записи совпадает с порядком seq.
	if got := bytes.Count(meta.Bytes(), []byte(`"seq":`)); got != want {
		t.Errorf("в jsonl %d записей, ожидалось %d", got, want)
	}
}
