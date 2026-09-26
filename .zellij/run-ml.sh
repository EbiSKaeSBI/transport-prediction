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
    echo "артефакт $MODEL не найден — соберите 'make ml-replay-frames ml-datasets ml-train ml-train-late'; сервис модели не поднят"
    exec sleep infinity
fi
# P(late)-голова подключается, только если пара лежит рядом и подходит по
# списку признаков (serve.py сверяет его с регрессией). Без неё p_late в
# ответах остаётся null — честнее, но панель «Вероятность опоздания» молчит,
# поэтому артефакт по умолчанию ждём и подключаем молча.
LATE="${ML_LATE_MODEL:-artifacts/model_v1online_late.json}"
LATE_ARGS=()
if [ -f "$LATE" ]; then
    LATE_ARGS=(--late-model "$LATE")
else
    echo "P(late)-голова $LATE не найдена — отвечаю без вероятностей (p_late = null)"
fi
exec .venv/bin/python -m predictor.serve --model "$MODEL" "${LATE_ARGS[@]}" \
    --host 127.0.0.1 --port "${ML_PORT:-8000}"
