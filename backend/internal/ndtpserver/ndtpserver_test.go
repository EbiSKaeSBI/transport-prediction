package ndtpserver

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
)

type testServer struct {
	*Server
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	return &testServer{
		Server: &Server{
			Addr:        "127.0.0.1:0",
			ReadTimeout: 0,
			Logger:      nil,
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *testServer) start(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось создать listener: %v", err)
	}
	s.listener = listener
	s.Addr = listener.Addr().String()
	go func() {
		if err := s.Server.Serve(listener, s.ctx); err != nil && s.ctx.Err() == nil {
			t.Errorf("сервер завершился с ошибкой: %v", err)
		}
	}()
	return s.Addr
}

func (s *testServer) stop() {
	s.cancel()
}

type realtimeRecord struct {
	unitID uint32
	frame  ndtp.Frame
	cells  []ndtp.Cell
}

type captureHandler struct {
	mu          sync.Mutex
	handshakes  []uint32
	realtime    []realtimeRecord
	malformed   []error
	disconnects []uint32

	handshakeCh  chan uint32
	realtimeCh   chan realtimeRecord
	disconnectCh chan uint32
}

func newCaptureHandler() *captureHandler {
	return &captureHandler{
		handshakeCh:  make(chan uint32, 64),
		realtimeCh:   make(chan realtimeRecord, 64),
		disconnectCh: make(chan uint32, 64),
	}
}

func (h *captureHandler) OnHandshake(unitID uint32, req ndtp.ConnRequest) {
	h.mu.Lock()
	h.handshakes = append(h.handshakes, unitID)
	h.mu.Unlock()
	select {
	case h.handshakeCh <- unitID:
	default:
	}
}

func (h *captureHandler) OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell) {
	record := realtimeRecord{unitID: unitID, frame: frame, cells: cells}
	h.mu.Lock()
	h.realtime = append(h.realtime, record)
	h.mu.Unlock()
	select {
	case h.realtimeCh <- record:
	default:
	}
}

func (h *captureHandler) OnMalformed(unitID uint32, frame ndtp.Frame, err error) {
	h.mu.Lock()
	h.malformed = append(h.malformed, err)
	h.mu.Unlock()
}

func (h *captureHandler) OnDisconnect(unitID uint32) {
	h.mu.Lock()
	h.disconnects = append(h.disconnects, unitID)
	h.mu.Unlock()
	select {
	case h.disconnectCh <- unitID:
	default:
	}
}

func (h *captureHandler) snapshot() (int, []realtimeRecord, int, []uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	realtime := make([]realtimeRecord, len(h.realtime))
	copy(realtime, h.realtime)
	disconnects := make([]uint32, len(h.disconnects))
	copy(disconnects, h.disconnects)
	return len(h.handshakes), realtime, len(h.malformed), disconnects
}

func connectDialer(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("не удалось подключиться к %s: %v", addr, err)
	}
	return conn
}

func writeAll(t *testing.T, conn net.Conn, data []byte) {
	t.Helper()
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("ошибка записи: %v", err)
	}
}

func waitClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}

func expectHandshake(t *testing.T, h *captureHandler) uint32 {
	t.Helper()
	select {
	case unitID := <-h.handshakeCh:
		return unitID
	case <-time.After(3 * time.Second):
		t.Fatal("handshake не получен")
		return 0
	}
}

func expectRealtime(t *testing.T, h *captureHandler) realtimeRecord {
	t.Helper()
	select {
	case record := <-h.realtimeCh:
		return record
	case <-time.After(3 * time.Second):
		t.Fatal("realtime-пакет не получен")
		return realtimeRecord{}
	}
}

func expectDisconnect(t *testing.T, h *captureHandler) uint32 {
	t.Helper()
	select {
	case unitID := <-h.disconnectCh:
		return unitID
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect не вызван")
		return 0
	}
}

// waitActiveConns ждёт нужного числа активных соединений.
func waitActiveConns(t *testing.T, ts *testServer, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ts.Metrics.Snapshot().ActiveConns == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ActiveConns %d, ожидалось %d", ts.Metrics.Snapshot().ActiveConns, want)
}

