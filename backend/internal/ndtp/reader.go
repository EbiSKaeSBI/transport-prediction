package ndtp

import (
	"fmt"
	"io"
)

// Reader поочерёдно читает кадры NDTP из потока.
//
// Reader не буферизует между вызовами и не переживает разрывы потока: при
// ошибке Next следует считать поток нечитаемым. Значение Frame.Raw на каждом
// успешном вызове уникально и не переиспользуется.
type Reader struct {
	src     io.Reader
	header  [NPLSize]byte
	payload []byte
}

// NewReader создаёт Reader поверх src. src не должен возвращать
// (0, nil): используется io.ReadFull, который трактует такой возврат как
// отсутствие данных.
func NewReader(src io.Reader) *Reader {
	return &Reader{src: src, payload: make([]byte, 0, 512)}
}

// Next читает очередной кадр целиком и возвращает io.EOF в конце потока.
// Остальные ошибки разбора возвращаются как есть, обёрнутые в значения
// ошибок пакета ndtp.
func (r *Reader) Next() (Frame, error) {
	if _, err := io.ReadFull(r.src, r.header[:]); err != nil {
		return Frame{}, err
	}
	npl, err := ParseNPL(r.header[:])
	if err != nil {
		return Frame{}, err
	}
	size := int(npl.DataSize)
	if cap(r.payload) < size {
		r.payload = make([]byte, size)
	}
	body := r.payload[:size]
	if _, err := io.ReadFull(r.src, body); err != nil {
		return Frame{}, fmt.Errorf("ndtp: недобранное тело (%d байт ожидалось): %w", size, err)
	}
	raw := make([]byte, NPLSize+size)
	copy(raw, r.header[:])
	copy(raw[NPLSize:], body)
	return ParseFrame(raw)
}
