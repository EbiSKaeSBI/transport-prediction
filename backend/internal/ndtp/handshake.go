package ndtp

import (
	"encoding/binary"
	"fmt"
)

// Фиксированные значения тела handshake-пакета.
const (
	// ConnRequestBodySize — размер тела ConnRequest в байтах.
	ConnRequestBodySize = 18
	// ProtocolVersionHigh — старшая часть версии протокола.
	ProtocolVersionHigh = 6
	// ProtocolVersionLow — младшая часть версии протокола.
	ProtocolVersionLow = 2
	// DefaultMaxPacket — размер пакета по умолчанию.
	DefaultMaxPacket = 65535
)

// Флаги ConnRequest. Младшие три бита слова Flags объявлены отдельными полями
// класса G6NphConnRequest вендора, остальные 13 бит — единое поле
// connectionFlags.
const (
	// ConnFlagEncryption — бит 0: соединение зашифровано. Парсер читает
	// только открытый текст, поэтому handshake с таким флагом отклоняется.
	ConnFlagEncryption uint16 = 1 << 0
	// ConnFlagCRC — бит 1: устройство объявляет использование контрольной
	// суммы. Вендор не выставляет этот бит, но CRC в NPL считает и проверяет
	// всегда, поэтому флаг не управляет проверкой суммы.
	ConnFlagCRC uint16 = 1 << 1
	// ConnFlagSimulate — бит 2: устройство в режиме симуляции.
	ConnFlagSimulate uint16 = 1 << 2
)

// ConnRequest — тело handshake-пакета (NPH type 100).
type ConnRequest struct {
	// ProtoVersionHigh — старшая часть версии протокола, у эмулятора 6.
	ProtoVersionHigh uint16
	// ProtoVersionLow — младшая часть версии протокола, у эмулятора 2.
	ProtoVersionLow uint16
	// Flags — флаги соединения, см. ConnFlagCRC и остальные.
	Flags uint16
	// PeerAddress — адрес узла.
	PeerAddress uint32
	// MaxPacketSize — максимальный размер пакета, который готов принять узел.
	MaxPacketSize uint32
	// Reserved — зарезервированное поле, не используется.
	Reserved uint32
}

// ParseConnRequest разбирает тело handshake-пакета. Требует не менее
// ConnRequestBodySize байт.
func ParseConnRequest(b []byte) (ConnRequest, error) {
	if len(b) < ConnRequestBodySize {
		return ConnRequest{}, fmt.Errorf("%w: handshake %d байт, нужно %d", ErrBodyShort, len(b), ConnRequestBodySize)
	}
	return ConnRequest{
		ProtoVersionHigh: binary.LittleEndian.Uint16(b[0:2]),
		ProtoVersionLow:  binary.LittleEndian.Uint16(b[2:4]),
		Flags:            binary.LittleEndian.Uint16(b[4:6]),
		PeerAddress:      binary.LittleEndian.Uint32(b[6:10]),
		MaxPacketSize:    binary.LittleEndian.Uint32(b[10:14]),
		Reserved:         binary.LittleEndian.Uint32(b[14:18]),
	}, nil
}

// Version возвращает версию протокола в виде строки, например "6.2".
func (c ConnRequest) Version() string {
	return fmt.Sprintf("%d.%d", c.ProtoVersionHigh, c.ProtoVersionLow)
}

// Encryption сообщает, объявлено ли шифрование соединения.
func (c ConnRequest) Encryption() bool { return c.Flags&ConnFlagEncryption != 0 }

// CRCEnabled сообщает, установлен ли флаг использования контрольной суммы.
func (c ConnRequest) CRCEnabled() bool { return c.Flags&ConnFlagCRC != 0 }

// Simulated сообщает, работает ли устройство в режиме симуляции.
func (c ConnRequest) Simulated() bool { return c.Flags&ConnFlagSimulate != 0 }

// Validate проверяет, что handshake принят парсером.
//
// Единственная причина отказа — объявленное шифрование: парсер не
// реализует криптографию, и продолжение работы означало бы разбор
// шифротекста как обычного потока. Версию протокола проверять не следует:
// она передаётся для диагностики, а эмулятор сообщает 6.2.
func (c ConnRequest) Validate() error {
	if c.Encryption() {
		return fmt.Errorf("%w: ConnRequest объявляет шифрование (flags=0x%04X)", ErrEncrypted, c.Flags)
	}
	return nil
}
