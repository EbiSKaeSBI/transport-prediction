"""Тесты плана, который строится по трекам эмулятора.

Запуск: python3 -m pytest tests/test_plan_from_capture.py
"""

from __future__ import annotations

import datetime
import math
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))

import pytest  # noqa: E402

import plan_from_capture as pfc  # noqa: E402

import ndtp_feed as feed  # noqa: E402

LON0, LAT0 = 37.5, 55.7


def assert_plan_shape(body: list[str], units: int) -> None:
    """Проверки, общие для всех планов: строгий ход времени и шаг остановок.

    Шаг проверяется по хорде — именно её рисует карта и по ней же меряет
    матчер, поэтому расхождение с настоящей дорогой и есть то, что нужно
    удержать в узде. Стык повторов рейса пропускается: это другой участок
    расписания, а не дорога, и одной прямой его склеивать нельзя.
    """
    per_unit: dict[int, list[tuple[datetime.datetime, tuple[float, float], int]]] = {}
    for row in body:
        cols = row.split(",")
        at = datetime.datetime.strptime(cols[1], "%Y-%m-%d %H:%M:%S")
        m_lon, m_lat = cols[5].replace("POINT (", "").replace(")", "").split()
        per_unit.setdefault(int(cols[4]), []).append(
            (at, (float(m_lon), float(m_lat)), feed.plan_id_parts(int(cols[0]))[1])
        )

    assert set(per_unit) == set(range(1, units + 1)), sorted(per_unit)
    for tr_id, points in per_unit.items():
        times = [at for at, _, _ in points]
        assert times == sorted(times), f"tr={tr_id}: время в плане не возрастает"
        assert len(set(times)) == len(times), f"tr={tr_id}: две остановки на один момент"
        # Шов между повторами рейса: это другой участок расписания, и по
        # времени он неотличим от обычной ноги (столько же минут), поэтому
        # опознаётся только номером повтора в action_id.
        seams = [i for i, (a, b) in enumerate(zip(points, points[1:]), start=1)
                 if a[2] != b[2]]
        worst = 0.0
        for i in range(len(points) - 1):
            if i + 1 in seams:
                continue
            a, b = points[i][1], points[i + 1][1]
            worst = max(worst, pfc.haversine_m(a[0], a[1], b[0], b[1]))
        # +1 м на округление координат в CSV
        assert worst <= feed.STOP_SPACING_M + 1.0, (
            f"tr={tr_id}: хорда {worst:.0f} м длиннее шага {feed.STOP_SPACING_M:.0f} м"
        )


def _eastward_track(n: int, speed_mps: float = 10.0, dt: int = 1, lon0: float = LON0):
    """Трек на восток: скорость ровно speed_mps, шаг по времени dt."""
    m_lon = pfc.m_per_deg_lon(LAT0)
    return [
        (1000 + i * dt, lon0 + speed_mps * dt * i / m_lon, LAT0) for i in range(n)
    ]


def test_haversine_matches_known_distance():
    # 0.001° широты ≈ 111.19 м (радиус, которым меряет и plan_from_capture)
    d = pfc.haversine_m(LON0, LAT0, LON0, LAT0 + 0.001)
    assert 111.0 < d < 111.6, d
    # 0.001° долготы на 55.7° ≈ 62.8 м
    d = pfc.haversine_m(LON0, LAT0, LON0 + 0.001, LAT0)
    assert 62.0 < d < 63.5, d


def test_bearing_cardinals():
    assert pfc.bearing_deg(LON0, LAT0, LON0, LAT0 + 0.001) == 0.0  # север
    east = pfc.bearing_deg(LON0, LAT0, LON0 + 0.001, LAT0)
    assert 89.0 < east < 91.0, east  # восток
    south = pfc.bearing_deg(LON0, LAT0, LON0, LAT0 - 0.001)
    assert 179.0 < south < 181.0, south  # юг
    west = pfc.bearing_deg(LON0, LAT0, LON0 - 0.001, LAT0)
    assert 269.0 < west < 271.0, west  # запад


def test_track_speed_ignores_single_spike():
    track = _eastward_track(21, speed_mps=10.0)
    # один выброс: 100 м/с на одном шаге — медиана обязана его пережить
    spike_ts, spike_lon, _ = track[10]
    m_lon = pfc.m_per_deg_lon(LAT0)
    track[10] = (spike_ts, spike_lon + 90.0 / m_lon, LAT0)
    assert 9.0 < pfc.track_speed(track) < 11.0, pfc.track_speed(track)


def test_position_at_inside_track_is_interpolation():
    track = _eastward_track(11, speed_mps=10.0)
    lon, lat = pfc.position_at(track, 1005)
    m_lon = pfc.m_per_deg_lon(LAT0)
    assert lon == 37.5 + 50.0 / m_lon
    assert lat == LAT0


