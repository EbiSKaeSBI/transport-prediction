package ndtp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestChecksumModbus(t *testing.T) {
	got := checksum([]byte("123456789"))
	if got != 0x4B37 {
		t.Fatalf("CRC-16/Modbus(\"123456789\") = 0x%04X, ожидалось 0x4B37", got)
	}
}

func TestChecksumEmpty(t *testing.T) {
	if got := checksum(nil); got != 0xFFFF {
		t.Fatalf("CRC пустого сообщения = 0x%04X, ожидалось 0xFFFF", got)
	}
}

func TestSwapBytes(t *testing.T) {
	if got := swapBytes(0x1234); got != 0x3412 {
		t.Fatalf("swapBytes(0x1234) = 0x%04X, ожидалось 0x3412", got)
	}
}

func loadGolden(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "packets.bin"))
	if err != nil {
		t.Fatalf("не читать golden-файл: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("golden-файл пуст")
	}
	return data
}

func goldenFrames(t *testing.T) []Frame {
	t.Helper()
	data := loadGolden(t)
	reader := NewReader(bytes.NewReader(data))
	var frames []Frame
	for {
		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("кадр %d: %v", len(frames), err)
		}
		frames = append(frames, frame)
		if len(frames) > 1000 {
			t.Fatal("подозрительно много кадров")
		}
	}
	if len(frames) == 0 {
		t.Fatal("golden-файл не содержит кадров")
	}
	return frames
}

func TestGoldenFramesParse(t *testing.T) {
	frames := goldenFrames(t)
	for i, frame := range frames {
		if frame.NPL.Signature != Signature {
			t.Fatalf("кадр %d: сигнатура 0x%04X", i, frame.NPL.Signature)
		}
		if frame.NPL.FrameType != NPLTypeNPH {
			t.Fatalf("кадр %d: тип кадра 0x%02X", i, frame.NPL.FrameType)
		}
		if frame.NPH.ServiceID != ServiceNavdata {
			t.Fatalf("кадр %d: serviceId %d, ожидался NAVDATA", i, frame.NPH.ServiceID)
		}
		if frame.NPH.Type != NPHTypeRealtime {
			t.Fatalf("кадр %d: тип NPH %d, ожидался REALTIME", i, frame.NPH.Type)
		}
		if frame.NPL.PeerAddress != 1166336 {
			t.Fatalf("кадр %d: peer %d, ожидалось 1166336", i, frame.NPL.PeerAddress)
		}
		if len(frame.Raw) != NPLSize+int(frame.NPL.DataSize) {
			t.Fatalf("кадр %d: длина %d не совпадает с dataSize %d",
				i, len(frame.Raw), frame.NPL.DataSize)
		}
	}
}

func TestGoldenCellsDecode(t *testing.T) {
	for i, frame := range goldenFrames(t) {
		cells, err := ParseCells(frame.Body)
		if err != nil {
			t.Fatalf("кадр %d: %v", i, err)
		}
		if len(cells) != 5 {
			t.Fatalf("кадр %d: ячеек %d, ожидалось 5", i, len(cells))
		}
		wantTypes := []CellType{CellNav00, CellUsi08, CellTermo16, CellIntSensor02, CellCan10}
		for j, want := range wantTypes {
			if cells[j].Type() != want {
				t.Fatalf("кадр %d ячейка %d: тип %s, ожидался %s", i, j, cells[j].Type(), want)
			}
		}
		nav, ok := cells[0].Nav00()
		if !ok {
			t.Fatalf("кадр %d: первая ячейка не Nav00", i)
		}
		if !nav.LocationValid() {
			t.Fatalf("кадр %d: location_valid=false, хотя байты 0xE0", i)
		}
		lat, lon := nav.Latitude(), nav.Longitude()
		if lat < 55.0 || lat > 56.5 {
			t.Fatalf("кадр %d: широта %.7f вне Москвы", i, lat)
		}
		if lon < 37.0 || lon > 38.0 {
			t.Fatalf("кадр %d: долгота %.7f вне Москвы", i, lon)
		}
		if _, ok := cells[1].Usi08(); !ok {
			t.Fatalf("кадр %d: вторая ячейка не Usi08", i)
		}
		if _, ok := cells[2].Termo16(); !ok {
			t.Fatalf("кадр %d: третья ячейка не Termo16", i)
		}
		if _, ok := cells[3].IntSensor02(); !ok {
			t.Fatalf("кадр %d: четвёртая ячейка не IntSensor02", i)
		}
		if _, ok := cells[4].Can10(); !ok {
			t.Fatalf("кадр %d: пятая ячейка не Can10", i)
		}
	}
}

