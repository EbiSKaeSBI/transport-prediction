#!/usr/bin/env bash
# Эмулятор NDTP в пейне Zellij: поднимает JAR, заливает конфиг юнитов
# (ids из train/traffic.csv) и показывает живой лог.
set -u
cd "$(dirname "$0")/.."
if ! python3 scripts/emu_native.py serve; then
    echo
    echo "Эмулятор не поднят: поместите ndtp-telemetry-emulator.tar в корень"
    echo "репозитория и выполните 'make emu-extract'."
    exec bash -l
fi
python3 scripts/emu_native.py config --unit 664030:3000 --unit 794446:3000 || true
echo "--- эмулятор работает, лог .cache/ndtp-emu/emu.log (Ctrl-^ не убивает процесс) ---"
exec tail -n +1 -F .cache/ndtp-emu/emu.log
