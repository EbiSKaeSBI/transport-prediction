"""ML-сервис инференса: FastAPI HTTP+JSON на :8000 (ADR-0002, §4.5 architecture.md).

gRPC из initial-схемы перегружен для 30–56 ТС — на проволосе HTTP+JSON,
а ``proto/ml.proto`` остаётся каноническим описанием контракта полей.

CLI (от корня репозитория)::

    python -m predictor.serve --model ml/artifacts/model_v1v3b.json --port 8000

Без ``--model`` (или с ``--no-model``), а также если загрузка артефакта
на старте упала — сервис стартует в fallback-режиме (§4.7: «Модель не
загрузилась → правило pred = cur_dev_s с явной пометкой»), HTTP всё равно 200.

Эндпоинты (§4.5):

- ``POST /predict`` — один feature vector → ответ контракта ниже;
- ``POST /predict/batch`` — массив таких же запросов, один проход CatBoost;
- ``GET  /model/info`` — версия, фичи, target-режим, MAE, дата, счётчики;
- ``POST /model/reload`` — горячая подмена артефакта под локом; провал
  загрузки = HTTP 400, СТАРАЯ модель остаётся в работе;
- ``GET  /healthz`` / ``GET /metrics`` — состояние и Prometheus-текст
  (латентность p50/p95/p99, число запросов, все деградации видимы в /metrics
  — критерий 5).

Формат запроса (плоский dict контракта): обязательные ``sample_id``,
``cur_dev_s``, опциональный ``horizon_s`` и дальше имена фич из
``model/info`` (``features`` того же metrics-файла) со значениями
float|bool|null. Состав фич валидируется по списку модели: отсутствующие
обязательные фичи = 422 с внятным сообщением; лишние ключи игнорируются
(контракт допускает надмножество — Go-клиент шлёт секцию features целиком).

Формат ответа (§4.5): ``{sample_id, delay_s, p_late, p_ontime, p_early,
reason, horizon_min, source, model_version}``. ``delay_s`` для delta-режима
= ``cur_dev_s + дельта`` через общую :func:`predictor.composition.compose_prediction`
(тот же код, что CLI :mod:`predictor.predict`). ``p_late`` — вероятность
порога красной зоны (delay >= 120 с), парный классификатор
:mod:`predictor.late` (v4), подключаемый ``--late-model`` и сверенный по
списку фич с регрессией при загрузке; без него ``null`` — честнее нуля,
из которого пороги риска gateway (0.3/0.6) сделали бы «вечно зелёный» мир.
``p_ontime``/``p_early`` — ``null``: трёхклассовая голова не реализована,
импровизировать вероятности из регрессии без обоснования нечем. ``reason``
— правила §5.4 (:func:`predictor.late.infer_reason`, строки паритетны с
replay-генератором); у fallback-ответа ``null``: объяснять кадр, который
даже не дошёл до валидации фич, — обещать больше, чем знаешь. ``source`` —
``model`` либо ``fallback_cur_dev`` — та самая «явная пометка» из §4.7.
"""

from __future__ import annotations

import argparse
import sys
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np
import polars as pl
from fastapi import FastAPI
from fastapi.responses import JSONResponse, PlainTextResponse
from pydantic import BaseModel, ConfigDict

from predictor import late as late_head
from predictor.composition import (
    compose_prediction,
    load_model,
    metrics_for_model,
    model_feature_names,
    target_mode_for,
)
from predictor.features import to_matrix

#: Окно скользящих замеров латентности инференса для квантилей /metrics.
LATENCY_WINDOW = 2048

#: Имена фич, у которых есть выделенное поле запроса (не лежат в extras).
_DEDICATED_FIELDS = ('sample_id', 'cur_dev_s', 'horizon_s')


class PredictRequest(BaseModel):
    """Один feature vector контракта §4.5 (плоский dict, extras = фичи)."""

    model_config = ConfigDict(extra='allow')

    sample_id: str
    cur_dev_s: float
    horizon_s: float | None = None

    def feature_values(self) -> dict[str, Any]:
        """Имена фич -> значения: extras запроса + выделенные поля."""
        combined: dict[str, Any] = dict(self.model_extra or {})
        if self.horizon_s is not None:
            combined['horizon_s'] = self.horizon_s
        combined['cur_dev_s'] = self.cur_dev_s
        return combined


