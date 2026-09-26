"""Многошаговое геометрическое построение плана из траекторий эмулятора.

Модуль вызывается из scripts/ndtp_feed.py (режим --plan-from-capture) и
живёт отдельно, потому что у эмулятора и у golden-фида разная природа
источника:

- фид переигрывает ОДИН замкнутый след, и всё смещение точки известно
  заранее (накат CYCLE_LON_E7 на круг);
- эмулятор ведёт КАЖДУЮ машину по своей дороге, и будущего не знает никто.

Поэтому маршрут строится по факту: пройденный путь — как есть, а «вперёд»
продлевается по курсу, скорости и кривизне последней минуты записи. Это
честное продолжение: известно направление, неизвестна будущая траектория
дороги. Угадывание ограничено — см. ARC_CAP_M, иначе из 150-метровой записи
вырастает дуга в несколько километров, и план уезжает от машины быстрее,
чем машина уезжает от него.
"""

from __future__ import annotations

import math
import statistics
from typing import Iterable, Sequence

EARTH_R_M = 6371008.8
M_PER_DEG_LAT = math.pi / 180.0 * EARTH_R_M
Track = "list[tuple[int, float, float]]"  # (unix-время, lon, lat)

ARC_WINDOW_S = 60.0
"""Окно записи, по которому оценивается кривизна дороги на конце трека."""

ARC_CAP_M = 800.0
"""Сколько метров будущего пути ещё доверяем измеренной кривизне, а дальше
ведём по прямой. Кривизна известна только там, где машина проехала; на
kilометры вперёд её форма не значит ничего, а ошибка накапливается линейно."""

MAX_TURN_DEG_S = 4.0
"""Потолок скорости поворота, градусов в секунду. Машина, идущая 80 км/ч,
вписывается в поворот на 90° за 4–6 с; всё быстрее — не дорога, а шум
координат в вибрации приёмника."""

MIN_TURN_WINDOW_S = 20.0
"""Минимальная длительность окна, на котором вообще считается поворот: на
двух соседних точках направление — это шум, а не геометрия дороги."""

MIN_TURN_RUN_M = 20.0
"""Минимальный пройденный путь в окне. На почти неподвижной машине знаменатель
у поворота стремится к нулю, и rate уходит в бесконечность."""

TURN_BASE_S = 8.0
"""Длина базы, по которой берётся касательная у края окна. Длиннее — уже не
касательная, а хорда дуги; короче — не усредняет шум координат."""

TURN_DEADBAND_DEG = 10.0
"""Накопленный поворот в окне, ниже которого дорога считается прямой."""

ARC_STEP_M = 20.0
"""Шаг интегрирования дуги, метров."""


def m_per_deg_lon(lat: float) -> float:
    return M_PER_DEG_LAT * math.cos(math.radians(lat))


def haversine_m(lon1: float, lat1: float, lon2: float, lat2: float) -> float:
    """Расстояние между точками на сфере, м."""
    p1, p2 = math.radians(lat1), math.radians(lat2)
    dp = p2 - p1
    dl = math.radians(lon2 - lon1)
    a = math.sin(dp / 2) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dl / 2) ** 2
    return 2 * EARTH_R_M * math.asin(math.sqrt(a))


def bearing_deg(lon1: float, lat1: float, lon2: float, lat2: float) -> float:
    """Азимут от первой точки ко второй, градусы от севера по часовой."""
    p1, p2 = math.radians(lat1), math.radians(lat2)
    dl = math.radians(lon2 - lon1)
    y = math.sin(dl) * math.cos(p2)
    x = math.cos(p1) * math.sin(p2) - math.sin(p1) * math.cos(p2) * math.cos(dl)
    return math.degrees(math.atan2(y, x)) % 360.0