def test_position_at_future_follows_course():
    # сверка с допуском: скорость и курс меряются по треку, а трек задан
    # параллелью — на сфере это чуть не геодезическая линия, отсюда ~0.1%
    track = _eastward_track(11, speed_mps=10.0)
    lon, lat = pfc.position_at(track, track[-1][0] + 60)
    m_lon = pfc.m_per_deg_lon(LAT0)
    assert lon == pytest.approx(37.5 + (10 * 10 + 600.0) / m_lon, rel=2e-3)
    assert lat == pytest.approx(LAT0, abs=1e-6)


def test_position_at_past_goes_backwards():
    track = _eastward_track(11, speed_mps=10.0)
    lon, _ = pfc.position_at(track, track[0][0] - 30)
    m_lon = pfc.m_per_deg_lon(LAT0)
    assert lon == pytest.approx(37.5 - 300.0 / m_lon, rel=2e-3)


def _circle_track(radius_m: float, speed_mps: float = 15.0, n: int = 121,
                  t0: int = 1000) -> list:
    """Трек по окружности заданного радиуса: восток x, север y.

    Настоящая окружность, а не «держим широту и шевелим долготой»: во втором
    случае курс почти не меняется и кривизну измерять нечем.
    """
    omega = speed_mps / radius_m
    m_lon = pfc.m_per_deg_lon(LAT0)
    return [
        (
            t0 + i,
            LON0 + radius_m * math.sin(omega * i) / m_lon,
            LAT0 + radius_m * (1.0 - math.cos(omega * i)) / pfc.M_PER_DEG_LAT,
        )
        for i in range(n)
    ]


def _true_turn_rate(radius_m: float, speed_mps: float = 15.0) -> float:
    """Кривизна окружности в углах картографии: влево — отрицательная."""
    return -speed_mps / radius_m * 180.0 / math.pi


@pytest.mark.parametrize("radius_m", [400.0, 600.0, 1500.0, 3000.0])
def test_turn_rate_recovers_measured_curvature(radius_m):
    track = _circle_track(radius_m)
    want = _true_turn_rate(radius_m)
    got = pfc._turn_rate_deg_s(track, True, pfc.ARC_WINDOW_S)
    assert got == pytest.approx(want, rel=0.02), f"{radius_m}: {got} != {want}"
    # назад по той же дороге знак поворота противоположный
    assert pfc._turn_rate_deg_s(track, False, pfc.ARC_WINDOW_S) == pytest.approx(-want, rel=0.02)


def test_turn_rate_is_zero_on_straight_with_gps_noise():
    # ±0.5 м на координату: на прямом участке это весь «поворот», который видит
    # приёмник. Если бы шум проходил в оценку, план получал бы дугу там, где
    # дорога прямая, и через 15 минут уезжал на сотни метров.
    m_lon = pfc.m_per_deg_lon(LAT0)
    noise = 0.5 / m_lon
    track = [
        (1000 + i,
         LON0 + 15.0 * i / m_lon + noise * math.sin(i * 2.7),
         LAT0 + noise * math.cos(i * 1.9))
        for i in range(121)
    ]
    assert pfc._turn_rate_deg_s(track, True, pfc.ARC_WINDOW_S) == 0.0


def test_turn_rate_ignores_gentle_arc_below_deadband():
    # R=12 км — это уже почти прямая: поворот за окно меньше шума, кривизну
    # выдумывать нельзя
    track = _circle_track(12000.0)
    assert pfc._turn_rate_deg_s(track, True, pfc.ARC_WINDOW_S) == 0.0


def test_position_at_curves_only_inside_cap():
    # За пределами ARC_CAP_M дуга обязана закончиться: иначе «измеренная
    # кривизна» растягивается на километры будущей дороги, которой не видел
    # никто. Все три точки взяты далеко за капом (при 15 м/с кап в 800 м
    # проезжается за 53 с), поэтому обязаны лежать на одной прямой.
    track = _circle_track(600.0)
    after_cap_s = (pfc.ARC_CAP_M / 15.0) + 300.0
    a = pfc.position_at(track, track[-1][0] + after_cap_s)
    b = pfc.position_at(track, track[-1][0] + after_cap_s + 60)
    c = pfc.position_at(track, track[-1][0] + after_cap_s + 120)
    # Коллинеарность меряется в координатах, а не сравнением азимутов: прямая
    # в lon/lat — это локсодромия, и её азимут большой окружности плавно
    # дрейфует (~0.005° на 900 м на этих широтах) даже при полном отсутствии
    # поворота. Допуск в метрах: плоская модель, в которой масштаб долготы
    # зависит от текущей широты, оставляет на «прямой» остаточную спираль
    # около 0.1 м на 5 км. Продолжайся дуга — уход был бы сотни метров.
    dev_m = pfc.haversine_m(b[0], b[1], (a[0] + c[0]) / 2.0, (a[1] + c[1]) / 2.0)
    assert dev_m < 1.0, f"после капа {dev_m:.2f} м ухода от прямой"
    # шаг по прямой ровно speed*60 м
    assert pfc.haversine_m(a[0], a[1], b[0], b[1]) == pytest.approx(900.0, rel=0.01)