@dataclass(frozen=True)
class ModelState:
    """Иммутабельный снимок загруженной модели (замена — только целиком).

    ``late_*`` — парный бинарный классификатор P(delay >= 120 с)
    (:mod:`predictor.late`, v4): свой артефакт, те же фичи в том же порядке
    (паритет проверен при загрузке), один проход матрицы на обе головы.
    """

    path: str
    model: Any  # CatBoostRegressor
    features: list[str]
    target_mode: str
    version: str
    metrics: dict | None
    trained_at: str  # mtime артефакта (в metrics-json даты обучения нет)
    loaded_at: str
    late_model: Any = None           # CatBoostClassifier | None
    late_version: str | None = None
    late_metrics: dict | None = None
    late_path: str | None = None


@dataclass
class Counters:
    """Счётчики запросов/деградаций и скользящее окно латентности (§4.7)."""

    requests: dict[str, int] = field(default_factory=dict)
    fallback_responses: int = 0
    invalid_requests: int = 0
    load_failures: int = 0
    reloads: int = 0
    latencies_s: deque[float] = field(default_factory=lambda: deque(maxlen=LATENCY_WINDOW))

    def bump(self, key: str) -> None:
        self.requests[key] = self.requests.get(key, 0) + 1


def _iso(ts: float) -> str:
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(ts))


def load_state(model_path: str | Path,
               late_path: str | Path | None = None) -> ModelState:
    """Загрузить артефакт + метрики; любая ошибка — исключение (не fallback).

    Fallback решает вызывающий (:func:`create_app` на старте ловит и работает
    без модели; /model/reload отвечает 400 и не трогает старую состояние).
    ``late_path`` — артефакт P(late)-классификатора: ошибка загрузки или
    расхождение списка фич считаются ошибкой всей пары (атомарно — gateway
    остаётся на предыдущем снимке, чем на регрессии без головы вероятности).
    """
    model_path = Path(model_path)
    if not model_path.is_file():
        raise FileNotFoundError(f'артефакт модели не найден: {model_path}')
    model = load_model(model_path)
    metrics = metrics_for_model(model_path)
    features = model_feature_names(model, model_path, metrics)
    late_model = late_version = late_metrics = None
    if late_path is not None:
        late_model, late_metrics = late_head.load_late(late_path)
        late_head.require_parity(list(late_metrics['features']), features)
        late_version = Path(late_path).stem.removeprefix('model_')
    return ModelState(
        path=str(model_path),
        model=model,
        features=features,
        target_mode=target_mode_for(metrics),
        version=model_path.stem.removeprefix('model_'),
        metrics=metrics,
        trained_at=_iso(model_path.stat().st_mtime),
        loaded_at=_iso(time.time()),
        late_model=late_model,
        late_version=late_version,
        late_metrics=late_metrics,
        late_path=str(late_path) if late_path is not None else None,
    )


def _as_float(value: Any, name: str, sample_id: str) -> float | None:
    """Значение фичи из JSON -> float|null; bool -> 1.0/0.0 (как в to_matrix)."""
    if value is None:
        return None
    if isinstance(value, bool):
        return float(value)
    if isinstance(value, (int, float)):
        return float(value)
    raise ValueError(
        f'sample_id={sample_id!r}: фича {name!r} должна быть числом или null, '
        f'получено {type(value).__name__}'
    )


def _require_features(state: ModelState, req: PredictRequest) -> dict[str, Any]:
    """Собрать значения всех фич модели; нехватка = ValueError -> 422.

    Лишние ключи запроса отбрасываются молча (см. module docstring).
    """
    values = req.feature_values()
    missing = [name for name in state.features if name not in values]
    if missing:
        raise ValueError(
            f'sample_id={req.sample_id!r}: в запросе нет обязательных фич модели: '
            f'{missing} (полный список — GET /model/info)'
        )
    return values


