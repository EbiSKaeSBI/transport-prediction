"""Инференс обученной модели по датасету и вывод в формате submission.

CLI (от корня репозитория):

    python -m predictor.predict \\
        --model ml/artifacts/model_v1.json \\
        --dataset ml/artifacts/dataset_validate.parquet \\
        --out ml/artifacts/predictions_validate.csv

Список фич фиксируется по самой модели (``model.feature_names_``,
подстраховка — ``features`` из ``metrics_v1.json`` рядом с моделью), а не
пересчитывается по датасету: все-null-фильтр в
:func:`predictor.features.select_features` зависит от данных и на другом
сплите мог бы выдать другой состав.

Формат вывода — CSV с ``;`` и колонками ``sample_id;prediction`` по одной
строке на каждый ``sample_id`` датасета (см. scripts/make_submission.py
``--model``). Режим таргета берётся из ``metrics_*.json`` рядом с моделью:
для delta-моделей v3 (``target_mode='delta'``) итоговое предсказание =
``cur_dev_s`` датасета + дельта модели (§5.1 architecture.md).
Предсказания пустых фич CatBoost отдаёт как числа, NaN в
выходе быть не должно; если nan всё же появился — считаем его по нулевому
предсказанию отклонения и печатаем предупреждение (для безкадровых точек
это безопасный дефолт).
"""

from __future__ import annotations

import argparse
import csv
import json
import sys
from pathlib import Path

import numpy as np
import polars as pl
from catboost import CatBoostRegressor

from predictor.features import to_matrix

MODEL_FEATURES_KEY = 'features'


def _metrics_for(model_path: Path) -> dict | None:
    """Metrics-файл рядом с моделью (по суффиксу), подстраховка — v1.

    None, если файла нет: тогда режим таргета считаем 'abs' (исторические
    модели v1/v1b/v2 всегда учили абсолютный таргет).
    """
    candidates = [
        model_path.with_name(model_path.stem.replace('model_', 'metrics_') + '.json'),
        model_path.with_name('metrics_v1.json'),
    ]
    for metrics in candidates:
        if metrics.exists():
            return json.loads(metrics.read_text(encoding='utf-8'))
    return None


def _target_mode(metrics: dict | None) -> str:
    """Режим сборки предсказания из метрик модели: 'abs' или 'delta' (v3)."""
    if not metrics:
        return 'abs'
    mode = metrics.get('target_mode')
    if mode:
        return str(mode)
    # историческая подстраховка: режим мог быть выведен из имени таргета
    return 'delta' if metrics.get('target') == 'delay_delta_s' else 'abs'


def _model_feature_names(
    model: CatBoostRegressor, model_path: Path, metrics: dict | None
) -> list[str]:
    names = list(model.feature_names_ or [])
    if names:
        return names
    if metrics and metrics.get(MODEL_FEATURES_KEY):
        return list(metrics[MODEL_FEATURES_KEY])
    raise ValueError(
        f'{model_path}: в модели нет имён фич и metrics-файл рядом не найден — '
        'невозможно гарантировать состав признаков'
    )


def predict(dataset_path: str | Path, model_path: str | Path) -> pl.DataFrame:
    """По датасету parquet -> таблица ``sample_id, prediction`` (все строки).

    Режим таргета читается из метрик модели: 'abs' — вывод модели и есть
    предсказание; 'delta' (v3, §5.1) — итог собираем как
    ``cur_dev_s + delta_pred``. Для delta-режима ``cur_dev_s`` обязан быть
    заполнен во всех строках датасета (ADR-0007: в train/validate/test это
    так, cur_dev_s добит хинтом — hint-fallback); null — падаем с
    внятной ошибкой, а не молча портим submission.
    """
    df = pl.read_parquet(dataset_path)
    model = CatBoostRegressor()
    # catboost 1.2.10 не выводит формат из расширения (.json падает с
    # "Incorrect model file descriptor") — задаём явно по суффиксу.
    fmt = 'json' if str(model_path).endswith('.json') else 'cbm'
    model.load_model(str(model_path), format=fmt)
    metrics = _metrics_for(Path(model_path))
    cols = _model_feature_names(model, Path(model_path), metrics)
    missing = [c for c in cols if c not in df.columns]
    if missing:
        raise ValueError(f'{dataset_path}: нет фич модели: {missing}')

    X = to_matrix(df, cols).to_numpy().astype(np.float64)
    pred = np.asarray(model.predict(X), dtype=np.float64)
    if _target_mode(metrics) == 'delta':
        if 'cur_dev_s' not in df.columns:
            raise ValueError(
                f'{dataset_path}: модель в delta-режиме, а датасет без cur_dev_s — '
                'сборка cur_dev_s + delta невозможна'
            )
        cur = df['cur_dev_s'].to_numpy().astype(np.float64)
        n_null = int(np.isnan(cur).sum())
        if n_null:
            raise ValueError(
                f'{dataset_path}: delta-режим требует cur_dev_s без null (ADR-0007), '
                f'null в {n_null} строках'
            )
        pred = cur + pred
    n_nan = int((~np.isfinite(pred)).sum())
    if n_nan:
        print(f'предупреждение: {n_nan} nan-предсказаний заменены на 0', file=sys.stderr)
        pred = np.nan_to_num(pred, nan=0.0)
    return pl.DataFrame({
        'sample_id': df['sample_id'],
        'prediction': np.round(pred, 3),
    })


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description='Инференс модели по датасету в формат submission')
    ap.add_argument('--model', required=True, help='model_v1.json (CatBoost)')
    ap.add_argument('--dataset', required=True, help='parquet-датасет (validate/train/test)')
    ap.add_argument('--out', required=True, help='куда положить predictions CSV (;)')
    args = ap.parse_args(argv)

    result = predict(args.dataset, args.model)
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open('w', newline='', encoding='utf-8') as fh:
        writer = csv.writer(fh, delimiter=';')
        writer.writerow(['sample_id', 'prediction'])
        writer.writerows(result.rows())
    print(f'{out}: {result.height} строк предсказаний '
          f'(min {result["prediction"].min():.1f} | max {result["prediction"].max():.1f} с)')
    return 0


if __name__ == '__main__':
    sys.exit(main())
