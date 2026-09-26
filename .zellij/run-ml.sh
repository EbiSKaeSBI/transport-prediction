#!/usr/bin/env bash
# Сервис модели (FastAPI, :8000) в пейне Zellij.
# Модель по умолчанию — онлайн-контракта (#36): те же признаки, что отдаёт
# Go-кадр, иначе MLClient честно отказывается прогнозировать моделью.
# Своя модель: ML_MODEL=ml/artifacts/model_v1v3b.json make dev
set -u
cd "$(dirname "$0")/.."
cd ml
if [ ! -x .venv/bin/python ]; then
    echo "нет ml/.venv — запустите 'make ml-setup' (или uv sync в ml/); сервис модели не поднят"
    exec sleep infinity
fi
MODEL="${ML_MODEL:-artifacts/model_v1online.json}"
if [ ! -f "$MODEL" ]; then
    echo "артефакт $MODEL не найден — соберите 'make ml-train'; сервис модели не поднят"
    exec sleep infinity
fi
exec .venv/bin/python -m predictor.serve --model "$MODEL" --host 127.0.0.1 --port "${ML_PORT:-8000}"