func TestNav00CoordinateSigns(t *testing.T) {
	cases := []struct {
		name     string
		extraDop uint8
		wantLat  float64
		wantLon  float64
	}{
		{"север и восток", ExtraDopLatNorth | ExtraDopLonEast, 55.7551234, 37.6173210},
		{"юг и запад", 0, -55.7551234, -37.6173210},
		{"север и запад", ExtraDopLatNorth, 55.7551234, -37.6173210},
		{"юг и восток", ExtraDopLonEast, -55.7551234, 37.6173210},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cell := Nav00{
				LatitudeRaw:  557551234,
				LongitudeRaw: 376173210,
				ExtraDop:     tc.extraDop,
			}
			if got := cell.Latitude(); math.Abs(got-tc.wantLat) > 1e-9 {
				t.Errorf("Latitude() = %.10f, ожидалось %.10f", got, tc.wantLat)
			}
			if got := cell.Longitude(); math.Abs(got-tc.wantLon) > 1e-9 {
				t.Errorf("Longitude() = %.10f, ожидалось %.10f", got, tc.wantLon)
			}
		})
	}
}

func TestNav00ValidityBit(t *testing.T) {
	valid := Nav00{ExtraDop: ExtraDopValid}
	if !valid.LocationValid() {
		t.Error("бит 7 должен означать location_valid")
	}
	invalid := Nav00{ExtraDop: ExtraDopValid - 1}
	if invalid.LocationValid() {
		t.Error("без бита 7 location_valid должен быть false")
	}
}

func TestCellSizesMatchConstants(t *testing.T) {
	want := map[CellType]int{
		CellNav00: 26, CellIntSensor02: 26, CellCrown03: 14, CellIrma04: 15,
		CellKdm05: 6, CellIdn06: 9, CellIdn07: 1, CellUsi08: 6,
		CellReg09: 40, CellCan10: 37, CellRfid12: 5, CellPlo13: 13,
		CellBms14: 15, CellLls15: 50, CellTermo16: 8, CellAlcohol1st17: 46,
		CellCAN18: 50, CellGSMstations19: 40, CellM333CAN20: 8,
		CellAlcohol2nd21: 180, CellServerStatistics22: 24,
		CellTrackerStatistics23: 16, CellZipSensorData100: 44,
	}
	if len(cellSizes) != len(want) {
		t.Fatalf("в таблице %d типов, ожидалось %d", len(cellSizes), len(want))
	}
	for cellType, size := range want {
		got, ok := cellType.Size()
		if !ok {
			t.Errorf("%s: размера нет в таблице", cellType)
			continue
		}
		if got != size {
			t.Errorf("%s: размер %d, ожидалось %d", cellType, got, size)
		}
	}
}

func TestScanCellsGolden(t *testing.T) {
	for i, frame := range goldenFrames(t) {
		headers, err := ScanCells(frame.Body)
		if err != nil {
			t.Fatalf("кадр %d: %v", i, err)
		}
		if len(headers) != 5 {
			t.Fatalf("кадр %d: найдено %d ячеек, ожидалось 5", i, len(headers))
		}
		total := 0
		for _, header := range headers {
			total += 2 + header.Size
		}
		if total != len(frame.Body) {
			t.Fatalf("кадр %d: сумма ячеек %d, тело %d", i, total, len(frame.Body))
		}
	}
}

func buildFrame(t *testing.T, serviceID, nphType uint16, body []byte) []byte {
	t.Helper()
	nph := make([]byte, NPHSize+len(body))
	binary.LittleEndian.PutUint16(nph[0:2], serviceID)
	binary.LittleEndian.PutUint16(nph[2:4], nphType)
	binary.LittleEndian.PutUint16(nph[4:6], NPHFlagRequest)
	binary.LittleEndian.PutUint32(nph[6:10], 42)
	copy(nph[NPHSize:], body)

	frame := make([]byte, NPLSize+len(nph))
	binary.LittleEndian.PutUint16(frame[0:2], Signature)
	binary.LittleEndian.PutUint16(frame[2:4], uint16(len(nph)))
	binary.LittleEndian.PutUint16(frame[4:6], 0)
	binary.LittleEndian.PutUint16(frame[6:8], swapBytes(checksum(nph)))
	frame[8] = NPLTypeNPH
	binary.LittleEndian.PutUint32(frame[9:13], 4242)
	binary.LittleEndian.PutUint16(frame[13:15], 7)
	copy(frame[NPLSize:], nph)
	return frame
}

