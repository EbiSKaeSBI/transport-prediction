#!/usr/bin/env python3
"""Реальный контур: фид живёт на настоящих данных датасета, а не на синтетике.

Что было не так с живой сценой: эмулятор гоняет машины по случайным кривым
(docs/Emulator-and-Telematic-Packets-Specification.md, «План стареет»), а
plan_from_capture додумывает маршрут следом за машиной и пересобирает его
каждую минуту — линии на дашборде оказывались экстраполяцией и менялись от
перезагрузки к перезагрузке.

Здесь план и телеметрия берутся из датасета ТЗ как есть: traffic.csv даёт
реальные кадры (координаты, скорость, курс), schedule.csv — реальный план
(остановки, геометрия, факт). Машины идут по настоящим дорогам в настоящем
порядке, линии стабильны между перезагрузками, и cur_dev_s — настоящая
план-фактическая задержка, а не сценарная +150 с.

Ось времени. Кадры NDTP несут uint32-epoch, план гейтвей трактует наивное
время как UTC (docs/tz-conformance.md). Мы проигрываем «часы датасета»
относительно якоря: row_clock → real_epoch = t0 + (row_sec − anchor_sec)/K,
где K — --speed. K=1: тот же ход дня, что в датасете, но начатый «сейчас».
План проходит то же преобразование — машина и расписание едут синхронно при
любом K.

Только stdlib. Запуск (порт гейтвея :9201):

    python3 scripts/dataset_feed.py --plan-out plan.csv --binding-out binding.csv \
        --generate-only                       # файлы до serve
    python3 scripts/dataset_feed.py --plan-out plan.csv --binding-out binding.csv
"""
from __future__ import annotations

import argparse
import calendar
import csv
import math
import socket
import struct
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ndtp_feed as feed  # каркас кадров, CRC, handshake, PLAN_HEADER — отсюда же

NPL_SIZE, NPH_SIZE = feed.NPL_SIZE, feed.NPH_SIZE

EXTRA_DOP = 0x80 | 0x20 | 0x40  # Valid | LatNorth | LonEast

# Смещения полей Nav00 в payload ячейки (backend/internal/ndtp/cells.go:207).
NAV00_TS, NAV00_LON, NAV00_LAT = 0, 4, 8
NAV00_EXTRA, NAV00_BAT = 12, 13
NAV00_SPEED_AVG, NAV00_SPEED_MAX, NAV00_COURSE = 14, 16, 18
NAV00_TRACK, NAV00_ALT = 20, 22
NAV00_NSAT, NAV00_PDOP = 24, 25


def sec_of_day(dt: datetime) -> float:
    return dt.hour * 3600 + dt.minute * 60 + dt.second + dt.microsecond / 1e6


def parse_clock(value: str) -> float:
    h, _, m = value.partition(":")
    s, _, frac = (m or "0").partition(".")
    return int(h) * 3600 + int(s) * 60 + float(f"{frac or 0}")


def load_units(traffic_path: Path, schedule_path: Path, want: int,
               ) -> "dict[int, tuple[int, list[dict]]]":
    """Юниты, у которых есть и телеметрия с координатами, и строки плана.

    Отбор — по числу координатных строк (богаче трек), tr_id юнита обязан
    встретиться в schedule.csv: иначе машине нечем строить цель и прогноз.
    """
    tr_by_unit: "dict[int, set[str]]" = {}
    coords: "dict[int, list[dict]]" = {}
    with traffic_path.open(newline="", encoding="utf-8-sig") as fh:
        for row in csv.DictReader(fh):
            try:
                lon = float(row["lon"]); lat = float(row["lat"])
                ev = datetime.strptime(row["event_time"][:19], "%Y-%m-%d %H:%M:%S")
            except (ValueError, TypeError, KeyError):
                continue
            u = int(row["unit_id"])
            tr_by_unit.setdefault(u, set()).add(row["tr_id"])
            rec = coords.setdefault(u, [])
            rec.append({
                "sec": sec_of_day(ev), "lon": lon, "lat": lat,
                "speed": float(row["speed"] or 0),
                "course": float(row["heading"] or 0),
                "valid": str(row["location_valid"]).strip().lower() in ("true", "1"),
                "tr": row["tr_id"],
            })
    trs_in_plan: "dict[str, int]" = {}
    with schedule_path.open(newline="", encoding="utf-8-sig") as fh:
        for row in csv.DictReader(fh):
            trs_in_plan[row["tr_id"]] = trs_in_plan.get(row["tr_id"], 0) + 1
    eligible = []
    for u, rows in coords.items():
        # Только настоящие маршруты датасета: синтетические test-клоны
        # идентифицируются tr==unit и диапазоном 900000x (те же геометрии,
        # что у живых tr, надуманными id).
        def real_tr(t: str) -> bool:
            return t != str(u) and not t.startswith("9000") and trs_in_plan.get(t, 0) >= 50
        shared = sorted(t for t in tr_by_unit[u] if real_tr(t))
        if len(rows) >= 200 and shared:
            eligible.append((len(rows), u, shared[0]))
    eligible.sort(key=lambda t: (-t[0], t[1]))
    return {u: (int(tr), coords[u]) for _, u, tr in eligible[:want]}


