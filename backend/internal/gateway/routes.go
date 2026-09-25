package gateway

// routes регистрирует таблицу маршрутов.
//
// Регистрация на http.ServeMux с шаблонами из Go 1.22, а не на своём
// маршрутизаторе: список эндпоинтов короткий и фиксирован, а самописный
// разбор путей — это исходник из пятидесяти строк, в котором рано или поздно
// появится путь, забытый при добавлении. Шаблоны стандартной библиотеки
// дают взамен проверку метода: POST на /healthz вернёт 405, а не 404, и
// опечатку в методе видно сразу.
//
// Порядок регистрации не важен: ServeMux выбирает самый специфичный шаблон,
// поэтому /api/v1/vehicles/{id} и /api/v1/vehicles/{id}/trajectory не спорят
// друг с другом.
func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.health)
	m.HandleFunc("GET /readyz", s.readiness)
	m.HandleFunc("GET /metrics", s.metrics)

	m.HandleFunc("GET /api/v1/vehicles", s.vehicles)
	m.HandleFunc("GET /api/v1/vehicles/{id}", s.vehicleByID)
	m.HandleFunc("GET /api/v1/vehicles/{id}/trajectory", s.trajectory)

	m.HandleFunc("GET /api/v1/routes", s.routes_)
	m.HandleFunc("GET /api/v1/routes/{id}/stops", s.routeStops)

	m.HandleFunc("GET /api/v1/incidents", s.incidentsList)
	m.HandleFunc("GET /api/v1/incidents/{id}", s.incidentByID)
	m.HandleFunc("POST /api/v1/incidents/{id}/ack", s.ackIncident)

	m.HandleFunc("GET /api/v1/predictions/{sample_id}", s.prediction)
	m.HandleFunc("POST /api/v1/predict", s.predict)
}
