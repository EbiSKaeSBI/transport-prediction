#!/usr/bin/env bash
# NDTP-приёмник transportctl serve в пейне Zellij.
# Live-контур по умолчанию: план/привязку генерит соседний пейн run-feed.sh
# (ждём до 2 минут), модель — run-ml.sh на :8000. Дожидаться модель не
# нужно: MLClient сверяет контракт при старте и повторяет попытку сам,
# когда сервис модели поднимется.
# DRY_RUN=1 — старый режим: только копить телеметрию, без прогнозов.
set -u
cd "$(dirname "$0")/.."
cd backend
if [ "${DRY_RUN:-0}" = "1" ]; then
    exec go run ./cmd/transportctl serve --listen :9201 --dry-run --stats 30s
fi
for _ in $(seq 1 120); do
    [ -f ../plan.csv ] && [ -f ../binding.csv ] && break
    sleep 1
done
if [ ! -f ../plan.csv ] || [ ! -f ../binding.csv ]; then
    echo "план/привязка не появились за 2 мин (пейн feed не поднят?) — работаю без прогнозов"
    exec go run ./cmd/transportctl serve --listen :9201 --dry-run --stats 30s
fi
exec go run ./cmd/transportctl serve --listen :9201 --http :8080 \
    --plan ../plan.csv --binding ../binding.csv \
    --ml "${ML_URL:-http://127.0.0.1:8000}" \
    --tick 5s --grid 5s --stats 30s
