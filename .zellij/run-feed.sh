#!/usr/bin/env bash
# NDTP-фид golden-потока в пейне Zellij: генерит план/привязку под «сейчас»
# и гонит живые кадры в transportctl serve. План пишется в корень репо —
# run-backend.sh ждёт именно ../plan.csv / ../binding.csv.
# Перестроить план, не перезапуская backend, нельзя: serve читает его на
# старте; для демонстрации это не нужно — фид переподключается сам.
set -u
cd "$(dirname "$0")/.."
python3 scripts/ndtp_feed.py --plan-out plan.csv --binding-out binding.csv --generate-only \
    || { echo "не сгенерирован план — фид не поднят"; exec sleep infinity; }
exec python3 scripts/ndtp_feed.py --port "${NDTP_PORT:-9201}" --every 1
