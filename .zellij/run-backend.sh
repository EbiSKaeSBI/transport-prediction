#!/usr/bin/env bash
# NDTP-приёмник transportctl serve в пейне Zellij.
# Если в корне появятся plan.csv + binding.csv — сервер сразу заработает
# в режиме прогноза, иначе — только копит телеметрию (--dry-run).
set -u
cd "$(dirname "$0")/.."
cd backend
if [ -f ../plan.csv ] && [ -f ../binding.csv ]; then
    exec go run ./cmd/transportctl serve --listen :9201 \
        --plan ../plan.csv --binding ../binding.csv --stats 30s
fi
exec go run ./cmd/transportctl serve --listen :9201 --dry-run --stats 30s
