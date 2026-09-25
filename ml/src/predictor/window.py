"""Оконные и контекстные фичи (секции window/context контракта v1).

Все фичи строятся СТРОГО as-of T: используются только строки телеметрии
того же ``tr_id`` c ``event_time <= t`` кадра (инвариант no_future_leak из
features/v1.yaml; после ADR 0004 — «производные фичи только из event_time <= T»,
утечка в внешнем cur_dev_s зафиксирована ADR и сюда не попадает).

Базовые определения (v1):

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

Фичи движения (добавлены задачей #24, вариант v2; все опциональны — без
соответствующего входа колонки создаются null):

* ``trend_5`` / ``momentum`` — по ряду отклонений от графика ПОСЛЕДНИХ
  ЗАВЕРШЁННЫХ к T остановок того же ``tr_id`` из schedule (вход
  ``schedule_df``): ``dev_i = time_fact_begin_i − time_begin_i`` (секунды)
  для строк с ``time_fact_begin <= T``; ``trend_5`` — наклон линейной
  регрессии (МНК) по последним <=5 dev (единицы контракта — s_per_stop;
  ряд короче 2 точек ⇒ null); ``momentum`` — последняя разность ряда
  ``dev_k − dev_{k−1}`` (s; разность соседних остановок — та же
  размерность s_per_stop). Раньше ``momentum`` был прокси
  ``zero_ratio_1m − zero_ratio_10m`` — компромиссом v1, заменён честным
  определением. Для validate передаётся ``schedule_plan.csv`` БЕЗ
  ``time_fact_begin`` ⇒ trend_5/momentum там заведомо null — ЭТО ожидаемое
  расхождение strict/validate (аналог cur_dev_s здесь хинта не имеет; не
  импутируем ничем, null и есть null, CatBoost переваривает).
* ``dwell_p90_route_s`` — p90 длительностей dwell-эпизодов У ТОЙ ЖЕ
  ФИЗИЧЕСКОЙ ОСТАНОВКИ, что цель кадра (``target_stop_id``): «route» без
  route_id трактуем как остановку — geoms повторяющихся tt_action_item
  одного физического места совпадают, ключ физ.остановки — округлённая
  точка geom. Эпизод = >=2 подряд точки ``speed == 0`` одного tr_id по
  всей истории traffic (null-speed рвёт серию); длительность на T =
  ``min(end, T) − start``, где ``end`` — первый ненулевой ход того же tr_id
  строго после серии (открытый эпизод обрезается по T: «до T не уехал» —
  факт на T). Эпизод «подтверждён на T», только если его ВТОРАЯ нулевая
  точка <= T (иначе сам факт существования эпизода узнавался бы из
  будущего). Принадлежность остановке — координаты первой точки эпизода в
  радиусе ``STOP_RADIUS_M`` (80 м). Меньше 2 подтверждённых эпизодов у
  остановки к T ⇒ null.
* ``speed_deficit_ratio_5m`` = ``1 − speed_mean_5m / профиль``, где профиль —
  медианная скорость traffic ВСЕХ tr_id в бакете (дистанция точки до
  ближайшей остановки графика с шагом ``PROFILE_DIST_BIN_M`` × бакет часа
  с шагом ``PROFILE_HOUR_BIN_H``) — «типичная скорость на подлёте к цели в
  это время суток». Профиль считается ТОЛЬКО из входа ``profile_df`` (для
  всех фолдов — train/traffic.csv): статистика фолдов не смешивается
  (иначе leakage между сплитами через статистику), а infer-фолды видят
  профиль train — осознанный консерватизм. Точки дальше
  ``PROFILE_MAX_DIST_M`` от любой остановки в профиль не входят («не
  подлёт»); нулевые скорости в медиану не входят (стоянка — не ход).
  Требует ``schedule_df`` (остановки для дистанций) и ``profile_df``.

Пропущенные фичи контракта (колонки создаются null с этим обоснованием):

* ``n_vehicles_on_route`` — tr_id -> route не выводится из traffic/schedule
  без route_id (см. docs/architecture.md §4.6 — кластеризация остановок, вне
  скоупа этапа).
"""

