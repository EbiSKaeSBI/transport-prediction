package gateway

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/horizon"
)

// Спецификация сверяется с таблицей маршрутов, а не с её копией в тесте:
// копия расходилась бы с кодом тихо, и к проверке относились бы как к
// формальности.

// specPathPattern — путь в разделе paths спецификации.
var specPathPattern = regexp.MustCompile(`^  (/\S*):\s*$`)

// specMethodPattern — метод внутри пути.
var specMethodPattern = regexp.MustCompile(`^ {4}(get|post|put|patch|delete):\s*$`)

// specFromYAML достаёт пары «МЕТОД путь» из встроенной спецификации.
//
// Разбор поверхностный и нарочно: понимать весь YAML здесь незачем, нужны
// только отступы верхних уровней, потому что структура файла фиксирована и
// любое её изменение проходит через этот тест.
func specFromYAML(t *testing.T) map[string]bool {
	t.Helper()
	text := openapiYAML()
	if strings.TrimSpace(text) == "" {
		t.Fatal("спецификация пуста или не встроена")
	}
	out := make(map[string]bool)
	inPaths := false
	var path string
	for _, raw := range strings.Split(text, "\n") {
		line := yamlLine(raw)
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		// Верхний уровень: paths, components, info.
		if raw[0] != ' ' && raw[0] != '#' {
			inPaths = strings.HasPrefix(line, "paths:")
			continue
		}
		if !inPaths {
			continue
		}
		if m := specPathPattern.FindStringSubmatch(line); m != nil {
			path = m[1]
			continue
		}
		if m := specMethodPattern.FindStringSubmatch(line); m != nil && path != "" {
			out[strings.ToUpper(m[1])+" "+path] = true
		}
	}
	return out
}

// codeRoutes возвращает пары «МЕТОД путь» из таблицы маршрутов.
func codeRoutes(t *testing.T) map[string]bool {
	t.Helper()
	s := testServer(t)
	out := make(map[string]bool)
	for _, rt := range s.routeTable() {
		fields := strings.Fields(rt.pattern)
		if len(fields) != 2 {
			t.Fatalf("шаблон %q разобран не как «МЕТОД путь»", rt.pattern)
		}
		out[fields[0]+" "+fields[1]] = true
	}
	return out
}

func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Спецификация обязана описывать ровно те маршруты, которые зарегистрированы,
// и ни одного больше. Расхождение в любую сторону — ошибка: маршрут без
// описания не найдут, а описание без маршрута будет обещать то, чего нет.
func TestSpecMatchesRoutes(t *testing.T) {
	spec, code := specFromYAML(t), codeRoutes(t)
	if onlySpec := diff(spec, code); len(onlySpec) > 0 {
		t.Errorf("в спецификации есть, а в коде нет: %v", onlySpec)
	}
	if onlyCode := diff(code, spec); len(onlyCode) > 0 {
		t.Errorf("в коде есть, а в спецификации нет: %v", onlyCode)
	}
}

// Служебные и лента помечены doc: незадокументированный маршрут обязан быть
// виден в этом тесте, а не тихо выпасть из проверки.
func TestEveryRouteIsDocumented(t *testing.T) {
	for _, rt := range testServer(t).routeTable() {
		if !rt.doc {
			t.Errorf("маршрут %q не помечен как документированный", rt.pattern)
		}
	}
}

