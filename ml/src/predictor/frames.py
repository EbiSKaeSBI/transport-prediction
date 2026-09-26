"""Загрузка кадров признаков Go-replay (JSONL) в схему контракта features/v1.yaml.

Формат выхода Go — JSONL, а не parquet, как предполагал текст задачи #22:
известное расхождение (в Go ничего не меняем), файл `ml/artifacts/features_*.jsonl`,
одна JSON-строка на кадр:
`{sample_id, unit_id, tr_id, t, target_stop_id, horizon_s, ambiguous, variants,
values: {...}, staleness_s, points_in_window, lag_s}`.

Ловушки данных (проверены фактически, см. также ADR 0004):

1. NULL-поля внутри ``values`` в JSON ПРОПУЩЕНЫ целиком (например
   ``cur_dev_s``, ``headway_s`` есть не во всех кадрах). Отсутствующий ключ =
   null, а не 0; загрузчик дополняет все поля контракта, отсутствующие -> null.
2. Поле ``t`` несёт суффикс ``Z``, но это НАИВНОЕ стенное МСК без конвертации
   (Go дублирует ``Z`` к наивному времени; epoch в ``sample_id`` =
   ``timegm(naive)``). ``Z`` просто срезается, часовые пояса не конвертируются;
   все склейки с traffic/labels идут по наивному МСК.
3. ``target_stop_id`` по контракту int32, но фактические геомы
   (``53699018678``) не влезают в int32 — храним Int64 (расхождение
   контракта с данными, фиксируем здесь).
"""

from __future__ import annotations

import datetime as dt
import json
from collections.abc import Mapping
from pathlib import Path

import polars as pl

#: Ключевые/служебные колонки кадра вне секций фич контракта.
KEY_COLUMNS: dict[str, pl.DataType] = {
    'sample_id': pl.Utf8,
    'unit_id': pl.Int64,
    'tr_id': pl.Int64,
    't': pl.Datetime('us'),
    'target_stop_id': pl.Int64,
    'horizon_s': pl.Float32,
    'ambiguous': pl.Boolean,
    'variants': pl.Int32,
}

#: Фичи, которые считает Go и которые приходят в ``values`` кадра, по секциям
#: контракта v1. extra — фичи Go вне контракта (пробрасываем как есть).
CONTRACT_SECTIONS: dict[str, dict[str, pl.DataType]] = {
    'given': {
        # target_time_begin в кадре Go отсутствует — приходит из labels/points
        # на этапе сборки датасета (dataset.py).
        'cur_dev_s': pl.Float32,
    },
    'schedule': {
        'plan_travel_s': pl.Float32,
        'slack_s': pl.Float32,
        'headway_s': pl.Float32,
        'trip_index': pl.Int32,
        'is_terminal_stop': pl.Boolean,
        'manual_fill': pl.Boolean,
    },
    'go_state': {
        'speed_current': pl.Float32,
        'speed_seg_avg': pl.Float32,
        'speed_seg_max': pl.Float32,
        'dwell_current_s': pl.Float32,
        'dwell_last_s': pl.Float32,
        'distance_to_target_m': pl.Float32,
        'stops_remaining': pl.Int32,
        'route_progress': pl.Float32,
        'drift_last3_slope': pl.Float32,
        'consecutive_late_stops': pl.Int32,
    },
    'quality': {
        'staleness_s': pl.Float32,
        'points_in_window': pl.Int32,
        'lag_s': pl.Float32,
    },
}

#: Поля ``values`` из Go, не входящие в контракт (дублируют верхнеуровневые
#: флаги неоднозначности); сохраняем, чтобы не терять информацию кадра.
EXTRA_COLUMNS: dict[str, pl.DataType] = {
    'target_ambiguous': pl.Boolean,
}

#: Полный канонический порядок колонок загруженного кадра.
FRAME_COLUMNS: list[str] = [
    *KEY_COLUMNS,
    *(c for sec in ('given', 'schedule', 'go_state') for c in CONTRACT_SECTIONS[sec]),
    *EXTRA_COLUMNS,
    *(c for c in CONTRACT_SECTIONS['quality']),
]