from __future__ import annotations

import numpy as np
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

#: Null-плейсхолдеры фич, не выводимых из данных раздачи (см. docstring).
_SKIPPED_NULLS: dict[str, pl.DataType] = {
    'n_vehicles_on_route': pl.Int32,
}

#: Фичи, требующие дополнительных входов (schedule/episodes/profile);
#: без этих входов колонки создаются null.
_CONDITIONAL_NULLS: dict[str, pl.DataType] = {
    'trend_5': pl.Float32,
    'momentum': pl.Float32,
    'dwell_p90_route_s': pl.Float32,
    'speed_deficit_ratio_5m': pl.Float32,
}

#: Число последних завершённых остановок в ряду отклонений (trend_5).
TREND_STOPS = 5

#: Радиус привязки dwell-эпизода к физической остановке, м.
STOP_RADIUS_M = 80.0

#: Шаг бакета дистанции до ближайшей остановки в профиле скорости, м.
PROFILE_DIST_BIN_M = 500.0

#: Шаг бакета часа суток в профиле скорости, ч.
PROFILE_HOUR_BIN_H = 2

#: Профиль «скорость на подлёте»: точки дальше этого расстояния от любой
#: остановки в профиль не входят.
PROFILE_MAX_DIST_M = 2000.0

#: Строк телеметрии в чанке nearest_stop_distance (экономия памяти).
_PROFILE_CHUNK = 20_000

#: Метры на градус (Москва; долгота с поправкой cos(55.8°)) — точности
#: достаточно для порогов 80/2000 м и бакетов 500 м.
_M_PER_DEG_LAT = 110_570.0
_M_PER_DEG_LON = 63_200.0


def stop_geoms(schedule_df: pl.DataFrame) -> pl.DataFrame:
    """Таблица остановок графика: ``[tt_action_item_id, stop_key, lon, lat]``.

    ``stop_key`` — «физическая остановка»: строка округлённых до 4 знаков
    координат (геомы одного физического места повторяются в графике при
    разных рейсах/tr_id). Колонка ``geom`` — WKT ``POINT (lon lat)``.
    """
    need = {'tt_action_item_id', 'geom'}
    if not need <= set(schedule_df.columns):
        raise ValueError(
            f'schedule_df: не хватает колонок {sorted(need - set(schedule_df.columns))}'
        )
    return (
        schedule_df.select(
            'tt_action_item_id',
            pl.col('geom').str.extract(r'POINT \(([^)]*)\)').alias('_pair'),
        )
        .with_columns(
            pl.col('_pair').str.split(' ').list.get(0).cast(pl.Float64).alias('lon'),
            pl.col('_pair').str.split(' ').list.get(1).cast(pl.Float64).alias('lat'),
        )
        .drop('_pair')
        .filter(pl.col('lon').is_not_null() & pl.col('lat').is_not_null())
        .unique(subset='tt_action_item_id', maintain_order=True)
        .with_columns(
            (pl.col('lon').round(4).cast(pl.Utf8) + pl.lit('#')
             + pl.col('lat').round(4).cast(pl.Utf8)).alias('stop_key')
        )
    )


def physical_stops(stops_df: pl.DataFrame) -> pl.DataFrame:
    """Дедуп остановок по stop_key: ``[stop_key, lon, lat]``."""
    return stops_df.select('stop_key', 'lon', 'lat').unique(subset='stop_key', maintain_order=True)