func buildRealtimeFrame(t *testing.T, peerAddress uint32, nav ndtp.Nav00) []byte {
	t.Helper()
	body := make([]byte, 2+ndtp.SizeNav00)
	body[0] = byte(ndtp.CellNav00)
	body[1] = 0
	data := body[2:]

	binary.LittleEndian.PutUint32(data[0:4], nav.Timestamp)
	binary.LittleEndian.PutUint32(data[4:8], nav.LongitudeRaw)
	binary.LittleEndian.PutUint32(data[8:12], nav.LatitudeRaw)
	data[12] = nav.ExtraDop
	data[13] = nav.BatVoltage
	binary.LittleEndian.PutUint16(data[14:16], nav.SpeedAvg)
	binary.LittleEndian.PutUint16(data[16:18], nav.SpeedMax)
	binary.LittleEndian.PutUint16(data[18:20], nav.Course)
	binary.LittleEndian.PutUint16(data[20:22], nav.Track)
	binary.LittleEndian.PutUint16(data[22:24], nav.Altitude)
	data[24] = nav.Nsat
	data[25] = nav.Pdop

	return buildFrame(t, ndtp.ServiceNavdata, ndtp.NPHTypeRealtime, peerAddress, body)
}

func buildHandshakeFrame(t *testing.T, peerAddress uint32) []byte {
	t.Helper()
	body := make([]byte, ndtp.ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ndtp.ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ndtp.ProtocolVersionLow)
	binary.LittleEndian.PutUint16(body[4:6], 0)
	binary.LittleEndian.PutUint32(body[6:10], peerAddress)
	binary.LittleEndian.PutUint32(body[10:14], ndtp.DefaultMaxPacket)
	binary.LittleEndian.PutUint32(body[14:18], 0)

	return buildFrame(t, ndtp.ServiceGenericControls, ndtp.NPHTypeConnRequest, peerAddress, body)
}

func buildRawFrame(t *testing.T, serviceID, nphType uint16, peerAddress uint32, body []byte, crc uint16) []byte {
	t.Helper()
	nph := make([]byte, ndtp.NPHSize+len(body))
	binary.LittleEndian.PutUint16(nph[0:2], serviceID)
	binary.LittleEndian.PutUint16(nph[2:4], nphType)
	binary.LittleEndian.PutUint16(nph[4:6], ndtp.NPHFlagRequest)
	binary.LittleEndian.PutUint32(nph[6:10], 42)
	copy(nph[ndtp.NPHSize:], body)

	frame := make([]byte, ndtp.NPLSize+len(nph))
	binary.LittleEndian.PutUint16(frame[0:2], ndtp.Signature)
	binary.LittleEndian.PutUint16(frame[2:4], uint16(len(nph)))
	binary.LittleEndian.PutUint16(frame[4:6], 0)
	binary.LittleEndian.PutUint16(frame[6:8], crc)
	frame[8] = ndtp.NPLTypeNPH
	binary.LittleEndian.PutUint32(frame[9:13], peerAddress)
	binary.LittleEndian.PutUint16(frame[13:15], 7)
	copy(frame[ndtp.NPLSize:], nph)
	return frame
}

func buildFrame(t *testing.T, serviceID, nphType uint16, peerAddress uint32, body []byte) []byte {
	t.Helper()
	nph := make([]byte, ndtp.NPHSize+len(body))
	copy(nph, buildRawFrame(t, serviceID, nphType, peerAddress, body, 0)[ndtp.NPLSize:])
	return buildRawFrame(t, serviceID, nphType, peerAddress, body,
		swapBytes(checksum(nph)))
}

func swapBytes(v uint16) uint16 {
	return v<<8 | v>>8
}

var testModbusTable = buildTestModbusTable()

func buildTestModbusTable() [256]uint16 {
	const crc16Init uint16 = 0xFFFF
	const crc16Poly uint16 = 0xA001

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
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc = crc>>8 ^ testModbusTable[byte(crc)^b]
	}
	return crc
}

