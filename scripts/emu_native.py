"""Жизненный цикл эмулятора NDTP без Docker.

Образ ndtp-telemetry-emulator:1.0 — это OCI с Ubuntu 26.04 и Temurin 17 внутри.
Docker здесь недоступен, но образ amd64, а JRE требует только libc/libdl/
libpthread, которые есть на хосте. Поэтому JRE и app.jar достаются из слоёв
напрямую, и эмулятор запускается как обычный процесс.

Команды:
    python3 scripts/emu_native.py extract          достать JRE и app.jar
    python3 scripts/emu_native.py check            проверить, что java жива
    python3 scripts/emu_native.py serve            запустить в фоне, ждать API
    python3 scripts/emu_native.py config           залить конфиг на :18080
    python3 scripts/emu_native.py stop             остановить

Каталог .cache/ndtp-emu/ в .gitignore.
"""

from __future__ import annotations

import argparse
import gzip
import io
import json
import os
import shutil
import signal
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
IMAGE = ROOT / "ndtp-telemetry-emulator.tar"
DEST = ROOT / ".cache" / "ndtp-emu"
JRE_PREFIX = "opt/java/openjdk"
APP_MEMBER = "app/app.jar"
JAVA = DEST / JRE_PREFIX / "bin" / "java"
JAR = DEST / "app" / "app.jar"
PIDFILE = DEST / "emu.pid"
LOGFILE = DEST / "emu.log"
API = "http://127.0.0.1:18080"


def extract_prefix(tar: tarfile.TarFile, prefix: str, dest: Path) -> int:
    written = 0
    for member in tar.getmembers():
        if not member.name.startswith(prefix + "/"):
            continue
        relative = member.name
        if ".." in Path(relative).parts:
            continue
        target = dest / relative
        if member.isdir():
            target.mkdir(parents=True, exist_ok=True)
        elif member.issym():
            target.parent.mkdir(parents=True, exist_ok=True)
            if target.is_symlink() or target.exists():
                target.unlink()
            os.symlink(member.linkname, target)
        elif member.isfile():
            target.parent.mkdir(parents=True, exist_ok=True)
            source = tar.extractfile(member)
            if source is None:
                continue
            with source, target.open("wb") as handle:
                shutil.copyfileobj(source, handle)
            os.chmod(target, member.mode & 0o777)
            written += 1
    return written


def command_extract() -> int:
    if JAVA.exists() and JAR.exists():
        print(f"Уже распаковано в {DEST.relative_to(ROOT)}")
        return 0
    if not IMAGE.exists():
        print(f"Нет образа {IMAGE}", file=sys.stderr)
        return 1

    DEST.mkdir(parents=True, exist_ok=True)
    print(f"Читаю {IMAGE.name} ({IMAGE.stat().st_size / 1048576:.0f} МБ)")

    found_jre = found_app = False
    with tarfile.open(IMAGE) as outer:
        for member in outer.getmembers():
            if not member.isfile() or member.size < 1_000_000:
                continue
            handle = outer.extractfile(member)
            if handle is None or handle.read(2) != b"\x1f\x8b":
                continue
            handle.seek(0)
            data = gzip.decompress(handle.read())
            with tarfile.open(fileobj=io.BytesIO(data)) as layer:
                names = layer.getnames()
                if not found_jre and f"{JRE_PREFIX}/bin/java" in names:
                    count = extract_prefix(layer, JRE_PREFIX, DEST)
                    print(f"  JRE: {count} файлов")
                    found_jre = True
                if not found_app and APP_MEMBER in names:
                    target = JAR
                    target.parent.mkdir(parents=True, exist_ok=True)
                    source = layer.extractfile(APP_MEMBER)
                    if source is not None:
                        with source, target.open("wb") as out:
                            shutil.copyfileobj(source, out)
                        print(f"  app.jar: {target.stat().st_size / 1048576:.1f} МБ")
                        found_app = True
            if found_jre and found_app:
                break

    if not (found_jre and found_app):
        print(
            f"Не удалось найти оба компонента (jre={found_jre}, app={found_app})",
            file=sys.stderr,
        )
        return 1
    return 0


