#!/usr/bin/env python3
"""Фидер NDTP для живого E2E дашборда (GitLab #36, этап 5).

Гонит на transportctl serve (:9201) НАСТОЯЩИЕ кадры протокола NDTP,
пересобранные из golden-потока backend/internal/ndtp/testdata/golden/
packets.bin: байты ячеек переиспользуются дословно; на каждом круге
меняются метка времени Nav00 (сдвиг на «сейчас») и долгота (накат
CYCLE_LON_E7, чтобы точка не откатывалась на стыке кадров) —
сериализация кадра, CRC-16/Modbus со свапом байтов в NPL и handshake
(CONN_REQUEST, unit_id = PeerAddress тела) воспроизводят формат из
backend/internal/ndtp (frame.go, crc.go, handshake.go) один в один.

Дополнительно --plan-out/--binding-out пишут синтетический план-график и
привязку под текущие часы (той же формы, что writePlanCSV в e2e-тестах Go):
первые остановки с time_fact_begin = план +150 с, чтобы конвейер посчитал
cur_dev_s >= 120 и лента породила красный инцидент на живых данных.

Координаты остановок берутся из кинематики самого потока, а не из выдуманной
прямой: план — это места, где машина окажется в назначенное время. Иначе
план лежит в километре от траектории, точка не попадает на маршрут (допуск
привязки 150 м) и участки риска не красятся никогда.

--plan-from-capture решает ту же задачу для НАСТОЯЩЕГО эмулятора: там у
каждой машины своя дорога, поэтому маршрут строится на каждый юнит из
трека его собственной телеметрии (scripts/plan_from_capture.py).

Только stdlib. Запуск:

    python3 scripts/ndtp_feed.py --plan-out /tmp/live_plan.csv \
        --binding-out /tmp/live_binding.csv --generate-only   # файлы до serve
    python3 scripts/ndtp_feed.py --plan-out ... --binding-out ... --every 1
    python3 scripts/ndtp_feed.py --plan-from-capture --golden /tmp/emu \
        --plan-out plan.csv --binding-out binding.csv         # план для эмулятора

--golden переопределяет источник кадров: снять эмулятор через
`transportctl ndtp-capture` и перегнать запись, план посчитается по ней же.

Фидер не завершается: шлёт по пакету в --every секунд, пока его не убьют
(пауза в 5 с и повторный запуск — штатная проверка реконнекта).
"""
from __future__ import annotations

import argparse
import math
import socket
import struct
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

import plan_from_capture

ROOT = Path(__file__).resolve().parent.parent
GOLDEN = ROOT / "backend" / "internal" / "ndtp" / "testdata" / "golden" / "packets.bin"

NPL_SIZE, NPH_SIZE = 15, 10
CELL_NAV00, NAV00_SIZE = 0, 26
NPH_TYPE_CONN, NPH_TYPE_REALTIME = 100, 101
SERVICE_GENERIC, SERVICE_NAVDATA = 0, 1

# PeerAddress в заголовке NPL: по нему кадры разносятся по юнитам, у
# эмулятора на каждый unitId своё соединение и своя дорога.
NPL_PEER = 9

PLAN_HEADER = (
    "tt_action_item_id,time_begin,order_date,manual_fill,tr_id,geom,"
    "building_address,time_fact_begin"
)

# Смещения Nav00 в теле ячейки: метка события, долгота, широта — все в 1e-7°.
NAV00_TS, NAV00_LON, NAV00_LAT = 0, 4, 8

# Накат долготы на круг зацикленного golden-потока, 1e-7° за круг (один
# оборот = len(nav_frames) кадров). Держит машину на востоке без отката при
# переходе с последнего кадра на первый. План считается по той же величине,
# поэтому стоит только сменить это число — и план поедет вместе с машиной.
CYCLE_LON_E7 = 4125

# Остановки относительно «сейчас», минуты. Минус — уже состоявшиеся: у них
# факт +150 с, и это сценарий «накопленное отставание», из-за которого
# конвейер видит cur_dev_s >= 120. Плюс — цели горизонта (T+10; T+15].
STOP_OFFSETS_MIN = (-20, -15, -10, -5, 11, 12, 13, 14, 15)
LATE_FACT_S = 150

