package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/pipeline"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/predictor"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/schedule"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/statestore"
	"github.com/ebiskauesbi/transport-prediction/backend/internal/telemetry"
)

// goldenUnit — устройство, с которым снят золотой поток.
//
// Идентификатора в кадре нет: в протоколе он принадлежит соединению, и
// привязку задаёт тот, кто соединение принял. В тесте он назван прямо, иначе
// пришлось бы вытаскивать его из имени файла фикстуры.
const goldenUnit = 1166336

// feedCard — карточка машины в том виде, в каком её отдаёт лента.
type feedCard struct {
	UnitID        uint32    `json:"unit_id"`
	TRID          int64     `json:"tr_id"`
	LocationValid bool      `json:"location_valid"`
	Latitude      float64   `json:"lat"`
	Longitude     float64   `json:"lon"`
	Prediction    *feedPred `json:"prediction"`
}

type feedPred struct {
	DeltaS        float64 `json:"delta_s"`
	PredictedDevS float64 `json:"predicted_dev_s"`
	Source        string  `json:"source"`
}

// TestGoldenStreamReachesDashboard гонит настоящий поток пакетов через все
// звенки до карточки на приборной панели.
//
// Соседний тест подставляет точку в накопитель руками. Это удобно, но
// оставляет за скобками ровно то, ради чего проверка нужна: разбор пакета,
// ячеек, координат и времени события. Ошибка в любом из этих мест не ломает
// подставленную точку и всплывает только на площадке, на живом потоке, где
// машина на карте стоит не там.
//
// Время теста живёт на оси золотого потока, а не на текущих часах: точки
// сняты вчера, и кадр, посчитанный на сегодняшний T, не нашёл бы состояния.
// Ось держит replayClock, см. replayClock.
//
// При этом карточка не приходит помеченной устаревшей, и это не oversight:
// накопитель мерит устаревание по времени приёма пакета, а не по времени
// события внутри него. При проигрывании записи приём только что был, и
// карточка честно говорит «свежая». Проверять тут нечего: на живой площадке
// приём всегда настоящий, и устаревание считается само.
func TestGoldenStreamReachesDashboard(t *testing.T) {
	var singleCalls, batchCalls, batchFrames atomic.Int64
	mlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": "v1", "features": predictor.FeatureContract(),
			})
		case "/predict":
			singleCalls.Add(1)
			// Сервис отдаёт готовую сумму (ADR-0007): фейк читаем cur_dev_s
			// из запроса и возвращаем delay_s = cur_dev_s + 42, чтобы
			// проверяемая добавка осталась 42.
			var rq struct {
				CurDev *float64 `json:"cur_dev_s"`
			}
			_ = json.NewDecoder(r.Body).Decode(&rq)
			delay := 42.0
			if rq.CurDev != nil {
				delay += *rq.CurDev
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"delay_s": delay, "p_late": 0.75, "model_version": "v1",
			})
		case "/predict/batch":
			var req struct {
				Frames []struct {
					SampleID string   `json:"sample_id"`
					CurDevS  *float64 `json:"cur_dev_s"`
				} `json:"frames"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			batchCalls.Add(1)
			batchFrames.Add(int64(len(req.Frames)))
			out := make([]map[string]any, 0, len(req.Frames))
			for _, f := range req.Frames {
				// Как и в одиночном фейке: ответ — готовая сумма,
				// проверяемая добавка 42 вычитается на стороне клиента.
				delay := 42.0
				if f.CurDevS != nil {
					delay += *f.CurDevS
				}
				out = append(out, map[string]any{
					"sample_id": f.SampleID, "delay_s": delay,
					"p_late": 0.75, "model_version": "v1",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"predictions": out})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mlSrv.Close()

	stream := loadGoldenStream(t)
	refFirst, refLast := goldenReference(t)

	// Первым проходом узнаём T: от него зависит расписание, а T лежит в
	// данных. Накоп��итель отдельный, чтобы второй проход не удваивал точки.
	at := lastEventTime(t, stream)

	addr := freeAddr(t)
	store := statestore.New()
	binding := writeBindingFor(t, goldenUnit)
	clock := &replayClock{at: at}
	observer := telemetry.New(nil,
		telemetry.WithLogger(quietServeLogger()), telemetry.WithClock(clock.Now))
	chain, ml := predictorChain(mlSrv.URL, 2*time.Second, quietServeLogger())
	svc, err := buildService(serviceOptions{
		Pipeline: pipeline.Config{
			Store: store, Schedule: writePlanCSV(t, at), Binding: binding,
			Observer: observer,
			// Секундная сетка: в тесте не пять минут ждать первую границу.
			Grid: time.Second, Logger: quietServeLogger(),
		},
		Predictor: chain,
		ML:        ml,
		Queue:     8,
		HTTPAddr:  addr,
		Logger:    quietServeLogger(),
	})
	if err != nil {
		t.Fatalf("buildService: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go svc.Scheduler.Run(ctx)
	go svc.Hub.Run(ctx)
	go func() { _ = svc.Gateway.ListenAndServe(ctx) }()
	waitHTTPReady(t, "http://"+addr+"/healthz")

	// Ленту поднимаем до тика: карточка уходит один раз на первый тик по
	// машине, и пропущенное событие уже не вернуть.
	conn := dialFeed(t, addr)
	readFeedEvent(t, conn) // снимок инцидентов
	readFeedEvent(t, conn) // снимок машин

	fed := feedGoldenStream(t, svc.Pipeline, clock, stream)
	if fed.frames == 0 {
		t.Fatal("золотой поток не содержит кадров")
	}

	svc.Pipeline.Tick(ctx, at)
	if got := svc.Pipeline.Stats().Frames; got != 1 {
		t.Fatalf("кадров построено %d, ожидался 1; статистика: %+v", got, svc.Pipeline.Stats())
	}

	// Карточка появляется раньше прогноза: она собирается из накопителя, а
	// модель ещё отвечает. Ждём именно прогноз, иначе проверка ловила бы
	// гонку, а не потерю.
	card := waitCardWithPrediction(t, "http://"+addr+"/api/v1/vehicles/"+strconv.Itoa(goldenUnit))

	// Главная проверка: координаты обязаны быть те, что лежат в потоке.
	// Подставленная точка прошла бы при любом разборе, настоящая проходит
	// только если пакет, ячейка Nav00 и шкала координат сошлись.
	var got struct {
		TRID          int64   `json:"tr_id"`
		LocationValid bool    `json:"location_valid"`
		Latitude      float64 `json:"lat"`
		Longitude     float64 `json:"lon"`
		SpeedKmh      float64 `json:"speed_kmh"`
		Stale         bool    `json:"stale"`
		Prediction    *struct {
			DeltaS        float64 `json:"delta_s"`
			PredictedDevS float64 `json:"predicted_dev_s"`
			Source        string  `json:"source"`
		} `json:"prediction"`
	}
	if err := json.Unmarshal([]byte(card), &got); err != nil {
		t.Fatalf("карточка не разобрана: %v; тело: %s", err, card)
	}
	if got.TRID != 1 {
		t.Errorf("в карточке tr_id %d, ожидался 1: привязка не доехала", got.TRID)
	}
	if !got.LocationValid {
		t.Errorf("координаты не достоверны: %s", card)
	}
	// Карточка несёт последнюю точку потока, а не первую: машина за эти
	// секунды проехала, и на карте должна быть последняя известная позиция.
	// Сравниваем с capture, а не с собственным разбором — иначе проверка
	// сравнивала бы систему с самой собой.
	if d := got.Latitude - refLast.lat; d > 1e-4 || d < -1e-4 {
		t.Errorf("широта %g, в capture %g", got.Latitude, refLast.lat)
	}
	if d := got.Longitude - refLast.lon; d > 1e-4 || d < -1e-4 {
		t.Errorf("долгота %g, в capture %g", got.Longitude, refLast.lon)
	}
	if got.SpeedKmh != refLast.speed {
		t.Errorf("скорость %g, в capture %g", got.SpeedKmh, refLast.speed)
	}
	if got.Prediction == nil {
		t.Fatalf("в карточке нет прогноза: %s", card)
	}
	// Прогноз модели обязан дойти: иначе по всему звену прошёл нулевой
	// baseline, и потери не видно. В золотом кадре cur_dev_s не измерен,
	// поэтому проверяется итоговое отклонение (delay_s), а не вычитаемая
	// из него добавка.
	if got.Prediction.PredictedDevS != 42 {
		t.Errorf("прогноз модели %g, ожидалось 42", got.Prediction.PredictedDevS)
	}
	if got.Prediction.Source != string(predictor.SourceML) {
		t.Errorf("источник прогноза %q, ожидался %q", got.Prediction.Source, predictor.SourceML)
	}

	// Начало пути сверяем с capture отдельно: карточка показывает последнюю
	// точку, и по ней одной не видно, что порядок в накопителе не перепутан.
	track := waitBody(t, "http://"+addr+"/api/v1/vehicles/"+strconv.Itoa(goldenUnit)+"/trajectory")
	var path struct {
		Points []struct {
			EventTime time.Time `json:"event_time"`
			Latitude  float64   `json:"lat"`
			Longitude float64   `json:"lon"`
		} `json:"points"`
	}
	if err := json.Unmarshal([]byte(track), &path); err != nil {
		t.Fatalf("траектория не разобрана: %v; тело: %s", err, track)
	}
	if len(path.Points) != fed.frames {
		t.Errorf("в траектории %d точек, в потоке %d кадров", len(path.Points), fed.frames)
	} else {
		head := path.Points[0]
		if d := head.Latitude - refFirst.lat; d > 1e-4 || d < -1e-4 {
			t.Errorf("первая точка пути на широте %g, в capture %g", head.Latitude, refFirst.lat)
		}
		if d := head.Longitude - refFirst.lon; d > 1e-4 || d < -1e-4 {
			t.Errorf("первая точка пути на долготе %g, в capture %g", head.Longitude, refFirst.lon)
		}
		if got := head.EventTime.Unix(); got != refFirst.unix {
			t.Errorf("время первой точки %d, в capture %d", got, refFirst.unix)
		}
	}

	// Лента отдаёт ту же карточку, что и REST, и доезжает до подписчика.
	fromFeed := vehicleCardFromFeed(t, conn, goldenUnit)
	if !fromFeed.LocationValid {
		t.Errorf("в карточке ленты нет координат: %+v", fromFeed)
	}
	if d := fromFeed.Latitude - refLast.lat; d > 1e-4 || d < -1e-4 {
		t.Errorf("в карточке ленты широта %g, ожидалась %g", fromFeed.Latitude, refLast.lat)
	}
	if fromFeed.TRID != 1 {
		t.Errorf("в карточке ленты tr_id %d, ожидался 1: привязка не доехала", fromFeed.TRID)
	}
	if fromFeed.Prediction == nil || fromFeed.Prediction.PredictedDevS != 42 {
		t.Errorf("в карточке ленты нет добавки модели: %+v", fromFeed.Prediction)
	}

	if singleCalls.Load()+batchCalls.Load() == 0 {
		t.Error("модель не получила ни одного кадра")
	}
	if got := svc.Scheduler.Stats().Predicted; got != 1 {
		t.Errorf("Predicted %d, ожидался 1", got)
	}
	if got := svc.Scheduler.Stats().Dropped; got != 0 {
		t.Errorf("Dropped %d, ожидался 0: очередь должна была принять единственный кадр", got)
	}

	// Метрики обязаны видеть и очередь, и время обращения к модели.
	metrics := waitBody(t, "http://"+addr+"/metrics")
	for _, want := range []string{
		"transport_gateway_predictions_submitted_total 1",
		"transport_gateway_predictions_predicted_total 1",
		"transport_gateway_predictions_dropped_total 0",
		"transport_gateway_inference_latency_samples_total 1",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("в метриках нет %q:\n%s", want, metrics)
		}
	}
}

// loadGoldenStream читает настоящий поток пакетов, снятый с устройства.
func loadGoldenStream(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "ndtp", "testdata", "golden", "packets.bin")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("золотой поток не прочитать: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("золотой поток пуст")
	}
	return data
}

// feedGoldenStream гонит поток через настоящий разбор: ndtp.Reader читает
// кадры, ndtp.ParseCells разбирает ячейки, пайплайн собирает наблюдение.
// Ровно эти вызовы делает ndtpserver на живом соединении, и подменять их
// рукописным заполнением точки здесь нельзя — весь смысл проверки в них.
func feedGoldenStream(t *testing.T, pipe *pipeline.Pipeline, clock *replayClock, stream []byte) goldenFeed {
	t.Helper()
	reader := ndtp.NewReader(bytes.NewReader(stream))
	fed := goldenFeed{}
	for {
		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("кадр %d не прочитан: %v", fed.frames, err)
		}
		if frame.NPL.Encrypted() {
			t.Fatalf("кадр %d объявлен зашифрованным: парсер читает открытый текст", fed.frames)
		}
		cells, err := ndtp.ParseCells(frame.Body)
		if err != nil {
			t.Fatalf("кадр %d: ячейки не разобраны: %v", fed.frames, err)
		}
		// Часы двигаем на время события кадра: observer берёт момент приёма
		// оттуда, и он обязан лежать на оси записи.
		obs := telemetry.Build(goldenUnit, cells, time.Time{})
		clock.at = obs.EventTime
		pipe.OnRealtime(goldenUnit, frame, cells)
		fed.frames++
		fed.lat, fed.lon, fed.speed = obs.Latitude, obs.Longitude, obs.Speed
	}
	return fed
}

// goldenFeed — что удалось снять с золотого потока. Последняя позиция
// возвращается наружу, чтобы карточку сравнивать с данными, а не с
// выдуманным числом: значение меняется вместе с фикстурой, и тест должен
// падать на изменении фикстуры, а не на изменении рукописного ожидания.
type goldenFeed struct {
	frames   int
	lat, lon float64
	speed    float64
}

// lastEventTime узнаёт время последнего события в потоке разбором в
// никуда: расписание строится под T, а T лежит в данных, и угадывать его по
// имени фикстуры нельзя.
func lastEventTime(t *testing.T, stream []byte) time.Time {
	t.Helper()
	reader := ndtp.NewReader(bytes.NewReader(stream))
	var last time.Time
	for {
		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("кадр не прочитан: %v", err)
		}
		cells, err := ndtp.ParseCells(frame.Body)
		if err != nil {
			t.Fatalf("ячейки не разобраны: %v", err)
		}
		obs := telemetry.Build(goldenUnit, cells, time.Now())
		if obs.EventTime.After(last) {
			last = obs.EventTime
		}
	}
	if last.IsZero() {
		t.Fatal("в потоке нет ни одного события со временем")
	}
	return last
}

// goldenPoint — точка глазами снятого устройства, взятая из разобранного
// capture, а не из нашего же разбора.
//
// Проверять разбор надо о том, чего мы не вычисляли сами. Если эталон брать
// из того же кода, который проверяем, то тест сравнивает систему с самой
// собой: переставить широту с долготой и он останется зелёным. Поэтому
// эталон читается из packets.jsonl — это то, что пришло с устройства.
type goldenPoint struct {
	unix  int64
	lat   float64 // LatitudeRaw / 1e-7
	lon   float64 // LongitudeRaw / 1e-7
	speed float64
}

// goldenReference читает эталонные точки из разобранного capture.
//
// Шкала координат 1e-7 зашита в протокол, и это единственное место, где она
// записана словами: в коде она размазана по делителям, и ошибиться в ней
// можно незаметно.
func goldenReference(t *testing.T) (first, last goldenPoint) {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "ndtp", "testdata", "golden", "packets.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("эталон не прочитать: %v", err)
	}
	const scale = 1e7
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec struct {
			Cells []struct {
				Decoded struct {
					Timestamp    int64   `json:"Timestamp"`
					LongitudeRaw float64 `json:"LongitudeRaw"`
					LatitudeRaw  float64 `json:"LatitudeRaw"`
					SpeedAvg     float64 `json:"SpeedAvg"`
				} `json:"decoded"`
			} `json:"cells"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("запись %d capture не разобрана: %v", i, err)
		}
		if len(rec.Cells) == 0 {
			t.Fatalf("запись %d capture без ячеек", i)
		}
		d := rec.Cells[0].Decoded
		p := goldenPoint{unix: d.Timestamp, lat: d.LatitudeRaw / scale,
			lon: d.LongitudeRaw / scale, speed: d.SpeedAvg}
		if i == 0 {
			first = p
		}
		last = p
	}
	return first, last
}

