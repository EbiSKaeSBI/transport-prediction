package schedule

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParsePointWKT разбирает геометрию вида "POINT (37.43070705 55.8040083)" и
// возвращает долготу и широту в порядке, принятом в GeoJSON: сначала
// longitude, затем latitude.
//
// Порядок в WKT противоположен привычному (широта, долгота), и перестановка
// пары — самая вероятная ошибка при такой загрузке: она даёт правдоподобные
// числа вроде 37.43 и 55.80, но помещает остановку в 600 км от города. Поэтому
// порядок возврата зафиксирован здесь явно, а проверка диапазона — в ParsePointWKT
// через валидацию координат.
func ParsePointWKT(value string) (lon, lat float64, err error) {
	trimmed := strings.TrimSpace(value)
	open := strings.IndexByte(trimmed, '(')
	if open < 0 {
		return 0, 0, fmt.Errorf("в геометрии %q нет скобки", value)
	}
	close := strings.LastIndexByte(trimmed, ')')
	if close < open {
		return 0, 0, fmt.Errorf("в геометрии %q скобки перепутаны местами", value)
	}
	kind := strings.ToUpper(strings.TrimSpace(trimmed[:open]))
	if kind != "POINT" {
		// Молча брать первые два числа линии или многоугольника нельзя:
		// получилась бы точка в другом месте.
		return 0, 0, fmt.Errorf("геометрия %q не POINT", value)
	}
	body := strings.TrimSpace(trimmed[open+1 : close])
	fields := strings.Fields(body)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("в POINT %q ожидались две координаты, получено %d", value, len(fields))
	}
	first, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("не разобрана координата %q в %q: %w", fields[0], value, err)
	}
	second, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("не разобрана координата %q в %q: %w", fields[1], value, err)
	}
	// В WKT сначала долгота.
	lon, lat = first, second
	if err := validateLonLat(lon, lat); err != nil {
		return 0, 0, fmt.Errorf("геометрия %q: %w", value, err)
	}
	return lon, lat, nil
}

// validateLonLat отсекает переставленные местами пары и ноль-заглушку.
func validateLonLat(lon, lat float64) error {
	if math.IsNaN(lon) || math.IsNaN(lat) {
		return fmt.Errorf("координаты не число")
	}
	if lon == 0 && lat == 0 {
		return fmt.Errorf("пара координат (0, 0) является заглушкой, а не точкой")
	}
	if lat < -90 || lat > 90 {
		return fmt.Errorf("широта %.8f вне диапазона [-90, 90]: вероятно, переставлены координаты", lat)
	}
	if lon < -180 || lon > 180 {
		return fmt.Errorf("долгота %.8f вне диапазона [-180, 180]: вероятно, переставлены координаты", lon)
	}
	return nil
}
