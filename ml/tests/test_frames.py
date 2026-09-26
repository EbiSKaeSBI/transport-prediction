"""Загрузка реальных JSONL-кадров Go-replay в схему контракта v1."""

from __future__ import annotations

import datetime as dt

import polars as pl
import pytest

from predictor.frames import FRAME_COLUMNS, load_frames


@pytest.fixture(scope='module')
def train(features_train_path) -> pl.DataFrame:
    return load_frames(features_train_path)


def test_rows_count(train):
    assert train.height == 4099
    assert train['sample_id'].n_unique() == 4099


def test_contract_schema(train):
    # Все имена фич контракта (given/schedule/go_state/quality) присутствуют.
    assert train.columns == FRAME_COLUMNS
    schema = train.schema
    assert schema['cur_dev_s'] == pl.Float32
    assert schema['speed_current'] == pl.Float32
    assert schema['stops_remaining'] == pl.Int32
    assert schema['is_terminal_stop'] == pl.Boolean
    assert schema['manual_fill'] == pl.Boolean
    assert schema['ambiguous'] == pl.Boolean
    assert schema['t'] == pl.Datetime('us')
    assert schema['target_stop_id'] == pl.Int64  # контракт int32 не влезает в данные


def test_naive_msk_time_no_zone_conversion(train):
    # '2026-01-06T02:15:00Z' — это наивное стенное МСК: Z срезан, НЕ конвертировано.
    row = train.filter(pl.col('sample_id') == '122048_1767665700')
    assert row['t'].item() == dt.datetime(2026, 1, 6, 2, 15)
    assert row['t'].dtype.time_zone is None


def test_missing_value_keys_are_null_not_zero(train):
    # У этих двух кадров ключа cur_dev_s в values нет -> null (проверено по исходному JSONL).
    two = train.filter(pl.col('sample_id').is_in(['122048_1767665700', '122048_1767666000']))
    assert two.height == 2
    assert two['cur_dev_s'].null_count() == 2
    present = train.filter(pl.col('sample_id') == '122048_1767666300')
    assert present['cur_dev_s'].item() == pytest.approx(0.0)
    # heading_error_deg и layover_min не приходят из Go никогда -> целые null-колонки.
    assert train['heading_error_deg'].null_count() == train.height
    assert train['layover_min'].null_count() == train.height


def test_quality_features_present(train):
    # quality-фичи — верхний уровень записи JSONL, не values.
    assert train['staleness_s'].null_count() == 0
    assert train['points_in_window'].null_count() == 0
    assert train['lag_s'].null_count() == 0


def test_horizon_range(train):
    # Инвариант контракта 600 < horizon_s <= 900 проверяется загрузчиком
    # (нарушение -> ValueError); здесь фиксируем фактический диапазон.
    assert train['horizon_s'].min() > 600
    assert train['horizon_s'].max() <= 900


def test_validate_file(features_validate_path):
    v = load_frames(features_validate_path)
    assert v.height == 142
    assert v.columns == FRAME_COLUMNS
    # В validate-кадрах Go не отдал ни одного cur_dev_s (факт раздачи).
    assert v['cur_dev_s'].null_count() == 142


def test_duplicate_with_conflict_rejected(tmp_path):
    line_ok = (
        '{"sample_id":"1_2","unit_id":1,"tr_id":1,"t":"2026-01-06T02:15:00Z",'
        '"target_stop_id":10,"horizon_s":720,"ambiguous":false,"variants":1,'
        '"values":{"cur_dev_s":1},"staleness_s":1.0,"points_in_window":5,"lag_s":0}'
    )
    line_conflict = line_ok.replace('"cur_dev_s":1', '"cur_dev_s":2')
    p = tmp_path / 'dup.jsonl'
    p.write_text(line_ok + '\n' + line_conflict + '\n', encoding='utf-8')
    with pytest.raises(ValueError, match='sample_id'):
        load_frames(p)