def test_position_at_future_distance_follows_speed_not_chord():
    # Продление обязано идти по длине пути, а не по хорде окна: на повороте
    # хорда короче дуги, и на 15 минутах вперёд это сотни метров ухода.
    track = _circle_track(600.0)
    for secs in (20, 120, 900):
        lon, lat = pfc.position_at(track, track[-1][0] + secs)
        # сравниваем путь по Polyline, а не хорду: хорда на дуге короче пути
        walked = 0.0
        prev = (track[-1][1], track[-1][2])
        steps = 40
        for i in range(1, steps + 1):
            p_lon, p_lat = pfc.position_at(
                track, track[-1][0] + secs * i / steps)
            walked += pfc.haversine_m(prev[0], prev[1], p_lon, p_lat)
            prev = (p_lon, p_lat)
        assert walked == pytest.approx(15.0 * secs, rel=0.02), \
            f"{secs}s: пройдено {walked:.0f} м вместо {15.0 * secs:.0f}"


def test_position_at_past_distance_follows_speed():
    track = _circle_track(600.0)
    walked = 0.0
    prev = (track[0][1], track[0][2])
    steps = 40
    for i in range(1, steps + 1):
        p_lon, p_lat = pfc.position_at(track, track[0][0] - 300 * i / steps)
        walked += pfc.haversine_m(prev[0], prev[1], p_lon, p_lat)
        prev = (p_lon, p_lat)
    assert walked == pytest.approx(4500.0, rel=0.02), walked


def test_plan_covers_past_and_future_stops_per_unit(tmp_path, monkeypatch):
    """Три юнита — три маршрута, у каждого своя дорога и свой action_id."""
    m_lon = pfc.m_per_deg_lon(LAT0)
    units = {}
    for i, unit in enumerate((1166336, 1166337, 1166338)):
        lat = LAT0 + i * 0.01
        # каждая машина едет на восток, но с разной скоростью
        units[unit] = [
            (1000 + k, LON0 + (5.0 * (i + 1) * k) / m_lon, lat) for k in range(120)
        ]

    plan = tmp_path / "plan.csv"
    binding = tmp_path / "binding.csv"
    now = datetime.datetime(2026, 1, 1, 12, 0, 0, tzinfo=datetime.timezone.utc)
    feed.write_plan_for_tracks(plan, binding, units, sorted(units), now)

    lines = plan.read_text(encoding="utf-8").strip().split("\n")
    assert lines[0] == feed.PLAN_HEADER
    body = lines[1:]
    # Стопов больше, чем скелетных смещений: ноги между ними добиты
    # промежуточными остановками. Проверяем не число, а свойства — иначе
    # любое уплотнение ломает тест, не будучи ошибкой.
    assert len(body) >= 9 * len(units)
    ids = [int(row.split(",")[0]) for row in body]
    assert len(set(ids)) == len(ids), "action_id должны быть уникальны"
    trs = {int(row.split(",")[4]) for row in body}
    assert trs == {1, 2, 3}, trs
    assert_plan_shape(body, len(units))

    # прошлые остановки (T-20..T-5) имеют факт +150 с, будущие — пустую
    # колонку: факта у них ещё нет, их горизонт сам посчитает
    past = 0
    future = 0
    for row in body:
        cols = row.split(",")
        fact = cols[7]
        if fact == "":
            future += 1
        else:
            past += 1
            planned = datetime.datetime.strptime(cols[1], "%Y-%m-%d %H:%M:%S")
            actual = datetime.datetime.strptime(fact, "%Y-%m-%d %H:%M:%S")
            assert (actual - planned) == datetime.timedelta(seconds=feed.LATE_FACT_S), row
    # Факты только у скелетных остановок: 4 прошедших и 5 будущих на юнита.
    # Промежуточные точки уплотнения факта не имеют никогда — они не
    # наблюдение, а геометрия дороги между наблюдениями.
    assert past == 4 * len(units) and future == len(body) - past, (past, future)

    assert binding.read_text(encoding="utf-8") == (
        "tr_id,unit_id\n1,1166336\n2,1166337\n3,1166338\n"
    )