def build_dev_trend(frames_df: pl.DataFrame, schedule_df: pl.DataFrame) -> pl.DataFrame:
    """trend_5/momentum по ряду отклонений завершённых к T остановок того же tr_id.

    Строки графика с ``time_fact_begin <= T`` дают ``dev = fact − plan`` (секунды);
    берём последние <=5 по времени факта. Ряд короче 2 точек ⇒ обе фичи null.
    Если колонки ``time_fact_begin`` нет вовсе или она вся null (schedule_plan
    валидации) — все-null trend_5/momentum: ожидаемое расхождение strict/plan
    (см. module docstring).
    """
    need = {'tr_id', 'time_begin', 'time_fact_begin'}
    if not need <= set(schedule_df.columns):
        raise ValueError(
            f'schedule_df: не хватает колонок {sorted(need - set(schedule_df.columns))}'
        )
    fr = frames_df.select('sample_id', 'tr_id', 't').with_row_index('fi')
    sch = schedule_df.filter(pl.col('time_fact_begin').is_not_null())
    if sch.height == 0:
        return (
            fr.select('sample_id')
            .with_columns(
                pl.lit(None, dtype=pl.Float32).alias('trend_5'),
                pl.lit(None, dtype=pl.Float32).alias('momentum'),
            )
        )
    dev = (
        sch.select(
            'tr_id',
            'time_fact_begin',
            (pl.col('time_fact_begin') - pl.col('time_begin'))
            .dt.total_seconds()
            .cast(pl.Float64)
            .alias('dev_s'),
        )
        .filter(pl.col('dev_s').is_not_null())
    )
    # Единственный as-of фильтр: факт остановки <= T кадра. Всё остальное —
    # функции только от этого множества строк (инвариант no_future_leak).
    j = (
        fr.join(dev, on='tr_id', how='inner')
        .filter(pl.col('time_fact_begin') <= pl.col('t'))
        .sort('fi', 'time_fact_begin', 'dev_s')
        .with_row_index('_i')
    )
    j = (
        j.with_columns(
            _pos=pl.col('_i') - pl.col('_i').min().over('fi'),
            _k=pl.len().over('fi'),
        )
        # хвостовые <=5 остановок: позиции [max(0, k-5), k-1]
        .filter(pl.col('_pos') >= (pl.col('_k') - TREND_STOPS).clip(lower_bound=0))
        .with_columns(_x=pl.col('_pos') - pl.col('_pos').min().over('fi'))
    )
    agg = j.group_by('fi').agg(
        n=pl.len().cast(pl.Float64),
        sx=pl.col('_x').sum().cast(pl.Float64),
        sy=pl.col('dev_s').sum(),
        sxy=(pl.col('_x') * pl.col('dev_s')).sum(),
        sx2=(pl.col('_x') * pl.col('_x')).sum(),
        last_dev=pl.col('dev_s').filter(pl.col('_pos') == pl.col('_k') - 1).first(),
        prev_dev=pl.col('dev_s').filter(pl.col('_pos') == pl.col('_k') - 2).first(),
    )
    return (
        fr.join(agg, on='fi', how='left')
        .with_columns(
            trend_5=pl.when(pl.col('n') >= 2)
            .then(
                (pl.col('n') * pl.col('sxy') - pl.col('sx') * pl.col('sy'))
                / (pl.col('n') * pl.col('sx2') - pl.col('sx') * pl.col('sx'))
            )
            .cast(pl.Float32),
            momentum=pl.when(pl.col('n') >= 2)
            .then(pl.col('last_dev') - pl.col('prev_dev'))
            .cast(pl.Float32),
        )
        .select('sample_id', 'trend_5', 'momentum')
    )


