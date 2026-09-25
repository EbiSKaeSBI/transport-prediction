package ndtp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// buildTestFrame собирает корректный кадр NDTP: NPL, один NPH и тело.
func buildTestFrame(t *testing.T, serviceID, nphType uint16, nplFlags uint16, body []byte) []byte {
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
	binary.LittleEndian.PutUint16(frame[4:6], nplFlags)
	binary.LittleEndian.PutUint16(frame[6:8], swapBytes(checksum(nph)))
	frame[8] = NPLTypeNPH
	binary.LittleEndian.PutUint32(frame[9:13], 4242)
	binary.LittleEndian.PutUint16(frame[13:15], 7)
	copy(frame[NPLSize:], nph)
	return frame
}

// Риск 1. Вендорская функция wrapNphPacket записывает dataSize как
// nph.size() + payload.size(), то есть в кадре ровно один NPH. Тест
// закрепляет равенство dataSize = NPHSize + длина тела на реальных
// пакетах эмулятора и на собранных вручную.
func TestDataSizeIsOneNPHPlusBody(t *testing.T) {
	for _, body := range [][]byte{nil, make([]byte, 1), make([]byte, 700)} {
		frame := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, 0, body)
		parsed, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("тело %d байт: %v", len(body), err)
		}
		want := NPHSize + len(body)
		if int(parsed.NPL.DataSize) != want {
			t.Errorf("dataSize %d, ожидалось %d", parsed.NPL.DataSize, want)
		}
		if len(parsed.Body) != len(body) {
			t.Errorf("тело %d байт, ожидалось %d", len(parsed.Body), len(body))
		}
		if len(parsed.Raw) != NPLSize+want {
			t.Errorf("Raw %d байт, ожидалось %d", len(parsed.Raw), NPLSize+want)
		}
	}
}

func TestDataSizeMatchesGoldenCapture(t *testing.T) {
	path := filepath.Join("testdata", "golden", "allcells.bin")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение golden-файла: %v", err)
	}
	reader := NewReader(bytes.NewReader(blob))
	count := 0
	for {
		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("кадр %d: %v", count, err)
		}
		if int(frame.NPL.DataSize) != NPHSize+len(frame.Body) {
			t.Errorf("кадр %d: dataSize %d != NPH+body %d",
				count, frame.NPL.DataSize, NPHSize+len(frame.Body))
		}
		count++
	}
	if count == 0 {
		t.Fatal("golden-файл не содержит кадров")
	}
	t.Logf("проверено %d кадров реального захвата", count)
}

// Риск 2. NPL с флагом шифрования должен отбрасываться с ErrEncrypted,
// а не разбираться как открытый текст.
func TestParseFrameRejectsEncryptedNPL(t *testing.T) {
	body := make([]byte, 2+SizeNav00)
	body[0] = byte(CellNav00)
	frame := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, NPLFlagEncryption, body)

	parsed, err := ParseFrame(frame)
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("ожидалась ErrEncrypted, получено %v", err)
	}
	if parsed.Raw != nil {
		t.Error("при ошибке кадр не должен возвращаться")
	}
}

// Флаг шифрования в NPL не должен влиять на разбор, если он не установлен:
// соседние биты выставлять допустимо.
func TestParseFrameAcceptsOtherNPLFlags(t *testing.T) {
	body := make([]byte, 2+SizeNav00)
	body[0] = byte(CellNav00)
	for _, flags := range []uint16{0, NPLFlagCRC, NPLFlagDelay, NPLFlagCRC | NPLFlagDelay} {
		frame := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, flags, body)
		if _, err := ParseFrame(frame); err != nil {
			t.Errorf("flags 0x%04X: %v", flags, err)
		}
	}
}

// Риск 2 на уровне handshake: ConnRequest с флагом шифрования отклоняется.
func TestConnRequestValidateRejectsEncryption(t *testing.T) {
	request := ConnRequest{ProtoVersionHigh: 6, ProtoVersionLow: 2}
	if err := request.Validate(); err != nil {
		t.Fatalf("handshake без шифрования отклонён: %v", err)
	}

	request.Flags = ConnFlagEncryption
	err := request.Validate()
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("ожидалась ErrEncrypted, получено %v", err)
	}
}

// Флаги CRC и симуляции не должны приводить к отказу: они справочные.
func TestConnRequestValidateAllowsNonFatalFlags(t *testing.T) {
	for _, flags := range []uint16{
		ConnFlagCRC,
		ConnFlagSimulate,
		ConnFlagCRC | ConnFlagSimulate,
	} {
		request := ConnRequest{Flags: flags}
		if err := request.Validate(); err != nil {
			t.Errorf("flags 0x%04X отклонены: %v", flags, err)
		}
	}
}