func buildUsi08(number uint8, level uint16) []byte {
	cell := make([]byte, 2+SizeUsi08)
	cell[0] = byte(CellUsi08)
	cell[1] = number
	cell[2] = 1
	binary.LittleEndian.PutUint16(cell[3:5], level)
	binary.LittleEndian.PutUint16(cell[5:7], level/2)
	cell[7] = 20
	return cell
}

func TestTwoUsi08InOnePacket(t *testing.T) {
	body := append(buildUsi08(1, 1000), buildUsi08(2, 2000)...)
	frame := buildFrame(t, ServiceNavdata, NPHTypeRealtime, body)

	parsed, err := ParseFrame(frame)
	if err != nil {
		t.Fatalf("разбор кадра: %v", err)
	}
	cells, err := ParseCells(parsed.Body)
	if err != nil {
		t.Fatalf("разбор ячеек: %v", err)
	}
	if len(cells) != 2 {
		t.Fatalf("ячеек %d, ожидалось 2", len(cells))
	}
	if cells[0].Number != 1 || cells[1].Number != 2 {
		t.Errorf("номера %d и %d, ожидались 1 и 2", cells[0].Number, cells[1].Number)
	}
	first, _ := cells[0].Usi08()
	second, _ := cells[1].Usi08()
	if first.LevelMM != 1000 {
		t.Errorf("LevelMM первой ячейки %d, ожидалось 1000", first.LevelMM)
	}
	if second.LevelMM != 2000 {
		t.Errorf("LevelMM второй ячейки %d, ожидалось 2000", second.LevelMM)
	}
}

func TestParseFrameDetectsBadCRC(t *testing.T) {
	frame := buildFrame(t, ServiceNavdata, NPHTypeRealtime, buildUsi08(1, 500))
	frame[NPLSize+NPHSize+2] ^= 0xFF

	_, err := ParseFrame(frame)
	if !errors.Is(err, ErrCRC) {
		t.Fatalf("ожидалась ErrCRC, получено %v", err)
	}
}

func TestParseFrameDetectsBadSignature(t *testing.T) {
	frame := buildFrame(t, ServiceNavdata, NPHTypeRealtime, buildUsi08(1, 500))
	frame[0] = 0x00

	_, err := ParseFrame(frame)
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("ожидалась ErrSignature, получено %v", err)
	}
}

func TestParseFrameRejectsTruncated(t *testing.T) {
	frame := buildFrame(t, ServiceNavdata, NPHTypeRealtime, buildUsi08(1, 500))

	if _, err := ParseFrame(frame[:len(frame)-3]); !errors.Is(err, ErrDataSize) {
		t.Fatalf("обрезка тела: ожидалась ErrDataSize, получено %v", err)
	}
	if _, err := ParseFrame(frame[:8]); !errors.Is(err, ErrShortFrame) {
		t.Fatalf("обрезка NPL: ожидалась ErrShortFrame, получено %v", err)
	}
}

func TestParseFrameRejectsBadDataSize(t *testing.T) {
	frame := buildFrame(t, ServiceNavdata, NPHTypeRealtime, buildUsi08(1, 500))
	binary.LittleEndian.PutUint16(frame[2:4], 3)

	_, err := ParseFrame(frame)
	if !errors.Is(err, ErrDataSize) {
		t.Fatalf("dataSize меньше NPH: ожидалась ErrDataSize, получено %v", err)
	}
}

func TestParseCellsRejectsUnknownType(t *testing.T) {
	body := make([]byte, 2+4)
	body[0] = 1
	body[1] = 0

	if _, err := ParseCells(body); err == nil {
		t.Fatal("ожидалась ошибка на неизвестном типе ячейки 1")
	}
}