// Спецификация должна быть валидным YAML хотя бы настолько, чтобы её принял
// синтаксический анализатор. Без зависимости с YAML-парсером проверяются те
// ошибки, которые на этом файле и случаются: табы вместо пробелов и
// двоеточие с пробелом внутри обычного значения.
func TestSpecIsStructurallySane(t *testing.T) {
	// plainScalar — значение на одной строке без кавычек и без литерала
	// блока. Двоеточие с пробелом внутри такого значения YAML читает как
	// отображение, и файл перестаёт парситься целиком.
	plain := regexp.MustCompile(`^(description|summary|example|title|detail):\s+(\S.*)$`)
	for i, raw := range strings.Split(openapiYAML(), "\n") {
		if strings.ContainsRune(raw, '\t') {
			t.Errorf("строка %d содержит таб: YAML требует пробелов", i+1)
		}
		m := plain.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			continue
		}
		val := m[2]
		if val[0] == '"' || val[0] == '\'' || val[0] == '|' || val[0] == '>' {
			continue
		}
		if strings.HasPrefix(val, "[") || strings.HasPrefix(val, "{") {
			continue
		}
		if strings.Contains(val, ": ") || strings.HasSuffix(val, ":") {
			t.Errorf("строка %d: значение %q без кавычек, но с двоеточием — "+
				"YAML прочитает его как отображение", i+1, m[1])
		}
	}
	for _, want := range []string{
		"openapi: 3.0.3", "info:", "paths:", "components:",
		"/api/v1/predict", "/api/v1/incidents/{id}/ack", "/ws/stream",
	} {
		if !strings.Contains(openapiYAML(), want) {
			t.Errorf("в спецификации нет %q", want)
		}
	}
}

// Спецификация обязана объявлять все 21 признак, иначе проверка контракта
// через POST /predict не имеет смысла: опечатка в имени прошла бы
// незамеченной, а именно её ловит сверка имён.
func TestSpecListsEveryFeatureName(t *testing.T) {
	text := openapiYAML()
	for _, name := range horizon.FeatureNames() {
		if !strings.Contains(text, name) {
			t.Errorf("в спецификации нет признака %q", name)
		}
	}
}

// Спецификация отдаётся как YAML, а не как JSON: её должны открыть и
// редактор, и валидатор, и человек.
func TestOpenAPISpecIsServed(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type %q, ожидался application/yaml", ct)
	}
	if !strings.Contains(rec.Body.String(), "openapi: 3.0.3") {
		t.Error("тело не похоже на спецификацию")
	}
}

// Страница обязана быть без внешних адресов. Приборная панель часто стоит в
// контуре, где CDN не разрешён, и страница, которая без сети не открывается,
// там просто не работает. Проверка наивная, но ровно та ошибка, ради которой
// она написана, в неё попадает.
func TestSwaggerPageIsOffline(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/swagger", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type %q, ожидался text/html", ct)
	}
	page := rec.Body.String()
	for _, bad := range []string{"http://", "https://", "//cdn.", "src=\"//"} {
		if strings.Contains(page, bad) {
			t.Errorf("страница ссылается наружу: найдено %q", bad)
		}
	}
	if !strings.Contains(page, "openapi.yaml") {
		t.Error("страница не забирает спецификацию с этого же хоста")
	}
}

// Страница обязана забирать спецификацию относительным адресом, иначе смена
// порта или схемы её потеряет.
func TestSwaggerPageFetchesSpecRelatively(t *testing.T) {
	page := swaggerHTMLForTest()
	if !strings.Contains(page, `fetch("openapi.yaml")`) {
		t.Error("спецификация забирается не относительным адресом")
	}
	if strings.Contains(page, `fetch("http`) || strings.Contains(page, `fetch("/openapi`) {
		t.Error("спецификация забирается по абсолютному адресу")
	}
}

// Никакой CDN-зависимости быть не должно и в самой спецификации: она
// раздаётся наружу как есть, и ссылка на внешний ресурс в ней сделала бы
// спецификацию неполной для читателя без сети.
func TestSpecHasNoExternalReferences(t *testing.T) {
	for i, line := range strings.Split(openapiYAML(), "\n") {
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "http://") || strings.Contains(line, "https://") {
			// localhost в servers — единственное допустимое исключение.
			if strings.Contains(line, "localhost") {
				continue
			}
			t.Errorf("строка %d ссылается наружу: %s", i+1, line)
		}
	}
}

// Число эндпоинтов зафиксировано: изменение означает, что это решение
// принималось осознанно, а не случайно добавилось вместе с новым
// обработчиком.
func TestEndpointCount(t *testing.T) {
	n := len(testServer(t).routeTable())
	if n != 16 {
		t.Errorf("маршрутов %d, ожидалось 16", n)
	}
	if got := len(specFromYAML(t)); got != n {
		t.Errorf("в спецификации %d эндпоинтов, а в коде %d", got, n)
	}
}
