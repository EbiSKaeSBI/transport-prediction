"""Feature list контракта v1: числовые/булевы фичи прогнозного датасета.

Единственная точка истины о том, какие колонки parquet-датасета
(:mod:`predictor.dataset`) идут в модель. Правила отбора для версии ``v1``:

- берём только числовые и булевы колонки (datetime/строки в модель не идут);
- исключаем ключи склейки, технические колонки датасета и все таргеты
  (``target_delay_s``, ``delay_delta_s``, ``target_class``) — список в
  :data:`EXCLUDED_COLUMNS`;
- исключаем ``target_stop_id``: Int64-идентификатор остановки не
  генерализуется и в v1 выступает как categorical-костыль (решение по
  тексту задачи #23, отклонение от «Дано» в §5.2 architecture.md);
- программно отбрасываем все-null колонки (в раздаче #22 это
  ``heading_error_deg``, ``layover_min``, ``n_vehicles_on_route``; фичи
  движения #24 — ``speed_deficit_ratio_5m``, ``dwell_p90_route_s``,
  ``trend_5``, ``momentum`` — живые в train/test-датасетах, но в
  validate-датасете trend_5/momentum все-null (в schedule_plan.csv нет
  фактов): в модель на train-отборе они входят, а на infer CatBoost
  трактует их null как пропуск — ожидаемое расхождение strict/validate);
  фильтр общий, на случай изменений данных.

Функция :func:`select_features` параметризована по ``version``, чтобы v2/v3
дособирали признаки без переписывания отбора.
"""

from __future__ import annotations

import polars as pl

#: Версии контракта, поддержанные отбором.
FEATURE_VERSIONS = ('v1',)

#: Ключи, технические колонки и таргеты — никогда не фичи.
EXCLUDED_COLUMNS: frozenset[str] = frozenset({
    # ключи датасета (sample_id уникален, tr_id — группировка для CV)
    'sample_id', 'tr_id', 'unit_id', 't',
    'ambiguous', 'variants',
    # идентификатор цели (не генерализуется, см. module docstring)
    'target_stop_id',
    # datetime-ключи: не числовые, но фиксируем явно (ловушка утечки §5.3)
    'target_time_begin',
    # таргеты
    'target_delay_s', 'target_class', 'delay_delta_s',
    # служебный флаг метки (не входит в секции features/v1.yaml)
    'target_ambiguous',
    # диагностика фолбэка v1b: исход cur_dev_s (0 — Go-кадр, 1 — хинт labels).
    # Не фича: на онлайне флаг вычислить нельзя, а его прогнозность прямо
    # противоречит no_future_leak (ADR 0004).
    'cur_dev_from_hint',
})

_NUMERIC_DTYPES: frozenset[type[pl.DataType]] = frozenset({
    pl.Int8, pl.Int16, pl.Int32, pl.Int64,
    pl.UInt8, pl.UInt16, pl.UInt32, pl.UInt64,
    pl.Float32, pl.Float64,
})


def _is_numeric_or_bool(dtype: pl.DataType) -> bool:
    return dtype in _NUMERIC_DTYPES or dtype == pl.Boolean


def select_features(
    df: pl.DataFrame,
    version: str = 'v1',
) -> tuple[list[str], dict[str, pl.DataType], list[str]]:
    """Отобрать фичи контракта ``version`` из датасета.

    Возвращает ``(cols, dtypes, categorical_cols)``: имена колонок в исходном
    порядке датасета, их polars-типы и список categorical-фич (пустой для v1 —
    CatBoost учит их числовыми, булевы приводятся к int на входе в модель).

    Все-null колонки отбрасываются по данным самого ``df`` (см. module
    docstring). Вызов на train- и infer-датасетах может дать разный список —
    инференс всегда фиксирует фичи по модели, а не по датасету
    (:mod:`predictor.predict`).
    """
    if version not in FEATURE_VERSIONS:
        raise ValueError(f'неизвестная версия фич {version!r}, есть {FEATURE_VERSIONS}')
    cols: list[str] = []
    dtypes: dict[str, pl.DataType] = {}
    for name in df.columns:
        if name in EXCLUDED_COLUMNS or not _is_numeric_or_bool(df.schema[name]):
            continue
        if df.get_column(name).null_count() == df.height:
            continue  # все-null фича контракта — мёртвая колонка раздачи
        cols.append(name)
        dtypes[name] = df.schema[name]
    return cols, dtypes, []


def to_matrix(df: pl.DataFrame, cols: list[str]) -> pl.DataFrame:
    """Матрица фич: булевы колонки приведены к Int8 (CatBoost object-bool не любит)."""
    exprs = []
    for c in cols:
        expr = pl.col(c)
        if df.schema[c] == pl.Boolean:
            expr = expr.cast(pl.Int8)
        exprs.append(expr)
    return df.select(exprs)
