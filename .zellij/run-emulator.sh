#!/usr/bin/env bash
# Эмулятор NDTP в пейне Zellij. В реальном контуре он по умолчанию НЕ
# нужен: машины гонит dataset_feed из train/traffic.csv. Эмулятор — источник
# случайных кривых (docs/Emulator-and-Telematic-Packets-Specification.md,
# «План стареет: эмулятор едет быстрее плана»), полезен для стресс-проверок:
# переподключения, off-route поведения, загрузки statestore.
# Включается: EMULATOR=1 make dev (или make emu-up + make emu-config).
set -u
cd "$(dirname "$0")/.."
if [ "${EMULATOR:-0}" != "1" ]; then
    echo "Эмулятор выключен: живой поток играет реальный датасет (dataset_feed)."
    echo "Нужен эмулятор — EMULATOR=1 make dev (или make emu-up + make emu-config)."
    exec bash -l
fi
if ! python3 scripts/emu_native.py serve; then
    echo
    echo "Эмулятор не поднят: поместите ndtp-telemetry-emulator.tar в корень"
    echo "репозитория и выполните 'make emu-extract'."
    exec bash -l
fi
python3 scripts/emu_native.py config --unit 664030:3000 --unit 794446:3000 || true
echo "--- эмулятор работает, лог .cache/ndtp-emu/emu.log (Ctrl-^ не убивает процесс) ---"
exec tail -n +1 -F .cache/ndtp-emu/emu.log
