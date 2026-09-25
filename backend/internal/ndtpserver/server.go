// Package ndtpserver принимает NDTP-подключения от транспортных средств и
// разбирает поток телеметрии.
//
// Поведение строгое: первым кадром обязан быть CONN_REQUEST сервиса
// GenericControls, далее идут REALTIME-пакеты сервиса Navdata. Любая
// структурная ошибка, включая неизвестный тип ячейки, несовпадение CRC,
// неожиданный serviceId и объявленное шифрование, рвёт соединение. Нарушение
// фиксируется в метриках, а не исправляется молча.
//
// Известные, но не несущие телеметрии пакеты — RESULT в обоих сервисах и
// HISTORY в Navdata — соединение не рвут: они просто считаются и
// пропускаются. Шифрование не реализовано, поэтому кадр с таким флагом
// отбрасывается явно, а не разбирается как открытый текст.
package ndtpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
)

// Metrics — потокобезопасные счётчики сервера. Все поля обновляются через
// sync/atomic, читать их следует через Snapshot.
type Metrics struct {
	// Connections — всего принято соединений.
	Connections atomic.Int64
	// ActiveConns — соединений в работе прямо сейчас.
	ActiveConns atomic.Int64
	// Handshakes — успешно разобранных CONN_REQUEST.
	Handshakes atomic.Int64
	// RealtimePackets — успешно разобранных REALTIME-пакетов.
	RealtimePackets atomic.Int64
	// CRCErrors — отказов из-за несовпадения контрольной суммы.
	CRCErrors atomic.Int64
	// DecodeErrors — отказов по прочим причинам разбора.
	DecodeErrors atomic.Int64
	// CellsDecoded — всего разобрано ячеек.
	CellsDecoded atomic.Int64
	// BytesRead — всего прочитано байт кадров.
	BytesRead atomic.Int64
	// SkippedPackets — кадров известных, но не несущих телеметрии типов
	// (RESULT, HISTORY), которые соединение пережило без разбора ячеек.
	SkippedPackets atomic.Int64
	// EncryptedRejects — отказов из-за объявленного шифрования.
	EncryptedRejects atomic.Int64
	// UnsupportedTypeRejects — отказов из-за типа пакета, не входящего в
	// известный протоколу набор.
	UnsupportedTypeRejects atomic.Int64
}

// Snapshot — согласованный набор значений метрик, пригодный для экспорта.
type Snapshot struct {
	Connections            int64 `json:"connections"`
	ActiveConns            int64 `json:"active_connections"`
	Handshakes             int64 `json:"handshakes"`
	RealtimePackets        int64 `json:"realtime_packets"`
	CRCErrors              int64 `json:"crc_errors"`
	DecodeErrors           int64 `json:"decode_errors"`
	CellsDecoded           int64 `json:"cells_decoded"`
	BytesRead              int64 `json:"bytes_read"`
	SkippedPackets         int64 `json:"skipped_packets"`
	EncryptedRejects       int64 `json:"encrypted_rejects"`
	UnsupportedTypeRejects int64 `json:"unsupported_type_rejects"`
}

// Snapshot читает все счётчики. Значения между полями не атомарны между
// собой, что для наблюдательной метрики несущественно.
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		Connections:            m.Connections.Load(),
		ActiveConns:            m.ActiveConns.Load(),
		Handshakes:             m.Handshakes.Load(),
		RealtimePackets:        m.RealtimePackets.Load(),
		CRCErrors:              m.CRCErrors.Load(),
		DecodeErrors:           m.DecodeErrors.Load(),
		CellsDecoded:           m.CellsDecoded.Load(),
		BytesRead:              m.BytesRead.Load(),
		SkippedPackets:         m.SkippedPackets.Load(),
		EncryptedRejects:       m.EncryptedRejects.Load(),
		UnsupportedTypeRejects: m.UnsupportedTypeRejects.Load(),
	}
}

// Handler получает события жизненного цикла соединения. Реализация должна
// быть безопасна для конкурентного вызова из разных соединений.
type Handler interface {
	// OnHandshake вызывается после успешного разбора CONN_REQUEST. unitID
	// берётся из поля PeerAddress тела handshake.
	OnHandshake(unitID uint32, req ndtp.ConnRequest)
	// OnRealtime вызывается для каждого успешно разобранного пакета. unitID
	// равен адресу, объявленному в handshake.
	OnRealtime(unitID uint32, frame ndtp.Frame, cells []ndtp.Cell)
	// OnMalformed вызывается, когда кадр разобран, но его ячейки нет.
	OnMalformed(unitID uint32, frame ndtp.Frame, err error)
	// OnDisconnect вызывается при закрытии соединения. unitID равен нулю,
	// если handshake не был установлен.
	OnDisconnect(unitID uint32)
}

// Server — TCP-сервер, принимающий NDTP-подключения.
type Server struct {
	// Addr — адрес прослушивания, например ":9201". Используется только
	// ListenAndServe.
	Addr string
	// Handler — получатель событий. Допускается nil: сервер работает,
	// только считая метрики.
	Handler Handler
	// Metrics — счётчики сервера.
	Metrics Metrics
	// Logger — журнал. При nil используется slog.Default.
	Logger *slog.Logger
	// ReadTimeout — таймаута чтения пакета. При значении 0 или меньше
	// таймаут не применяется, что нужно для потоков с редкими пакетами.
	ReadTimeout time.Duration
}

