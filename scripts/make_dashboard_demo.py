#!/usr/bin/env python3
"""Генератор демо-потока для дашборда (критерий 4).

Собирает в dashboard/public/demo/ три артефакта:

  routes.geojson      полилинии маршрутов и остановки (кластеризация Jaccard
                      по множествам остановок — того же класса, что описан
                      в docs/architecture.md §4.6);
  stream.ndjson       события потока {type: meta|vehicle|frame|incident|model},
                      упорядоченные по времени — тот же контракт, что по WS
                      будет отдавать gateway (этап 4);
  frames.jsonl        кадры признаков — прозрачно копируются с выхода
                      `transportctl features --frames`, то есть признаки
                      считает Go, а не этот скрипт.

Инциденты до подключения модели считаются правилом-фолбэком
`predicted = cur_dev_s` (ADR 0003, порог 120 с из docs/architecture.md §4.1).
Скрипт только stdlib; запуск:

    python3 scripts/make_dashboard_demo.py [--every 60]

"""

from __future__ import annotations

import argparse
import csv
import json
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEMO_DIR = ROOT / "dashboard" / "public" / "demo"
VALIDATE = ROOT / "validate"
MSK = timezone(timedelta(hours=3))

INCIDENT_THRESHOLD_S = 120.0  # §4.1: predicted >= 120 ∧ горизонт в окне
DAY_START = datetime(2026, 1, 6, 0, 0, tzinfo=MSK)
DAY_END = datetime(2026, 1, 7, 0, 0, tzinfo=MSK)


def detect_delimiter(path: Path) -> str:
    head = path.open(encoding="utf-8", errors="replace").readline()
    return ";" if head.count(";") > head.count(",") else ","


def read_table(path: Path) -> list[dict[str, str]]:
    delim = detect_delimiter(path)
    with path.open(encoding="utf-8", newline="", errors="replace") as fh:
        return list(csv.DictReader(fh, delimiter=delim))


def parse_time(value: str) -> datetime | None:
    value = value.strip()
    if not value:
        return None
    for fmt in ("%Y-%m-%d %H:%M:%S.%f", "%Y-%m-%d %H:%M:%S", "%Y-%m-%dT%H:%M:%S.%fZ", "%Y-%m-%dT%H:%M:%SZ"):
        try:
            dt = datetime.strptime(value, fmt)
        except ValueError:
            continue
        # Всё в датасете — наивные стенные часы МСК. Go в поле t кадра
        # дописывает «Z» к тому же наивному времени без конвертации
        # (проверено: epoch из sample_id == timegm(naive) для 1534/1534
        # кадров), поэтому читаем Z-строки в ту же стенную ось.
        return dt.replace(tzinfo=MSK)
    return None


def parse_point(geom: str) -> tuple[float, float] | None:
    # "POINT (37.43070705 55.8040083)" → (lon, lat)
    body = geom.partition("(")[2].partition(")")[0].strip()
    parts = body.split()
    if len(parts) != 2:
        return None
    try:
        return float(parts[0]), float(parts[1])
    except ValueError:
        return None


def load_schedule() -> dict[int, list[dict]]:
    """tr_id → упорядоченные по плановому времени остановки рейса."""
    by_tr: dict[int, list[dict]] = {}
    for row in read_table(VALIDATE / "schedule_plan.csv"):
        geom = parse_point(row.get("geom", ""))
        t = parse_time(row.get("time_begin", ""))
        if geom is None or t is None or t < DAY_START or t > DAY_END:
            continue
        stop = row.get("tt_action_item_id", "")
        entry = {
            "stop_id": int(stop) if stop.isdigit() else 0,
            "lon": geom[0], "lat": geom[1],
            "time_begin": t.isoformat(),
            "name": row.get("building_address", "").strip('"'),
            "manual_fill": row.get("manual_fill", "").lower() == "true",
        }
        try:
            tr_id = int(row["tr_id"])
        except (KeyError, ValueError):
            continue
        by_tr.setdefault(tr_id, []).append(entry)
    for stops in by_tr.values():
        stops.sort(key=lambda s: s["time_begin"])
    return by_tr


def cluster_routes(by_tr: dict[int, list[dict]]) -> dict[int, str]:
    """Жадная кластеризация tr_id по Jaccard множеств остановок."""
    sets = {tr: {s["stop_id"] for s in stops} for tr, stops in by_tr.items()}
    assigned: dict[int, str] = {}
    route_idx = 0
    for tr, own in sets.items():
        if tr in assigned or not own:
            continue
        route = f"R{route_idx}"
        route_members = [own]
        assigned[tr] = route
        for other, own2 in sets.items():
            if other in assigned or not own2:
                continue
            inter = len(own & own2)
            union = len(own | own2)
            if union and inter / union >= 0.5:
                assigned[other] = route
                route_members.append(own2)
        route_idx += 1
    return assigned


