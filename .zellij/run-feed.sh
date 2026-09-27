#!/usr/bin/env bash
# NDTP-фид в пейне Zellij: РЕАЛЬНЫЙ контур — dataset_feed проигрывает
# train/traffic.csv (настоящая телеметрия) и пишет план из
# train/schedule.csv (настоящее расписание). План и привязка пишутся в корень
# репо на старте фида — run-backend.sh ждёт именно ../plan.csv /
# ../binding.csv, а работающий serve подхватывает их за секунду по
# --plan-watch, без перезапуска.
# Старый синтетический контур (golden-кинематика + сценарная задержка +150 с)
# остаётся в ndtp_feed.py: EMU_FEED=1 возвращает его.
set -u
cd "$(dirname "$0")/.."
if [ "${EMU_FEED:-0}" = "1" ]; then
    python3 scripts/ndtp_feed.py --plan-out plan.csv --binding-out binding.csv --generate-only \
        || { echo "не сгенерирован план — фид не поднят"; exec sleep infinity; }
    exec python3 scripts/ndtp_feed.py --port "${NDTP_PORT:-9201}" --every 1
fi
exec python3 scripts/dataset_feed.py \
    --dataset-dir "${REAL_DATASET:-train}" --units "${REAL_UNITS:-4}" \
    --speed "${REAL_SPEED:-1}" \
    --plan-out plan.csv --binding-out binding.csv