def axis_time(sec: float, anchor: float, k: float, t0: float) -> float:
    """Часовая секунда строки датасета → unix-эпоха оси прогона."""
    return t0 + (sec - anchor) / k


def patch_nav00(frame: "bytes | bytearray", ts: int, req: int, peer: int,
                lon_e7: int, lat_e7: int, speed: int, course: int) -> bytes:
    """Кадры датасета: template-байты golden + наши Nav00/peer/req/CRC."""
    f = bytearray(frame)
    f[11] = peer & 0xFF  # NPL.PeerAddress
    for off in feed.nav00_offsets(f):
        struct.pack_into("<I", f, off + NAV00_TS, ts & 0xFFFFFFFF)
        struct.pack_into("<I", f, off + NAV00_LON, lon_e7 & 0xFFFFFFFF)
        struct.pack_into("<I", f, off + NAV00_LAT, lat_e7 & 0xFFFFFFFF)
        f[off + NAV00_EXTRA] = EXTRA_DOP
        f[off + NAV00_BAT] = 126  # 2520 мВ — штатное питание
        struct.pack_into("<H", f, off + NAV00_SPEED_AVG, min(speed, 65535))
        struct.pack_into("<H", f, off + NAV00_SPEED_MAX, min(speed, 65535))
        struct.pack_into("<H", f, off + NAV00_COURSE, max(0, min(int(course), 360)) % 65536)
        struct.pack_into("<H", f, off + NAV00_TRACK, 0)
        struct.pack_into("<H", f, off + NAV00_ALT, 150)
        f[off + NAV00_NSAT] = 10
        f[off + NAV00_PDOP] = 2
    body_len = struct.unpack_from("<H", f, 2)[0]
    rest = bytes(f[NPL_SIZE:NPL_SIZE + body_len])
    f[6:8] = struct.pack("<H", feed.swap16(feed.crc16_modbus(rest)))
    struct.pack_into("<H", f, 13, req & 0xFFFF)
    return bytes(f)


