// Package ndtp реализует разбор протокола NDTP (NPL/NPH) и ячеек телематрии
// транспортного средства.
//
// Кадр состоит из заголовка NPL фиксированного размера, ровно одного заголовка
// NPH и тела. Все целые числа little-endian. Контрольная сумма —
// CRC-16/Modbus, покрывает NPH вместе с телом; в NPL она хранится со свапом
// байтов.
//
// Раскладка заголовков и набор типов пакетов восстановлены из байткода
// классов G6Npl, G6NphHeader, G6NphConnRequest, G6NphSrvNavdataConstant и
// G6NhpSrvGenericControlsConstant поставщика. В частности, младшие три бита
// слова флагов NPL объявлены как nplFlagEncryption, nplFlagCrc и
// nplFlagDelay, а бит запроса существует только в NPH. Отсюда же известно,
// что вендор всегда заполняет поле CRC и не выставляет бит nplFlagCrc, так
// что сумма проверяется независимо от флага, а кадры типов NPL_TYPE_ERROR и
// NPL_TYPE_DEBUG парсер не принимает.
//
// Тело realtime-кадра — последовательность ячеек. Длина ячейки в потоке не
// передаётся: она определяется по типу через реестр размеров. Поэтому
// разбор ячеек строгий: неизвестный тип или нехватка байтов считаются ошибкой.
//
// Шифрование не реализовано. Кадр, объявивший шифрование в NPL или в теле
// handshake, отклоняется явно, чтобы шифротекст не разбирался как данные.
package ndtp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Размеры и фиксированные значения заголовков NDTP.
const (
	// NPLSize — размер заголовка NPL в байтах.
	NPLSize = 15
	// NPHSize — размер заголовка NPH в байтах.
	NPHSize = 10
	// Signature — ожидаемая сигнатура кадра в начале NPL.
	Signature = 0x7E7E
	// MaxDataSize — максимальный dataSize из NPL.
	MaxDataSize = 0xFFFF
)

// Типы NPL.
const (
	// NPLTypeError — кадр с ошибкой передачи.
	NPLTypeError uint8 = 1
	// NPLTypeNPH — кадр содержит ровно один NPH с телом.
	NPLTypeNPH uint8 = 2
	// NPLTypeDebug — служебный кадр отладки.
	NPLTypeDebug uint8 = 3
)

// Идентификаторы сервисов NPH.
const (
	// ServiceGenericControls — сервис generic controls.
	ServiceGenericControls uint16 = 0
	// ServiceNavdata — сервис навигационных данных (телеметрия).
	ServiceNavdata uint16 = 1
)

// Типы NPH.
const (
	// NPHTypeResult — ответ сервера на запрос: код результата в теле.
	NPHTypeResult uint16 = 0
	// NPHTypeConnRequest — handshake: установка соединения.
	NPHTypeConnRequest uint16 = 100
	// NPHTypeHistory — архивный пакет навигационных данных. Сервис Navdata
	// использует тот же номер, что и CONN_REQUEST в сервисе GenericControls,
	// поэтому тип опознаётся только вместе с serviceId.
	NPHTypeHistory uint16 = 100
	// NPHTypeRealtime — поток данных телематрии.
	NPHTypeRealtime uint16 = 101
)

// Флаги NPL. Младшие три бита слова Flags объявлены отдельными полями класса
// G6Npl вендора, остальные 13 бит — единое поле flags.
const (
	// NPLFlagEncryption — бит 0: содержимое кадра зашифровано.
	NPLFlagEncryption uint16 = 1 << 0
	// NPLFlagCRC — бит 1: признак CRC кадра. Вендор всегда заполняет поле CRC
	// и всегда выставляет этот бит в ноль, поэтому проверка суммы
	// выполняется независимо от флага.
	NPLFlagCRC uint16 = 1 << 1
	// NPLFlagDelay — бит 2: кадр с задержкой.
	NPLFlagDelay uint16 = 1 << 2
)

// Флаги NPH.
const (
	// NPHFlagRequest — бит 0: пакет является запросом, иначе ответом.
	NPHFlagRequest uint16 = 1 << 0
)