func testNav(timestamp uint32, speedAvg uint16) ndtp.Nav00 {
	return ndtp.Nav00{
		Timestamp:    timestamp,
		LongitudeRaw: 376173210,
		LatitudeRaw:  557551234,
		ExtraDop:     ndtp.ExtraDopValid | ndtp.ExtraDopLatNorth | ndtp.ExtraDopLonEast,
		BatVoltage:   12,
		SpeedAvg:     speedAvg,
		SpeedMax:     speedAvg + 15,
		Course:       230,
		Track:        12345,
		Altitude:     150,
		Nsat:         8,
		Pdop:         1,
	}
}

func TestServerAcceptsHandshake(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))

	if unitID := expectHandshake(t, handler); unitID != 1166336 {
		t.Errorf("unitID %d, ожидалось 1166336", unitID)
	}
}

func TestServerRejectsWrongService(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	body := make([]byte, ndtp.ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ndtp.ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ndtp.ProtocolVersionLow)
	binary.LittleEndian.PutUint32(body[6:10], 1166336)

	writeAll(t, conn, buildFrame(t, 999, ndtp.NPHTypeConnRequest, 1166336, body))
	waitClosed(t, conn)

	handshakes, _, malformed, _ := handler.snapshot()
	if handshakes != 0 {
		t.Errorf("handshake %d, ожидалось 0 при неверном serviceId", handshakes)
	}
	if malformed != 0 {
		t.Errorf("OnMalformed %d, ожидалось 0: кадр не дошёл до разбора ячеек", malformed)
	}
	if ts.Metrics.Snapshot().DecodeErrors == 0 {
		t.Error("DecodeErrors не вырос")
	}
}

func TestServerRejectsWrongNPHType(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	body := make([]byte, ndtp.ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ndtp.ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ndtp.ProtocolVersionLow)
	binary.LittleEndian.PutUint32(body[6:10], 1166336)

	writeAll(t, conn, buildFrame(t, ndtp.ServiceGenericControls, 999, 1166336, body))
	waitClosed(t, conn)

	handshakes, _, _, _ := handler.snapshot()
	if handshakes != 0 {
		t.Errorf("handshake %d, ожидалось 0 при неверном типе NPH", handshakes)
	}
	if ts.Metrics.Snapshot().DecodeErrors == 0 {
		t.Error("DecodeErrors не вырос")
	}
}

func TestServerRejectsBadCRC(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	body := make([]byte, ndtp.ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ndtp.ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ndtp.ProtocolVersionLow)
	binary.LittleEndian.PutUint32(body[6:10], 1166336)

	writeAll(t, conn, buildRawFrame(t, ndtp.ServiceGenericControls,
		ndtp.NPHTypeConnRequest, 1166336, body, 0x0000))
	waitClosed(t, conn)

	handshakes, _, _, _ := handler.snapshot()
	if handshakes != 0 {
		t.Errorf("handshake %d, ожидалось 0 при битом CRC", handshakes)
	}
	if ts.Metrics.Snapshot().CRCErrors != 1 {
		t.Errorf("CRCErrors %d, ожидалась 1", ts.Metrics.Snapshot().CRCErrors)
	}
}

func TestServerHandlesRealtimePackets(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))
	expectHandshake(t, handler)

	writeAll(t, conn, buildRealtimeFrame(t, 1166336, testNav(1700000000, 4500)))
	first := expectRealtime(t, handler)
	writeAll(t, conn, buildRealtimeFrame(t, 1166336, testNav(1700000010, 5000)))
	second := expectRealtime(t, handler)

	if first.unitID != 1166336 || second.unitID != 1166336 {
		t.Errorf("unitID %d и %d, ожидалось 1166336", first.unitID, second.unitID)
	}
	if len(first.cells) != 1 {
		t.Fatalf("ячеек %d, ожидалась 1", len(first.cells))
	}
	nav, ok := first.cells[0].Nav00()
	if !ok {
		t.Fatal("первая ячейка не Nav00")
	}
	if nav.SpeedAvg != 4500 {
		t.Errorf("SpeedAvg %d, ожидалось 4500", nav.SpeedAvg)
	}
	if !nav.LocationValid() {
		t.Error("location_valid должен быть true")
	}
	if nav.Timestamp != 1700000000 {
		t.Errorf("Timestamp %d, ожидалось 1700000000", nav.Timestamp)
	}
	secondNav, _ := second.cells[0].Nav00()
	if secondNav.SpeedAvg != 5000 {
		t.Errorf("SpeedAvg второго пакета %d, ожидалось 5000", secondNav.SpeedAvg)
	}
	if secondNav.Timestamp != 1700000010 {
		t.Errorf("Timestamp второго пакета %d, ожидалось 1700000010", secondNav.Timestamp)
	}
}