def dwell_episodes(
    traffic_df: pl.DataFrame, stops_df: pl.DataFrame, *, min_points: int = 2
) -> pl.DataFrame:
    """Dwell-эпизоды по всей телеметрии: ``[tr_id, stop_key, start, second, end]``.

    Эпизод — ``min_points`` (>=2) подряд точек одного tr_id с ``speed == 0``
    (null-speed — location_valid=False — рвёт серию; точки без координат
    отбрасываются). ``start``/``second`` — времена первой/второй точки серии,
    ``end`` — время первого НЕНУЛЕВОГО хода того же tr_id строго после
    последней нулевой точки серии (null, если до конца данных машина не
    ехала). ``stop_key`` — физ.остановка, чья geom-точка в радиусе
    ``STOP_RADIUS_M`` от координат первой точки серии (отдельная строка на
    каждую попавшую остановку; обычно их 0-1).

    Precompute один раз по всему traffic: будущее может лишь двигать ``end``
    открытой серии, а as-of величина ``min(end, T)`` от него не зависит при
    ``second <= T`` — инвариант no_future_leak покрыт тестами test_leak.
    """
    need = {'tr_id', 'event_time', 'speed', 'lon', 'lat'}
    if not need <= set(traffic_df.columns):
        raise ValueError(
            f'traffic_df: не хватает колонок {sorted(need - set(traffic_df.columns))}'
        )
    pts = (
        traffic_df.select(
            'tr_id',
            'event_time',
            pl.col('speed').cast(pl.Float64),
            pl.col('lon').cast(pl.Float64),
            pl.col('lat').cast(pl.Float64),
        )
        .drop_nulls(['event_time', 'lon', 'lat'])
        # Полностью детерминированный порядок: ties по event_time
        # (zero+nonzero пакеты на один момент) не должны зависеть от входа.
        .sort('tr_id', 'event_time', pl.col('speed').cast(pl.Float64))
        .with_columns(_nz=pl.col('speed').ne(0).fill_null(True))
    )
    runs = (
        pts.with_columns(_run=pl.col('_nz').cum_sum().over('tr_id'))
        .filter(~pl.col('_nz'))
        .group_by('tr_id', '_run')
        .agg(
            times=pl.col('event_time'),
            # anchor = координаты начала серии: min — детерминированно при ties
            lon=pl.col('lon').min(),
            lat=pl.col('lat').min(),
        )
        .with_columns(
            n=pl.col('times').list.len(),
            start=pl.col('times').list.get(0),
            # серия может быть короче min_points — get(1) тогда null (отфильтруется)
            second=pl.col('times').list.get(1, null_on_oob=True),
            last_zero=pl.col('times').list.max(),
        )
        .filter(pl.col('n') >= min_points)
        .drop('times', 'n', '_run')
    )
    if runs.height == 0:
        return runs.with_columns(
            pl.lit(None, dtype=pl.Datetime('us')).alias('end'),
            pl.lit(None, dtype=pl.Utf8).alias('stop_key'),
        ).select('tr_id', 'stop_key', 'start', 'second', 'end')
    # end: первый ненулевой ход того же tr_id СТРОГО после последней нулевой
    # точки серии (ключ join_asof сдвинут на 1 мкс — ties zero/nonzero-
    # пакетов на один момент не обрезают эпизод преждевременно).
    movers = (
        pts.filter(pl.col('_nz'))
        .select('tr_id', 'event_time')
        .unique()
        .sort('tr_id', 'event_time')
    )
    runs = runs.with_columns(
        _lk=pl.col('last_zero') + pl.duration(microseconds=1)
    ).sort('tr_id', '_lk')
    if movers.height:
        runs = runs.join_asof(
            movers, left_on='_lk', right_on='event_time', by='tr_id', strategy='forward',
        ).rename({'event_time': 'end'})
    else:
        runs = runs.with_columns(pl.lit(None, dtype=pl.Datetime('us')).alias('end'))
    # Привязка к физ.остановкам: эпизодов тысячи, остановок сотни — cross
    # join дёшев (<10 млн пар).
    phys = physical_stops(stops_df).rename({'lon': '_slon', 'lat': '_slat'})
    near = (
        runs.join(phys, how='cross')
        .with_columns(
            _d=(
                ((pl.col('lon') - pl.col('_slon')) * _M_PER_DEG_LON) ** 2
                + ((pl.col('lat') - pl.col('_slat')) * _M_PER_DEG_LAT) ** 2
            ).sqrt()
        )
        .filter(pl.col('_d') <= STOP_RADIUS_M)
        .group_by('tr_id', 'start', 'second', 'end')
        .agg(stop_key=pl.col('stop_key').unique())
        .explode('stop_key')
    )
    return near.select('tr_id', 'stop_key', 'start', 'second', 'end').sort(
        'stop_key', 'start', 'tr_id'
    )