// Ошибки разбора. Все они обёрнуты в fmt.Errorf с %w, поэтому проверять
// следует через errors.Is.
var (
	// ErrShortFrame — байт меньше, чем нужно для заголовка.
	ErrShortFrame = errors.New("ndtp: кадр короче заголовка")
	// ErrSignature — сигнатура NPL не равна 0x7E7E.
	ErrSignature = errors.New("ndtp: неверная сигнатура NPL")
	// ErrFrameType — frameType не равен 0x02.
	ErrFrameType = errors.New("ndtp: неверный тип кадра NPL")
	// ErrDataSize — dataSize не согласуется с длиной буфера.
	ErrDataSize = errors.New("ndtp: dataSize выходит за пределы кадра")
	// ErrCRC — контрольная сумма не совпала.
	ErrCRC = errors.New("ndtp: контрольная сумма не совпала")
	// ErrServiceID — serviceId не равен ожидаемому.
	ErrServiceID = errors.New("ndtp: неожиданный serviceId")
	// ErrNPHType — тип NPH не равен ожидаемому.
	ErrNPHType = errors.New("ndtp: неожиданный тип NPH")
	// ErrBodyShort — тело короче, чем объявлено.
	ErrBodyShort = errors.New("ndtp: тело короче заголовка NPH")
	// ErrEmptyPayload — ячейка без данных.
	ErrEmptyPayload = errors.New("ndtp: пустой payload ячейки")
	// ErrEncrypted — кадр или handshake объявляет шифрование, которое парсер
	// не реализует. Отказ явный: иначе шифротекст разбирался бы как мусор.
	ErrEncrypted = errors.New("ndtp: шифрование не поддерживается")
	// ErrUnsupportedNPHType — тип NPH не входит в известный протоколу набор.
	ErrUnsupportedNPHType = errors.New("ndtp: неизвестный тип NPH")
)

// NPL — заголовок кадра длины 15 байт.
type NPL struct {
	// Signature — всегда 0x7E7E.
	Signature uint16
	// DataSize — размер NPH вместе с телом, в байтах. Вендор собирает кадр
	// как NPH.size() + payload.size(), то есть в кадре ровно один NPH.
	DataSize uint16
	// Flags — флаги кадра, см. NPLFlagEncryption и остальные.
	Flags uint16
	// CRC — контрольная сумма тела со свапом байтов.
	CRC uint16
	// FrameType — тип кадра, для NPH всегда NPLTypeNPH.
	FrameType uint8
	// PeerAddress — адрес узла-отправителя.
	PeerAddress uint32
	// RequestID — идентификатор запроса.
	RequestID uint16
}

// NPH — заголовок пакета длины 10 байт.
type NPH struct {
	// ServiceID — идентификатор сервиса, см. ServiceGenericControls и ServiceNavdata.
	ServiceID uint16
	// Type — тип пакета, см. NPHTypeConnRequest и NPHTypeRealtime.
	Type uint16
	// Flags — флаги пакета, см. NPHFlagRequest.
	Flags uint16
	// RequestID — идентификатор запроса.
	RequestID uint32
}

// Frame — полностью разобранный кадр NDTP.
type Frame struct {
	// NPL — заголовок кадра.
	NPL NPL
	// NPH — заголовок пакета.
	NPH NPH
	// Body — тело пакета: для realtime это поток ячеек, для handshake —
	// 18 байт ConnRequest.
	Body []byte
	// Raw — исходные байты кадра целиком, включая NPL.
	Raw []byte
}

// Encrypted сообщает, объявлено ли шифрование содержимого кадра в NPL.
// Парсер читает только открытый текст, поэтому такой кадр отбрасывается:
// иначе шифротекст был бы разобран как мусор без явной ошибки.
func (n NPL) Encrypted() bool { return n.Flags&NPLFlagEncryption != 0 }

// CRCFlagSet сообщает, установлен ли бит NPLFlagCRC. Вендор не выставляет его
// ни в одном кадре и считает CRC безусловно, поэтому флаг носит
// справочный характер и не влияет на проверку суммы.
func (n NPL) CRCFlagSet() bool { return n.Flags&NPLFlagCRC != 0 }

// Delayed сообщает, помечен ли кадр как отложенный.
func (n NPL) Delayed() bool { return n.Flags&NPLFlagDelay != 0 }

// IsRequest сообщает, установлен ли бит запроса в флагах NPH.
func (n NPH) IsRequest() bool {
	return n.Flags&NPHFlagRequest != 0
}

// PeerAddress возвращает адрес узла-отправителя из NPL.
func (f Frame) PeerAddress() uint32 {
	return f.NPL.PeerAddress
}

// ParseNPL разбирает заголовок NPL и проверяет сигнатуру, тип кадра и
// минимальный размер dataSize.
func ParseNPL(b []byte) (NPL, error) {
	if len(b) < NPLSize {
		return NPL{}, fmt.Errorf("%w: NPL %d байт, нужно %d", ErrShortFrame, len(b), NPLSize)
	}
	npl := NPL{
		Signature:   binary.LittleEndian.Uint16(b[0:2]),
		DataSize:    binary.LittleEndian.Uint16(b[2:4]),
		Flags:       binary.LittleEndian.Uint16(b[4:6]),
		CRC:         binary.LittleEndian.Uint16(b[6:8]),
		FrameType:   b[8],
		PeerAddress: binary.LittleEndian.Uint32(b[9:13]),
		RequestID:   binary.LittleEndian.Uint16(b[13:15]),
	}
	if npl.Signature != Signature {
		return NPL{}, fmt.Errorf("%w: получено 0x%04X", ErrSignature, npl.Signature)
	}
	if npl.FrameType != NPLTypeNPH {
		return NPL{}, fmt.Errorf("%w: получено 0x%02X", ErrFrameType, npl.FrameType)
	}
	if int(npl.DataSize) < NPHSize {
		return NPL{}, fmt.Errorf("%w: dataSize=%d меньше NPH (%d)", ErrDataSize, npl.DataSize, NPHSize)
	}
	return npl, nil
}