// replayClock отдаёт время золотого потока.
//
// При проигрывании записи момент приёма равен моменту записи: пакет «пришёл»
// тогда, когда был снят. Иначе кадр, посчитанный на T золотого потока, не
// нашёл бы состояния — накопитель режет историю по обеим меткам, и приём
// «сегодня» отсекает вчерашние точки. Это то же допущение, на котором стоит
// офлайн-разбор записи, только здесь оно сделано явно.
type replayClock struct{ at time.Time }

func (c *replayClock) Now() time.Time { return c.at }

// feedEvent — событие ленты в том виде, в каком оно приходит по WS.
type feedEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// dialFeed подключается к ленте гейтвея и ждёт, пока подписка встанет.
func dialFeed(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws/stream", nil)
	if err != nil {
		t.Fatalf("лента не подключилась: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	waitFeedClients(t, addr, 1)
	return conn
}

func readFeedEvent(t *testing.T, conn *websocket.Conn) feedEvent {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ev feedEvent
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("событие ленты не прочитано: %v", err)
	}
	return ev
}

// waitFeedClients дожидается, пока гейтвей увидит подписчика. Иначе тик может
// пройти раньше, чем подписка встанет, и карточка уйдёт в никуда.
func waitFeedClients(t *testing.T, addr string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/metrics"); err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			for _, line := range strings.Split(string(body), "\n") {
				v, ok := strings.CutPrefix(line, "transport_gateway_ws_clients ")
				if ok && strings.TrimSpace(v) == strconv.Itoa(want) {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("подписчик не появился в метриках за три секунды")
}

// vehicleCardFromFeed читает карточку нужной машины из ленты, пропуская
// инциденты и чужие карточки.
func vehicleCardFromFeed(t *testing.T, conn *websocket.Conn, unit uint32) feedCard {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev := readFeedEvent(t, conn)
		if ev.Type != "vehicle_update" {
			continue
		}
		var card feedCard
		if err := json.Unmarshal(ev.Data, &card); err != nil {
			t.Fatalf("карточка ленты не разобрана: %v; сырое: %s", err, ev.Data)
		}
		if card.UnitID == unit {
			return card
		}
	}
	t.Fatalf("карточка машины %d не доехала по ленте за три секунды", unit)
	return feedCard{}
}

// waitCardWithPrediction дожидается карточки с прогнозом: без прогноза она
// появляется сразу, и ждать тут нечего.
func waitCardWithPrediction(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = waitBody(t, url)
		if !strings.Contains(last, `"prediction": null`) {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("прогноз не доехал в карточку за десять секунд: %s", last)
	return last
}

// writeBindingFor привязывает устройство к маршруту 1.
func writeBindingFor(t *testing.T, unit uint32) *schedule.Binding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "traffic.csv")
	csv := "tr_id,unit_id\n1," + strconv.FormatUint(uint64(unit), 10) + "\n"
	if err := os.WriteFile(path, []byte(csv), 0o644); err != nil {
		t.Fatalf("привязка: %v", err)
	}
	binding, err := schedule.LoadBindingFile(path)
	if err != nil {
		t.Fatalf("привязка: %v", err)
	}
	return binding
}