_ALL_CONTRACT_FEATURES: Mapping[str, pl.DataType] = {
    **{c: d for sec in CONTRACT_SECTIONS.values() for c, d in sec.items()},
    **EXTRA_COLUMNS,
}


def _parse_t(value: str) -> dt.datetime:
    """t: naive МСК с обязательным суффиксом Z (см. module docstring, п. 2)."""
    if not value.endswith('Z'):
        raise ValueError(f'кадр: время t без суффикса Z: {value!r}')
    # Z срезается, конвертации поясов нет: суффикс — артефакт сериализации Go.
    return dt.datetime.fromisoformat(value[:-1])


def load_frames(path: str | Path) -> pl.DataFrame:
    """Прочитать JSONL кадров Go-replay и привести к схеме контракта v1.

    Возвращает плоский DataFrame: ключевые колонки + все фичи секций
    given/schedule/go_state/quality контракта (отсутствующие в ``values`` ->
    null) + ``target_ambiguous``. Дедупликация по ``sample_id``: полностью
    идентичные дубли схлопываются, дубли с расходящимися значениями — ошибка.

    Инвариант контракта ``horizon_window`` (600 < horizon_s <= 900)
    проверяется; нарушение — ValueError.
    """
    rows: list[dict] = []
    with open(path, encoding='utf-8') as fh:
        for line in fh:
            if not line.strip():
                continue
            rec = json.loads(line)
            vals = dict(rec.get('values') or {})
            # horizon_s дублируется в values; сверяем и берём верхнеуровневый.
            h_dup = vals.pop('horizon_s', None)
            if h_dup is not None and float(h_dup) != float(rec['horizon_s']):
                raise ValueError(
                    f"{rec['sample_id']}: horizon_s в values ({h_dup}) "
                    f"не совпадает с верхнеуровневым ({rec['horizon_s']})"
                )
            row = {k: rec[k] for k in KEY_COLUMNS if k in rec}
            row['t'] = _parse_t(rec['t'])
            # quality-фичи лежат ВЕРХНИМ уровнем записи (не в values), всё
            # остальное из контракта — в values; отсутствующий ключ = null.
            for col_name in _ALL_CONTRACT_FEATURES:
                if col_name in CONTRACT_SECTIONS['quality']:
                    row[col_name] = rec.get(col_name)
                else:
                    row[col_name] = vals.get(col_name)
            unknown = set(vals) - set(_ALL_CONTRACT_FEATURES)
            if unknown:
                raise ValueError(f"{rec['sample_id']}: неизвестные поля values {sorted(unknown)}")
            rows.append(row)

    df = pl.DataFrame(rows, schema_overrides={**KEY_COLUMNS, **_ALL_CONTRACT_FEATURES})

    # Дедуп по sample_id с проверкой: unique() по всем колонкам не должен
    # терять строки — иначе под одним ключом лежат разные кадры.
    if df.height == 0:
        raise ValueError(f'{path}: файл кадров пуст')
    deduped = df.unique()
    if deduped.height != df.height:
        raise ValueError(
            f'{path}: дубликаты sample_id с расходящимися значениями '
            f'({df.height - deduped.height} строк)'
        )
    df = deduped
    n_dup = df.height - df['sample_id'].n_unique()
    if n_dup:
        raise ValueError(f'{path}: {n_dup} неидентичных кадров с общим sample_id')

    # Инвариант контракта: 600 < horizon_s <= 900.
    bad = df.filter(~((pl.col('horizon_s') > 600) & (pl.col('horizon_s') <= 900)))
    if bad.height:
        raise ValueError(
            f"{path}: {bad.height} кадров вне горизонта (600; 900], "
            f"например {bad['sample_id'].head(3).to_list()}"
        )

    return df.select(FRAME_COLUMNS)