// Риск 2. Шифрование объявляется в двух местах: в NPL и в теле handshake.
// Оба случая обязаны приводить к отказу.
func TestEncryptionDeclaredInEitherPlaceIsRejected(t *testing.T) {
	body := make([]byte, ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ProtocolVersionLow)

	t.Run("только ConnRequest", func(t *testing.T) {
		withFlag := make([]byte, ConnRequestBodySize)
		copy(withFlag, body)
		binary.LittleEndian.PutUint16(withFlag[4:6], ConnFlagEncryption)

		frame := buildTestFrame(t, ServiceGenericControls, NPHTypeConnRequest, 0, withFlag)
		parsed, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("кадр не разобран: %v", err)
		}
		request, err := ParseConnRequest(parsed.Body)
		if err != nil {
			t.Fatalf("handshake не разобран: %v", err)
		}
		if err := request.Validate(); !errors.Is(err, ErrEncrypted) {
			t.Fatalf("ожидалась ErrEncrypted, получено %v", err)
		}
	})

	t.Run("только NPL", func(t *testing.T) {
		frame := buildTestFrame(t, ServiceGenericControls, NPHTypeConnRequest, NPLFlagEncryption, body)
		if _, err := ParseFrame(frame); !errors.Is(err, ErrEncrypted) {
			t.Fatalf("ожидалась ErrEncrypted, получено %v", err)
		}
	})
}

// Риск 4. Вендор считает и записывает CRC безусловно, а бит NPLFlagCRC не
// выставляет ни в одном кадре. Значит сумма проверяется всегда: и при
// нулевом флаге, и при выставленном.
func TestCRCCheckedRegardlessOfFlag(t *testing.T) {
	body := make([]byte, 2+SizeNav00)
	body[0] = byte(CellNav00)

	for _, flags := range []uint16{0, NPLFlagCRC} {
		frame := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, flags, body)

		frameOK, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("flags 0x%04X: корректный кадр отклонён: %v", flags, err)
		}
		if frameOK.NPL.CRCFlagSet() != (flags&NPLFlagCRC != 0) {
			t.Errorf("flags 0x%04X: CRCFlagSet()=%v", flags, frameOK.NPL.CRCFlagSet())
		}

		// Порча одного байта тела обязана ломать кадр при любом флаге.
		broken := make([]byte, len(frame))
		copy(broken, frame)
		broken[len(broken)-1] ^= 0xFF

		if _, err := ParseFrame(broken); !errors.Is(err, ErrCRC) {
			t.Errorf("flags 0x%04X: ожидалась ErrCRC, получено %v", flags, err)
		}
	}
}

// Риск 4. Настоящие пакеты эмулятора несут флаги NPL = 0 и корректную
// сумму, что подтверждает безусловную проверку CRC.
func TestGoldenCaptureHasZeroFlagsAndValidCRC(t *testing.T) {
	path := filepath.Join("testdata", "golden", "allcells.bin")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение golden-файла: %v", err)
	}
	reader := NewReader(bytes.NewReader(blob))
	count := 0
	for {
		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("кадр %d: %v", count, err)
		}
		if frame.NPL.Flags != 0 {
			t.Errorf("кадр %d: флаги NPL 0x%04X, эмулятор шлёт 0", count, frame.NPL.Flags)
		}
		if frame.NPL.Encrypted() {
			t.Errorf("кадр %d: неожиданный флаг шифрования", count)
		}
		if !frame.NPH.IsRequest() {
			t.Errorf("кадр %d: NPH без бита запроса", count)
		}
		count++
	}
	t.Logf("проверено %d кадров: CRC сходится при флагах 0", count)
}

// Риск 3. Набор типов NPH зафиксирован по константам вендора:
// сервис GenericControls — RESULT(0) и CONN_REQUEST(100),
// сервис Navdata — RESULT(0), HISTORY(100) и REALTIME(101).
func TestKnownNPHTypesMatchVendor(t *testing.T) {
	cases := []struct {
		service   uint16
		nphType   uint16
		known     bool
		telemetry bool
	}{
		{ServiceGenericControls, NPHTypeResult, true, false},
		{ServiceGenericControls, NPHTypeConnRequest, true, false},
		{ServiceGenericControls, NPHTypeRealtime, false, false},
		{ServiceGenericControls, 9999, false, false},
		{ServiceNavdata, NPHTypeResult, true, false},
		{ServiceNavdata, NPHTypeHistory, true, false},
		{ServiceNavdata, NPHTypeRealtime, true, true},
		{ServiceNavdata, 9999, false, false},
		{0x7777, NPHTypeRealtime, false, false},
	}
	for _, c := range cases {
		got, telemetry := ClassifyNPH(c.service, c.nphType)
		if got != c.known {
			t.Errorf("service=%d type=%d: известен=%v, ожидалось %v",
				c.service, c.nphType, got, c.known)
		}
		if telemetry != c.telemetry {
			t.Errorf("service=%d type=%d: телеметрия=%v, ожидалось %v",
				c.service, c.nphType, telemetry, c.telemetry)
		}
	}
}

// Идентификатор типа 100 переиспользован двумя сервисами, поэтому сам по
// себе он не определяет ни handshake, ни архив.
func TestTypeHundredIsAmbiguousWithoutService(t *testing.T) {
	if NPHTypeConnRequest != NPHTypeHistory {
		t.Fatal("в константах вендора оба типа равны 100")
	}
	if _, telemetry := ClassifyNPH(ServiceGenericControls, NPHTypeConnRequest); telemetry {
		t.Error("CONN_REQUEST не должен считаться телеметрией")
	}
	if _, telemetry := ClassifyNPH(ServiceNavdata, NPHTypeHistory); telemetry {
		t.Error("HISTORY не должен считаться телеметрией")
	}
}