# Максимальный шаг между соседними остановками плана, м.
#
# Ровно тот же допуск, по которому точка считается едущей по маршруту
# (ON_ROUTE_TOLERANCE_M в дашборде и гейтвее). Совпадение не случайно: и
# карта, и матчер рисуют и меряют ломаную по остановкам, а промах хорды
# относительно настоящей дороги растёт как c²/(8R) — от длины хорды. При шаге
# 150 м на городском вираже (R ≈ 200 м) это около 15 м, то есть ошибка
# отрисовки остаётся на порядок меньше допуска привязки. Раньше шаг доходил
# до 800 м, и одна только хорда съедала 30–160 м из того же допуска.
STOP_SPACING_M = 150.0

# Сколько раз нога может быть поделена ещё пополам, прежде чем шаг признать
# неустранимым. Четыре удвоения — до 16 частей, этого хватает с запасом для
# отношения «хорда ноги в 16 раз длиннее шага», то есть ноги до 2.4 км.
STOP_SPACING_REFINEMENTS = 4

# Ширина поля «номер точки внутри ноги» в action_id. Три разряда — с запасом
# больше, чем дают STOP_SPACING_REFINEMENTS при шаге 150 м.
STOP_SUB_FIELD = 1000


def plan_action_id(tr_id: int, rep: int, off_index: int, sub: int) -> int:
    """action_id плана: tr | повтор | смещение | точка внутри ноги.

    Схема разбирается обратно без догадок — по тысячной части повтор, по
    сотой части... точнее, по делению на STOP_SUB_FIELD номер скелетного
    смещения и остатком номер точки внутри ноги. Промежуточные точки не
    отличить от скелетных иначе, а различать их нужно: только у скелетной
    остановки есть факт.
    """
    if not 0 <= sub < STOP_SUB_FIELD:
        raise ValueError(
            f"точка внутри ноги {sub} не помещается в поле из {STOP_SUB_FIELD} "
            f"разрядов: action_id схлопнется с соседним смещением"
        )
    return (tr_id * 1000 + rep) * STOP_SUB_FIELD * len(STOP_OFFSETS_MIN) + \
        off_index * STOP_SUB_FIELD + sub


def plan_id_parts(action_id: int) -> "tuple[int, int, int, int]":
    """Обратная разборка plan_action_id.

    Возвращает (tr_id, повтор, номер скелетного смещения, точка внутри ноги).
    Точка внутри ноги равна нулю у самой скелетной остановки — только у неё
    есть факт, поэтому по ней видно, какие строки плана наблюдения, а какие
    добавлены уплотнением.


    Нужна не только для отладки. Стык двух повторов рейса нельзя опознать по
    времени: он занимает столько же минут, сколько обычная нога, и отличается
    только тем, что это другой участок расписания. По времени он неотличим от
    нормальной ноги, а по номеру повтора — отличим однозначно.
    """
    sub = action_id % STOP_SUB_FIELD
    rest = action_id // STOP_SUB_FIELD
    off_index = rest % len(STOP_OFFSETS_MIN)
    tr_rep = rest // len(STOP_OFFSETS_MIN)
    return tr_rep // 1000, tr_rep % 1000, off_index, sub


def crc16_modbus(data: bytes) -> int:
    crc = 0xFFFF
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = (crc >> 1) ^ 0xA001 if crc & 1 else crc >> 1
    return crc & 0xFFFF


def swap16(v: int) -> int:
    return ((v << 8) | (v >> 8)) & 0xFFFF


def build_frame(peer: int, service: int, nph_type: int, body: bytes, req_id: int = 1) -> bytes:
    """Кадр NDTP: NPL + NPH + тело; CRC по NPH+телу, в NPL лежит со свапом."""
    nph = struct.pack("<HHHI", service, nph_type, 1, req_id)  # флаг request
    npl_wo_crc = bytearray(struct.pack("<HHHHBIH", 0x7E7E, NPH_SIZE + len(body), 0, 0, 2, peer, req_id))
    crc = swap16(crc16_modbus(nph + body))
    npl_wo_crc[6:8] = struct.pack("<H", crc)
    return bytes(npl_wo_crc) + nph + body


def build_handshake(unit_id: int) -> bytes:
    body = struct.pack("<HHHIII", 6, 2, 0, unit_id, 65535, 0)  # proto 6.2, без шифрования
    assert len(body) == 18
    return build_frame(unit_id, SERVICE_GENERIC, NPH_TYPE_CONN, body)