def build_routes(by_tr, tr2route) -> dict:
    """routes.geojson: по одной полилинии на tr_id (маршрут — цвет по route id)."""
    features: list[dict] = []
    stops_seen: dict[int, tuple[float, float, str, str]] = {}
    for tr, stops in sorted(by_tr.items()):
        route = tr2route.get(tr, f"X{tr}")
        coords = [[s["lon"], s["lat"]] for s in stops]
        if not coords:
            continue
        features.append({
            "type": "Feature",
            "properties": {"kind": "route", "tr_id": tr, "route": route,
                           "stops": len(coords)},
            "geometry": {"type": "LineString", "coordinates": coords},
        })
        for s in stops:
            if s["stop_id"] and s["stop_id"] not in stops_seen:
                stops_seen[s["stop_id"]] = (s["lon"], s["lat"], route, s["name"])
    for stop_id, (lon, lat, route, name) in sorted(stops_seen.items()):
        features.append({
            "type": "Feature",
            "properties": {"kind": "stop", "stop_id": stop_id, "route": route,
                           "name": name},
            "geometry": {"type": "Point", "coordinates": [lon, lat]},
        })
    return {"type": "FeatureCollection",
            "properties": {"routes": len(set(tr2route.values()))},
            "features": features}


def load_frames() -> list[dict]:
    path = DEMO_DIR / "frames.jsonl"
    if not path.exists():
        sys.exit("нет demo/frames.jsonl — сначала:g "
                 "go run ./cmd/transportctl features --plan ../validate/schedule_plan.csv "
                 "--binding ../validate/traffic.csv --input ../validate/traffic.csv "
                 "--tick 5m --frames --out ../dashboard/public/demo/frames.jsonl")
    out = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        f = json.loads(line)
        t = parse_time(f.get("t", ""))
        # кадры вне суточного окна в поток не попадают — иначе нарушается
        # обещание «события упорядочены в рамках окна» из docs/dashboard.md
        if t is not None and DAY_START <= t < DAY_END:
            out.append(f)
    return out


def incident_reason(values: dict) -> str:
    # Не values.get(key, default): Go пишет пропуски явным JSON null,
    # ключ тогда присутствует, и None ломает сравнения ниже.
    def num(key: str, default: float) -> float:
        v = values.get(key)
        return default if v is None else float(v)

    dwell = num("dwell_current_s", 0.0)
    speed = num("speed_current", 99.0)
    headway = num("headway_s", 999.0)
    if dwell >= 60:
        return "длительный простой на остановке"
    if dwell >= 20:
        return "увеличенное время стоянки"
    if speed <= 5:
        return "низкая скорость движения"
    if headway < 120:
        return "малый интервал, эффект «паровозика»"
    return "накопленное отставание без текущего простоя"


def vehicle_events(every_s: int):
    """Прореженная телеметрия validate → события vehicle (только валидные координаты).

    Строки в traffic.csv идут не по времени (в пределах машины перемешаны),
    поэтому прореживание — после сортировки по event_time внутри tr_id.
    """
    tracks: dict[int, list[tuple[datetime, float, float, float, float]]] = {}
    for row in read_table(VALIDATE / "traffic.csv"):
        if row.get("location_valid", "").strip().lower() not in ("true", "1"):
            continue
        try:
            tr_id = int(row["tr_id"])
            lon, lat = float(row["lon"]), float(row["lat"])
        except (KeyError, ValueError):
            continue
        t = parse_time(row.get("event_time", ""))
        if t is None or t < DAY_START or t > DAY_END:
            continue
        try:
            speed = float(row.get("speed") or 0.0)
            heading = float(row.get("heading") or 0.0)
        except ValueError:
            speed = heading = 0.0
        tracks.setdefault(tr_id, []).append((t, lon, lat, speed, heading))
    events = []
    for tr_id, points in tracks.items():
        points.sort(key=lambda p: p[0])
        prev: datetime | None = None
        for t, lon, lat, speed, heading in points:
            if prev and (t - prev).total_seconds() < every_s:
                continue
            prev = t
            events.append({
                "type": "vehicle", "ts": int(t.timestamp()), "tr_id": tr_id,
                "lon": round(lon, 5), "lat": round(lat, 5),
                "speed": round(speed, 1), "heading": heading,
            })
    return events