// ParseNPH разбирает заголовок NPH.
func ParseNPH(b []byte) (NPH, error) {
	if len(b) < NPHSize {
		return NPH{}, fmt.Errorf("%w: NPH %d байт, нужно %d", ErrShortFrame, len(b), NPHSize)
	}
	return NPH{
		ServiceID: binary.LittleEndian.Uint16(b[0:2]),
		Type:      binary.LittleEndian.Uint16(b[2:4]),
		Flags:     binary.LittleEndian.Uint16(b[4:6]),
		RequestID: binary.LittleEndian.Uint32(b[6:10]),
	}, nil
}

// ParseFrame разбирает целый кадр: NPL, NPH, проверку CRC и выделение тела.
//
// В кадре находится ровно один NPH: вендорская функция wrapNphPacket пишет
// dataSize = nph.size() + payload.size(), поэтому тело кадра — это все байты
// после заголовка NPH. DataSize обязан равняться NPHSize плюс длина тела;
// иначе в кадре либо лишние байты, либо несколько NPH, что для этого
// протокола невозможно и означает повреждение потока.
//
// CRC покрывает NPH вместе с телом и проверяется всегда, независимо от флага
// NPLFlagCRC: вендор считает и записывает сумму безусловно, а сам бит не
// выставляет ни в одном кадре.
//
// Кадр с установленным NPLFlagEncryption отбрасывается с ErrEncrypted:
// парсер не реализует шифрование и не должен молча разбирать шифротекст.
//
// При ошибке возвращается обёрнутое значение из списка ошибок пакета,
// которое проверяется через errors.Is.
func ParseFrame(b []byte) (Frame, error) {
	npl, err := ParseNPL(b)
	if err != nil {
		return Frame{}, err
	}
	if npl.Encrypted() {
		return Frame{}, fmt.Errorf("%w: NPL объявляет шифрование (flags=0x%04X)", ErrEncrypted, npl.Flags)
	}
	total := NPLSize + int(npl.DataSize)
	if len(b) < total {
		return Frame{}, fmt.Errorf("%w: есть %d байт, объявлено %d", ErrDataSize, len(b), total)
	}
	rest := b[NPLSize:total]
	nph, err := ParseNPH(rest)
	if err != nil {
		return Frame{}, err
	}
	if len(rest) < NPHSize {
		return Frame{}, fmt.Errorf("%w: dataSize=%d меньше NPH (%d)", ErrDataSize, npl.DataSize, NPHSize)
	}
	if got := checksum(rest); swapBytes(got) != npl.CRC {
		return Frame{}, fmt.Errorf("%w: в NPL 0x%04X, посчитано 0x%04X", ErrCRC, npl.CRC, swapBytes(got))
	}
	return Frame{NPL: npl, NPH: nph, Body: rest[NPHSize:], Raw: b[:total]}, nil
}

// ClassifyNPH определяет, известен ли тип пакета протоколу и несёт ли он
// телеметрию.
//
// Идентификаторы типов переиспользованы между сервисами, поэтому тип сам по
// себе не определяет назначение пакета: в GenericControls значение 100 —
// CONN_REQUEST, а в Navdata то же значение 100 — HISTORY. Наборы типов
// воспроизведены по константам классов G6NhpSrvGenericControlsConstant и
// G6NphSrvNavdataConstant вендора.
//
// Первый результат сообщает, входит ли тип в известный набор, второй —
// содержит ли пакет поток ячеек телематрии. Неизвестный тип требует отказа,
// известный, но не телеметрический (RESULT, HISTORY) — пропускается.
func ClassifyNPH(serviceID, nphType uint16) (known, telemetry bool) {
	switch serviceID {
	case ServiceGenericControls:
		switch nphType {
		case NPHTypeResult, NPHTypeConnRequest:
			return true, false
		}
	case ServiceNavdata:
		switch nphType {
		case NPHTypeResult, NPHTypeHistory:
			return true, false
		case NPHTypeRealtime:
			return true, true
		}
	}
	return false, false
}

// ValidateService проверяет, что serviceId равен ожидаемому.
func (f Frame) ValidateService(serviceID uint16) error {
	if f.NPH.ServiceID != serviceID {
		return fmt.Errorf("%w: получено %d, ожидалось %d", ErrServiceID, f.NPH.ServiceID, serviceID)
	}
	return nil
}

// ValidateType проверяет, что тип NPH равен ожидаемому.
func (f Frame) ValidateType(nphType uint16) error {
	if f.NPH.Type != nphType {
		return fmt.Errorf("%w: получено %d, ожидалось %d", ErrNPHType, f.NPH.Type, nphType)
	}
	return nil
}