def split_frames(stream: bytes) -> list[bytes]:
    """Режет поток на целые кадры (NPL 15 байт + dataSize), как ndtp.Reader."""
    out, off = [], 0
    while off + NPL_SIZE <= len(stream):
        sig, data_size = struct.unpack_from("<HH", stream, off)
        if sig != 0x7E7E:
            off += 1  # выровнялись по середине тела — ищем сигнатуру
            continue
        total = NPL_SIZE + data_size
        if off + total > len(stream):
            break
        out.append(stream[off:off + total])
        off += total
    return out


CELL_SIZES = {
    0: 26, 2: 26, 3: 14, 4: 15, 5: 6, 6: 9, 7: 1, 8: 6, 9: 40, 10: 37,
    12: 5, 13: 13, 14: 15, 15: 50, 16: 8, 17: 46, 18: 50, 19: 40, 20: 8,
    21: 180, 22: 24, 23: 16, 100: 44,
}  # реестр размеров ячеек — копия cellSizes из backend/internal/ndtp/cells.go


def nav00_offsets(frame: "bytes | bytearray") -> list[int]:
    """Смещения payload ВСЕХ ячеек Nav00 в кадре (после NPL+NPH).

    Обход идёт по полному реестру размеров: в теле realtime-кадра Nav00 не
    обязан быть первым и может повторяться (а telemetry.Build применяет
    ячейки по порядку — выигрывает последняя). Патчить только начало тела
    означало бы оставить в Observation старое время события.
    """
    offs, p = [], NPL_SIZE + NPH_SIZE
    while p + 2 <= len(frame):
        cell_type = frame[p]
        size = CELL_SIZES.get(cell_type)
        if size is None or p + 2 + size > len(frame):
            break
        if cell_type == CELL_NAV00:
            offs.append(p + 2)
        p += 2 + size
    return offs


def patch_frame(frame: bytes, ts: int, req_id: int, lon_shift_e7: int = 0) -> bytes:
    """Тот же кадр с новой меткой Nav00 и пересчитанной CRC (NPL+NPH+тело).

    lon_shift_e7 — непрерывный накат долготы по кругу зацикленного golden-
    потока (шаг серии 11 кадров ≈ 4125·1e-7°), чтобы машина ползла на
    восток, а не телепортировалась назад каждый оборот.
    """
    f = bytearray(frame)
    for off in nav00_offsets(f):
        struct.pack_into("<I", f, off, ts & 0xFFFFFFFF)
        if lon_shift_e7:
            lon = struct.unpack_from("<I", f, off + 4)[0]
            struct.pack_into("<I", f, off + 4, (lon + lon_shift_e7) & 0xFFFFFFFF)
    body_len = struct.unpack_from("<H", f, 2)[0]
    rest = bytes(f[NPL_SIZE:NPL_SIZE + body_len])
    f[6:8] = struct.pack("<H", swap16(crc16_modbus(rest)))
    struct.pack_into("<H", f, 13, req_id & 0xFFFF)
    return bytes(f)


def frame_points(nav_frames: "list[bytes]") -> "list[tuple[float, float]]":
    """Координаты Nav00-кадров в градусах: [(lon, lat), ...]."""
    pts = []
    for f in nav_frames:
        off = nav00_offsets(f)[0]
        lon = struct.unpack_from("<I", f, off + NAV00_LON)[0] / 1e7
        lat = struct.unpack_from("<i", f, off + NAV00_LAT)[0] / 1e7
        pts.append((lon, lat))
    return pts


def playback_velocity(pts: "list[tuple[float, float]]", every: float) -> "tuple[float, float]":
    """Чистое смещение точки за единицу времени, м/с: (вдоль долготы, вдоль широты).

    Golden- след замкнут: за круг широта возвращается к исходной, а долгота
    доходит до последнего кадра и на стыке кругов откатывается на этот шаг
    назад. Поэтому из пути внутри записи в чистое смещение попадает только
    накат CYCLE_LON_E7 — ровно то, чем круг за кругом растёт положение точки.
    Считать «хорду + накат» нельзя: хорда откатывается, и план уехал бы вдвое
    дальше машины, а точка за 35 минут вышла бы за допуск привязки.

    Один круг — len(pts) кадров по --every секунд; темп задан дедлайнами в
    цикле отправки, поэтому --every здесь и есть фактический интервал.
    """
    if len(pts) < 2 or every <= 0:
        return 0.0, 0.0
    lap_s = len(pts) * every
    m_per_deg_lon = plan_from_capture.m_per_deg_lon(pts[0][1])
    return CYCLE_LON_E7 * 1e-7 * m_per_deg_lon / lap_s, 0.0