def track_speed(track: Sequence[tuple[int, float, float]]) -> float:
    """Медианная скорость по шагам трека, м/с.

    Медиана, а не среднее: у эмулятора скорость гуляет (светофоры, разгон),
    и одно выбросовое значение не должно утянуть за собой весь маршрут.
    """
    speeds = []
    for (t0, lon0, lat0), (t1, lon1, lat1) in zip(track, track[1:]):
        dt = t1 - t0
        if dt > 0:
            speeds.append(haversine_m(lon0, lat0, lon1, lat1) / dt)
    if not speeds:
        return 0.0
    return float(statistics.median(speeds))


def _endpoint(track: Sequence[tuple[int, float, float]], at_end: bool, window_s: float
              ) -> "tuple[float, float, float]":
    """Скорость и азимут на конце трека по окну времени: (speed, bearing, ts).

    Скорость считается по длине дуги, а не по хорде окна: на повороте хорда
    короче пройденного пути на 2/θ, и при продлении на 15 минут эта ошибка
    превращается в сотни метров ухода плана от дороги.
    """
    if at_end:
        seg = [p for p in track if p[0] >= track[-1][0] - window_s]
    else:
        seg = [p for p in track if p[0] <= track[0][0] + window_s]
    ts, lon, lat = seg[-1] if at_end else seg[0]
    if len(seg) < 2:
        return 0.0, 0.0, ts
    first, last = (seg[0], seg[-1])
    dt = last[0] - first[0]
    dist = sum(haversine_m(a[1], a[2], b[1], b[2]) for a, b in zip(seg, seg[1:]))
    if dt <= 0 or dist < 1e-6:
        return 0.0, 0.0, ts
    return dist / dt, bearing_deg(first[1], first[2], last[1], last[2]), ts


def _shortest_delta(a: float, b: float) -> float:
    """Кратчайшая разница азимутов в (-180, 180]: поворот с 350° на 10° это +20°."""
    return (b - a + 180.0) % 360.0 - 180.0


def _turn_rate_deg_s(track: Sequence[tuple[int, float, float]], at_end: bool,
                     window_s: float) -> float:
    """Скорость поворота на конце трека, градусов в секунду, со знаком.

    Оценка по касательным на двух концах окна, а не по соседним азимутам:
    разница соседних точек — это в основном шум приёмника, и медиана таких
    разностей на прямой дороге даёт ненулевой поворот. Касательные берутся по
    короткой базе (TURN_BASE_S) у каждого края окна, поэтому усредняют шум
    отдельно от геометрии.

    Окно всегда читается в порядке времени (от ранних точек к поздним), а
    знак для движения назад меняется один раз в конце: по той же дороге,
    пройденной в обратную сторону, поворот имеет противоположный знак.

    Короткая дуга в пределах TURN_DEADBAND_DEG считается прямой: на прямой
    участок накопленный поворот — шум, а разница между прямой и такой дугой на
    расстоянии ARC_CAP_M меньше допуска привязки к маршруту.
    """
    seg = ([p for p in track if p[0] >= track[-1][0] - window_s] if at_end
           else [p for p in track if p[0] <= track[0][0] + window_s])
    if len(seg) < 3:
        return 0.0
    early, late = seg[0], seg[-1]
    dt = late[0] - early[0]
    if dt < MIN_TURN_WINDOW_S:
        return 0.0
    if haversine_m(early[1], early[2], late[1], late[2]) < MIN_TURN_RUN_M:
        return 0.0
    head_i = next((i for i, p in enumerate(seg) if p[0] - early[0] >= TURN_BASE_S), None)
    tail_i = next((i for i in range(len(seg) - 1, -1, -1)
                   if late[0] - seg[i][0] >= TURN_BASE_S), None)
    if head_i is None or tail_i is None or tail_i - head_i < 1:
        return 0.0
    b_early = bearing_deg(seg[0][1], seg[0][2], seg[head_i][1], seg[head_i][2])
    b_late = bearing_deg(seg[tail_i][1], seg[tail_i][2], seg[-1][1], seg[-1][2])
    turn = _shortest_delta(b_early, b_late)
    if abs(turn) < TURN_DEADBAND_DEG:
        return 0.0
    # Касательные берутся по базам у краёв окна, поэтому измеренный поворот
    # приходится на dt - TURN_BASE_S, а не на dt: делить на dt занижало бы
    # кривизну на 8/60 — на пятнадцати минутах это сотни метров.
    turn_dt = late[0] - early[0] - TURN_BASE_S
    if turn_dt < MIN_TURN_WINDOW_S:
        return 0.0
    rate = turn / turn_dt
    rate = max(-MAX_TURN_DEG_S, min(MAX_TURN_DEG_S, rate))
    return rate if at_end else -rate