def build_stop_dwell_p90(
    frames_df: pl.DataFrame, episodes_df: pl.DataFrame, stops_df: pl.DataFrame
) -> pl.DataFrame:
    """dwell_p90_route_s: p90 длительностей dwell-эпизодов у остановки кадра as-of T.

    Кадр привязывается к остановке своего ``target_stop_id`` (``stops_df`` —
    выход :func:`stop_geoms`). Подтверждение эпизода к T: ``second <= T``;
    длительность на T: ``min(end, T) − start`` (открытый обрезается по T —
    «до T не уехал»). Меньше 2 подтверждённых эпизодов у остановки ⇒ null.
    """
    fr = (
        frames_df.select('sample_id', 'target_stop_id', 't')
        .with_row_index('fi')
        .join(stops_df.select(pl.col('tt_action_item_id'), 'stop_key'),
              left_on='target_stop_id', right_on='tt_action_item_id', how='left')
    )
    ep = episodes_df.join(
        fr.select('fi', 't', 'stop_key').filter(pl.col('stop_key').is_not_null()),
        on='stop_key',
        how='inner',
    )
    # Два as-of-условия: эпизод подтверждён к T (second <= T) и начат до T.
    ep = ep.filter((pl.col('second') <= pl.col('t')) & (pl.col('start') <= pl.col('t')))
    ep = ep.with_columns(
        dur_s=(
            pl.min_horizontal(pl.coalesce('end', pl.col('t')), pl.col('t')) - pl.col('start')
        ).dt.total_seconds(),
    )
    if ep.height:
        p90 = ep.group_by('fi').agg(
            dwell_p90_route_s=pl.col('dur_s').quantile(0.9, interpolation='linear')
            .cast(pl.Float32),
            _n=pl.len(),
        ).with_columns(
            dwell_p90_route_s=pl.when(pl.col('_n') >= 2).then(pl.col('dwell_p90_route_s')),
        ).drop('_n')
    else:
        p90 = fr.select('fi').clear().with_columns(
            pl.lit(None, dtype=pl.Float32).alias('dwell_p90_route_s')
        )
    return fr.join(p90, on='fi', how='left').select('sample_id', 'dwell_p90_route_s')


def nearest_stop_distance(
    df: pl.DataFrame, stops_df: pl.DataFrame, max_dist_m: float
) -> pl.Series:
    """Дистанция (м) каждой строки ``[lon, lat]`` до ближайшей физ.остановки.

    Чанками по ``_PROFILE_CHUNK`` строк поверх numpy (полный cross-join
    traffic×остановок не строим). Точки дальше ``max_dist_m`` — null.
    """
    phys = physical_stops(stops_df)
    slon = np.asarray(phys['lon'].to_numpy(), dtype=np.float64)
    slat = np.asarray(phys['lat'].to_numpy(), dtype=np.float64)
    lon = np.asarray(df['lon'].to_numpy(), dtype=np.float64)
    lat = np.asarray(df['lat'].to_numpy(), dtype=np.float64)
    out = np.full(lon.shape, np.nan, dtype=np.float64)
    for i0 in range(0, len(lon), _PROFILE_CHUNK):
        i1 = min(i0 + _PROFILE_CHUNK, len(lon))
        d = np.hypot(
            (lon[i0:i1, None] - slon[None, :]) * _M_PER_DEG_LON,
            (lat[i0:i1, None] - slat[None, :]) * _M_PER_DEG_LAT,
        )
        dm = d.min(axis=1)
        out[i0:i1] = np.where(dm <= max_dist_m, dm, np.nan)
    return pl.DataFrame({'dist_m': out}).to_series()