def command_check() -> int:
    if not JAVA.exists():
        print("JRE не распакован, сначала extract", file=sys.stderr)
        return 1
    result = subprocess.run(
        [str(JAVA), "-version"], capture_output=True, text=True, check=False
    )
    version = (result.stderr or result.stdout).strip().splitlines()
    print(" ".join(version) if version else "java не ответила")
    if not JAR.exists():
        print("app.jar отсутствует", file=sys.stderr)
        return 1
    print(f"app.jar {JAR.stat().st_size / 1048576:.1f} МБ")
    return result.returncode


def api_get(path: str, timeout: float = 3.0) -> object:
    with urllib.request.urlopen(f"{API}{path}", timeout=timeout) as response:
        return json.loads(response.read())


def command_serve(args: argparse.Namespace) -> int:
    if PIDFILE.exists() and PIDFILE.read_text().strip():
        print(f"Эмулятор уже запущен, pid {PIDFILE.read_text().strip()}")
        return 0
    if not JAVA.exists() or not JAR.exists():
        print("Сначала extract", file=sys.stderr)
        return 1

    DEST.mkdir(parents=True, exist_ok=True)
    log = LOGFILE.open("ab")
    process = subprocess.Popen(
        [str(JAVA), "-Djava.awt.headless=true", "-jar", str(JAR)],
        stdout=log,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    PIDFILE.write_text(str(process.pid))

    deadline = time.time() + args.wait
    while time.time() < deadline:
        if process.poll() is not None:
            print(f"Эмулятор упал, код {process.returncode}. Лог:", file=sys.stderr)
            print(LOGFILE.read_text(errors="replace")[-2000:], file=sys.stderr)
            PIDFILE.unlink(missing_ok=True)
            return 1
        try:
            api_get("/api/cells", timeout=1.0)
            print(f"Эмулятор поднят, pid {process.pid}, API {API}")
            return 0
        except (urllib.error.URLError, OSError, json.JSONDecodeError):
            time.sleep(0.5)
    print(f"API не поднялся за {args.wait} с. Лог:", file=sys.stderr)
    print(LOGFILE.read_text(errors="replace")[-2000:], file=sys.stderr)
    return 1


def command_stop() -> int:
    if not PIDFILE.exists():
        print("Эмулятор не запущен")
        return 0
    pid = int(PIDFILE.read_text().strip())
    try:
        os.killpg(os.getpgid(pid), signal.SIGTERM)
    except (ProcessLookupError, PermissionError):
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    PIDFILE.unlink(missing_ok=True)
    print(f"Остановлен pid {pid}")
    return 0


def command_config(args: argparse.Namespace) -> int:
    units = []
    for raw in args.unit:
        unit_id, interval = raw.split(":", 1)
        units.append(
            {
                "unitId": int(unit_id),
                "intervalMs": int(interval),
                "autoGenerate": not args.no_autogenerate,
                "cells": json.loads(args.cells) if args.cells else [],
            }
        )
    payload = json.dumps(
        {"targetHost": args.host, "targetPort": args.port, "units": units}
    ).encode()
    request = urllib.request.Request(
        f"{API}/api/config",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            body = response.read()
    except urllib.error.HTTPError as error:
        print(
            f"HTTP {error.code}: {error.read().decode(errors='replace')}",
            file=sys.stderr,
        )
        return 1
    print(f"Конфиг принят: {len(units)} юнитов -> {args.host}:{args.port}")
    print(body.decode(errors="replace")[:200] if body else "")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("extract")
    sub.add_parser("check")
    serve = sub.add_parser("serve")
    serve.add_argument("--wait", type=int, default=90)
    sub.add_parser("stop")
    config = sub.add_parser("config")
    config.add_argument("--host", default="127.0.0.1")
    config.add_argument("--port", type=int, default=9201)
    config.add_argument(
        "--unit", action="append", required=True, metavar="ID:INTERVAL_MS"
    )
    config.add_argument(
        "--cells",
        default="",
        help='JSON-массив ячеек вида \'[{"type":"G6CellNav00"}]\'',
    )
    config.add_argument(
        "--no-autogenerate",
        action="store_true",
        help="не добавлять автогенерируемые ячейки, использовать только --cells",
    )
    args = parser.parse_args()

    if args.command == "extract":
        return command_extract()
    if args.command == "check":
        return command_check()
    if args.command == "serve":
        return command_serve(args)
    if args.command == "stop":
        return command_stop()
    if args.command == "config":
        return command_config(args)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