func TestServerMetrics(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))
	expectHandshake(t, handler)
	writeAll(t, conn, buildRealtimeFrame(t, 1166336, testNav(1700000000, 4500)))
	expectRealtime(t, handler)

	snap := ts.Metrics.Snapshot()
	if snap.Connections != 1 {
		t.Errorf("Connections %d, ожидалось 1", snap.Connections)
	}
	if snap.ActiveConns != 1 {
		t.Errorf("ActiveConns %d, ожидалось 1", snap.ActiveConns)
	}
	if snap.Handshakes != 1 {
		t.Errorf("Handshakes %d, ожидалось 1", snap.Handshakes)
	}
	if snap.RealtimePackets != 1 {
		t.Errorf("RealtimePackets %d, ожидалось 1", snap.RealtimePackets)
	}
	if snap.CellsDecoded != 1 {
		t.Errorf("CellsDecoded %d, ожидалось 1", snap.CellsDecoded)
	}
	if snap.BytesRead == 0 {
		t.Error("BytesRead не вырос")
	}
	if snap.CRCErrors != 0 || snap.DecodeErrors != 0 {
		t.Errorf("ошибок быть не должно: CRC=%d Decode=%d", snap.CRCErrors, snap.DecodeErrors)
	}
}

func TestServerDisconnect(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))
	expectHandshake(t, handler)

	conn.Close()
	expectDisconnect(t, handler)

	// Счётчик ждём, а не читаем сразу: отложенные функции handle
	// выполняются в обратном порядке, поэтому OnDisconnect, на котором
	// синхронизируется expectDisconnect, срабатывает раньше уменьшения
	// ActiveConns. Проверка без ожидания была гонкой и падала примерно в
	// одном прогоне из пяти.
	waitActiveConns(t, ts, 0)
	if snap := ts.Metrics.Snapshot(); snap.CRCErrors != 0 || snap.DecodeErrors != 0 {
		t.Errorf("штатное отключение не должно считаться ошибкой: CRC=%d Decode=%d",
			snap.CRCErrors, snap.DecodeErrors)
	}
}

func TestServerRejectsEmptyHandshake(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	npl := make([]byte, ndtp.NPLSize)
	binary.LittleEndian.PutUint16(npl[0:2], ndtp.Signature)
	binary.LittleEndian.PutUint16(npl[2:4], ndtp.NPHSize)
	npl[8] = ndtp.NPLTypeNPH
	binary.LittleEndian.PutUint32(npl[9:13], 1166336)

	writeAll(t, conn, npl)
	conn.Close()
	expectDisconnect(t, handler)

	handshakes, realtime, _, _ := handler.snapshot()
	if handshakes != 0 {
		t.Errorf("handshake %d, ожидалось 0 при пустом теле", handshakes)
	}
	if len(realtime) != 0 {
		t.Errorf("realtime %d, ожидалось 0", len(realtime))
	}
}

