package gateway

import "net/http"

// route — строка таблицы маршрутов.
//
// Таблица объявлена данными, а не регистрацией в коде, по одной причине:
// тест сверяет спецификацию именно с ней. Если бы маршруты только
// регистрировались, тесту пришлось бы держать второй список, и к проверке
// относились бы как к формальности — он расходился бы с кодом тихо. Здесь
// расхождение ломает сборку.
type route struct {
	pattern string
	handler http.HandlerFunc
	// doc — видны ли маршрут в OpenAPI. Служебные и лента описаны в
	// спецификации, но не имеют обычного JSON-ответа, поэтому помечены
	// явно, а не молча выпали бы из проверки.
	doc bool
}

// routeTable — все маршруты гейтвея.
//
// Регистрация на http.ServeMux с шаблонами из Go 1.22, а не на своём
// маршрутизаторе: список эндпоинтов короткий и фиксирован, а самописный
// разбор путей — это исходник из пятидесяти строк, в котором рано или поздно
// появится путь, забытый при добавлении. Шаблоны стандартной библиотеки
// дают взамен проверку метода: POST на /healthz вернёт 405, а не 404, и
// опечатку в методе видно сразу.
//
// Порядок не важен: ServeMux выбирает самый специфичный шаблон, поэтому
// /api/v1/vehicles/{id} и /api/v1/vehicles/{id}/trajectory не спорят друг с
// другом.
func (s *Server) routeTable() []route {
	return []route{
		{"GET /healthz", s.health, true},
		{"GET /readyz", s.readiness, true},
		{"GET /metrics", s.metrics, true},
		{"GET /openapi.yaml", s.openapiSpec, true},
		{"GET /swagger", s.swaggerUI, true},

		{"GET /ws/stream", s.stream, true},

		{"GET /api/v1/vehicles", s.vehicles, true},
		{"GET /api/v1/vehicles/{id}", s.vehicleByID, true},
		{"GET /api/v1/vehicles/{id}/trajectory", s.trajectory, true},

		{"GET /api/v1/routes", s.routes_, true},
		{"GET /api/v1/routes/{id}/stops", s.routeStops, true},

		{"GET /api/v1/incidents", s.incidentsList, true},
		{"GET /api/v1/incidents/{id}", s.incidentByID, true},
		{"POST /api/v1/incidents/{id}/ack", s.ackIncident, true},

		{"GET /api/v1/predictions/{sample_id}", s.prediction, true},
		{"POST /api/v1/predict", s.predict, true},

		{"GET /api/v1/metrics/latency", s.metricsLatency, true},
		{"GET /api/v1/model", s.modelMeta, true},
	}
}

func (s *Server) routes() {
	for _, rt := range s.routeTable() {
		s.mux.HandleFunc(rt.pattern, rt.handler)
	}
}