def write_real_plan(plan_path: Path, binding_path: Path,
                    units: "dict[int, tuple[int, list[dict]]]",
                    schedule_path: Path, anchor: float, k: float, t0: float) -> None:
    """План — строки schedule.csv выбранных tr, приведённые к оси прогона.

    Формат — как в датасете (гейтвей читает по заголовкам), время — то же
    преобразование, что у кадров: факт и план остаются синхронны с машиной
    при любом --speed.
    """
    trs = {str(tr) for tr, _ in units.values()}
    stamp = "%Y-%m-%d %H:%M:%S"
    rows = [feed.PLAN_HEADER]
    used = 0
    with schedule_path.open(newline="", encoding="utf-8-sig") as fh:
        for row in csv.DictReader(fh):
            if row["tr_id"] not in trs:
                continue
            try:
                plan = datetime.strptime(row["time_begin"][:19], "%Y-%m-%d %H:%M:%S")
                e = axis_time(sec_of_day(plan), anchor, k, t0)
                at = datetime.fromtimestamp(e, timezone.utc).replace(tzinfo=None).strftime(stamp)
                fact = ""
                if row.get("time_fact_begin"):
                    try:
                        fac = datetime.strptime(row["time_fact_begin"][:19], "%Y-%m-%d %H:%M:%S")
                        ef = axis_time(sec_of_day(fac), anchor, k, t0)
                        fact = datetime.fromtimestamp(ef, timezone.utc).replace(tzinfo=None).strftime(stamp)
                    except ValueError:
                        fact = ""
            except (ValueError, KeyError):
                continue
            rows.append(",".join([
                row["tt_action_item_id"], at, at.split(" ")[0],
                row.get("manual_fill", "False"), row["tr_id"],
                row["geom"], (row.get("building_address") or "остановка").replace(",", ";"),
                fact,
            ]))
            used += 1
    plan_path.write_text("\n".join(rows) + "\n", encoding="utf-8")
    binding = ["tr_id,unit_id"]
    for u, (tr, _) in sorted(units.items(), key=lambda kv: kv[1][0]):
        binding.append(f"{tr},{u}")
    binding_path.write_text("\n".join(binding) + "\n", encoding="utf-8")
    print(f"план: {plan_path} ({used} остановок реального расписания), привязка: {binding_path}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--dataset-dir", default="train", help="каталог с traffic.csv и schedule.csv")
    ap.add_argument("--units", type=int, default=3, help="сколько юнитов взять (топ по строкам с координатами)")
    ap.add_argument("--speed", type=float, default=1.0, help="скорость прогона: K секунд датасета на секунду реального времени")
    ap.add_argument("--start", default="", help="часовой якорь HH:MM:SS (по умолчанию — первая координатная строка выбранных юнитов)")
    ap.add_argument("--host", default="127.0.1.1")
    ap.add_argument("--port", type=int, default=9201)
    ap.add_argument("--plan-out", type=Path, default=None)
    ap.add_argument("--binding-out", type=Path, default=None)
    ap.add_argument("--generate-only", action="store_true", help="только plan/binding, без потока")
    ap.add_argument("--template", type=Path, default=feed.GOLDEN, help="источник байтового template кадра")
    args = ap.parse_args()

    ds = Path(args.dataset_dir)
    units = load_units(ds / "traffic.csv", ds / "schedule.csv", args.units)
    if not units:
        print("нет юнитов, у которых есть и координаты, и строки плана", file=sys.stderr)
        return 1
    anchor = parse_clock(args.start) if args.start else sec_of_day(datetime.now())
    t0 = time.time()
    for u, (tr, rows) in sorted(units.items(), key=lambda kv: kv[1][0]):
        first = min(r["sec"] for r in rows)
        print(f"  unit {u} → tr {tr}: строк {len(rows)}, начало {first/3600:.1f} ч")

    if args.plan_out and args.binding_out:
        write_real_plan(args.plan_out, args.binding_out, units, ds / "schedule.csv",
                        anchor, args.speed, t0)
    if args.generate_only:
        return 0

    frames = feed.split_frames(args.template.read_bytes())
    nav = [f for f in frames if feed.nav00_offsets(f)]
    if not nav:
        print(f"в {args.template} нет Nav00-кадров", file=sys.stderr)
        return 1
    template = nav[0]

    # По соединению на юнит — как у эмулятора: unit_id в handshake и PeerAddress.
    streams = []
    for u, (tr, rows) in units.items():
        rows = sorted((r for r in rows if r["valid"]), key=lambda r: r["sec"])
        if not rows:
            print(f"unit {u}: нет строк с location_valid — пропускаем")
            continue
        # Курсор — на 120 с до якоря: короткий хвост прошлого даёт конвейеру
        # окно точек сразу, а не через десять минут ожидания.
        seed_from = anchor - 120.0
        i = 0
        while i < len(rows) and rows[i]["sec"] < seed_from:
            i += 1
        streams.append({"unit": u, "rows": rows, "i": i, "sock": None, "req": 0})
    if not streams:
        print("нет валидных строк ни у одного юнита", file=sys.stderr)
        return 1

    def connect(s) -> bool:
        try:
            sock = socket.create_connection((args.host, args.port), timeout=5)
        except OSError as e:
            return False
        sock.settimeout(0.2)
        sock.sendall(feed.build_handshake(s["unit"]))
        try:
            sock.recv(4096)
        except socket.timeout:
            pass
        s["sock"] = sock
        return True

    for s in streams:
        if not connect(s):
            print(f"unit {s['unit']}: :{args.host}:{args.port} не отвечает — повтор через 2 с")
    print(f"прогон: {len(streams)} юнитов, K={args.speed}, якорь {anchor/3600:.2f} ч, цель — {args.host}:{args.port}")

    sent_total = 0
    try:
        while True:
            now = time.time()
            for s in streams:
                if not s["sock"] and not connect(s):
                    continue
                rows = s["rows"]
                # всё, что по оси прогона уже «наступило»; догон без опережения часов
                budget = 500
                while s["i"] < len(rows) and axis_time(rows[s["i"]]["sec"], anchor, args.speed, t0) <= now and budget:
                    r = rows[s["i"]]
                    e = int(axis_time(r["sec"], anchor, args.speed, t0))
                    s["req"] += 1
                    frame = patch_nav00(template, e, s["req"], s["unit"],
                                        round(r["lon"] * 1e7), round(r["lat"] * 1e7),
                                        max(0, int(r["speed"])), r["course"])
                    try:
                        s["sock"].sendall(frame)
                    except (BrokenPipeError, ConnectionResetError, OSError):
                        s["sock"].close(); s["sock"] = None
                        break
                    s["i"] += 1
                    budget -= 1
                    sent_total += 1
                    if budget == 0:
                        break
                if s["sock"]:
                    try:
                        s["sock"].recv(65536)
                    except (socket.timeout, BlockingIOError):
                        pass
            if all(s["i"] >= len(s["rows"]) for s in streams):
                print(f"день проигран ({sent_total} кадров) — фид завершается")
                return 0
            time.sleep(0.25)
    except KeyboardInterrupt:
        print(f"остановлено, отправлено кадров: {sent_total}")
        return 0
    finally:
        for s in streams:
            if s["sock"]:
                s["sock"].close()


if __name__ == "__main__":
    sys.exit(main())