func TestServerRejectsUnknownNPHType(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))
	expectHandshake(t, handler)

	writeAll(t, conn, buildFrame(t, ndtp.ServiceNavdata, 9999, 1166336, make([]byte, ndtp.SizeNav00)))
	waitClosed(t, conn)

	_, realtime, malformed, _ := handler.snapshot()
	if len(realtime) != 0 {
		t.Errorf("realtime %d, ожидалось 0 при неизвестном типе NPH", len(realtime))
	}
	if malformed != 0 {
		t.Errorf("OnMalformed %d, ожидалось 0: проверка типа NPH происходит раньше", malformed)
	}
	snap := ts.Metrics.Snapshot()
	if snap.Handshakes != 1 {
		t.Errorf("Handshakes %d, ожидался 1", snap.Handshakes)
	}
	if snap.RealtimePackets != 0 {
		t.Errorf("RealtimePackets %d, ожидалось 0", snap.RealtimePackets)
	}
	if snap.DecodeErrors == 0 {
		t.Error("DecodeErrors не вырос")
	}
}

func TestServerReportsMalformedCells(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))
	expectHandshake(t, handler)

	broken := make([]byte, 2+ndtp.SizeUsi08)
	broken[0] = 1
	writeAll(t, conn, buildFrame(t, ndtp.ServiceNavdata, ndtp.NPHTypeRealtime, 1166336, broken))
	waitClosed(t, conn)

	_, _, malformed, _ := handler.snapshot()
	if malformed != 1 {
		t.Errorf("OnMalformed %d, ожидалась 1 для неизвестного типа ячейки", malformed)
	}
	if snap := ts.Metrics.Snapshot(); snap.RealtimePackets != 0 {
		t.Errorf("RealtimePackets %d, ожидалось 0", snap.RealtimePackets)
	}
}

func TestServerSurvivesNilHandler(t *testing.T) {
	ts := newTestServer(t)
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))

	broken := make([]byte, 2+ndtp.SizeUsi08)
	broken[0] = 1
	writeAll(t, conn, buildFrame(t, ndtp.ServiceNavdata, ndtp.NPHTypeRealtime, 1166336, broken))
	waitClosed(t, conn)

	snap := ts.Metrics.Snapshot()
	if snap.Handshakes != 1 {
		t.Errorf("Handshakes %d, ожидался 1 при nil-обработчике", snap.Handshakes)
	}
	if snap.Connections != 1 {
		t.Errorf("Connections %d, ожидалось 1", snap.Connections)
	}
	if snap.RealtimePackets != 0 {
		t.Errorf("RealtimePackets %d, ожидалось 0", snap.RealtimePackets)
	}
}

// buildHandshakeFrameWithFlags собирает handshake с заданными флагами тела.
func buildHandshakeFrameWithFlags(t *testing.T, peerAddress uint32, connFlags uint16) []byte {
	t.Helper()
	body := make([]byte, ndtp.ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ndtp.ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ndtp.ProtocolVersionLow)
	binary.LittleEndian.PutUint16(body[4:6], connFlags)
	binary.LittleEndian.PutUint32(body[6:10], peerAddress)
	binary.LittleEndian.PutUint32(body[10:14], ndtp.DefaultMaxPacket)
	return buildFrame(t, ndtp.ServiceGenericControls, ndtp.NPHTypeConnRequest, peerAddress, body)
}

// Риск 2. Handshake с объявленным шифрованием обязан быть отклонён до
// регистрации соединения: иначе сервер принял бы шифротекст как данные.
func TestServerRejectsEncryptedHandshake(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrameWithFlags(t, 1166336, ndtp.ConnFlagEncryption))
	waitClosed(t, conn)

	handshakes, realtime, _, _ := handler.snapshot()
	if handshakes != 0 {
		t.Errorf("handshake %d, ожидалось 0 при шифровании", handshakes)
	}
	if len(realtime) != 0 {
		t.Errorf("realtime %d, ожидалось 0", len(realtime))
	}
	snap := ts.Metrics.Snapshot()
	if snap.Handshakes != 0 {
		t.Errorf("Handshakes %d, ожидалось 0", snap.Handshakes)
	}
	if snap.EncryptedRejects != 1 {
		t.Errorf("EncryptedRejects %d, ожидалась 1", snap.EncryptedRejects)
	}
	if snap.DecodeErrors == 0 {
		t.Error("DecodeErrors не вырос")
	}
}