def _predict_requests(app_state: AppState, reqs: list[PredictRequest]) -> tuple[list[dict], float]:
    """Один проход CatBoost по пачке запросов -> (ответы, секунды инференса)."""
    state = app_state.snapshot()
    if state is None:
        # §4.7: модель не загружена/загрузка упала — предсказание cur_dev_s,
        # явная пометка source, вероятности/причина отсутствуют (v4/#9).
        out = [
            {
                'sample_id': r.sample_id,
                'delay_s': r.cur_dev_s,
                'p_late': None,
                'p_ontime': None,
                'p_early': None,
                'reason': None,
                'horizon_min': (r.horizon_s / 60.0) if r.horizon_s is not None else None,
                'source': 'fallback_cur_dev',
                'model_version': None,
            }
            for r in reqs
        ]
        app_state.counters.fallback_responses += len(out)
        return out, 0.0

    rows = []
    for r in reqs:
        values = _require_features(state, r)
        rows.append({name: _as_float(values[name], name, r.sample_id)
                     for name in state.features})
    data = {name: [row[name] for row in rows] for name in state.features}
    df = pl.DataFrame(data, schema={name: pl.Float64 for name in state.features})
    X = to_matrix(df, state.features).to_numpy().astype(np.float64)
    t0 = time.perf_counter()
    raw = np.asarray(state.model.predict(X), dtype=np.float64)
    # p_late — та же матрица X: фичи классификатора сверены с регрессией
    # при загрузке (load_state), поэтому второй проход по кадру не нужен.
    p_late = (late_head.predict_p_late(state.late_model, X)
              if state.late_model is not None else None)
    elapsed = time.perf_counter() - t0
    cur = np.array([r.cur_dev_s for r in reqs], dtype=np.float64)
    delay = compose_prediction(raw, cur, state.target_mode, context='/predict')
    delay = np.nan_to_num(delay, nan=0.0)  # NaN-вывод = нулевое смещение (как в predict.py)

    answers = []
    for i, (r, d) in enumerate(zip(reqs, delay, strict=True)):
        horizon = r.horizon_s
        if horizon is None:
            h_val = r.feature_values().get('horizon_s')
            horizon = float(h_val) if h_val is not None else None
        answers.append({
            'sample_id': r.sample_id,
            'delay_s': float(d),
            'p_late': None if p_late is None else float(p_late[i]),
            'p_ontime': None,   # трёхклассовая голова — не реализована
            'p_early': None,
            'reason': late_head.infer_reason(rows[i]),
            'horizon_min': (horizon / 60.0) if horizon is not None else None,
            'source': 'model',
            'model_version': state.version,
        })
    return answers, elapsed


class AppState:
    """Реестр модели + счётчики: атомарная замена под локом (/model/reload)."""

    def __init__(self) -> None:
        self._state: ModelState | None = None
        self._load_error: str | None = None
        self._lock = threading.Lock()
        self.counters = Counters()

    def snapshot(self) -> ModelState | None:
        return self._state  # иммутабельный объект — атомарная видимость ссылки

    @property
    def load_error(self) -> str | None:
        return self._load_error

    def try_load(self, model_path: str | Path, *, late_path: str | Path | None = None,
                 count_reload: bool = False,
                 ) -> tuple[ModelState | None, str | None]:
        """(новая модель | None, ошибка). Провал — старое состояние не трогается.

        Успех — атомарная замена ссылки под локом (§ hot reload без рестарта).
        """
        try:
            new = load_state(model_path, late_path)
        except Exception as exc:  # noqa: BLE001 — любой провал артефакта = деградация
            with self._lock:
                self.counters.load_failures += 1
            return None, f'{type(exc).__name__}: {exc}'
        with self._lock:
            self._state = new
            self._load_error = None
            if count_reload:
                self.counters.reloads += 1
        return new, None

    def set_start_failure(self, error: str) -> None:
        self._load_error = error


