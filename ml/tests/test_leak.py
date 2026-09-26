"""Инвариант no_future_leak (features/v1.yaml, docs/architecture.md §5.3).

Формулировка после ADR 0004: производные (window/context) фичи строятся
только из телеметрии с ``event_time <= T``; внешний ``cur_dev_s`` из кадра —
вход оракула, его утечка зафиксирована ADR и тестам не подлежит.

Тест на РЕАЛЬНОМ train-сниппете: собираем оконные фичи, затем для каждого
кадра заменяем все строки traffic со ``event_time > t`` детерминированным
«ядом» (скорость*3+7, location_valid=False, сдвинутый receive_time) и/или
добавляем строки строго в будущем — хеш md5 вектора признаков обязан
остаться прежним.

#24: тот же инвариант распространяется на фичи движения — trend_5/momentum
(строки графика с ``time_fact_begin <= T``), dwell_p90_route_s (эпизод
виден только при второй нулевой точке ``<= T``, длительность обрезана по T)
и speed_deficit_ratio_5m (оконная скорость as-of + статический train-профиль).
"""

from __future__ import annotations

import datetime as dt
import hashlib
import io

import polars as pl
import pytest

from predictor.dataset import load_schedule
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


# --------------------------- фичи движения (#24) ---------------------------

@pytest.fixture(scope='module')
def motion_ctx(snippet, train_schedule_path):
    """Кадр+телеметрия сниппета, график того же tr_id, фиксированный профиль."""
    frames, traffic = snippet
    sched = load_schedule(train_schedule_path).filter(pl.col('tr_id') == TR_ID)
    assert sched.height > 0, 'в графике нет строк TR_ID — тест был бы пустым'
    thr = frames['t'].max()
    assert thr < T_MAX, 'нужен запас «будущего» строк между max(t) и T_MAX'
    return frames, traffic, sched, thr


def _motion_build(frames, traffic, sched, profile):
    return build_window_features(
        frames, traffic, schedule_df=sched, profile_df=profile
    )


def test_motion_basics_populated(motion_ctx):
    """Sanity: на сниппете новые фичи непустые (иначе анти-утечка ничего не проверяет)."""
    frames, traffic, sched, _ = motion_ctx
    out = _motion_build(frames, traffic, sched, traffic)
    assert out['trend_5'].null_count() < out.height
    assert out['momentum'].null_count() < out.height


def test_future_traffic_and_schedule_do_not_change_motion_hash(motion_ctx):
    """Подмешивание/порча/удаление строк traffic И schedule строго после max(t)
    не двигает хеш полного окна фич (trend_5, momentum, dwell_p90, deficit)."""
    frames, traffic, sched, thr = motion_ctx
    baseline = _features_hash(_motion_build(frames, traffic, sched, traffic))

    # 1) Подмешивание в будущее: ненулевые «гонки», нулевые серии (новые
    #    dwell-эпизоды), null-speed; график — стопка будущих фактов с огромными dev.
    after = pl.col('event_time') > thr
    junk_traffic = traffic.with_columns(
        speed=pl.when(after).then(pl.lit(123.0)).when(after & pl.col('speed').eq(0))
        .then(pl.lit(0.0)).otherwise(pl.col('speed')),
    )
    extra_traffic = pl.concat([
        # нулевая серия в будущем у координат реальных пакетов — соблазн
        # dwell-эпизода; и null-speed серия (location_valid=False стиль)
        traffic.filter(after).with_columns(pl.lit(0.0).alias('speed')),
        traffic.filter(after).with_columns(pl.lit(None, dtype=pl.Float64).alias('speed')),
    ])
    junk_sched = pl.concat([
        sched,
        sched.head(8).with_columns(
            pl.col('tt_action_item_id') + pl.lit(50_000_000),
            (thr + pl.duration(seconds=60) * pl.int_range(1, 9)).alias('time_begin'),
            (thr + pl.duration(seconds=120) * pl.int_range(1, 9)
             + pl.duration(seconds=900)).alias('time_fact_begin'),
        ),
    ])
    mutated = _motion_build(frames, pl.concat([junk_traffic, extra_traffic]),
                            junk_sched, traffic)
    assert _features_hash(mutated) == baseline

    # 2) Порча будущих скоростей (с переворотом порядка строк — проверка на
    #    ties-детерминизм) и удаление всех будущих traffic-строк.
    #    schedule не трогаем: геометрия остановок — статический план, удаление
    #    строк с будущими фактами меняет мир (исчезает stop-геометрия), а не утечку.
    poisoned = traffic.with_columns(
        speed=pl.when(after).then(pl.col('speed') * 3.0 + 7.0).otherwise(pl.col('speed')),
    ).sort(pl.col('event_time').reverse())
    assert _features_hash(_motion_build(frames, poisoned, sched, traffic)) == baseline

    only_past_tr = traffic.filter(~after)
    assert only_past_tr.height < traffic.height
    assert _features_hash(
        _motion_build(frames, only_past_tr, sched, traffic)
    ) == baseline


