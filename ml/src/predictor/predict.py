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
``--model``). Предсказания пустых фич CatBoost отдаёт как числа, NaN в
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


def _model_feature_names(model: CatBoostRegressor, model_path: Path) -> list[str]:
    names = list(model.feature_names_ or [])
    if names:
        return names
    # Fallback: metrics рядом с моделью — имя по суффиксу модели
    # (model_v1b.json → metrics_v1b.json), подстраховка на историческое имя.
    candidates = [
        model_path.with_name(model_path.stem.replace('model_', 'metrics_') + '.json'),
        model_path.with_name('metrics_v1.json'),
    ]
    for metrics in candidates:
        if metrics.exists():
            data = json.loads(metrics.read_text(encoding='utf-8'))
            if data.get(MODEL_FEATURES_KEY):
                return list(data[MODEL_FEATURES_KEY])
    raise ValueError(
        f'{model_path}: в модели нет имён фич и metrics-файл рядом не найден — '
        'невозможно гарантировать состав признаков'
    )


def predict(dataset_path: str | Path, model_path: str | Path) -> pl.DataFrame:
    """По датасету parquet -> таблица ``sample_id, prediction`` (все строки)."""
    df = pl.read_parquet(dataset_path)
    model = CatBoostRegressor()
    # catboost 1.2.10 не выводит формат из расширения (.json падает с
    # "Incorrect model file descriptor") — задаём явно по суффиксу.
    fmt = 'json' if str(model_path).endswith('.json') else 'cbm'
    model.load_model(str(model_path), format=fmt)
    cols = _model_feature_names(model, Path(model_path))
    missing = [c for c in cols if c not in df.columns]
    if missing:
        raise ValueError(f'{dataset_path}: нет фич модели: {missing}')

    X = to_matrix(df, cols).to_numpy().astype(np.float64)
    pred = np.asarray(model.predict(X), dtype=np.float64)
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