def build_speed_profile(
    profile_traffic_df: pl.DataFrame, stops_df: pl.DataFrame,
    *, hour_bin_h: int = PROFILE_HOUR_BIN_H,
) -> pl.DataFrame:
    """Профиль «типичной скорости»: ``[dist_bin_m, hour_bin_h, speed_med_kmh]``.

    Бакеты: дистанция до ближайшей остановки (шаг ``PROFILE_DIST_BIN_M``,
    точки дальше ``PROFILE_MAX_DIST_M`` отбрасываются) × бакет часа суток
    (шаг ``hour_bin_h``; 24 = без бакета по времени — аблиация #24).
    Медиана ``speed`` по всем tr_id переданного профиля-файла; нулевые
    скорости не входят («типичный ход», не стоянка); строки без
    координат/скорости отбрасываются.
    """
    if hour_bin_h <= 0 or 24 % hour_bin_h:
        raise ValueError(f'hour_bin_h должен делить сутки: {hour_bin_h}')
    pts = (
        profile_traffic_df.select(
            'event_time',
            pl.col('lon').cast(pl.Float64),
            pl.col('lat').cast(pl.Float64),
            pl.col('speed').cast(pl.Float64),
        )
        .drop_nulls(['event_time', 'lon', 'lat', 'speed'])
        .filter(pl.col('speed') > 0)
    )
    if pts.height == 0:
        return pl.DataFrame(
            schema={'dist_bin_m': pl.Float64, 'hour_bin_h': pl.Int32, 'speed_med_kmh': pl.Float64}
        )
    pts = pts.with_columns(
        dist_m=nearest_stop_distance(pts, stops_df, PROFILE_MAX_DIST_M),
    ).drop_nulls('dist_m')
    return pts.with_columns(
        dist_bin_m=(pl.col('dist_m') / PROFILE_DIST_BIN_M).round(0) * PROFILE_DIST_BIN_M,
        hour_bin_h=((pl.col('event_time').dt.hour() // hour_bin_h)
                    * hour_bin_h).cast(pl.Int32),
    ).group_by('dist_bin_m', 'hour_bin_h').agg(
        speed_med_kmh=pl.col('speed').median(),
    ).sort('dist_bin_m', 'hour_bin_h')


def build_speed_deficit(
    base_df: pl.DataFrame, profile_df: pl.DataFrame, *, hour_bin_h: int = PROFILE_HOUR_BIN_H
) -> pl.DataFrame:
    """speed_deficit_ratio_5m = 1 − speed_mean_5m / медиана профиля бакета кадра.

    ``base_df`` — кадр + оконные фичи (нужны ``sample_id``, ``t``,
    ``distance_to_target_m``, ``speed_mean_5m``). Бакет кадра: дистанция до
    ЦЕЛИ (go_state) с шагом ``PROFILE_DIST_BIN_M`` × бакет часа T шагом
    ``PROFILE_HOUR_BIN_H`` — те же шаги, что у профиля (дистанция кадра — до
    своей цели, дистанция точки профиля — до ближайшей остановки графика;
    согласованное упрощение «подлёт к остановке»). Профильная медиана
    отсутствует/0 ⇒ null; speed_mean_5m null ⇒ null. Отрицательные значения
    легальны: едем быстрее типичного подлёта.
    """
    need = {'sample_id', 't', 'distance_to_target_m', 'speed_mean_5m'}
    if not need <= set(base_df.columns):
        raise ValueError(f'base_df: не хватает колонок {sorted(need - set(base_df.columns))}')
    fr = base_df.select(
        'sample_id',
        't',
        pl.col('distance_to_target_m').cast(pl.Float64).alias('dist_m'),
        pl.col('speed_mean_5m').cast(pl.Float64),
    ).with_columns(
        dist_bin_m=(pl.col('dist_m') / PROFILE_DIST_BIN_M).round(0) * PROFILE_DIST_BIN_M,
        hour_bin_h=((pl.col('t').dt.hour() // hour_bin_h) * hour_bin_h).cast(pl.Int32),
    )
    out = fr.join(profile_df, on=('dist_bin_m', 'hour_bin_h'), how='left').with_columns(
        ratio=(1.0 - pl.col('speed_mean_5m') / pl.col('speed_med_kmh')).cast(pl.Float32),
    )
    return out.select(
        'sample_id',
        speed_deficit_ratio_5m=pl.when(
            pl.col('speed_med_kmh') > 0
        ).then(pl.col('ratio')).cast(pl.Float32),
    )


def build_window_features(
    frames_df: pl.DataFrame,
    traffic_df: pl.DataFrame,
    *,
    schedule_df: pl.DataFrame | None = None,
    profile_df: pl.DataFrame | None = None,
    episodes_df: pl.DataFrame | None = None,
    profile_hour_bin_h: int = PROFILE_HOUR_BIN_H,
) -> pl.DataFrame:
    """Собрать window/context-фичи по кадрам из телеметрии строго as-of T.

    ``frames_df`` — выход :func:`predictor.frames.load_frames` (нужны колонки
    ``sample_id``, ``tr_id``, ``t``; для фич движения — также
    ``target_stop_id`` и ``distance_to_target_m``); ``traffic_df`` — телеметрия
    того же сплита (``tr_id``, ``event_time``, ``speed``; naive-МСК datetime).
    Опциональные входы v2 (см. module docstring): ``schedule_df`` — график
    того же сплита (trend_5/momentum + остановки dwell/профиля);
    ``profile_df`` — traffic фолда-эталона для профиля скорости (всегда
    train!); ``episodes_df`` — предвычисленные dwell-эпизоды
    (:func:`dwell_episodes`) для переиспользования между фолдами. Без
    входов соответствующие фичи — null-колонки.

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
            hour=pl.col('t').dt.hour().cast(pl.Int32),
            is_peak=pl.col('t').dt.hour().is_in([7, 8, 17, 18, 19]),
            day_of_week=pl.col('t').dt.weekday().cast(pl.Int32),
        )
        .drop('_last_event')
    )

    # --- фичи движения (#24): подключаются только при своих входах ---
    if schedule_df is not None:
        stops = stop_geoms(schedule_df)
        out = out.join(build_dev_trend(frames_df, schedule_df), on='sample_id', how='left')
        ep = episodes_df if episodes_df is not None else dwell_episodes(traffic_df, stops)
        out = out.join(build_stop_dwell_p90(frames_df, ep, stops), on='sample_id', how='left')
        if profile_df is not None:
            profile = build_speed_profile(profile_df, stops, hour_bin_h=profile_hour_bin_h)
            out = out.join(
                frames_df.select('sample_id', 'distance_to_target_m'),
                on='sample_id', how='left',
            )
            out = (
                out.join(
                    build_speed_deficit(out, profile, hour_bin_h=profile_hour_bin_h),
                    on='sample_id', how='left',
                )
                .drop('distance_to_target_m')
            )

    # Null-плейсхолдеры недоступных фич контракта.
    placeholders = dict(_SKIPPED_NULLS)
    for name, dtype in _CONDITIONAL_NULLS.items():
        if name not in out.columns:
            placeholders[name] = dtype
    out = out.with_columns(
        pl.lit(None, dtype=dtype).alias(name) for name, dtype in placeholders.items()
    )

    return out.select(
        'sample_id',
        't',
        *(
            pl.col(c).cast(pl.Float32)
            if c not in ('is_peak', 'hour', 'day_of_week', 'n_vehicles_on_route')
            else pl.col(c)
            for c in WINDOW_FEATURE_COLUMNS
        ),
    )