def test_tracks_by_unit_groups_frames_by_peer():
    """Кадры разных юнитов не должны сливаться в один трек."""

    # строим кадры через сам фид: он же владеет сериализацией
    import struct

    def frame(unit: int, ts: int, lon_e7: int) -> bytes:
        body = struct.pack("<I", ts) + struct.pack("<I", lon_e7) + b"\x00" * 18
        assert len(body) == feed.NAV00_SIZE
        cell = struct.pack("<BB", feed.CELL_NAV00, 0) + body
        return feed.build_frame(unit, feed.SERVICE_NAVDATA, feed.NPH_TYPE_REALTIME, cell)

    frames = [
        frame(1166336, 1000, 375000000),
        frame(1166337, 1000, 375100000),
        frame(1166336, 1001, 375000001),
    ]
    tracks = feed.tracks_by_unit(frames)
    assert set(tracks) == {1166336, 1166337}
    assert len(tracks[1166336]) == 2
    assert len(tracks[1166337]) == 1
    assert tracks[1166337][0][1] == 37.51


def test_plan_from_capture_matches_observed_speed():
    """План «вперёд» не должен быть выдуманной прямой: скорость — из трека."""
    track = _eastward_track(60, speed_mps=8.0)
    speed = pfc.track_speed(track)
    assert 7.9 < speed < 8.1
    lon, _ = pfc.position_at(track, track[-1][0] + 600)
    m_lon = pfc.m_per_deg_lon(LAT0)
    # Допуск 1e-6, а не 1e-9: трек задан параллелью, а продление считается по
    # дуге большой окружности, курс которой на широте 55.7° равен 89.9989° —
    # на 4.8 км это 8 см ухода на юг и обратно в градусах долготы. Раньше
    # деление на метры градуса долготы брало широту начала шага, и ошибка
    # взаимно сокращалась с неточностью курса до 1e-9; теперь широта берётся
    # после смещения (это верно), и видно настоящую величину.
    assert math.isclose(lon - track[-1][1], 8.0 * 600 / m_lon, rel_tol=1e-6)


def test_plan_repeats_trip_with_unique_ids_and_shifted_times(tmp_path):
    """Живой прогон требует повторов: расписание абсолютное и не умеет циклов.

    Без повторов конвейер через четыре минуты честно отказывает
    (no_target_in_horizon), хотя телеметрия идёт: у тика в окне горизонта
    не остаётся ни одной остановки. Повтор должен сдвигать время на период
    рейса и давать свои action_id, иначе claim() откажет в повторной цели.
    """
    m_lon = pfc.m_per_deg_lon(LAT0)
    units = {1166336: [(1000 + k, LON0 + (5.0 * k) / m_lon, LAT0) for k in range(120)]}
    plan = tmp_path / "plan.csv"
    binding = tmp_path / "binding.csv"
    now = datetime.datetime(2026, 1, 1, 12, 0, 0, tzinfo=datetime.timezone.utc)
    feed.write_plan_for_tracks(plan, binding, units, [1166336], now, repeat=3)

    body = plan.read_text(encoding="utf-8").strip().split("\n")[1:]
    assert len(body) >= 9 * 3
    ids = [int(row.split(",")[0]) for row in body]
    assert len(set(ids)) == len(ids), "action_id повторов должны быть уникальны"
    assert_plan_shape(body, 1)

    # Момент прихода к скелетной остановке каждого смещения в каждом повторе.
    # Смещение и повтор разбираются из action_id, точка внутри ноги равна нулю
    # только у самой скелетной остановки.
    def times(off_index: int) -> list[datetime.datetime]:
        out = []
        for row in body:
            cols = row.split(",")
            _, _, off, sub = feed.plan_id_parts(int(cols[0]))
            if off == off_index and sub == 0:
                out.append(datetime.datetime.strptime(cols[1], "%Y-%m-%d %H:%M:%S"))
        return out

    # одна остановка каждого смещения встречается в каждом повторе
    first, second, third = times(0)
    assert second - first == third - second, "период повтора должен быть постоянным"
    # и он не меньше самого рейса, иначе повторы наезжают друг на друга
    span = datetime.timedelta(
        minutes=max(feed.STOP_OFFSETS_MIN) - min(feed.STOP_OFFSETS_MIN)
    )
    assert (second - first) >= span

    # Факт есть только у остановок, которые уже случились: в повторе
    # «прошлые» по времени рейса остановки ещё впереди. Иначе гейтвей считает
    # отставание по невозможному наблюдению, признаки рассыпаются,
    # предиктор падает в baseline и риск молча становится зелёным.
    # plan.csv пишет UTC, а now в тесте и так UTC
    now_made = now.replace(tzinfo=None)
    past_rows = [r for r in body if r.split(",")[7] != ""]
    for row in past_rows:
        planned = datetime.datetime.strptime(row.split(",")[1], "%Y-%m-%d %H:%M:%S")
        assert planned < now_made, f"факт у будущей остановки: {row}"
    # прошедшая часть первого рейса сохранила факты (+LATE_FACT_S)
    assert len(past_rows) == 4, past_rows
