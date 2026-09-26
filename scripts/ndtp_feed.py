#!/usr/bin/env python3
"""Фидер NDTP для живого E2E дашборда (GitLab #36, этап 5).

Гонит на transportctl serve (:9201) НАСТОЯЩИЕ кадры протокола NDTP,
пересобранные из golden-потока backend/internal/ndtp/testdata/golden/
packets.bin: байты ячеек переиспользуются дословно, меняется только
метка времени Nav00 (сдвиг на «сейчас») — сериализация кадра, CRC-16/Modbus
со свапом байтов в NPL и handshake (CONN_REQUEST, unit_id = PeerAddress
тела) воспроизводят формат из backend/internal/ndtp (frame.go, crc.go,
handshake.go) один в один.

Дополнительно --plan-out/--binding-out пишет синтетический план-график и
привязку под текущие часы (той же формы, что writePlanCSV в e2e-тестах Go):
первые остановки с time_fact_begin = план +150 с, чтобы конвейер посчитал
cur_dev_s >= 120 и лента породила красный инцидент на живых данных.

Только stdlib. Запуск:

    python3 scripts/ndtp_feed.py --plan-out /tmp/live_plan.csv \
        --binding-out /tmp/live_binding.csv --generate-only   # файлы до serve
    python3 scripts/ndtp_feed.py --plan-out ... --binding-out ... --every 1

Фидер не завершается: шлёт по пакету в --every секунд, пока его не убьют
(пауза в 5 с и повторный запуск — штатная проверка реконнекта).
"""
from __future__ import annotations

import argparse
import socket
import struct
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GOLDEN = ROOT / "backend" / "internal" / "ndtp" / "testdata" / "golden" / "packets.bin"

NPL_SIZE, NPH_SIZE = 15, 10
CELL_NAV00, NAV00_SIZE = 0, 26
NPH_TYPE_CONN, NPH_TYPE_REALTIME = 100, 101
SERVICE_GENERIC, SERVICE_NAVDATA = 0, 1


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


def write_plan_and_binding(plan_path: Path, binding_path: Path, unit_id: int, now: datetime) -> None:
    """Синтетический план под живые часы: формат — как writePlanCSV в e2e Go.

    time_begin без зоны, разбирается гейтвеем как UTC — пишем UTC-моменты.
    Прошедшие остановки (T-20..T-5 мин) с фактом +150 с: накопленное
    отставание cur_dev_s = 150 >= порога 120 → красный риск и инцидент.
    Будущие (+11..+15 мин) — цели горизонта (T+10; T+15].
    """
    stamp = "%Y-%m-%d %H:%M:%S"
    rows = ["tt_action_item_id,time_begin,order_date,manual_fill,tr_id,geom,building_address,time_fact_begin"]
    base = now.replace(microsecond=0)
    for i, off_min in enumerate([-20, -15, -10, -5, 11, 12, 13, 14, 15]):
        at = base + timedelta(minutes=off_min)
        lon = 37.600 + 0.007 * i
        lat = 55.800
        line = f"{100 + i},{at.astimezone(timezone_utc).strftime(stamp)},{at.astimezone(timezone_utc).strftime(stamp)},False,1,POINT ({lon:.6f} {lat:.6f}),стоп-{i}"
        if off_min < 0:
            fact = at + timedelta(seconds=150)
            line += f",{fact.astimezone(timezone_utc).strftime(stamp)}"
        else:
            line += ","
        rows.append(line)
    plan_path.write_text("\n".join(rows) + "\n", encoding="utf-8")
    binding_path.write_text(f"tr_id,unit_id\n1,{unit_id}\n", encoding="utf-8")


timezone_utc = timezone.utc


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=9201)
    ap.add_argument("--unit", type=int, default=1166336, help="unit_id устройства (PeerAddress handshake)")
    ap.add_argument("--every", type=float, default=1.0, help="шаг отправки realtime-кадров, с")
    ap.add_argument("--seed-window", type=float, default=60.0,
                    help="как глубоко в прошлое поставить предзагрузку точек")
    ap.add_argument("--plan-out", type=Path, default=None)
    ap.add_argument("--binding-out", type=Path, default=None)
    ap.add_argument("--generate-only", action="store_true", help="только файлы плана/привязки")
    args = ap.parse_args()

    if args.plan_out and args.binding_out:
        write_plan_and_binding(args.plan_out, args.binding_out, args.unit, datetime.now(timezone_utc))
        print(f"план: {args.plan_out}, привязка: {args.binding_out}")
        if args.generate_only:
            return 0

    frames = split_frames(GOLDEN.read_bytes())
    nav_frames = [f for f in frames if nav00_offsets(f)]
    if not nav_frames:
        print("в golden-потоке нет Nav00-кадров", file=sys.stderr)
        return 1
    # базовая метка golden-потока — от неё считаем честный сдвиг всей серии
    base_ts = struct.unpack_from("<I", nav_frames[0], NPL_SIZE + NPH_SIZE + 2)[0]
    print(f"golden-кадров: {len(frames)}, с Nav00: {len(nav_frames)}, базовая метка: {base_ts}")

    sock = socket.create_connection((args.host, args.port), timeout=5)
    sock.settimeout(0.2)
    sock.sendall(build_handshake(args.unit))
    try:
        sock.recv(4096)  # ответ сервера на handshake (если есть) — игнорируем
    except socket.timeout:
        pass
    print(f"handshake отправлен на {args.host}:{args.port}, unit_id={args.unit}")

    sent = 0
    req = 0
    cycle = 4125  # шаг серии golden-кадров по долготе, 1e-7°
    t_feed = time.time()
    try:
        while True:
            now_ts = int(time.time())
            if sent == 0:
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
            else:
                idx = sent % len(nav_frames)
                lap = sent // len(nav_frames)
                req += 1
                sock.sendall(patch_frame(nav_frames[idx], now_ts, req,
                                         lon_shift_e7=lap * cycle))
                sent += 1
            try:
                sock.recv(65536)  # дренируем входящие (ack/result), сокет не блокируется
            except (socket.timeout, BlockingIOError):
                pass
            time.sleep(args.every)
    except KeyboardInterrupt:
        pass
    finally:
        sock.close()
    print(f"отправлено realtime-кадров: {sent} за {time.time() - t_feed:.1f} с")
    return 0


if __name__ == "__main__":
    sys.exit(main())