def _step_point(lon: float, lat: float, bearing: float, d: float
                ) -> "tuple[float, float]":
    """Сдвиг на d метров по азимуту (d со знаком: назад — отрицательный)."""
    lat += d * math.cos(math.radians(bearing)) / M_PER_DEG_LAT
    lon += d * math.sin(math.radians(bearing)) / m_per_deg_lon(lat)
    return lon, lat


def _advance(lon: float, lat: float, bearing: float, rate: float, dist_m: float,
             speed: float) -> "tuple[float, float]":
    """Продвинуться на dist_m по дуге, дальше — по прямой (знак: назад).

    Дуга и прямая считаются в одной точности плоской аппроксимации: на
    сотнях метров разница сфероидной и плоской не видна, а километры
    продолжения всё равно не про дорогу, а про её направление.
    """
    if speed <= 1e-6 or dist_m == 0.0:
        return lon, lat
    sign = 1.0 if dist_m > 0 else -1.0
    total = abs(dist_m)
    curvy = min(total, ARC_CAP_M) if rate != 0.0 else 0.0
    straight = total - curvy
    if curvy > 0.0:
        steps = max(1, int(math.ceil(curvy / ARC_STEP_M)))
        step = sign * (curvy / steps)
        dt = abs(step) / speed
        for _ in range(steps):
            lon, lat = _step_point(lon, lat, bearing, step)
            bearing = (bearing + rate * dt) % 360.0
    if straight > 0.0:
        lon, lat = _step_point(lon, lat, bearing, sign * straight)
    return lon, lat


def position_at(track: Sequence[tuple[int, float, float]], t: int,
                past_window_s: float = 30.0, future_window_s: float = 20.0
                ) -> "tuple[float, float]":
    """Где машина окажется/была в момент unix-времени t.

    Внутри записи — линейная интерполяция по времени, то есть ровно тот путь,
    который эмулятор реально отрисовал. Снаружи — продолжение по курсу и
    скорости соответствующего конца трека с поправкой на кривизну: позади
    доступна только ранняя дорога, впереди — только то, что машина уже
    проехала, и ничего сверх того.
    """
    if not track:
        raise ValueError("пустой трек")
    if t >= track[-1][0]:
        speed, bearing, _ = _endpoint(track, True, future_window_s)
        rate = _turn_rate_deg_s(track, True, ARC_WINDOW_S)
        return _advance(track[-1][1], track[-1][2], bearing, rate,
                        speed * max(0.0, t - track[-1][0]), speed)
    if t <= track[0][0]:
        speed, bearing, _ = _endpoint(track, False, past_window_s)
        rate = _turn_rate_deg_s(track, False, ARC_WINDOW_S)
        return _advance(track[0][1], track[0][2], bearing, rate,
                        speed * max(0.0, track[0][0] - t), speed)
    for (t0, lon0, lat0), (t1, lon1, lat1) in zip(track, track[1:]):
        if t0 <= t <= t1:
            share = 0.0 if t1 == t0 else (t - t0) / (t1 - t0)
            return lon0 + (lon1 - lon0) * share, lat0 + (lat1 - lat0) * share
    return track[-1][1], track[-1][2]


def track_length_m(track: Iterable[tuple[int, float, float]]) -> float:
    return sum(
        haversine_m(a[1], a[2], b[1], b[2]) for a, b in zip(list(track), list(track)[1:])
    )
