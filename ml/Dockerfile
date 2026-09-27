# ML-ядро: FastAPI + CatBoost-инференс. Слой зависимостей отделён от кода,
# чтобы пересборка при правке src не перекачивала catboost.
FROM python:3.12-slim
WORKDIR /app
COPY pyproject.toml ./
COPY src ./src
RUN pip install --no-cache-dir .
# боевые артефакты (регрессор v1online + парный P(late) + метрики для /model/info)
COPY artifacts/model_v1online.json artifacts/model_v1online_late.json \
     artifacts/metrics_v1online.json artifacts/metrics_v1online_late.json ./artifacts/
EXPOSE 8000
CMD ["python", "-m", "predictor.serve", \
  "--model", "/app/artifacts/model_v1online.json", \
  "--late-model", "/app/artifacts/model_v1online_late.json", \
  "--host", "0.0.0.0", "--port", "8000"]