// Риск 1. Кадр с dataSize меньше NPH невозможен и должен отвергаться.
func TestDataSizeSmallerThanNPHRejected(t *testing.T) {
	npl := make([]byte, NPLSize)
	binary.LittleEndian.PutUint16(npl[0:2], Signature)
	binary.LittleEndian.PutUint16(npl[2:4], NPHSize-1)
	npl[8] = NPLTypeNPH

	if _, err := ParseNPL(npl); !errors.Is(err, ErrDataSize) {
		t.Fatalf("ожидалась ErrDataSize, получено %v", err)
	}
}

// Кадр с другим типом NPL отвергается: парсер понимает только кадры с NPH.
func TestNonNPHFrameTypeRejected(t *testing.T) {
	for _, frameType := range []uint8{NPLTypeError, NPLTypeDebug, 0xFF} {
		frame := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, 0, nil)
		frame[8] = frameType
		if _, err := ParseFrame(frame); !errors.Is(err, ErrFrameType) {
			t.Errorf("тип кадра 0x%02X: ожидалась ErrFrameType, получено %v", frameType, err)
		}
	}
}

// Незнакомая сигнатура отвергается на уровне NPL.
func TestBadSignatureRejected(t *testing.T) {
	frame := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, 0, nil)
	binary.LittleEndian.PutUint16(frame[0:2], 0x1234)
	if _, err := ParseFrame(frame); !errors.Is(err, ErrSignature) {
		t.Fatalf("ожидалась ErrSignature, получено %v", err)
	}
}

// Тело короче dataSize не принимается: это признак оборванного потока.
func TestTruncatedBodyRejected(t *testing.T) {
	frame := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, 0, make([]byte, 32))
	truncated := frame[:len(frame)-8]
	if _, err := ParseFrame(truncated); !errors.Is(err, ErrDataSize) {
		t.Fatalf("ожидалась ErrDataSize, получено %v", err)
	}
}

// Reader обязан дочитывать кадры подряд, не съедая начало следующего, и
// отдавать io.EOF в конце потока.
func TestReaderReadsConsecutiveFrames(t *testing.T) {
	body := make([]byte, 2+SizeNav00)
	body[0] = byte(CellNav00)
	first := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, 0, body)
	second := buildTestFrame(t, ServiceNavdata, NPHTypeRealtime, 0, body)
	stream := append(append([]byte{}, first...), second...)

	reader := NewReader(bytes.NewReader(stream))
	for i := range 2 {
		if _, err := reader.Next(); err != nil {
			t.Fatalf("кадр %d: %v", i, err)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("ожидался io.EOF, получено %v", err)
	}
}

// Риск 5. Границы валидации зафиксированы явно: перечислено всё, что
// подтверждено данными вендора, и не претендуется на покрытие протокола
// целиком.
//
// Подтверждено:
//   - раскладка NPL из 15 байт и NPH из 10 байт, порядок полей little-endian;
//   - в каждом кадре ровно один NPH, dataSize = NPHSize + длина тела;
//   - CRC-16/Modbus со свапом байтов, вычисляется по NPH вместе с телом;
//   - бит запроса присутствует в NPH и отсутствует в NPL;
//   - наборы типов пакетов для сервисов Navdata и GenericControls.
//
// Не подтверждено и потому не реализовано:
//   - шифрование (объявление отклоняется, сам протокол не реализован);
//   - кадры типов NPL_TYPE_ERROR и NPL_TYPE_DEBUG;
//   - NPH-пакеты сервиса Navdata, отличные от REALTIME;
//   - любые байты, которые вендор не передаёт: фикстуры покрывают только
//     реально встреченные значения, а не пространство всех возможных.
func TestValidationBoundariesAreDocumented(t *testing.T) {
	// Ни один сервис, кроме двух перечисленных, не считается известным.
	// Проверяем это явно, иначе сервер принял бы пакет неизвестного вида
	// и попытался разобрать его как телеметрию.
	for _, service := range []uint16{2, 7, 0x7777, 0xFFFF} {
		for _, nphType := range []uint16{0, 1, 100, 101, 9999} {
			known, telemetry := ClassifyNPH(service, nphType)
			if known || telemetry {
				t.Errorf("service=0x%04X type=%d: ожидалось known=false telemetry=false",
					service, nphType)
			}
		}
	}

	// Телеметрию несёт только REALTIME сервиса Navdata.
	for _, service := range []uint16{ServiceGenericControls, ServiceNavdata} {
		known, telemetry := ClassifyNPH(service, NPHTypeRealtime)
		wantTelemetry := service == ServiceNavdata
		if known != wantTelemetry || telemetry != wantTelemetry {
			t.Errorf("service=0x%04X: known=%v telemetry=%v, ожидалось %v",
				service, known, telemetry, wantTelemetry)
		}
	}
}
