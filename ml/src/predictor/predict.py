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
import sys
from pathlib import Path

import numpy as np
import polars as pl

from predictor.composition import (
    compose_prediction,
    load_model,
    metrics_for_model,
    model_feature_names,
    target_mode_for,
)
from predictor.features import to_matrix


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
    model = load_model(model_path)
    metrics = metrics_for_model(model_path)
    cols = model_feature_names(model, Path(model_path), metrics)
    missing = [c for c in cols if c not in df.columns]
    if missing:
        raise ValueError(f'{dataset_path}: нет фич модели: {missing}')

    X = to_matrix(df, cols).to_numpy().astype(np.float64)
    pred = np.asarray(model.predict(X), dtype=np.float64)
    mode = target_mode_for(metrics)
    if mode == 'delta' and 'cur_dev_s' not in df.columns:
        raise ValueError(
            f'{dataset_path}: модель в delta-режиме, а датасет без cur_dev_s — '
            'сборка cur_dev_s + delta невозможна'
        )
    cur = df['cur_dev_s'].to_numpy().astype(np.float64) if 'cur_dev_s' in df.columns else None
    pred = compose_prediction(pred, cur, mode, context=str(dataset_path))
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
