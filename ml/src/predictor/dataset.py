"""Сборка прогнозного датасета: кадры Go-replay ⨝ окно/контекст ⨝ метки.

CLI (запускать от корня репозитория):

    python -m predictor.dataset \\
        --frames ml/artifacts/features_train.jsonl \\
        --traffic train/traffic.csv \\
        --labels labels/labels_train.csv \\
        --out ml/artifacts/dataset_train.parquet

Колонки результата: ключ (``sample_id, tr_id, unit_id, t, target_stop_id,
horizon_s, target_time_begin``) + все фичи контракта v1 (given/schedule/
go_state из кадра, window/context из :mod:`predictor.window`, quality из
кадра) + таргеты ``target_delay_s``, ``target_class`` и вычисляемый
``delay_delta_s = target_delay_s − cur_dev_s``.

Таргетная ловушка (ADR 0004): ``cur_dev_s`` — из значений Go-кадра (секция
given). По умолчанию (v1b) включён fallback-импутация: если Go-значение null
(в validate-кадрах колонка пуста целиком — в ``schedule_plan.csv`` нет
``time_fact_begin``), берётся хинт ``cur_dev_s`` из labels/points; исход
фиксируется диагностической колонкой ``cur_dev_from_hint`` (Int8, в фичи
модели не входит, см. :data:`predictor.features.EXCLUDED_COLUMNS`). Флаг
``--no-cur-dev-fallback-hint`` возвращает строгое поведение v1: хинт
игнорируется, null остаётся null, ``cur_dev_from_hint`` — нули.

``delay_delta_s`` считается от итогового (возможно, импутированного)
``cur_dev_s``; при обоих null дельта null.

Метки склеиваются внутренней склейкой по ``sample_id`` (формат
``<tr_id>_<epoch(T)>``, epoch — naive-МСК как timegm). ``target_time_begin``
в Go-кадре отсутствует и приходит из labels.

Честное покрытие меток (замерено): ``labels/labels_train.csv`` покрывает все
4099 train-кадров; ``labels/labels_test.csv`` построен по ДРУГОЙ сетке точек
T и не пересекается с ``features_validate.jsonl`` ни одним ``sample_id``
(0 из 142). Поэтому для validate артефакта меток в раздаче нет: CLI принимает
``--labels`` опционально, а роль «метки» для validate играет подсказка
``validate/points.csv`` (в ней есть sample_id/tr_id/target_stop_id/
target_time_begin; target_delay_s/target_class остаются null). Финальная
валидация модели по меткам — задача evaluate-сплита, а не этого артефакта.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import polars as pl

from predictor.frames import CONTRACT_SECTIONS, load_frames
from predictor.window import WINDOW_FEATURE_COLUMNS, build_window_features

#: Схема телеметрии traffic.csv: packet_id/device_event_id — строковые id вида
#: "9000000_0"; event_time/receive_time — naive-МСК, как и t кадров.
TRAFFIC_SCHEMA: dict[str, pl.DataType] = {
    'packet_id': pl.Utf8,
    'tr_id': pl.Int64,
    'unit_id': pl.Int64,
    'event_time': pl.Datetime('us'),
    'device_event_id': pl.Utf8,
    'location_valid': pl.Boolean,
    'gps_time': pl.Utf8,
    'lon': pl.Float64,
    'lat': pl.Float64,
    'alt': pl.Float64,
    'speed': pl.Float64,
    'heading': pl.Float64,
    'receive_time': pl.Datetime('us'),
    'is_hist_data': pl.Boolean,
}

#: Колонки меток, попадающие в датасет (кроме ключа sample_id).
_LABEL_COLUMNS: dict[str, pl.DataType] = {
    'target_time_begin': pl.Datetime('us'),
    'target_delay_s': pl.Float32,
    'target_class': pl.Utf8,
}

#: Внутреннее имя хинта cur_dev_s из labels (в датасет не попадает, только
#: для imputation-фолбэка, см. module docstring).
HINT_COLUMN = 'hint_cur_dev_s'


def load_traffic(path: str | Path) -> pl.DataFrame:
    """Прочитать traffic.csv со схемой TRAFFIC_SCHEMA (naive-МСК без поясов)."""
    return pl.read_csv(path, schema=TRAFFIC_SCHEMA)


def load_labels(path: str | Path) -> pl.DataFrame:
    """Прочитать файл меток/подсказок по sample_id.

    Принимает и ``labels/labels_*.csv`` (полные метки), и
    ``validate/points.csv`` (подсказка организаторов: только тайминги цели).
    Отсутствующие колонки-таргеты дополняются null. Хинт ``cur_dev_s`` из
    файла меток сохраняется под внутренним именем :data:`HINT_COLUMN`
    (или как колонка всех null, если его нет) — дальше он либо идёт в
    imputation-фолбэк, либо отбрасывается при
    ``--no-cur-dev-fallback-hint``.
    """
    raw = pl.read_csv(path, schema_overrides={'sample_id': pl.Utf8})
    if 'sample_id' not in raw.columns:
        raise ValueError(f'{path}: нет колонки sample_id')
    if raw['sample_id'].n_unique() != raw.height:
        raise ValueError(f'{path}: sample_id не уникален')
    cols = [pl.col('sample_id')]
    for name, dtype in _LABEL_COLUMNS.items():
        if name not in raw.columns:
            cols.append(pl.lit(None, dtype=dtype).alias(name))
        elif dtype == pl.Datetime('us'):
            # В labels/points дробная секунда бывает nanosecond-формата
            # ".000000000" (покрывает все фактические значения — времена цели
            # целочисленно-секундные); polars-cast на неё спотыкается,
            # поэтому режем строку до секунд и парсим как naive-МСК.
            cols.append(
                pl.col(name).cast(pl.Utf8).str.slice(0, 19).str.to_datetime().cast(dtype)
            )
        else:
            cols.append(pl.col(name).cast(dtype))
    if 'cur_dev_s' in raw.columns:
        cols.append(pl.col('cur_dev_s').cast(pl.Float32).alias(HINT_COLUMN))
    else:
        cols.append(pl.lit(None, dtype=pl.Float32).alias(HINT_COLUMN))
    return raw.select(cols)


def _dataset_column_order() -> list[str]:
    order = ['sample_id', 'tr_id', 'unit_id', 't', 'target_stop_id', 'horizon_s',
             'ambiguous', 'variants', 'target_time_begin']
    for section in ('given', 'schedule', 'go_state'):
        order += list(CONTRACT_SECTIONS[section])
        if section == 'given':
            # диагностика фолбэка cur_dev_s — сразу за исходным признаком
            order.append('cur_dev_from_hint')
    order += ['target_ambiguous', *WINDOW_FEATURE_COLUMNS]
    order += list(CONTRACT_SECTIONS['quality'])
    order += ['target_delay_s', 'target_class', 'delay_delta_s']
    return order


def build_dataset(
    frames_path: str | Path,
    traffic_path: str | Path,
    labels_path: str | Path | None = None,
    *,
    join: str = 'inner',
    cur_dev_fallback_hint: bool = True,
) -> pl.DataFrame:
    """Кадры ⨝ окно/контекст ⨝ метки. ``join`` = 'inner' | 'left' (про метки).

    inner (дефолт, по задаче): остаются только кадры с меткой.
    left: сохраняются все кадры, у безметочных таргеты null — так собирается
    validate-артефакт, для которого меток в раздаче нет (см. module docstring).

    ``cur_dev_fallback_hint`` (дефолт True, вариант v1b): где Go-``cur_dev_s``
    null, подставить хинт из labels/points и пометить строку в
    ``cur_dev_from_hint``; False — строгое поведение v1 (null остаётся null).
    """
    if join not in ('inner', 'left'):
        raise ValueError(f"join должен быть 'inner' или 'left', получено {join!r}")
    frames = load_frames(frames_path)
    traffic = load_traffic(traffic_path)
    win = build_window_features(frames, traffic)

    df = frames.join(win.drop('t'), on='sample_id', how='left')
    assert df.height == frames.height, 'оконные фичи потеряли кадры'

    if labels_path is not None:
        labels = load_labels(labels_path)
        before = df.height
        df = df.join(labels, on='sample_id', how=join)
        matched = df.height
        print(
            f'метки {labels_path}: кадров {before}, после {join}-склейки {matched}'
            + (' (потерянных кадров нет)' if matched == before else
               f' — потеряно {before - matched} кадров без метки'),
            file=sys.stderr,
        )
    else:
        # Меток нет вовсе: таргет-колонки существуют и равны null (схема датасета
        # не зависит от наличия --labels).
        df = df.with_columns(
            pl.lit(None, dtype=dtype).alias(name) for name, dtype in _LABEL_COLUMNS.items()
        )
        df = df.with_columns(pl.lit(None, dtype=pl.Float32).alias(HINT_COLUMN))

    # --- фолбэк cur_dev_s хинтом из labels/points (вариант v1b) ---
    if cur_dev_fallback_hint:
        df = df.with_columns(
            cur_dev_from_hint=(
                pl.col('cur_dev_s').is_null() & pl.col(HINT_COLUMN).is_not_null()
            ).cast(pl.Int8),
            cur_dev_s=pl.coalesce('cur_dev_s', HINT_COLUMN).cast(pl.Float32),
        )
        n_hint = int(df['cur_dev_from_hint'].sum())
        n_null = df['cur_dev_s'].null_count()
        print(
            f'cur_dev_s: импутировано хинтом {n_hint}/{df.height} строк, '
            f'осталось null {n_null}',
            file=sys.stderr,
        )
    else:
        df = df.with_columns(
            cur_dev_from_hint=pl.lit(0, dtype=pl.Int8),
        )
    df = df.drop(HINT_COLUMN)

    # delay_delta_s = target_delay_s − cur_dev_s (от итогового cur_dev_s).
    df = df.with_columns(
        delay_delta_s=(pl.col('target_delay_s') - pl.col('cur_dev_s')).cast(pl.Float32)
    )
    missing = set(_dataset_column_order()) - set(df.columns)
    if missing:
        raise RuntimeError(f'внутренняя ошибка сборки, нет колонок {sorted(missing)}')
    return df.select(_dataset_column_order())


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog='python -m predictor.dataset',
        description='Сборка parquet-датасета: кадры Go-replay ⨝ окно ⨝ метки.',
    )
    parser.add_argument('--frames', required=True, help='JSONL кадров (ml/artifacts/features_*.jsonl)')
    parser.add_argument('--traffic', required=True, help='traffic.csv того же сплита')
    parser.add_argument('--labels', default=None,
                        help='labels/*.csv или validate/points.csv; без него таргеты null')
    parser.add_argument('--labels-join', choices=('inner', 'left'), default='inner',
                        help="склейка меток: inner (по задаче) или left (все кадры)")
    parser.add_argument('--cur-dev-fallback-hint', action=argparse.BooleanOptionalAction,
                        default=True,
                        help='импутировать пустой cur_dev_s хинтом из labels/points '
                             'с пометкой в cur_dev_from_hint (дефолт вкл; '
                             '--no-... — строгое поведение v1)')
    parser.add_argument('--out', required=True, help='выходной parquet')
    args = parser.parse_args(argv)

    df = build_dataset(args.frames, args.traffic, args.labels, join=args.labels_join,
                       cur_dev_fallback_hint=args.cur_dev_fallback_hint)
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    df.write_parquet(out)
    print(f'{out}: {df.height} строк × {df.width} колонок', file=sys.stderr)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