def tracks_by_unit(frames: "list[bytes]") -> "dict[int, list[tuple[int, float, float]]]":
    """Треки (unix-время, lon, lat) по юнитам: PeerAddress в заголовке NPL.

    У эмулятора на каждый unitId своё соединение и своя дорога, поэтому
    группировать кадры по юниту обязательно: смешанные координаты дают
    маршрут, которого не существует.
    """
    tracks: "dict[int, list[tuple[int, float, float]]]" = {}
    for f in frames:
        offs = nav00_offsets(f)
        if not offs:
            continue
        off = offs[0]
        unit = struct.unpack_from("<I", f, NPL_PEER)[0]
        ts = struct.unpack_from("<I", f, off + NAV00_TS)[0]
        lon = struct.unpack_from("<I", f, off + NAV00_LON)[0] / 1e7
        lat = struct.unpack_from("<i", f, off + NAV00_LAT)[0] / 1e7
        tracks.setdefault(unit, []).append((ts, lon, lat))
    for track in tracks.values():
        track.sort(key=lambda p: p[0])
    return tracks


def write_plan_for_tracks(
    plan_path: Path,
    binding_path: Path,
    tracks: "dict[int, list[tuple[int, float, float]]]",
    order: "list[int]",
    now: datetime,
    repeat: int = 1,
) -> None:
    """План на несколько машин по фактическим трекам эмулятора.

    Остановки каждой машины стоят там, где она была (T-20..T-5 — реальный
    пройденный путь) или окажется (T+11..T+15 — продолжение по курсу и
    скорости последних секунд). Будущее эмулятора не предсказуемо, поэтому
    «вперёд» — это касательная, а не обещание дороги: см.
    scripts/plan_from_capture.py.

    repeat — сколько раз повторить рейс каждой машины. Расписание в
    backend/internal/schedule абсолютное и не умеет повторов, а тик
    конвейера честно отказывает, когда в окне горизонта не осталось ни одной
    остановки (no_target_in_horizon) или окно расписания вышло (без_окна).
    Для живого прогона на эмуляторе это значит «прогнозы остановились через
    четыре минуты», при том что телеметрия продолжает идти. Реальный рейс
    ходит по расписанию много раз, поэтому и здесь рейс повторяется: та же
    дорога, тот же сдвиг времени, свои action_id — иначе claim() в
    конвейере отказал бы повторно отправлять ту же цель.
    """
    base = now.replace(microsecond=0)
    # Период чуть больше самого рейса: иначе первый повтор начинается
    # одновременно с последней остановкой предыдущего.
    period = timedelta(minutes=max(STOP_OFFSETS_MIN) - min(STOP_OFFSETS_MIN) + 2)
    rows = [PLAN_HEADER]
    binding = ["tr_id,unit_id"]
    for tr_id, unit in enumerate(order, start=1):
        track = tracks[unit]
        for rep in range(max(1, repeat)):
            rep_base = base + period * rep
            prev_at = None
            prev_pt = None
            # j — сквозной номер строки внутри повтора, попадает в подпись
            # остановки. Идентификатор же собирается из номера повтора и
            # номера скелетного смещения плюс номера точки внутри ноги: он
            # обязан быть уникален во всём файле (schedule.go отклоняет
            # дубли), а по такой разбивке его можно и разобрать — по
            # тысячной части повтор, по сотой части смещение, по единицам
            # номер точки внутри ноги (0 — сама скелетная остановка).
            j = 0
            for i, off_min in enumerate(STOP_OFFSETS_MIN):
                at = rep_base + timedelta(minutes=off_min)
                lon, lat = plan_from_capture.position_at(track, int(at.timestamp()))
                # Ноги между скелетными остановками добиваем промежуточными
                # точками. Промежуточные позиции берутся тем же position_at, а
                # не интерполяцией между концами: интерполяция срезала бы дугу
                # по всей ноге разом и вернула ровно ту хорду, ради устранения
                # которой ноги и дробятся.
                #
                # Число шагов сначала оценивается по хорде концов, но точки
                # расставлены по времени, а скорость на ноге не постоянна: на
                # разгоне шаг по расстоянию выходит больше оценки. Поэтому
                # дробление доводится до фактической гарантии — пока каждый
                # кусок не станет короче STOP_SPACING_M.
                if prev_at is not None and prev_pt is not None:
                    leg_m = plan_from_capture.haversine_m(
                        prev_pt[0], prev_pt[1], lon, lat)
                    span_s = (at - prev_at).total_seconds()
                    # Не чаще одной остановки в секунду: position_at всё равно
                    # квантует позицию до целой секунды, поэтому дробление
                    # быстрее Гц даёт повторяющиеся точки с разными метками
                    # времени. Предел достижим только на скорости выше
                    # STOP_SPACING_M м/с, то есть недостижим.
                    max_steps = max(1, int(span_s))
                    steps = min(max(1, math.ceil(leg_m / STOP_SPACING_M)),
                                max_steps)
                    mids: list[tuple[datetime.datetime, float, float]] = []
                    for _ in range(STOP_SPACING_REFINEMENTS):
                        mids = []
                        for k in range(1, steps):
                            mid_at = prev_at + timedelta(seconds=span_s * k / steps)
                            mid_lon, mid_lat = plan_from_capture.position_at(
                                track, int(mid_at.timestamp()))
                            mids.append((mid_at, mid_lon, mid_lat))
                        gaps = [plan_from_capture.haversine_m(
                            prev_pt[0], prev_pt[1], m[1], m[2]) for m in mids]
                        gaps.append(plan_from_capture.haversine_m(
                            mids[-1][1], mids[-1][2], lon, lat) if mids
                            else leg_m)
                        if max(gaps) <= STOP_SPACING_M or steps >= max_steps:
                            break
                        steps = min(steps * 2, max_steps)
                    for k, (mid_at, mid_lon, mid_lat) in enumerate(mids, start=1):
                        j += 1
                        rows.append(
                            plan_row(plan_action_id(tr_id, rep, i, k), tr_id, j,
                                     mid_at, mid_lon, mid_lat, fact=None)
                        )
                # Факт — это наблюдение, а не намерение: он есть только у
                # остановки, которая уже случилась. В повторе «прошлые» по
                # времени рейса остановки ещё впереди, и факт в их колонке
                # означал бы измеренную задержку в будущем. Гейтвей тогда
                # считает отставание по невозможному наблюдению, признаки
                # рассыпаются, предиктор падает в baseline и риск молча
                # становится зелёным.
                fact = plan_fact(at, off_min) if at < base else None
                j += 1
                rows.append(
                    plan_row(plan_action_id(tr_id, rep, i, 0), tr_id, j, at,
                             lon, lat, fact=fact)
                )
                prev_at, prev_pt = at, (lon, lat)
        binding.append(f"{tr_id},{unit}")
        span = plan_from_capture.track_length_m(track)
        speed = plan_from_capture.track_speed(track)
        print(
            f"  tr_id={tr_id} unit={unit}: точек {len(track)}, путь {span / 1000:.2f} км, "
            f"скорость {speed * 3.6:.1f} км/ч"
        )
    plan_path.write_text("\n".join(rows) + "\n", encoding="utf-8")
    binding_path.write_text("\n".join(binding) + "\n", encoding="utf-8")


