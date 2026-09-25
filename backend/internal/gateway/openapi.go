package gateway

import (
	"embed"
	"net/http"
	"strings"
)

// Спецификация и страница встроены в бинарник.
//
//go:embed openapi.yaml swagger.html
var docsFS embed.FS

// openapiSpec — GET /openapi.yaml.
//
// Спецификация отдаётся как есть, а не превращается в JSON: она должна
// открываться и в редакторе, и в CI-валидаторе, и человек должен видеть
// её глазами, а не через переводчик в объектную модель.
func (s *Server) openapiSpec(w http.ResponseWriter, r *http.Request) {
	data, err := docsFS.ReadFile("openapi.yaml")
	if err != nil {
		// Встроенный файл отсутствует только если сборка сломана, и тогда
		// честнее 500, чем пустой ответ.
		writeError(w, http.StatusInternalServerError, "internal",
			"спецификация не встроена в сборку")
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// swaggerUI — GET /swagger.
//
// Страница try-it, которая работает без интернета. CDN не используется:
// приборная панель часто стоит в контуре, где внешние адреса не
// разрешены, и страница, которая без сети не грузится, там просто не
// открывается. Поэтому встроенный минимальный просмотрщик: список
// эндпоинтов, схемы и форма ответа, а настоящие запросы человек делает
// curl-ом, и это честнее кнопки, которая отправляет его данные куда-то
// мимо гейтвея.
func (s *Server) swaggerUI(w http.ResponseWriter, _ *http.Request) {
	page, err := docsFS.ReadFile("swagger.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"страница не встроена в сборку")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// openapiYAML возвращает встроенную спецификацию. Отдельный доступ, чтобы
// тесты сверяли её с таблицей маршрутов, не поднимая HTTP.
func openapiYAML() string {
	data, err := docsFS.ReadFile("openapi.yaml")
	if err != nil {
		return ""
	}
	return string(data)
}

// yamlLine — строка спецификации без хвостовых пробелов. Нужна разбору в
// тесте, который читает структуру по отступам.
func yamlLine(raw string) string {
	return strings.TrimRight(raw, " \t\r")
}

// swaggerHTMLForTest возвращает встроенную страницу описания.
func swaggerHTMLForTest() string {
	data, err := docsFS.ReadFile("swagger.html")
	if err != nil {
		return ""
	}
	return string(data)
}
