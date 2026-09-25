"""Обучение CatBoost v1 (таргет ``target_delay_s``, loss=MAE).

Ватерлайн этапа v1 по §5.1 architecture.md: абсолютный таргет
``target_delay_s`` (дельта ``delay_delta_s`` — этап v3), CatBoostRegressor с
loss='MAE', ранняя остановка по ВНУТРЕННЕЙ group-split валидации внутри train
(:class:`sklearn.model_selection.GroupKFold` по ``tr_id`` — в test-holdout для
ранней остановки не заглядываем вообще).

Метрики после обучения: MAE на train (виденные модели строки), на внутренней
валидации, на честном holdout ``dataset_test.parquet`` — и baseline
``pred = cur_dev_s`` на том же holdout-subsetе. Baseline считается двумя
способами из-за 9% null в ``cur_dev_s`` (см. :func:`_baseline_metrics`) и
обязан быть обыгран моделью; если нет — числа фиксируются честно.

CLI (от корня репозитория):

    python -m predictor.train \\
        --train ml/artifacts/dataset_train.parquet \\
        --holdout ml/artifacts/dataset_test.parquet \\
        --out-dir ml/artifacts

Артефакты: ``model_v1{tag}.json`` (CatBoost), ``metrics_v1{tag}.json``
(метрики, feature_importances, зафиксированный список фич — источник истины
для :mod:`predictor.predict`); ``--tag b`` даёт вариант v1b, не затирая v1.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

import numpy as np
import polars as pl
from catboost import CatBoostRegressor, Pool
from sklearn.model_selection import GroupKFold

from predictor.features import select_features, to_matrix

TARGET = 'target_delay_s'
FEATURE_VERSION = 'v1'


def _mae(pred: np.ndarray, actual: np.ndarray) -> float:
    return float(np.mean(np.abs(pred - actual)))


def _train_model(
    df: pl.DataFrame,
    cols: list[str],
    *,
    iterations: int,
    depth: int,
    lr: float,
    seed: int,
    patience: int,
    internal_val_groups: int,
) -> tuple[CatBoostRegressor, dict]:
    """Групповая внутренняя валидация +CatBoost с ранней остановкой по ней.

    Одна контрольная грань GroupKFold по ``tr_id`` уходит в eval_set; обучение
    идёт на остальном train. Возвращает модель и метрики внутренней оценки.
    """
    X = to_matrix(df, cols).to_numpy().astype(np.float64)
    y = df[TARGET].to_numpy().astype(np.float64)
    groups = df['tr_id'].to_numpy()

    # GroupKFold не перемешивает и берёт группы по порядку появления;
    # перемешаем строки детерминированно, чтобы грань была типичной.
    order = np.argsort(np.mod(np.arange(len(y)) * 2654435761, len(y)), kind='stable')
    X, y, groups = X[order], y[order], groups[order]
    n_folds = internal_val_groups + 1
    val_idx = next(
        idx for _, idx in GroupKFold(n_splits=n_folds).split(X, y, groups)
    )
    tr_idx = np.setdiff1d(np.arange(len(y)), val_idx)

    model = CatBoostRegressor(
        loss_function='MAE',
        iterations=iterations,
        depth=depth,
        learning_rate=lr,
        random_seed=seed,
        early_stopping_rounds=patience,
        verbose=False,
        allow_writing_files=False,
    )
    # Pool с явными именами: они сохраняются в model_v1.json, и инференс
    # (predictor.predict) фиксирует состав фич по модели, а не по датасету.
    train_pool = Pool(X[tr_idx], y[tr_idx], feature_names=cols)
    val_pool = Pool(X[val_idx], y[val_idx], feature_names=cols)
    model.fit(train_pool, eval_set=val_pool)

    internal = {
        'n_train_rows': len(tr_idx),
        'n_internal_val_rows': len(val_idx),
        'best_iteration': int(model.get_best_iteration()),
        'mae_internal_val': _mae(model.predict(X[val_idx]), y[val_idx]),
        'mae_train_seen': _mae(model.predict(X[tr_idx]), y[tr_idx]),
        'mae_train_all': _mae(model.predict(X), y),
    }
    return model, internal


def _baseline_metrics(df: pl.DataFrame, pred: np.ndarray, actual: np.ndarray) -> dict:
    """MAE baseline'а ``cur_dev_s`` на holdout: два честных варианта null.

    - ``nan_to_zero``: пустой ``cur_dev_s`` (9% строк train-сплита) считается
      как предсказание 0 (дефолт :mod:`predictor.dataset` для submit-фолда);
    - ``skip_nan``: MAE только по строкам с непустым ``cur_dev_s``;
    - ``same_subset_as_model``: то же, что skip_nan, но ограничено строками,
      где модель дала конечное предсказание — честное «яблоко к яблоку».
    """
    cur = df['cur_dev_s'].to_numpy().astype(np.float64)
    ok_cur = ~np.isnan(cur)
    ok_model = np.isfinite(pred)
    out: dict = {
        'pred': 'cur_dev_s',
        'n_rows': len(actual),
        'n_cur_dev_nan': int((~ok_cur).sum()),
        'mae_nan_to_zero': _mae(np.where(ok_cur, cur, 0.0), actual),
        'mae_skip_nan': _mae(cur[ok_cur], actual[ok_cur]),
    }
    both = ok_cur & ok_model
    out['mae_same_subset'] = _mae(cur[both], actual[both])
    out['model_mae_same_subset'] = _mae(pred[both], actual[both])
    return out


def train(
    train_path: str | Path,
    holdout_path: str | Path,
    out_dir: str | Path,
    *,
    iterations: int = 3000,
    depth: int = 6,
    lr: float = 0.05,
    patience: int = 50,
    seed: int = 42,
    internal_val_groups: int = 4,
    tag: str = '',
) -> dict:
    """Полный прогон v1: отбор фич, обучение, метрики, сохранение артефактов.

    ``tag`` — суффикс имён артефактов ('' → model_v1.json/metrics_v1.json,
    'b' → model_v1b.json — вариант с hint-fallback cur_dev_s и т. п.).
    """
    t0 = time.monotonic()
    train_df = pl.read_parquet(train_path).drop_nulls(TARGET)
    holdout_df = pl.read_parquet(holdout_path).drop_nulls(TARGET)
    cols, _, _ = select_features(train_df, FEATURE_VERSION)

    model, internal = _train_model(
        train_df, cols,
        iterations=iterations, depth=depth, lr=lr, seed=seed,
        patience=patience, internal_val_groups=internal_val_groups,
    )

    Xh = to_matrix(holdout_df, cols).to_numpy().astype(np.float64)
    yh = holdout_df[TARGET].to_numpy().astype(np.float64)
    ph = model.predict(Xh)

    importance = sorted(
        zip(cols, model.feature_importances_), key=lambda p: -p[1]
    )
    # диагностика фолбэка v1b: доля строк, где cur_dev_s пришёл из хинта labels
    hint_diag = {}
    for split_name, split_df in (('train', train_df), ('holdout', holdout_df)):
        if 'cur_dev_from_hint' in split_df.columns:
            hint_diag[f'cur_dev_from_hint_{split_name}'] = int(
                split_df['cur_dev_from_hint'].sum()
            )
    metrics: dict = {
        'version': FEATURE_VERSION,
        'tag': tag,
        'target': TARGET,
        'loss': 'MAE',
        'n_train_rows': int(train_df.height),
        'n_holdout_rows': int(holdout_df.height),
        'features': cols,
        'hyperparams': {
            'iterations': iterations, 'depth': depth, 'learning_rate': lr,
            'early_stopping_rounds': patience, 'random_seed': seed,
            'internal_val_groups': internal_val_groups,
        },
        'mae_train_all': internal['mae_train_all'],
        'mae_train_seen': internal['mae_train_seen'],
        'internal_validation': internal,
        'mae_holdout_model': _mae(ph, yh),
        'n_holdout_pred_nan': int((~np.isfinite(ph)).sum()),
        'baseline_holdout': _baseline_metrics(holdout_df, ph, yh),
        **hint_diag,
        'feature_importances': {name: float(v) for name, v in importance},
        'train_seconds': round(time.monotonic() - t0, 1),
    }
    beats = metrics['mae_holdout_model'] < metrics['baseline_holdout']['mae_skip_nan']
    metrics['model_beats_baseline_on_holdout'] = bool(beats)

    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    model.save_model(out / f'model_v1{tag}.json', format='json')
    (out / f'metrics_v1{tag}.json').write_text(
        json.dumps(metrics, ensure_ascii=False, indent=2) + '\n', encoding='utf-8'
    )
    return metrics


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description='Обучить CatBoost v1 (MAE, абсолютный таргет)')
    ap.add_argument('--train', required=True, help='parquet train-датасета (с метками)')
    ap.add_argument('--holdout', required=True, help='parquet честного holdout-датасета')
    ap.add_argument('--out-dir', required=True, help='каталог артефактов модели/метрик')
    ap.add_argument('--iterations', type=int, default=3000)
    ap.add_argument('--depth', type=int, default=6)
    ap.add_argument('--learning-rate', type=float, default=0.05)
    ap.add_argument('--patience', type=int, default=50)
    ap.add_argument('--seed', type=int, default=42)
    ap.add_argument('--tag', default='',
                    help="суффикс имён артефактов ('' → model_v1.json, 'b' → model_v1b.json)")
    args = ap.parse_args(argv)

    metrics = train(
        args.train, args.holdout, args.out_dir,
        iterations=args.iterations, depth=args.depth, lr=args.learning_rate,
        patience=args.patience, seed=args.seed, tag=args.tag,
    )
    b = metrics['baseline_holdout']
    print(f"фич v1: {len(metrics['features'])} | train {metrics['n_train_rows']} строк, "
          f"holdout {metrics['n_holdout_rows']} строк | "
          f"лучшая итерация {metrics['internal_validation']['best_iteration']}")
    print(f"MAE train(все строки)   : {metrics['mae_train_all']:.2f} с")
    print(f"MAE internal val (группа): {metrics['internal_validation']['mae_internal_val']:.2f} с")
    print(f"MAE holdout (модель)    : {metrics['mae_holdout_model']:.2f} с")
    print(f"MAE holdout baseline cur_dev_s: nan→0 {b['mae_nan_to_zero']:.2f} с | "
          f"пропуск nan {b['mae_skip_nan']:.2f} с | та же выборка {b['mae_same_subset']:.2f} с")
    print(f"модель обыграла baseline на holdout: "
          f"{'ДА' if metrics['model_beats_baseline_on_holdout'] else 'НЕТ'}")
    top5 = list(metrics['feature_importances'].items())[:5]
    print('топ-5 фич: ' + ', '.join(f'{k}={v:.1f}' for k, v in top5))
    print(f"артефакты: model_v1{args.tag}.json, metrics_v1{args.tag}.json "
          f"(за {metrics['train_seconds']} c)")
    return 0


if __name__ == '__main__':
    sys.exit(main())