def plan_fact(at: datetime, off_min: int) -> "datetime | None":
    """Факт прихода для прошедшей остановки: машина была на +LATE_FACT_S позже.

    Будущие остановки факта не имеют — там ещё пустая колонка, и гейтвей сам
    считает их целями горизонта.
    """
    if off_min >= 0:
        return None
    return at + timedelta(seconds=LATE_FACT_S)


def plan_row(action_id: int, tr_id: int, index: int, at: datetime, lon: float, lat: float,
             fact: "datetime | None") -> str:
    """Одна строка plan.csv — формат как writePlanCSV в e2e Go.

    Время без зоны: гейтвей разбирает его как UTC. action_id уникален во всём
    файле (schedule.go отклоняет дубли), поэтому у нескольких машин он
    разводится по tr_id.
    """
    stamp = "%Y-%m-%d %H:%M:%S"
    at_utc = at.astimezone(timezone_utc)
    fact_s = "" if fact is None else fact.astimezone(timezone_utc).strftime(stamp)
    return (
        f"{action_id},{at_utc.strftime(stamp)},{at_utc.strftime(stamp)},False,{tr_id},"
        f"POINT ({lon:.6f} {lat:.6f}),стоп-{index},{fact_s}"
    )


def write_plan_and_binding(
    plan_path: Path,
    binding_path: Path,
    unit_id: int,
    now: datetime,
    origin: "tuple[float, float]",
    v_lon: float,
    v_lat: float,
) -> None:
    """Синтетический план под живые часы: формат — как writePlanCSV в e2e Go.

    Время остановок — как было (прошлые T-20..T-5 с фактом +150 с, будущие
    T+11..T+15 — цели горизонта). Координаты — куда машина реально доедет к
    этому времени: точка отправки плюс её собственная скорость из потока.

    time_begin без зоны, разбирается гейтвеем как UTC — пишем UTC-моменты.
    """
    rows = [PLAN_HEADER]
    base = now.replace(microsecond=0)
    lon0, lat0 = origin
    m_per_deg_lon = plan_from_capture.m_per_deg_lon(lat0)
    for i, off_min in enumerate(STOP_OFFSETS_MIN):
        at = base + timedelta(minutes=off_min)
        dt_s = off_min * 60
        lon = lon0 + (v_lon * dt_s) / m_per_deg_lon
        lat = lat0 + (v_lat * dt_s) / plan_from_capture.M_PER_DEG_LAT
        rows.append(
            plan_row(100 + i, 1, i, at, lon, lat, fact=plan_fact(at, off_min))
        )
    plan_path.write_text("\n".join(rows) + "\n", encoding="utf-8")
    binding_path.write_text(f"tr_id,unit_id\n1,{unit_id}\n", encoding="utf-8")