// Риск 2. Кадр с флагом шифрования в NPL отклоняется на разборе кадра,
// до проверки типа пакета.
func TestServerRejectsEncryptedNPL(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	body := make([]byte, ndtp.ConnRequestBodySize)
	binary.LittleEndian.PutUint16(body[0:2], ndtp.ProtocolVersionHigh)
	binary.LittleEndian.PutUint16(body[2:4], ndtp.ProtocolVersionLow)
	binary.LittleEndian.PutUint32(body[6:10], 1166336)

	frame := buildFrame(t, ndtp.ServiceGenericControls, ndtp.NPHTypeConnRequest, 1166336, body)
	binary.LittleEndian.PutUint16(frame[4:6], ndtp.NPLFlagEncryption)
	writeAll(t, conn, frame)
	waitClosed(t, conn)

	if handshakes, _, _, _ := handler.snapshot(); handshakes != 0 {
		t.Errorf("handshake %d, ожидалось 0 при флаге шифрования в NPL", handshakes)
	}
	if snap := ts.Metrics.Snapshot(); snap.Handshakes != 0 {
		t.Errorf("Handshakes %d, ожидалось 0", snap.Handshakes)
	}
}

// Риск 2. Справочные флаги CRC и симуляции handshake не должны приводить
// к отказу: они не меняют способ разбора.
func TestServerAcceptsNonFatalHandshakeFlags(t *testing.T) {
	for _, flags := range []uint16{
		0,
		ndtp.ConnFlagCRC,
		ndtp.ConnFlagSimulate,
		ndtp.ConnFlagCRC | ndtp.ConnFlagSimulate,
	} {
		t.Run("", func(t *testing.T) {
			ts := newTestServer(t)
			handler := newCaptureHandler()
			ts.Handler = handler
			addr := ts.start(t)
			defer ts.stop()

			conn := connectDialer(t, addr)
			defer conn.Close()

			writeAll(t, conn, buildHandshakeFrameWithFlags(t, 1166336, flags))
			if unitID := expectHandshake(t, handler); unitID != 1166336 {
				t.Errorf("flags 0x%04X: unitID %d", flags, unitID)
			}
			if snap := ts.Metrics.Snapshot(); snap.EncryptedRejects != 0 {
				t.Errorf("flags 0x%04X: EncryptedRejects %d, ожидалось 0",
					flags, snap.EncryptedRejects)
			}
		})
	}
}

// Риск 3. RESULT и HISTORY известны протоколу, но телеметрии не несут.
// Соединение обязано пережить их и продолжить принимать REALTIME.
func TestServerSkipsKnownNonTelemetryPackets(t *testing.T) {
	for _, nphType := range []uint16{ndtp.NPHTypeResult, ndtp.NPHTypeHistory} {
		t.Run("", func(t *testing.T) {
			ts := newTestServer(t)
			handler := newCaptureHandler()
			ts.Handler = handler
			addr := ts.start(t)
			defer ts.stop()

			conn := connectDialer(t, addr)
			defer conn.Close()

			writeAll(t, conn, buildHandshakeFrame(t, 1166336))
			expectHandshake(t, handler)

			// Тело этих пакетов не является потоком ячеек и не разбирается.
			writeAll(t, conn, buildFrame(t, ndtp.ServiceNavdata, nphType, 1166336,
				make([]byte, 4)))

			// Сразу после них идёт обычная телеметрия.
			nav := testNav(1000, 30)
			writeAll(t, conn, buildRealtimeFrame(t, 1166336, nav))
			expectRealtime(t, handler)

			snap := ts.Metrics.Snapshot()
			if snap.SkippedPackets != 1 {
				t.Errorf("type %d: SkippedPackets %d, ожидалась 1", nphType, snap.SkippedPackets)
			}
			if snap.RealtimePackets != 1 {
				t.Errorf("type %d: RealtimePackets %d, ожидался 1", nphType, snap.RealtimePackets)
			}
			if snap.UnsupportedTypeRejects != 0 {
				t.Errorf("type %d: UnsupportedTypeRejects %d, ожидалось 0",
					nphType, snap.UnsupportedTypeRejects)
			}
			if snap.DecodeErrors != 0 {
				t.Errorf("type %d: DecodeErrors %d, ожидалось 0", nphType, snap.DecodeErrors)
			}
		})
	}
}