def test_open_dwell_episode_length_clipped_at_T_not_by_future_end():
    """Рукотворный кейс: длительность незакрытого к T эпизода = min(end, T) − start.

    Два эпизода у остановки: закрытый 00:55→01:05 (600 с) и старт 01:10/01:11,
    открытый к T=01:30. Его длительность на T обязана быть min(end, T)−start =
    1200 с независимо от будущего: уехала в 01:40 (end есть, но > T), уехала
    в 05:00 или не уехала вовсе. p90([600, 1200]) = 600 + 0.9*600 = 1140.
    """
    t_stop = dt.datetime(2026, 1, 6)
    frames = pl.DataFrame({
        'sample_id': ['x_1'],
        'tr_id': pl.Series([1], dtype=pl.Int64),
        't': pl.Series([t_stop + dt.timedelta(minutes=90)], dtype=pl.Datetime('us')),
        'target_stop_id': pl.Series([7], dtype=pl.Int64),
        'distance_to_target_m': pl.Series([0.0], dtype=pl.Float32),
    })
    sched = pl.DataFrame({
        'tt_action_item_id': pl.Series([7], dtype=pl.Int64),
        'geom': ['POINT (37.6 55.7)'],
        'tr_id': pl.Series([1], dtype=pl.Int64),
        'time_begin': pl.Series([None], dtype=pl.Datetime('us')),
        'time_fact_begin': pl.Series([None], dtype=pl.Datetime('us')),
    })

    def traffic_with(mover_at):
        rows = [
            # закрытый эпизод: 00:55–01:04 нули, уехала 01:05 => dur 600
            (t_stop + dt.timedelta(minutes=55), 0.0),
            (t_stop + dt.timedelta(minutes=58), 0.0),
            (t_stop + dt.timedelta(minutes=64), 0.0),
            (t_stop + dt.timedelta(minutes=65), 30.0),
            # открытый к T эпизод: старт 01:10, вторая точка 01:11; mover — в будущем
            (t_stop + dt.timedelta(minutes=70), 0.0),
            (t_stop + dt.timedelta(minutes=71), 0.0),
        ]
        if mover_at is not None:
            rows.append((mover_at, 30.0))
        return pl.DataFrame({
            'tr_id': pl.Series([1] * len(rows), dtype=pl.Int64),
            'event_time': pl.Series([r[0] for r in rows], dtype=pl.Datetime('us')),
            'speed': pl.Series([r[1] for r in rows], dtype=pl.Float64),
            'lon': pl.Series([37.6] * len(rows), dtype=pl.Float64),
            'lat': pl.Series([55.7] * len(rows), dtype=pl.Float64),
        })

    vals = []
    for mover in (t_stop + dt.timedelta(minutes=100),   # 01:40 — после T
                  t_stop + dt.timedelta(minutes=300), None):
        out = _motion_build(frames, traffic_with(mover), sched, None)
        vals.append(out['dwell_p90_route_s'].item())
    # Без клипа по T первый вариант дал бы end−start = 1800 (p90=1680) — утечка.
    assert vals == pytest.approx([1140.0, 1140.0, 1140.0], abs=0.5)