func TestParseCellsRejectsShortPayload(t *testing.T) {
	body := make([]byte, 2+SizeUsi08-1)
	body[0] = byte(CellUsi08)

	_, err := ParseCells(body)
	if err == nil {
		t.Fatal("ожидалась ошибка на неполном payload ячейки")
	}
}

func TestParseCellsRejectsTrailingGarbage(t *testing.T) {
	body := append(buildUsi08(1, 100), 0xFF, 0xFF)

	if _, err := ParseCells(body); err == nil {
		t.Fatal("ожидалась ошибка на мусор в конце тела")
	}
}

func TestParseCellRequiresExactLength(t *testing.T) {
	body := append(buildUsi08(1, 100), 0x00)

	if _, err := ParseCell(body); err == nil {
		t.Fatal("ParseCell должен требовать ровно одну ячейку")
	}
	if _, err := ParseCells(body); err == nil {
		t.Fatal("хвост должен давать ошибку разбора ячеек")
	}
}

func TestReaderStopsAtEOF(t *testing.T) {
	reader := NewReader(bytes.NewReader(nil))
	if _, err := reader.Next(); err == nil {
		t.Fatal("ожидалась ошибка на пустом потоке")
	}
}

func TestConnRequestRoundTrip(t *testing.T) {
	body := make([]byte, ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ProtocolVersionLow)
	binary.LittleEndian.PutUint16(body[4:6], ConnFlagCRC)
	binary.LittleEndian.PutUint32(body[6:10], 1166336)
	binary.LittleEndian.PutUint32(body[10:14], DefaultMaxPacket)

	request, err := ParseConnRequest(body)
	if err != nil {
		t.Fatalf("разбор handshake: %v", err)
	}
	if request.Version() != "6.2" {
		t.Errorf("версия %q, ожидалась 6.2", request.Version())
	}
	if request.PeerAddress != 1166336 {
		t.Errorf("peer %d, ожидалось 1166336", request.PeerAddress)
	}
	if request.MaxPacketSize != DefaultMaxPacket {
		t.Errorf("maxPacket %d, ожидалось %d", request.MaxPacketSize, DefaultMaxPacket)
	}
	if !request.CRCEnabled() {
		t.Error("флаг CRC должен быть установлен")
	}
	if request.Encryption() {
		t.Error("шифрование не должно быть включено")
	}
}

func TestConnRequestRejectsShortBody(t *testing.T) {
	if _, err := ParseConnRequest(make([]byte, ConnRequestBodySize-1)); !errors.Is(err, ErrBodyShort) {
		t.Fatalf("ожидалась ErrBodyShort, получено %v", err)
	}
}

func TestCan10FuelFlags(t *testing.T) {
	percent := Can10{FuelLevel: 0x8000 | 55}
	if !percent.FuelIsPercent() {
		t.Error("старший бит должен означать проценты")
	}
	if percent.FuelValue() != 55 {
		t.Errorf("FuelValue %d, ожидалось 55", percent.FuelValue())
	}
	absolute := Can10{FuelLevel: 300}
	if absolute.FuelIsPercent() {
		t.Error("без старшего бита это абсолютное значение")
	}
	if absolute.FuelValue() != 300 {
		t.Errorf("FuelValue %d, ожидалось 300", absolute.FuelValue())
	}
}

func TestTermo16Connected(t *testing.T) {
	if !(Termo16{Status: 0}).Connected() {
		t.Error("Status=0 должен означать подключённый датчик")
	}
	if (Termo16{Status: 1}).Connected() {
		t.Error("Status=1 должен означать отключённый датчик")
	}
}

func TestCellTypeStringUnknown(t *testing.T) {
	if got := CellType(1).String(); got != "CellType(1)" {
		t.Errorf("String() = %q, ожидалось CellType(1)", got)
	}
	if got := CellNav00.String(); got != "G6CellNav00" {
		t.Errorf("String() = %q, ожидалось G6CellNav00", got)
	}
}

// allKnownCellTypes перечисляет все типы, зарегистрированные в протоколе, в
// порядке возрастания номера.
func allKnownCellTypes() []CellType {
	return []CellType{
		CellNav00, CellIntSensor02, CellCrown03, CellIrma04, CellKdm05,
		CellIdn06, CellIdn07, CellUsi08, CellReg09, CellCan10, CellRfid12,
		CellPlo13, CellBms14, CellLls15, CellTermo16, CellAlcohol1st17,
		CellCAN18, CellGSMstations19, CellM333CAN20, CellAlcohol2nd21,
		CellServerStatistics22, CellTrackerStatistics23, CellZipSensorData100,
	}
}