def load_points() -> list[dict]:
    """Официальные 151 прогнозная точка validate — законная подсказка cur_dev_s."""
    out = []
    for row in read_table(VALIDATE / "points.csv"):
        t = parse_time(row.get("T", ""))
        tgt = parse_time(row.get("target_time_begin", ""))
        try:
            tr_id = int(row["tr_id"])
            cur_dev = float(row["cur_dev_s"])
            stop = int(row["target_stop_id"])
        except (KeyError, ValueError):
            continue
        if t is None or tgt is None:
            continue
        out.append({"sample_id": row["sample_id"], "tr_id": tr_id,
                    "ts": int(t.timestamp()), "target_stop_id": stop,
                    "horizon_s": (tgt - t).total_seconds(), "cur_dev_s": cur_dev})
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--every", type=int, default=60, help="шаг прореживания телеметрии, сек")
    args = ap.parse_args()

    DEMO_DIR.mkdir(parents=True, exist_ok=True)
    by_tr = load_schedule()
    tr2route = cluster_routes(by_tr)
    routes = build_routes(by_tr, tr2route)
    (DEMO_DIR / "routes.geojson").write_text(
        json.dumps(routes, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")

    frames = load_frames()
    points = load_points()
    events: list[dict] = vehicle_events(args.every)

    # кадры Go-конвейера — визуализация горизонта и признаков; обогащаем их
    # подсказкой cur_dev_s из официальных точек (совпадение tr_id и T ±150 с)
    frame_ts = [(f, int(t.timestamp())) for f in frames if (t := parse_time(f["t"]))]
    point_frame: dict[str, dict] = {}
    for pt in points:
        cand = [(f, ts) for f, ts in frame_ts
                if f["tr_id"] == pt["tr_id"] and abs(ts - pt["ts"]) <= 150]
        if cand:
            best = min(cand, key=lambda x: abs(x[1] - pt["ts"]))[0]
            point_frame[pt["sample_id"]] = best
    for f, ts in frame_ts:
        cur_dev = next((p["cur_dev_s"] for p in points
                        if point_frame.get(p["sample_id"]) is f), None)
        events.append({
            "type": "frame", "ts": ts, "sample_id": f["sample_id"],
            "tr_id": f["tr_id"], "target_stop_id": f["target_stop_id"],
            "horizon_s": f["horizon_s"], "ambiguous": f.get("ambiguous", False),
            "cur_dev_s": cur_dev, "official": cur_dev is not None,
            # качество данных по features/v1.yaml (секция quality) идёт в
            # values — панель «карточка ТС» показывает все контрактные фичи
            "values": {**f["values"],
                       "staleness_s": f.get("staleness_s"),
                       "points_in_window": f.get("points_in_window"),
                       "lag_s": f.get("lag_s")},
        })
    # инциденты — по официальной сетке точек: правило-фолбэк predicted = cur_dev_s
    for pt in points:
        if pt["cur_dev_s"] >= INCIDENT_THRESHOLD_S and 600 < pt["horizon_s"] <= 900:
            nearest = point_frame.get(pt["sample_id"])
            values = nearest["values"] if nearest else {}
            events.append({
                "type": "incident", "ts": pt["ts"], "id": pt["sample_id"],
                "tr_id": pt["tr_id"], "target_stop_id": pt["target_stop_id"],
                "horizon_s": pt["horizon_s"],
                "predicted_delay_s": round(pt["cur_dev_s"], 1),
                "cur_dev_s": round(pt["cur_dev_s"], 1),
                "reason": incident_reason(values), "source": "rule:cur_dev",
            })

    events.append({
        "type": "model", "ts": int(DAY_START.timestamp()),
        "version": "v0 (cur_dev rule)", "model_version": "cur_dev_fallback",
        "trained_at": None,
        "mae_validate_s": 88.72, "mae_test_s": None,
        "note": "CatBoost (этап 3) ещё не подключён; это зафиксированный оракулом baseline cur_dev_s.",
    })
    events.sort(key=lambda e: e["ts"])

    meta = {
        "type": "meta", "ts": events[0]["ts"] if events else 0,
        "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "day": "2026-01-06", "window": [int(DAY_START.timestamp()), int(DAY_END.timestamp())],
        "vehicles": len({e["tr_id"] for e in events if e["type"] == "vehicle"}),
        "frames": len(frames),
        "incidents": sum(1 for e in events if e["type"] == "incident"),
        "routes": routes["properties"]["routes"],
        "every_s": args.every, "source_csv": "validate/",
    }
    with (DEMO_DIR / "stream.ndjson").open("w", encoding="utf-8") as fh:
        fh.write(json.dumps(meta, ensure_ascii=False) + "\n")
        for ev in events:
            fh.write(json.dumps(ev, ensure_ascii=False, separators=(",", ":")) + "\n")

    size = (DEMO_DIR / "stream.ndjson").stat().st_size / 1e6
    print(f"routes: {routes['properties']['routes']}, vehicles-events: "
          f"{sum(1 for e in events if e['type'] == 'vehicle')}, frames: {meta['frames']}, "
          f"incidents: {meta['incidents']}, stream: {size:.1f} MB → {DEMO_DIR}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
