"""Оконные и контекстные фичи (секции window/context контракта v1).

Все фичи строятся СТРОГО as-of T: используются только строки телеметрии
того же ``tr_id`` c ``event_time <= t`` кадра (инвариант no_future_leak из
features/v1.yaml; после ADR 0004 — «производные фичи только из event_time <= T»,
утечка в внешнем cur_dev_s зафиксирована ADR и сюда не попадает).

Определения:

* ``speed_mean_Nm`` — среднее ``speed`` по окну ``[T − N, T]``; null, если в
  окне нет точек с известной скоростью (пустой ``speed`` при location_valid=False).
* ``zero_ratio_Nm`` — доля точек с ``speed == 0`` среди точек с известной
  скоростью в окне. Контракт объявляет поле nullable=false, но при пустом
  окне доля неопределима — возвращаем null (осознанное расхождение, иначе 0.0
  врал бы о «нет пробок» в месте без данных).
* ``dwell_rolling_5m_s`` — прокси суммарного простоя за 5 мин: для каждой
  точки окна ``[T − 300, T]`` со ``speed == 0`` прибавляем интервал до
  СЛЕДУЮЩЕЙ точки этого же tr_id внутри окна, обрезанный моментом T.
  Последняя точка окна со ``speed == 0`` даёт вклад ``T − t_i``: «до T не
  уехала» — факт, известный на T (следующей точки <= T нет), а не утечка.
  Честный физический dwell по нерегулярным пакетам измерим только такой
  интерполяцией по межпакетным интервалам.
* ``headway_prev_s`` = T − event_time последней точки этого же tr_id
  (<= T); null, если точек до T нет.
* ``hour`` / ``is_peak`` / ``day_of_week`` — из naive-МСК времени T;
  пик 07–09 и 17–20 (интервалы полуоткрытые: час ∈ {7,8} ∪ {17,18,19});
  ``day_of_week`` — ISO (понедельник = 1).
* ``momentum`` = ``zero_ratio_1m − zero_ratio_10m`` (контракт: s_per_stop —
  единицы не соблюдены, это прокси тренда простоя; при null-компонентах null).

Пропущенные фичи контракта (колонки создаются null с этим обоснованием):

* ``speed_mean_*`` считаются; а вот ``speed_deficit_ratio_5m`` — нет: дефицит
  скорости vs профиль сегмента требует привязки к маршруту/сегменту, их в
  раздаче нет (route_id отсутствует, map-matching — отдельный трек).
* ``dwell_p90_route_s`` — p90 простоев по маршруту: тот же отсутствующий
  route_id, профиль простоев построить не из чего.
* ``n_vehicles_on_route`` — tr_id -> route не выводится из traffic/schedule
  без route_id (см. docs/architecture.md §4.6 — кластеризация остановок, вне
  скоупа этапа).
* ``trend_5`` — наклон ряда отклонений по последним 5 пройденным остановкам:
  вне кадра истории отклонений нет (в traffic.csv только координаты/скорость,
  а ``cur_dev_s`` — одно число на кадр, не ряд). Восстановить trend_5 из
  оконного трафика можно только через детекцию прибытий, которую ADR 0004
  признал невоспроизводящей time_fact_begin; честнее оставить null.
  ``momentum`` при этом считаем по нулевым скоростям (см. выше).
"""

from __future__ import annotations

import polars as pl

#: Минутные окна для speed_mean_* / zero_ratio_*.
WINDOW_MINUTES = (1, 3, 5, 10)

#: Порядок колонок-фич, возвращаемых build_window_features (window + context).
WINDOW_FEATURE_COLUMNS = [
    'speed_mean_1m', 'speed_mean_3m', 'speed_mean_5m', 'speed_mean_10m',
    'zero_ratio_1m', 'zero_ratio_3m', 'zero_ratio_5m', 'zero_ratio_10m',
    'speed_deficit_ratio_5m', 'dwell_p90_route_s', 'dwell_rolling_5m_s',
    'trend_5', 'momentum',
    'hour', 'is_peak', 'day_of_week', 'n_vehicles_on_route', 'headway_prev_s',
]

#: Скалярные константы для контракта: null-плейсхолдеры пропущенных фич
#: (обоснование — в module docstring) с их контрактными типами.
_SKIPPED_NULLS: dict[str, pl.DataType] = {
    'speed_deficit_ratio_5m': pl.Float32,
    'dwell_p90_route_s': pl.Float32,
    'trend_5': pl.Float32,
    'n_vehicles_on_route': pl.Int32,
}