// Неизвестный тип пакета по-прежнему рвёт соединение: угадывать структуру
// нельзя.
func TestServerRejectsUnknownTypeInNavdata(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))
	expectHandshake(t, handler)

	writeAll(t, conn, buildFrame(t, ndtp.ServiceNavdata, 4242, 1166336,
		make([]byte, ndtp.SizeNav00)))
	waitClosed(t, conn)

	snap := ts.Metrics.Snapshot()
	if snap.UnsupportedTypeRejects != 1 {
		t.Errorf("UnsupportedTypeRejects %d, ожидалась 1", snap.UnsupportedTypeRejects)
	}
	if snap.RealtimePackets != 0 {
		t.Errorf("RealtimePackets %d, ожидалось 0", snap.RealtimePackets)
	}
}

// Пакет сервиса, отличного от Navdata, после handshake также недопустим.
func TestServerRejectsForeignServiceAfterHandshake(t *testing.T) {
	ts := newTestServer(t)
	handler := newCaptureHandler()
	ts.Handler = handler
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()

	writeAll(t, conn, buildHandshakeFrame(t, 1166336))
	expectHandshake(t, handler)

	writeAll(t, conn, buildFrame(t, 7, ndtp.NPHTypeRealtime, 1166336,
		make([]byte, ndtp.SizeNav00)))
	waitClosed(t, conn)

	if snap := ts.Metrics.Snapshot(); snap.UnsupportedTypeRejects != 1 {
		t.Errorf("UnsupportedTypeRejects %d, ожидалась 1", snap.UnsupportedTypeRejects)
	}
}

// Молчащий клиент не должен удерживать горутину и дескриптор бесконечно.
// До появления HandshakeTimeout чтение первого кадра не имело срока, и
// подключившийся молча клиент держал соединение до конца процесса.
func TestServerClosesSilentClient(t *testing.T) {
	ts := newTestServer(t)
	ts.HandshakeTimeout = 100 * time.Millisecond
	addr := ts.start(t)
	defer ts.stop()

	conn := connectDialer(t, addr)
	defer conn.Close()
	// Ничего не пишем: сервер обязан закрыть соединение сам.

	closed := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		closed <- err
	}()

	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("соединение должно быть закрыто сервером, а не оставаться открытым")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("сервер не закрыл молчащее соединение по HandshakeTimeout")
	}

	if snap := ts.Metrics.Snapshot(); snap.HandshakeTimeouts != 1 {
		t.Errorf("HandshakeTimeouts %d, ожидалась 1", snap.HandshakeTimeouts)
	}
	// Таймаут handshake не должен выглядеть как ошибка разбора кадра.
	if snap := ts.Metrics.Snapshot(); snap.DecodeErrors != 0 || snap.CRCErrors != 0 {
		t.Errorf("молчание не должно считаться ошибкой разбора: decode=%d crc=%d",
			snap.DecodeErrors, snap.CRCErrors)
	}
}

// Остановка по отмене контекста обязана быть предсказуемой: контейнер
// получает SIGTERM, а Serve не должен ждать соединения, которое больше не
// пришлёт ни одного байта. Критерий 5 оценивает холодный старт и остановку.
func TestServeStopsWithSilentConnection(t *testing.T) {
	ts := newTestServer(t)
	// ReadTimeout и HandshakeTimeout намеренно не заданы: обрывать чтение
	// должна отмена контекста, а не таймаут.
	ts.ReadTimeout = 0
	ts.HandshakeTimeout = 0
	addr := ts.start(t)

	conn := connectDialer(t, addr)
	defer conn.Close()

	done := make(chan error, 1)
	go func() { done <- ts.Server.Serve(ts.listener, ts.ctx) }()

	time.Sleep(50 * time.Millisecond)
	ts.cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve вернул %v, ожидался nil при отмене контекста", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve не завершился: соединение не было закрыто по отмене контекста")
	}
}
