"""Сквозной тест v1-пайплайна на синтетике (без реальных датасетов).

Фикстура пародирует схему dataset_train/dataset_test #22: ключи, фичи
контракта (включая булевы и все-null колонку), таргеты. Реальные parquet-и
в unit-тесте не гоняем (долгие прогоны не место в тестах); ~300 строк
синтетики обучаются за доли секунды.
"""

from __future__ import annotations

import datetime as dt
import json
import math

import numpy as np
import polars as pl
import pytest

from predictor.features import EXCLUDED_COLUMNS, select_features
from predictor.predict import main as predict_main
from predictor.train import main as train_main

#: Фичи фикстуры: реальные имена контракта v1 (отбор по ним и проверяем).
_FIX_FEATURES = [
    'cur_dev_s', 'horizon_s', 'plan_travel_s', 'slack_s', 'trip_index',
    'is_terminal_stop', 'speed_current', 'dwell_current_s',
    'speed_mean_5m', 'zero_ratio_5m', 'dwell_rolling_5m_s',
    'hour', 'is_peak', 'day_of_week', 'headway_prev_s',
    'staleness_s', 'points_in_window', 'lag_s',
]


def _synthetic(n_rows: int, tr_ids: list[int], seed: int) -> pl.DataFrame:
    """Кадры: таргет = 0.8*cur_dev + 5*speed_mean_5m-дефицит + шум; NaN-структура как в раздаче."""
    rng = np.random.default_rng(seed)
    rows = n_rows
    groups = np.array([tr_ids[i % len(tr_ids)] for i in range(rows)])
    t0 = dt.datetime(2026, 1, 15, 8, 0)
    cur = rng.normal(120.0, 90.0, rows)
    cur[rng.random(rows) < 0.1] = np.nan  # null-структура cur_dev_s как в train
    speed5 = rng.normal(22.0, 8.0, rows)
    y = 0.8 * np.nan_to_num(cur) + (25.0 - speed5) * 2.0 + rng.normal(0.0, 15.0, rows)
    df = pl.DataFrame({
        'sample_id': [f'{g}_{1767000000 + i}' for i, g in enumerate(groups)],
        'tr_id': groups,
        'unit_id': groups * 10 + 1,
        'target_stop_id': (10_000_000 + rng.integers(0, rows, rows)).astype('int64'),
        'horizon_s': rng.uniform(601, 899, rows).astype(np.float32),
        'ambiguous': np.zeros(rows, dtype=bool),
        'variants': np.ones(rows, dtype=np.int32),
        'target_delay_s': y.astype(np.float32),
        'target_class': ['on_time'] * rows,
        'cur_dev_s': cur.astype(np.float32),
        # диагностика v1b: исход cur_dev_s; обязан отсеиваться отбором фич
        'cur_dev_from_hint': (rng.random(rows) < 0.1).astype(np.int8),
        'heading_error_deg': [None] * rows,  # все-null фича контракта -> фильтр
        'plan_travel_s': rng.uniform(300, 900, rows).astype(np.float32),
        'slack_s': rng.uniform(-200, 400, rows).astype(np.float32),
        'trip_index': rng.integers(1, 5, rows).astype(np.int32),
        'is_terminal_stop': rng.random(rows) < 0.2,
        'speed_current': rng.uniform(0, 60, rows).astype(np.float32),
        'dwell_current_s': rng.uniform(0, 60, rows).astype(np.float32),
        'speed_mean_5m': speed5.astype(np.float32),
        'zero_ratio_5m': rng.uniform(0, 0.5, rows).astype(np.float32),
        'dwell_rolling_5m_s': rng.uniform(0, 40, rows).astype(np.float32),
        'hour': (8 + np.arange(rows) // 12 % 10).astype(np.int32),
        'is_peak': (np.arange(rows) % 7 == 0),
        'day_of_week': np.full(rows, 3, dtype=np.int32),
        'headway_prev_s': rng.uniform(60, 900, rows).astype(np.float32),
        'staleness_s': np.zeros(rows, dtype=np.float32),
        'points_in_window': np.full(rows, 90, dtype=np.int32),
        'lag_s': np.zeros(rows, dtype=np.float32),
    })
    return df.with_columns(
        pl.datetime_range(t0, t0 + dt.timedelta(minutes=rows - 1), '1m',
                          eager=True).alias('t'),
        pl.datetime_range(t0 + dt.timedelta(minutes=15),
                          t0 + dt.timedelta(minutes=15 + rows - 1), '1m',
                          eager=True).alias('target_time_begin'),
        (pl.col('target_delay_s') - pl.col('cur_dev_s')).alias('delay_delta_s'),
    )


@pytest.fixture(scope='module')
def synthetic(tmp_path_factory) -> dict:
    out = tmp_path_factory.mktemp('v1synthetic')
    # 240/60 строк: фичи шумные, но сигнал (0.8*cur_dev + дефицит скорости)
    # обучаемый — на меньших выборках CatBoost неустойчиво проигрывает baseline.
    train = _synthetic(240, [101, 102, 103, 104, 105, 106], seed=1)
    holdout = _synthetic(60, [201, 202, 203], seed=2)
    train.write_parquet(out / 'dataset_train.parquet')
    holdout.write_parquet(out / 'dataset_test.parquet')
    return {'dir': out, 'train': train, 'holdout': holdout}


def test_select_features_rules(synthetic):
    cols, dtypes, categorical = select_features(synthetic['train'], 'v1')
    assert categorical == []
    # все-null фича контракта отфильтрована
    assert 'heading_error_deg' not in cols
    # ключи и таргеты исключены
    assert not (set(cols) & EXCLUDED_COLUMNS)
    # диагностика фолбэка v1b — не фича (в EXCLUDED_COLUMNS явно)
    assert 'cur_dev_from_hint' in synthetic['train'].columns
    assert 'cur_dev_from_hint' not in cols
    # булевы и числовые фичи контракта на месте
    assert {'cur_dev_s', 'speed_mean_5m', 'is_peak', 'hour'} <= set(cols)
    assert dtypes['is_peak'] == pl.Boolean
    with pytest.raises(ValueError):
        select_features(synthetic['train'], 'v9')


def test_train_and_predict_cli(synthetic, capsys):
    out = synthetic['dir']
    rc = train_main([
        '--train', str(out / 'dataset_train.parquet'),
        '--holdout', str(out / 'dataset_test.parquet'),
        '--out-dir', str(out),
        '--iterations', '300', '--patience', '25', '--depth', '3',
        '--learning-rate', '0.1',
    ])
    assert rc == 0
    model_path = out / 'model_v1.json'
    metrics_path = out / 'metrics_v1.json'
    assert model_path.exists() and metrics_path.exists()

    metrics = json.loads(metrics_path.read_text(encoding='utf-8'))
    assert 'heading_error_deg' not in metrics['features']
    assert 'cur_dev_from_hint' not in metrics['features']
    for key in ('mae_train_all', 'mae_holdout_model',
                'mae_internal_val'):
        value = metrics.get(key) if key != 'mae_internal_val' else metrics['internal_validation'][key]
        assert math.isfinite(value) and value >= 0.0
    # на синтетике с сильным сигналом модель обязана обыграть baseline
    assert metrics['model_beats_baseline_on_holdout'] is True
    assert metrics['n_holdout_pred_nan'] == 0
    assert sum(metrics['feature_importances'].values()) > 0

    rc = predict_main([
        '--model', str(model_path),
        '--dataset', str(out / 'dataset_test.parquet'),
        '--out', str(out / 'predictions_test.csv'),
    ])
    assert rc == 0
    preds = pl.read_csv(out / 'predictions_test.csv', separator=';')
    assert preds.columns == ['sample_id', 'prediction']
    assert preds.height == synthetic['holdout'].height
    assert preds['sample_id'].to_list() == synthetic['holdout']['sample_id'].to_list()
    assert preds['prediction'].null_count() == 0
    assert preds.lazy().filter(~pl.col('prediction').is_finite()).collect().height == 0


def test_train_tag_b_artifacts(synthetic):
    """--tag b пишет model_v1b/metrics_v1b, не затирая артефакты по умолчанию."""
    out = synthetic['dir']
    default_metrics = out / 'metrics_v1.json'
    before = default_metrics.read_bytes() if default_metrics.exists() else None
    rc = train_main([
        '--train', str(out / 'dataset_train.parquet'),
        '--holdout', str(out / 'dataset_test.parquet'),
        '--out-dir', str(out),
        '--iterations', '150', '--patience', '25', '--depth', '3',
        '--learning-rate', '0.1', '--tag', 'b',
    ])
    assert rc == 0
    assert (out / 'model_v1b.json').exists()
    metrics_b = json.loads((out / 'metrics_v1b.json').read_text(encoding='utf-8'))
    assert metrics_b['tag'] == 'b'
    assert 'cur_dev_from_hint' not in metrics_b['features']
    # диагностика фолбэка попадает в метрики, если колонка есть в датасете
    assert metrics_b['cur_dev_from_hint_train'] > 0
    if before is not None:
        assert default_metrics.read_bytes() == before  # v1 не тронут