func TestEveryKnownCellTypeHasParser(t *testing.T) {
	for _, cellType := range allKnownCellTypes() {
		if _, ok := cellSizes[cellType]; !ok {
			t.Errorf("тип %s не зарегистрирован в таблице размеров", cellType)
		}
		if _, ok := cellNames[cellType]; !ok {
			t.Errorf("у типа %s нет имени", cellType)
		}
		if _, ok := payloadParsers[cellType]; !ok {
			t.Errorf("у типа %s нет декодера", cellType)
		}
	}
	if len(payloadParsers) != len(cellSizes) {
		t.Errorf("декодеров %d, типов %d", len(payloadParsers), len(cellSizes))
	}
}

func TestAllCellTypesDecodeGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "allcells.bin"))
	if err != nil {
		t.Fatalf("не читать golden-файл: %v", err)
	}
	reader := NewReader(bytes.NewReader(data))
	frame, err := reader.Next()
	if err != nil {
		t.Fatalf("первый кадр: %v", err)
	}
	cells, err := ParseCells(frame.Body)
	if err != nil {
		t.Fatalf("разбор тела со всеми типами: %v", err)
	}
	want := allKnownCellTypes()
	if len(cells) != len(want) {
		t.Fatalf("ячеек %d, ожидалось %d", len(cells), len(want))
	}
	for i, cellType := range want {
		if cells[i].Type() != cellType {
			t.Errorf("ячейка %d: тип %s, ожидался %s", i, cells[i].Type(), cellType)
		}
	}
}

func TestEachCellTypeRejectsShortPayload(t *testing.T) {
	for _, cellType := range allKnownCellTypes() {
		size, ok := cellType.Size()
		if !ok {
			t.Fatalf("у типа %s нет размера", cellType)
		}
		body := append([]byte{uint8(cellType), 0}, make([]byte, size-1)...)
		if _, err := ParseCells(body); err == nil {
			t.Errorf("тип %s: payload на %d байт меньше ожидался как ошибка",
				cellType, size-1)
		}
	}
}

func TestZipSensorData100Fields(t *testing.T) {
	body := make([]byte, SizeZipSensor100)
	body[0] = 7
	body[1] = 0xA0
	body[2] = 0x20
	cells, err := ParseCells(append([]byte{uint8(CellZipSensorData100), 0}, body...))
	if err != nil {
		t.Fatalf("разбор ZipSensorData100: %v", err)
	}
	zip, ok := cells[0].ZipSensorData100()
	if !ok {
		t.Fatal("первая ячейка не ZipSensorData100")
	}
	if zip.IDSensor != 7 || zip.Flag != 0xA0 || zip.LenData != 32 {
		t.Errorf("поля заголовка разобраны неверно: %+v", zip)
	}
}

// wireFormatCells читает пакет, в котором эмулятору через конфигурацию заданы
// различимые значения всех полей всех типов ячеек, и разбирает его целиком.
func wireFormatCells(t *testing.T) []Cell {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "allfields.bin"))
	if err != nil {
		t.Fatalf("не читать golden-файл: %v", err)
	}
	frame, err := NewReader(bytes.NewReader(data)).Next()
	if err != nil {
		t.Fatalf("первый кадр: %v", err)
	}
	cells, err := ParseCells(frame.Body)
	if err != nil {
		t.Fatalf("разбор тела: %v", err)
	}
	return cells
}