// ListenAndServe создаёт слушатель на Addr и обслуживает соединения до
// отмены ctx.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("ndtpserver: не удалось слушать %s: %w", s.Addr, err)
	}
	return s.Serve(listener, ctx)
}

// Serve обслуживает уже созданный listener до отмены ctx, после чего ждёт
// завершения активных соединений. Слушатель закрывается при отмене ctx;
// ошибки чтения и записи в нём считаются штатным завершением.
//
// Возвращает nil при штатной остановке и ошибку, если Accept завершился
// не по отмене контекста.
func (s *Server) Serve(listener net.Listener, ctx context.Context) error {
	s.logger().Info("NDTP-сервер слушает", "addr", listener.Addr().String())

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("ndtpserver: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	s.Metrics.Connections.Add(1)
	s.Metrics.ActiveConns.Add(1)
	defer s.Metrics.ActiveConns.Add(-1)

	var unitID uint32
	defer func() {
		if s.Handler != nil {
			s.Handler.OnDisconnect(unitID)
		}
	}()

	reader := ndtp.NewReader(conn)

	frame, err := reader.Next()
	if err != nil {
		s.countParseError(err)
		return
	}
	if err := frame.ValidateService(ndtp.ServiceGenericControls); err != nil {
		s.Metrics.DecodeErrors.Add(1)
		s.logger().Warn("ожидался handshake", "err", err, "peer", conn.RemoteAddr())
		return
	}
	if err := frame.ValidateType(ndtp.NPHTypeConnRequest); err != nil {
		s.Metrics.DecodeErrors.Add(1)
		s.logger().Warn("ожидался CONN_REQUEST", "err", err, "peer", conn.RemoteAddr())
		return
	}
	request, err := ndtp.ParseConnRequest(frame.Body)
	if err != nil {
		s.Metrics.DecodeErrors.Add(1)
		s.logger().Warn("не разобран handshake", "err", err, "peer", conn.RemoteAddr())
		return
	}
	if err := request.Validate(); err != nil {
		s.Metrics.EncryptedRejects.Add(1)
		s.Metrics.DecodeErrors.Add(1)
		s.logger().Warn("соединение отклонено", "err", err, "peer", conn.RemoteAddr())
		return
	}
	unitID = request.PeerAddress
	s.Metrics.Handshakes.Add(1)
	s.logger().Info("handshake",
		"unit", unitID,
		"version", request.Version(),
		"max_packet", request.MaxPacketSize,
		"crc_flag", request.CRCEnabled(),
		"encrypted", request.Encryption(),
	)
	if s.Handler != nil {
		s.Handler.OnHandshake(unitID, request)
	}

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if s.ReadTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
		}
		frame, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
				s.countParseError(err)
			}
			return
		}
		known, telemetry := ndtp.ClassifyNPH(frame.NPH.ServiceID, frame.NPH.Type)
		if !known {
			// Пакет из сервиса, отличного от Navdata, либо тип, которого нет
			// в протоколе. В обоих случаях содержимое неизвестно, а строгая
			// политика требует разрыва, а не догадок о структуре.
			s.Metrics.UnsupportedTypeRejects.Add(1)
			s.Metrics.DecodeErrors.Add(1)
			s.logger().Warn("неизвестный пакет NDTP",
				"service", frame.NPH.ServiceID,
				"type", frame.NPH.Type,
				"unit", unitID)
			return
		}
		if !telemetry {
			// Известные, но не несущие телеметрии пакеты: RESULT в сервисе
			// Navdata и HISTORY с архивными данными. Соединение переживает
			// их штатно, иначе устройство с включённой выгрузкой архива
			// теряло бы соединение на каждом таком пакете.
			s.Metrics.SkippedPackets.Add(1)
			s.Metrics.BytesRead.Add(int64(len(frame.Raw)))
			s.logger().Debug("пропущен пакет без телеметрии",
				"service", frame.NPH.ServiceID,
				"type", frame.NPH.Type,
				"unit", unitID)
			continue
		}
		cells, err := ndtp.ParseCells(frame.Body)
		if err != nil {
			s.Metrics.DecodeErrors.Add(1)
			s.logger().Warn("ячейки не разобраны", "err", err, "unit", unitID)
			if s.Handler != nil {
				s.Handler.OnMalformed(unitID, frame, err)
			}
			return
		}
		s.Metrics.RealtimePackets.Add(1)
		s.Metrics.CellsDecoded.Add(int64(len(cells)))
		s.Metrics.BytesRead.Add(int64(len(frame.Raw)))
		if s.Handler != nil {
			s.Handler.OnRealtime(unitID, frame, cells)
		}
	}
}

func (s *Server) countParseError(err error) {
	if errors.Is(err, ndtp.ErrCRC) {
		s.Metrics.CRCErrors.Add(1)
	} else {
		s.Metrics.DecodeErrors.Add(1)
	}
	if !errors.Is(err, net.ErrClosed) {
		s.logger().Warn("пакет отброшен, соединение закрыто", "err", err)
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