def build_window_features(frames_df: pl.DataFrame, traffic_df: pl.DataFrame) -> pl.DataFrame:
    """Собрать window/context-фичи по кадрам из телеметрии строго as-of T.

    ``frames_df`` — выход :func:`predictor.frames.load_frames` (нужны колонки
    ``sample_id``, ``tr_id``, ``t``); ``traffic_df`` — телеметрия того же сплита
    (колонки ``tr_id``, ``event_time``, ``speed``; naive-МСК datetime).

    Возвращает DataFrame ``[sample_id, t, *WINDOW_FEATURE_COLUMNS]`` по
    числу кадров (кадры без телеметрии до T получают null-фичи окна).
    """
    need = {'sample_id', 'tr_id', 't'}
    if not need <= set(frames_df.columns):
        raise ValueError(f'frames_df: не хватает колонок {sorted(need - set(frames_df.columns))}')
    need_tr = {'tr_id', 'event_time', 'speed'}
    if not need_tr <= set(traffic_df.columns):
        raise ValueError(f'traffic_df: не хватает колонок {sorted(need_tr - set(traffic_df.columns))}')

    fr = (
        frames_df.select('sample_id', 'tr_id', 't')
        .with_row_index('fi')
    )
    # Ограничение join-размера: телеметрия только по tr_id из кадров.
    tr_ids = set(frames_df.get_column('tr_id').to_list())
    tr = (
        traffic_df.select('tr_id', 'event_time', pl.col('speed').cast(pl.Float64))
        .filter(pl.col('tr_id').is_in(tr_ids))
    )

    # Единственный фильтр «будущего»: event_time <= t. Всё остальное ниже —
    # функции только от этого множества строк (инвариант no_future_leak).
    j = fr.join(tr, on='tr_id', how='inner').filter(pl.col('event_time') <= pl.col('t'))

    # Двелл считаем по ДИСТИНКТНЫМ моментам: одновременные пакеты (в traffic
    # бывают ties по event_time) схлопываются в один «инстант», нулевой
    # скоростью инстанта считается, если speed == 0 у любой его строки.
    # Иначе вклад простоя зависел бы от порядка строк-дубликатов (недетермин).
    inst = (
        j.group_by('fi', 'event_time', 't')
        .agg(pl.col('speed').eq(0).fill_null(False).any().alias('_zero'))
        .sort('fi', 'event_time')
        .with_columns(
            gap=(pl.col('event_time').shift(-1) - pl.col('event_time')).dt.total_seconds().over('fi'),
        )
        .with_columns(
            # Последняя точка группы (gap null) «стоит» до самого T: факт
            # «к моменту T не уехала» известен на T и утечкой не является.
            # Интервалы, перехлёстывающие T, обрезаются до T.
            gap=pl.min_horizontal(
                pl.col('gap'),
                (pl.col('t') - pl.col('event_time')).dt.total_seconds(),
            ).cast(pl.Float64),
        )
        .group_by('fi')
        .agg(
            pl.col('gap').filter(
                pl.col('_zero')
                & (pl.col('event_time') >= pl.col('t') - pl.duration(minutes=5))
            ).sum().alias('dwell_rolling_5m_s')
        )
    )

    agg_exprs: list[pl.Expr] = [pl.col('event_time').max().alias('_last_event')]
    for m in WINDOW_MINUTES:
        inw = pl.col('event_time') >= pl.col('t') - pl.duration(minutes=m)
        agg_exprs += [
            pl.col('speed').filter(inw & pl.col('speed').is_not_null())
            .mean().alias(f'speed_mean_{m}m'),
            (inw & pl.col('speed').eq(0).fill_null(False)).sum().alias(f'_zero_cnt_{m}'),
            (inw & pl.col('speed').is_not_null()).sum().alias(f'_known_cnt_{m}'),
        ]
    agg = j.group_by('fi').agg(agg_exprs).with_columns(
        # 0/0 (все скорости в окне null) дал бы NaN — явно гасим в null.
        pl.when(pl.col(f'_known_cnt_{m}') > 0)
        .then(pl.col(f'_zero_cnt_{m}') / pl.col(f'_known_cnt_{m}'))
        .alias(f'zero_ratio_{m}m')
        for m in WINDOW_MINUTES
    ).drop(
        *[f'_zero_cnt_{m}' for m in WINDOW_MINUTES], *[f'_known_cnt_{m}' for m in WINDOW_MINUTES]
    )

    out = (
        fr.join(agg, on='fi', how='left')
        .join(inst, on='fi', how='left')
        .with_columns(
            headway_prev_s=(pl.col('t') - pl.col('_last_event')).dt.total_seconds(),
            momentum=pl.col('zero_ratio_1m') - pl.col('zero_ratio_10m'),
            hour=pl.col('t').dt.hour().cast(pl.Int32),
            is_peak=pl.col('t').dt.hour().is_in([7, 8, 17, 18, 19]),
            day_of_week=pl.col('t').dt.weekday().cast(pl.Int32),
        )
        .with_columns(
            pl.lit(None, dtype=dtype).alias(name) for name, dtype in _SKIPPED_NULLS.items()
        )
        .drop('_last_event')
        .select(
            'sample_id',
            't',
            *(
                pl.col(c).cast(pl.Float32)
                if c not in ('is_peak', 'hour', 'day_of_week', 'n_vehicles_on_route')
                else pl.col(c)
                for c in WINDOW_FEATURE_COLUMNS
            ),
        )
    )
    return out
