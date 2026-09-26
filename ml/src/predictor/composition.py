"""Общая сборка предсказания из режима таргета модели (v1/v2 abs, v3 delta).

Вынесено из :mod:`predictor.predict`, чтобы CLI-инференс по датасету и
ML-сервис (:mod:`predictor.serve`, ADR-0002) считали композицию
``cur_dev_s + дельта`` (§5.1 architecture.md) одним кодом, а не копией.

Режим берётся из ``metrics_*.json`` рядом с моделью: явный ``target_mode``,
подстраховка по имени таргета (``delay_delta_s`` -> delta), иначе ``abs``.
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
from catboost import CatBoostRegressor

#: Ключ со списком фич в metrics-файле (дублирует predictor.predict).
MODEL_FEATURES_KEY = 'features'


def metrics_for_model(model_path: str | Path) -> dict | None:
    """Metrics-файл рядом с моделью (по суффиксу), подстраховка — v1.

    None, если файла нет: тогда режим таргета считаем 'abs' (исторические
    модели v1/v1b/v2 всегда учили абсолютный таргет).
    """
    model_path = Path(model_path)
    candidates = [
        model_path.with_name(model_path.stem.replace('model_', 'metrics_') + '.json'),
        model_path.with_name('metrics_v1.json'),
    ]
    for metrics in candidates:
        if metrics.exists():
            return json.loads(metrics.read_text(encoding='utf-8'))
    return None


def target_mode_for(metrics: dict | None) -> str:
    """Режим сборки предсказания из метрик модели: 'abs' или 'delta' (v3)."""
    if not metrics:
        return 'abs'
    mode = metrics.get('target_mode')
    if mode:
        return str(mode)
    # историческая подстраховка: режим мог быть выведен из имени таргета
    return 'delta' if metrics.get('target') == 'delay_delta_s' else 'abs'


def model_feature_names(
    model: CatBoostRegressor, model_path: str | Path, metrics: dict | None
) -> list[str]:
    """Список фич модели: сначала ``model.feature_names_``, затем metrics."""
    names = list(model.feature_names_ or [])
    if names:
        return names
    if metrics and metrics.get(MODEL_FEATURES_KEY):
        return list(metrics[MODEL_FEATURES_KEY])
    raise ValueError(
        f'{model_path}: в модели нет имён фич и metrics-файл рядом не найден — '
        'невозможно гарантировать состав признаков'
    )


def compose_prediction(
    raw_pred: np.ndarray,
    cur_dev_s: np.ndarray | None,
    mode: str,
    context: str = '',
) -> np.ndarray:
    """Сырой вывод модели -> итоговое предсказание задержки, секунд.

    ``mode == 'abs'`` — вывод модели и есть предсказание. ``mode == 'delta'``
    (v3, §5.1) — прибавляем ``cur_dev_s``; он обязан быть без null
    (ADR-0007: в датасетах cur_dev_s добит хинтом), иначе падаем с внятной
    ошибкой, а не молча портм предсказание. ``context`` — подпись источника
    для текста ошибки (путь датасета или sample_id запроса).
    """
    raw_pred = np.asarray(raw_pred, dtype=np.float64)
    if mode != 'delta':
        return raw_pred
    if cur_dev_s is None:
        raise ValueError(
            f'{context}: модель в delta-режиме, а cur_dev_s отсутствует — '
            'сборка cur_dev_s + delta невозможна'
        )
    cur = np.asarray(cur_dev_s, dtype=np.float64)
    n_null = int(np.isnan(cur).sum())
    if n_null:
        raise ValueError(
            f'{context}: delta-режим требует cur_dev_s без null (ADR-0007), '
            f'null в {n_null} строках'
        )
    return cur + raw_pred


def load_model(model_path: str | Path) -> CatBoostRegressor:
    """Загрузка CatBoost с явным форматом по суффиксу (.json/.cbm)."""
    model = CatBoostRegressor()
    # catboost 1.2.10 не выводит формат из расширения (.json падает с
    # "Incorrect model file descriptor") — задаём явно по суффиксу.
    fmt = 'json' if str(model_path).endswith('.json') else 'cbm'
    model.load_model(str(model_path), format=fmt)
    return model
