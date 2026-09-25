"""Инвариант no_future_leak (features/v1.yaml, docs/architecture.md §5.3).

Формулировка после ADR 0004: производные (window/context) фичи строятся
только из телеметрии с ``event_time <= T``; внешний ``cur_dev_s`` из кадра —
вход оракула, его утечка зафиксирована ADR и тестам не подлежит.

Тест на РЕАЛЬНОМ train-сниппете: собираем оконные фичи, затем для каждого
кадра заменяем все строки traffic со ``event_time > t`` детерминированным
«ядом» (скорость*3+7, location_valid=False, сдвинутый receive_time) и/или
добавляем строки строго в будущем — хеш md5 вектора признаков обязан
остаться прежним.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import io

import polars as pl
import pytest

from predictor.frames import load_frames
from predictor.window import build_window_features

TR_ID = 9000000  # самый «кадронасыщенный» tr_id train-сплита
T_MAX = dt.datetime(2026, 1, 6, 12, 0)  # сниппет: трафик одного tr_id до 12:00


def _features_hash(df: pl.DataFrame) -> str:
    """md5 каноничной CSV-серизации (колонки фиксированы, сортировка по ключу)."""
    buf = io.BytesIO()
    df.sort('sample_id').write_csv(buf)
    return hashlib.md5(buf.getbuffer()).hexdigest()


@pytest.fixture(scope='module')
def snippet(features_train_path, train_traffic):
    frames = load_frames(features_train_path).filter(
        (pl.col('tr_id') == TR_ID) & (pl.col('t') <= T_MAX)
    )
    traffic = train_traffic.filter(
        (pl.col('tr_id') == TR_ID) & (pl.col('event_time') <= T_MAX)
    )
    assert frames.height >= 50, f'слишком маленький сниппет: {frames.height}'
    return frames, traffic


def test_shuffle_rows_after_T_does_not_change_hash(snippet):
    frames, traffic = snippet
    baseline = _features_hash(build_window_features(frames, traffic))

    thr = frames['t'].max()
    # «Заменить все строки traffic с event_time > t каждого кадра»: глобально
    # это строки после max(t) сниппета — они строго в будущем для ЛЮБОГО
    # кадра (строки между min(t) и max(t) — законное прошлое для поздних
    # кадров, их менять нельзя).
    after = pl.col('event_time') > thr
    mutated = traffic.with_columns(
        speed=pl.when(after).then(pl.col('speed') * 3.0 + 7.0).otherwise(pl.col('speed')),
        location_valid=pl.when(after).then(pl.lit(False)).otherwise(pl.col('location_valid')),
        receive_time=pl.when(after).then(pl.lit(None)).otherwise(pl.col('receive_time')),
    ).sort(pl.col('event_time').reverse())  # и перестановка порядка строк

    assert mutated.filter(after).height > 0, 'в сниппете нет строк после T — тест пустой'
    assert _features_hash(build_window_features(frames, mutated)) == baseline


def test_future_rows_appended_and_removed_do_not_change_hash(snippet):
    frames, traffic = snippet
    baseline = _features_hash(build_window_features(frames, traffic))

    tmax = frames['t'].max()
    junk = pl.DataFrame(
        {
            'tr_id': [TR_ID, TR_ID, TR_ID],
            # строго в будущем для ВСЕХ кадров сниппета: гигантские скорости
            'event_time': [tmax + dt.timedelta(seconds=s) for s in (30, 120, 3600)],
            'speed': [999.0, 0.0, -5.0],
        }
    )
    assert _features_hash(build_window_features(
        frames, pl.concat([traffic.select('tr_id', 'event_time', 'speed'), junk])
    )) == baseline

    # Удаление будущих строк тоже не должно двигать хеш.
    only_past = traffic.filter(pl.col('event_time') <= tmax)
    assert only_past.height < traffic.height
    assert _features_hash(build_window_features(frames, only_past)) == baseline


def test_point_exactly_at_T_is_used():
    """Граница as-of включительна: точка ровно на T попадает в окно, T+1s — нет."""
    t0 = dt.datetime(2026, 1, 6, 3, 0)
    frames = pl.DataFrame(
        {
            'sample_id': ['1_2'],
            'tr_id': pl.Series([1], dtype=pl.Int64),
            't': pl.Series([t0], dtype=pl.Datetime('us')),
        }
    )
    past = pl.DataFrame(
        {
            'tr_id': pl.Series([1, 1], dtype=pl.Int64),
            'event_time': pl.Series(
                [t0 - dt.timedelta(seconds=60), t0 - dt.timedelta(seconds=30)],
                dtype=pl.Datetime('us'),
            ),
            'speed': pl.Series([10.0, 20.0], dtype=pl.Float64),
        }
    )
    at_T = past.head(1).select(
        pl.col('tr_id'),
        pl.lit(t0).alias('event_time'),
        pl.lit(90.0).alias('speed'),
    )
    after_T = past.head(1).select(
        pl.col('tr_id'),
        pl.lit(t0 + dt.timedelta(seconds=1)).alias('event_time'),
        pl.lit(90.0).alias('speed'),
    )
    base = build_window_features(frames, past)['speed_mean_5m'].item()
    with_at = build_window_features(frames, pl.concat([past, at_T]))['speed_mean_5m'].item()
    with_after = build_window_features(frames, pl.concat([past, after_T]))['speed_mean_5m'].item()
    assert base == pytest.approx(15.0)
    assert with_at == pytest.approx(40.0)  # (10+20+90)/3 — точка на T учтена
    assert with_after == pytest.approx(15.0)  # точка после T не видна