func TestWireFormatFieldsMatchVendorSerializer(t *testing.T) {
	byType := make(map[CellType]Payload)
	for _, cell := range wireFormatCells(t) {
		byType[cell.Type()] = cell.Payload
	}
	if len(byType) != len(allKnownCellTypes()) {
		t.Fatalf("разобрано %d уникальных типов, ожидалось %d",
			len(byType), len(allKnownCellTypes()))
	}

	seqBytes := func(from, n int) []byte {
		out := make([]byte, n)
		for i := range out {
			out[i] = byte(from + i)
		}
		return out
	}
	u16s := func(from, n int) []uint16 {
		out := make([]uint16, n)
		for i := range out {
			out[i] = uint16(from + i)
		}
		return out
	}
	u32s := func(from, n int) []uint32 {
		out := make([]uint32, n)
		for i := range out {
			out[i] = uint32(from + i)
		}
		return out
	}

	checks := []struct {
		cellType CellType
		want     Payload
	}{
		{CellCrown03, Crown03{
			Odometer: 1, Zone: 2,
			DoorIn:  [4]uint8{3, 4, 5, 6},
			DoorOut: [4]uint8{7, 8, 9, 10},
		}},
		{CellIrma04, Irma04{
			Odometer: 2, Zone: 3,
			DoorIn:  [4]uint8{4, 5, 6, 7},
			DoorOut: [4]uint8{8, 9, 10, 11},
			Present: [4]bool{true, false, true, false},
			Closed:  [4]bool{true, false, true, false},
		}},
		{CellKdm05, Kdm05{
			PgmEnable: 1, PgmWidth: 2, PgmDensity: 3, PloughState: 4, BrushState: 5,
		}},
		{CellIdn06, Idn06{
			NumImpulseMin: 1, NumImpulseMax: 2, Time: 3,
			NumOverflow: 4, NumImpulse: 5, PresImpulse: 6,
		}},
		{CellIdn07, Idn07{Value: 1}},
		{CellReg09, Reg09{
			ID:   -1,
			Name: [32]byte(seqBytes(2, 32)),
		}},
		{CellRfid12, Rfid12{Key: [5]byte(seqBytes(1, 5))}},
		{CellPlo13, Plo13{
			Density: 1, Temperature: 2, Level: 3, LevelUnit: 4,
		}},
		{CellBms14, Bms14{
			MaxTemperature: 2, MinCellVoltage: 3, MaxCellVoltage: 4, Voltage: 5,
			CodeError0: true, CodeError1: false, CodeError2: true, CodeError3: false,
			CodeError: 2, Current: 11,
		}},
		{CellM333CAN20, M333CAN20{FlagHigh: 1, FlagLow: 2}},
		{CellServerStatistics22, ServerStatistics22{
			IDMax: 1, IDMin: 2, TmOldest: 3,
			TmOldestUnack: 4, CntUnack: 5, CntUnackLosted: 6,
		}},
		{CellTrackerStatistics23, TrackerStatistics23{
			CntAck: 1, CntAckRealtime: 2, CntNoack: 3, CntConnect: 4,
		}},
		{CellZipSensorData100, ZipSensorData100{
			IDSensor: 1, Flag: 2, LenData: 3, Data: [40]byte(seqBytes(4, 40)),
		}},
		{CellCAN18, CAN18{
			AnIn:      [8]uint16(u16s(1, 8)),
			DiIn:      9,
			DiOut:     10,
			DiCounter: [8]uint32(u32s(11, 8)),
		}},
		{CellGSMstations19, GSMstations19{
			MCC: 1, MNC: 2, LAC: 3, CID: 4, RSSI: 5, TimeAdv: 6,
			Neighbors: [6]GSMStation{
				{LAC: 7, CID: 8, RSSI: 9},
				{LAC: 10, CID: 11, RSSI: 12},
				{LAC: 13, CID: 14, RSSI: 15},
				{LAC: 16, CID: 17, RSSI: 18},
				{LAC: 19, CID: 20, RSSI: 21},
				{LAC: 22, CID: 23, RSSI: 24},
			},
		}},
		{CellAlcohol1st17, Alcohol1st17{
			Status: 1, AlcoEvent: 2, AlcoholVolume: 3, StartTime: 4, EndTime: 5,
			RecordCounter: 6, Temperature: 7, ReadAlcoEvent: 8, ReadAlcoholVolume: 9,
			ReadStartTime: 10, ReadEndTime: 11, CurrentTime: 12, DeviceStatus: 13,
			CurrentVolume: 14, CurrentSection: 15, Reserved16: 16, Reserved32: 17,
		}},
		{CellAlcohol2nd21, Alcohol2nd21{
			Status:                 1,
			RecordCounter:          2,
			DeviceStatusSection:    3,
			CurrentTime:            4,
			CurrentVolume:          5,
			AlcoEventSection:       6,
			SensorSerialNumber:     [8]byte(seqBytes(7, 8)),
			CounterStartValue:      15,
			CounterStopValue:       16,
			StartTime:              17,
			EndTime:                18,
			FinalTemperature:       19,
			Temperature:            20,
			Reserved16:             21,
			ReadAlcoEventSection:   22,
			ReadSensorSerialNumber: [8]byte(seqBytes(23, 8)),
			ReadCounterStartValue:  31,
			ReadCounterStopValue:   32,
			ReadStartTime:          33,
			ReadEndTime:            34,
			ReadFinalTemperature:   35,
			ProductType:            36,
			ProductCode:            [20]byte(seqBytes(37, 20)),
			OrganizationCode:       [18]byte(seqBytes(57, 18)),
			InstallDateTime:        [16]byte(seqBytes(75, 16)),
			ErrorDescription:       [50]byte(seqBytes(91, 50)),
		}},
	}
	for _, check := range checks {
		got, ok := byType[check.cellType]
		if !ok {
			t.Errorf("в пакете нет ячейки %s", check.cellType)
			continue
		}
		if !reflect.DeepEqual(got, check.want) {
			t.Errorf("%s разобран неверно:\n получено %+v\n ожидалось %+v",
				check.cellType, got, check.want)
		}
	}
}