def create_app(model_path: str | Path | None = None,
               late_model_path: str | Path | None = None) -> FastAPI:
    """Собрать FastAPI-приложение. ``model_path=None`` — сразу fallback-режим.

    Сбой загрузки на старте НЕ валит сервис: фиксируем ошибку и отвечаем
    fallback'ом (требование живой проверки: --model /nonexistent → /predict
    отдаёт fallback-ответ, а не 5xx).
    """
    app_state = AppState()
    if model_path is not None:
        new, err = app_state.try_load(model_path, late_path=late_model_path)
        if new is None:
            app_state.set_start_failure(err or 'неизвестная ошибка загрузки')
            print(f'предупреждение: модель не загружена ({err}) — fallback cur_dev_s',
                  file=sys.stderr)

    app = FastAPI(title='transport-prediction ML-модуль', version='v1',
                  description='HTTP+JSON поверх контракта proto/ml.proto (ADR-0002)')
    app.state.ml = app_state

    @app.post('/predict')
    def predict_one(req: PredictRequest) -> JSONResponse:
        app_state.counters.bump('predict')
        try:
            answers, elapsed = _predict_requests(app_state, [req])
        except ValueError as exc:
            app_state.counters.invalid_requests += 1
            return JSONResponse({'detail': str(exc)}, status_code=422)
        if app_state.snapshot() is not None:
            app_state.counters.latencies_s.append(elapsed)
        return JSONResponse(answers[0])

    @app.post('/predict/batch')
    def predict_batch(reqs: list[PredictRequest]) -> JSONResponse:
        app_state.counters.bump('predict_batch')
        if not reqs:
            return JSONResponse({'detail': 'пустой батч: ожидаем массив запросов'},
                                status_code=422)
        try:
            answers, elapsed = _predict_requests(app_state, reqs)
        except ValueError as exc:
            app_state.counters.invalid_requests += 1
            return JSONResponse({'detail': str(exc)}, status_code=422)
        if app_state.snapshot() is not None:
            app_state.counters.latencies_s.append(elapsed)
        return JSONResponse(answers)

    @app.get('/model/info')
    def model_info() -> dict:
        state = app_state.snapshot()
        counters = app_state.counters
        info: dict[str, Any] = {
            'model_loaded': state is not None,
            'load_error': app_state.load_error,
            'requests': dict(counters.requests),
            'fallback_responses': counters.fallback_responses,
            'invalid_requests': counters.invalid_requests,
            'load_failures': counters.load_failures,
            'reloads': counters.reloads,
            'inference_samples': len(counters.latencies_s),
        }
        if state is None:
            info['version'] = None
            info['late'] = None
            return info
        m = state.metrics or {}
        internal = m.get('internal_validation') or {}
        info.update({
            'version': state.version,
            'path': state.path,
            'target': m.get('target'),
            'target_mode': state.target_mode,
            'loss': m.get('loss'),
            'features': state.features,
            'feature_count': len(state.features),
            'trained_at': state.trained_at,  # mtime артефакта: метрики даты не хранят
            'loaded_at': state.loaded_at,
            'best_iteration': internal.get('best_iteration'),
            'mae': {k: v for k, v in m.items() if k.startswith('mae_')},
            'n_train_rows': m.get('n_train_rows'),
            'n_holdout_rows': m.get('n_holdout_rows'),
        })
        if state.late_metrics is not None:
            lm = state.late_metrics
            info['late'] = {
                'version': state.late_version,
                'threshold_s': lm.get('threshold_s'),
                'auc_holdout': lm.get('auc_holdout'),
                'accuracy_holdout': lm.get('accuracy_holdout'),
                'precision_holdout': lm.get('precision_holdout'),
                'recall_holdout': lm.get('recall_holdout'),
                'positive_rate_holdout': lm.get('positive_rate_holdout'),
            }
        else:
            info['late'] = None
        return info

    @app.post('/model/reload')
    def model_reload(payload: dict) -> JSONResponse:
        app_state.counters.bump('model_reload')
        path = payload.get('model')
        if not path or not isinstance(path, str):
            return JSONResponse({'detail': "нужно тело {\"model\": \"путь к артефакту\"}"},
                                status_code=400)
        old = app_state.snapshot()
        new, err = app_state.try_load(path, late_path=old.late_path if old else None,
                                      count_reload=True)
        if new is None:
            # провал загрузки = 4xx и СТАРАЯ модель остаётся в работе (§4.7)
            return JSONResponse({
                'status': 'error',
                'detail': err,
                'active_model': old.version if old else None,
            }, status_code=400)
        return JSONResponse({
            'status': 'reloaded',
            'old_model': old.version if old else None,
            'active_model': new.version,
            'target_mode': new.target_mode,
            'feature_count': len(new.features),
        })

    @app.get('/healthz')
    def healthz() -> dict:
        state = app_state.snapshot()
        return {
            'status': 'ok' if state is not None else 'degraded_fallback',
            'model_loaded': state is not None,
            'model_version': state.version if state else None,
            'load_error': app_state.load_error,
        }

    @app.get('/metrics')
    def metrics() -> PlainTextResponse:
        c = app_state.counters
        state = app_state.snapshot()
        lines: list[str] = [
            '# HELP predictor_model_loaded Загружена ли модель (0 — fallback §4.7).',
            '# TYPE predictor_model_loaded gauge',
            f'predictor_model_loaded {1 if state else 0}',
            '# HELP predictor_fallback_responses_total Ответы fallback cur_dev_s.',
            '# TYPE predictor_fallback_responses_total counter',
            f'predictor_fallback_responses_total {c.fallback_responses}',
            '# HELP predictor_invalid_requests_total Запросы 422 (нет фич контракта).',
            '# TYPE predictor_invalid_requests_total counter',
            f'predictor_invalid_requests_total {c.invalid_requests}',
            '# HELP predictor_model_load_failures_total Неудачные загрузки/релоады.',
            '# TYPE predictor_model_load_failures_total counter',
            f'predictor_model_load_failures_total {c.load_failures}',
            '# HELP predictor_reloads_total Успешные горячие релоады.',
            '# TYPE predictor_reloads_total counter',
            f'predictor_reloads_total {c.reloads}',
            '# HELP predictor_requests_total Запросы по эндпоинтам.',
            '# TYPE predictor_requests_total counter',
        ]
        for key, val in sorted(c.requests.items()):
            lines.append(f'predictor_requests_total{{endpoint="/{key}"}} {val}')
        lines += [
            '# HELP predictor_inference_latency_seconds Латентность инференса CatBoost.',
            '# TYPE predictor_inference_latency_seconds summary',
        ]
        if c.latencies_s:
            arr = np.array(c.latencies_s)
            for q in (0.5, 0.95, 0.99):
                lines.append(
                    f'predictor_inference_latency_seconds{{quantile="{q}"}} '
                    f'{float(np.quantile(arr, q)):.6f}'
                )
            lines.append(f'predictor_inference_latency_seconds_count {arr.size}')
            lines.append(f'predictor_inference_latency_seconds_sum {arr.sum():.6f}')
        else:
            lines.append('predictor_inference_latency_seconds_count 0')
        return PlainTextResponse('\n'.join(lines) + '\n',
                                 media_type='text/plain; version=0.0.4')

    return app


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description='ML-сервис predictor.serve (FastAPI, §4.5)')
    ap.add_argument('--model', default=None, help='путь к model_v1*.json (без него — fallback)')
    ap.add_argument('--late-model', default=None,
                    help='путь к model_*_late.json: парный P(late)-классификатор (v4)')
    ap.add_argument('--no-model', action='store_true', help='стартить сразу в fallback-режиме')
    ap.add_argument('--host', default='0.0.0.0')
    ap.add_argument('--port', type=int, default=8000)
    args = ap.parse_args(argv)

    import uvicorn

    model = None if (args.no_model or args.model is None) else args.model
    uvicorn.run(create_app(model, args.late_model), host=args.host, port=args.port,
                log_level='info')
    return 0


if __name__ == '__main__':
    sys.exit(main())
