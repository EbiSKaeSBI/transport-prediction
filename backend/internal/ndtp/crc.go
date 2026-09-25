package ndtp

const crc16Init uint16 = 0xFFFF

const crc16Poly uint16 = 0xA001

var modbusTable = buildModbusTable()

func buildModbusTable() [256]uint16 {
	var table [256]uint16
	for i := range table {
		crc := uint16(i)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ crc16Poly
			} else {
				crc >>= 1
			}
		}
		table[i] = crc
	}
	return table
}

func checksum(data []byte) uint16 {
	crc := crc16Init
	for _, b := range data {
		crc = crc>>8 ^ modbusTable[byte(crc)^b]
	}
	return crc
}

func swapBytes(v uint16) uint16 {
	return v<<8 | v>>8
}