func TestReg09NameString(t *testing.T) {
	cell := Reg09{ID: -42}
	copy(cell.Name[:], "bus-17")
	if got := cell.NameString(); got != "bus-17" {
		t.Errorf("NameString() = %q, ожидалось bus-17", got)
	}
	if got := (Reg09{}).NameString(); got != "" {
		t.Errorf("пустое имя дало %q", got)
	}
}

func TestBms14BitFields(t *testing.T) {
	body := make([]byte, SizeBms14)
	body[10] = 0xB5
	cells, err := ParseCells(append([]byte{uint8(CellBms14), 0}, body...))
	if err != nil {
		t.Fatalf("разбор Bms14: %v", err)
	}
	bms, ok := cells[0].Bms14()
	if !ok {
		t.Fatal("первая ячейка не Bms14")
	}
	if bms.CodeError0 != true || bms.CodeError1 != false ||
		bms.CodeError2 != true || bms.CodeError3 != false {
		t.Errorf("биты ошибок разобраны неверно: %+v", bms)
	}
	if bms.CodeError != 11 {
		t.Errorf("CodeError %d, ожидалось 11", bms.CodeError)
	}
}

func TestIrma04BitFields(t *testing.T) {
	body := make([]byte, SizeIrma04)
	body[14] = 0x25
	cells, err := ParseCells(append([]byte{uint8(CellIrma04), 0}, body...))
	if err != nil {
		t.Fatalf("разбор Irma04: %v", err)
	}
	irma, ok := cells[0].Irma04()
	if !ok {
		t.Fatal("первая ячейка не Irma04")
	}
	wantPresent := [4]bool{true, false, true, false}
	wantClosed := [4]bool{false, true, false, false}
	if irma.Present != wantPresent {
		t.Errorf("Present %v, ожидалось %v", irma.Present, wantPresent)
	}
	if irma.Closed != wantClosed {
		t.Errorf("Closed %v, ожидалось %v", irma.Closed, wantClosed)
	}
}

func TestGSMstations19Neighbors(t *testing.T) {
	body := make([]byte, SizeGSMstations19)
	body[0] = 0xD2
	body[1] = 0x04
	body[2] = 0x04
	cells, err := ParseCells(append([]byte{uint8(CellGSMstations19), 0}, body...))
	if err != nil {
		t.Fatalf("разбор GSMstations19: %v", err)
	}
	gsm, ok := cells[0].GSMstations19()
	if !ok {
		t.Fatal("первая ячейка не GSMstations19")
	}
	if gsm.MCC != 1234 {
		t.Errorf("MCC %d, ожидалось 1234", gsm.MCC)
	}
	if gsm.MNC != 4 {
		t.Errorf("MNC %d, ожидалось 4", gsm.MNC)
	}
	if gsm.Neighbors[0].LAC != 0 || gsm.Neighbors[5].RSSI != 0 {
		t.Errorf("соседние станции не обнулены: %+v", gsm.Neighbors)
	}
}