timezone_utc = timezone.utc


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=9201)
    ap.add_argument("--unit", type=int, action="append", default=None,
                    help="юнит: PeerAddress для фида; в режиме --plan-from-capture — "
                         "состав и порядок tr_id (по умолчанию все юниты из потока)")
    ap.add_argument("--every", type=float, default=1.0, help="шаг отправки realtime-кадров, с")
    ap.add_argument("--seed-window", type=float, default=60.0,
                    help="как глубоко в прошлое поставить предзагрузку точек")
    ap.add_argument("--plan-out", type=Path, default=None)
    ap.add_argument("--binding-out", type=Path, default=None)
    ap.add_argument("--generate-only", action="store_true", help="только файлы плана/привязки")
    ap.add_argument("--golden", type=Path, default=GOLDEN,
                    help="источник кадров (по умолчанию golden-поток; сюда же пишет ndtp-capture)")
    ap.add_argument("--plan-repeat", type=int, default=1,
                    help="сколько раз повторить рейс в плане (живой прогон: 5 и больше)")
    ap.add_argument("--plan-from-capture", action="store_true",
                    help="построить plan/binding по трекам юнитов из --golden и выйти "
                         "(для прогона через эмулятор: у каждой машины своя дорога)")
    args = ap.parse_args()

    peer_unit = args.unit[0] if args.unit else 1166336

    frames = split_frames(args.golden.read_bytes())
    nav_frames = [f for f in frames if nav00_offsets(f)]
    if not nav_frames:
        print(f"в {args.golden} нет Nav00-кадров", file=sys.stderr)
        return 1

    if args.plan_from_capture:
        if not (args.plan_out and args.binding_out):
            print("--plan-from-capture требует --plan-out и --binding-out", file=sys.stderr)
            return 1
        tracks = tracks_by_unit(frames)
        order = args.unit if args.unit else sorted(tracks)
        missing = [u for u in order if u not in tracks]
        if missing:
            print(f"в {args.golden} нет юнитов {missing}", file=sys.stderr)
            return 1
        if len(order) > 1:
            print(f"юнитов в потоке: {len(tracks)}, в плане: {len(order)}")
        write_plan_for_tracks(args.plan_out, args.binding_out, tracks, order,
                              datetime.now(timezone_utc), repeat=args.plan_repeat)
        print(f"план: {args.plan_out}, привязка: {args.binding_out}")
        return 0

    # базовая метка golden-потока — от неё считаем честный сдвиг всей серии
    base_ts = struct.unpack_from("<I", nav_frames[0], NPL_SIZE + NPH_SIZE + 2)[0]
    print(f"golden-кадров: {len(frames)}, с Nav00: {len(nav_frames)}, базовая метка: {base_ts}")

    # План считается ДО serve и по той же кинематике, которую пойдёт поток:
    # иначе остановки оказываются в километре от траектории, точка не ложится
    # на маршрут, и участки риска не рисуются (см. docs/dashboard.md,
    # «План строится по потоку, а не по выдуманной прямой»).
    if args.plan_out and args.binding_out:
        pts = frame_points(nav_frames)
        v_lon, v_lat = playback_velocity(pts, args.every)
        write_plan_and_binding(
            args.plan_out, args.binding_out, peer_unit, datetime.now(timezone_utc),
            pts[0], v_lon, v_lat,
        )
        print(
            f"план: {args.plan_out}, привязка: {args.binding_out}; "
            f"скорость потока {math.hypot(v_lon, v_lat) * 3.6:.1f} км/с, "
            f"старт ({pts[0][0]:.5f} {pts[0][1]:.5f})"
        )
        if args.generate_only:
            return 0

    sent = 0
    req = 0
    t_feed = time.time()
    # Цикл переподключения: рестарт gateway на демо — штатная операция, и
    # поток обязан возобновиться сам, без человека с клавиатурой.
    while True:
        try:
            sock = socket.create_connection((args.host, args.port), timeout=5)
        except OSError as e:
            print(f"подключение к {args.host}:{args.port} не поднялось ({e}) — повтор через 2 с")
            time.sleep(2)
            continue
        sock.settimeout(0.2)
        req += 1
        sock.sendall(build_handshake(peer_unit))
        try:
            sock.recv(4096)  # ответ сервера на handshake (если есть) — игнорируем
        except socket.timeout:
            pass
        print(f"handshake отправлен на {args.host}:{args.port}, unit_id={peer_unit}")
        sent = 0  # после перерыва заново предзагружаем окно: конвейеру нужны
                  # свежие точки за --seed-window, разрыв в накопителе недопустим
        preloading = True
        # Темп по дедлайнам, а не sleep(every) в конце итерации: таймаут на
        # дренаж входящих сам занимает до 0.2 с, и при sleep в конце фактическая
        # частота съезжала до 0.83 Гц вместо заявленных 1 Гц. План считается от
        # --every, значит --every обязан быть правдой.
        next_at = time.monotonic() + args.every
        try:
            while True:
                now_ts = int(time.time())
                if preloading:
                    # предзагрузка: окно точек за --seed-window секунд до «сейчас»,
                    # иначе первому тику конвейера нечего положить в окно признаков
                    k = max(1, len(nav_frames))
                    span = args.seed_window
                    for i in range(k):
                        ts = now_ts - int(span) + int(span * i / max(1, k - 1) * 0.999)
                        req += 1
                        sock.sendall(patch_frame(nav_frames[i % len(nav_frames)], ts, req,
                                                 lon_shift_e7=0))
                        sent += 1
                    preloading = False
                    next_at = time.monotonic() + args.every  # предзагрузка темп не съедает
                else:
                    idx = sent % len(nav_frames)
                    lap = sent // len(nav_frames)
                    req += 1
                    sock.sendall(patch_frame(nav_frames[idx], now_ts, req,
                                             lon_shift_e7=lap * CYCLE_LON_E7))
                    sent += 1
                try:
                    sock.recv(65536)  # дренируем входящие (ack/result), сокет не блокируется
                except (socket.timeout, BlockingIOError):
                    pass
                nap = next_at - time.monotonic()
                if nap > 0:
                    time.sleep(nap)
                next_at += args.every
        except KeyboardInterrupt:
            print(f"отправлено realtime-кадров: {sent} за {time.time() - t_feed:.1f} с")
            return 0
        except (BrokenPipeError, ConnectionResetError):
            print("связь потеряна — переподключение через 2 с")
        finally:
            sock.close()
        time.sleep(2)


if __name__ == "__main__":
    sys.exit(main())
