"""Сборка датасета: CLI end-to-end на мини-фикстуре + контроль реальных артефактов.

Мини-фикстура задаёт ручные ожидаемые значения оконных фич (окна, dwell-прокси,
граница as-of) и ловушку таргета: при непустом Go-``cur_dev_s`` дельта обязана
считаться с кадрового, а не меточного значения. Плюс фолбэк v1b: пустой Go
``cur_dev_s`` импутируется хинтом из labels с пометкой в ``cur_dev_from_hint``
(``--no-cur-dev-fallback-hint`` — строгое поведение v1).
"""

from __future__ import annotations

import datetime as dt
import json
from pathlib import Path

import polars as pl
import pytest

from predictor.dataset import main
from predictor.frames import load_frames

TRAFFIC_HEADER = ('packet_id,tr_id,unit_id,event_time,device_event_id,location_valid,'
                  'gps_time,lon,lat,alt,speed,heading,receive_time,is_hist_data')


def _frame(sample_id: str, tr_id: int, t: str, values: dict) -> str:
    return json.dumps({
        'sample_id': sample_id, 'unit_id': 900 + tr_id, 'tr_id': tr_id, 't': t,
        'target_stop_id': 53699018678, 'horizon_s': 720, 'ambiguous': False,
        'variants': 1, 'values': values, 'staleness_s': 4.1,
        'points_in_window': 10, 'lag_s': 0,
    })


def _traffic_row(tr_id: int, event_time: str, speed: str) -> str:
    return (f'p{tr_id}{event_time},{tr_id},{900 + tr_id},{event_time},d1,True,'
            f',37.6,55.7,,{speed},,2026-01-06 03:00:00,False')


@pytest.fixture()
def mini(tmp_path: Path) -> dict[str, Path]:
    frames = tmp_path / 'frames.jsonl'
    frames.write_text('\n'.join([
        # t в кадре — наивное МСК с «Z» (ловушка Go-сериализации)
        _frame('10001_1767665700', 10001, '2026-01-06T02:15:00Z', {'cur_dev_s': 100, 'speed_current': 0}),
        _frame('10001_1767666000', 10001, '2026-01-06T02:20:00Z', {'cur_dev_s': 50}),
        _frame('10002_1767666000', 10002, '2026-01-06T02:20:00Z', {'cur_dev_s': 0}),
        # кадр БЕЗ cur_dev_s в values (null, как все validate-кадры) — под фолбэк
        _frame('10003_1767666300', 10003, '2026-01-06T02:25:00Z', {'speed_current': 10}),
    ]) + '\n', encoding='utf-8')
    traffic = tmp_path / 'traffic.csv'
    traffic.write_text('\n'.join([
        TRAFFIC_HEADER,
        _traffic_row(10001, '2026-01-06 02:10:00', '0'),
        _traffic_row(10001, '2026-01-06 02:11:00', ''),  # пустой speed -> null
        _traffic_row(10001, '2026-01-06 02:12:00', '0'),
        _traffic_row(10001, '2026-01-06 02:16:00', '20'),
        _traffic_row(10001, '2026-01-06 02:18:00', '30'),
        _traffic_row(10001, '2026-01-06 02:19:59', '10'),
        _traffic_row(10001, '2026-01-06 02:20:30', '999'),  # строго в будущем для всех кадров
        _traffic_row(10002, '2026-01-06 02:19:00', '5'),
    ]) + '\n', encoding='utf-8')
    labels = tmp_path / 'labels.csv'
    # cur_dev_s меток (999/555) намеренно отличается от кадра (100/50):
    # дельта обязана считаться с кадрового (ADR 0004). У 10003 метка есть,
    # а в Go-кадре cur_dev_s null — её закрывает хинт-фолбэк (777).
    labels.write_text(
        'sample_id,tr_id,T,target_stop_id,target_time_begin,cur_dev_s,target_delay_s,target_class\n'
        '10001_1767665700,10001,2026-01-06 02:15:00,53699018678,'
        '2026-01-06 02:27:00.000000000,999,200,late\n'
        '10001_1767666000,10001,2026-01-06 02:20:00,53699018679,'
        '2026-01-06 02:32:00.000000000,555,-30,ontime\n'
        '10003_1767666300,10003,2026-01-06 02:25:00,53699018680,'
        '2026-01-06 02:37:00,777,400,late\n',
        encoding='utf-8',
    )
    return {'frames': frames, 'traffic': traffic, 'labels': labels, 'dir': tmp_path}


def _run_cli(mini, out_name: str, extra: list[str] | None = None) -> pl.DataFrame:
    out = mini['dir'] / out_name
    rc = main([
        '--frames', str(mini['frames']), '--traffic', str(mini['traffic']),
        '--labels', str(mini['labels']), '--out', str(out), *(extra or []),
    ])
    assert rc == 0
    return pl.read_parquet(out)


def test_cli_inner_join_mini(mini):
    ds = _run_cli(mini, 'ds.parquet')
    # 3-й кадр (10002) без метки выпадает при inner-склейке
    assert ds.height == 3
    assert ds.columns[:4] == ['sample_id', 'tr_id', 'unit_id', 't']
    f1 = ds.filter(pl.col('sample_id') == '10001_1767665700')
    f2 = ds.filter(pl.col('sample_id') == '10001_1767666000')
    # --- таргеты/ловушки ---
    assert f1['delay_delta_s'].item() == pytest.approx(100.0)  # 200 - 100 (cur_dev из КАДРА)
    assert f2['delay_delta_s'].item() == pytest.approx(-80.0)  # -30 - 50
    assert f1['target_delay_s'].item() == pytest.approx(200.0)
    assert f1['target_class'].item() == 'late'
    assert f1['target_time_begin'].item() == dt.datetime(2026, 1, 6, 2, 27)
    assert f1['cur_dev_s'].item() == pytest.approx(100.0)  # из values Go-кадра
    # --- окно/контекст, ручные значения ---
    assert f1['speed_mean_5m'].item() == pytest.approx(0.0)  # точки 02:10, 02:12 (02:11 — null)
    assert f1['zero_ratio_5m'].item() == pytest.approx(1.0)
    assert f1['headway_prev_s'].item() == pytest.approx(180.0)  # 02:15 - 02:12
    assert f1['dwell_rolling_5m_s'].item() == pytest.approx(240.0)  # 60 + clip(240 -> 180)
    assert f2['speed_mean_5m'].item() == pytest.approx(20.0)  # (20+30+10)/3; 999 из будущего не попал
    assert f2['speed_mean_1m'].item() == pytest.approx(10.0)
    assert f2['zero_ratio_5m'].item() == pytest.approx(0.0)
    assert f2['headway_prev_s'].item() == pytest.approx(1.0)
    # --- контекст ---
    assert f1['hour'].item() == 2 and f1['is_peak'].item() is False
    assert f1['day_of_week'].item() == 2  # вторник, ISO
    # --- пропущенные фичи контракта существуют и null ---
    for col in ('trend_5', 'dwell_p90_route_s', 'speed_deficit_ratio_5m',
                'n_vehicles_on_route', 'heading_error_deg', 'layover_min'):
        assert col in ds.columns
        assert ds[col].null_count() == ds.height


def test_cli_left_join_keeps_unlabeled(mini):
    ds = _run_cli(mini, 'ds_left.parquet', ['--labels-join', 'left'])
    assert ds.height == 4
    f3 = ds.filter(pl.col('sample_id') == '10002_1767666000')
    assert f3['target_delay_s'].item() is None
    assert f3['delay_delta_s'].item() is None
    assert f3['speed_mean_5m'].item() == pytest.approx(5.0)  # телеметрия своего tr_id


def test_cli_without_labels(mini):
    out = mini['dir'] / 'ds_nolabels.parquet'
    rc = main(['--frames', str(mini['frames']), '--traffic', str(mini['traffic']),
               '--out', str(out)])
    assert rc == 0
    ds = pl.read_parquet(out)
    assert ds.height == 4
    assert ds['target_class'].null_count() == 4


def test_cur_dev_hint_fallback_default_on(mini):
    """v1b: пустой Go cur_dev_s импутируется хинтом labels, флаг=1 (дефолт)."""
    ds = _run_cli(mini, 'ds_hint.parquet')
    h = ds.filter(pl.col('sample_id') == '10003_1767666300')
    assert h['cur_dev_s'].item() == pytest.approx(777.0)   # хинт из labels
    assert h['cur_dev_from_hint'].item() == 1
    assert h['delay_delta_s'].item() == pytest.approx(400.0 - 777.0)
    # непустые Go-значения не тронуты, флаг=0 (ловушка ADR 0004 в силе)
    for sid, val in (('10001_1767665700', 100.0), ('10001_1767666000', 50.0)):
        r = ds.filter(pl.col('sample_id') == sid)
        assert r['cur_dev_s'].item() == pytest.approx(val)
        assert r['cur_dev_from_hint'].item() == 0
    assert ds['delay_delta_s'].null_count() == 0
    assert 'hint_cur_dev_s' not in ds.columns


def test_cur_dev_hint_fallback_disabled(mini):
    """--no-cur-dev-fallback-hint: строгое поведение v1, null остаётся null."""
    ds = _run_cli(mini, 'ds_strict.parquet', ['--no-cur-dev-fallback-hint'])
    h = ds.filter(pl.col('sample_id') == '10003_1767666300')
    assert h['cur_dev_s'].item() is None
    assert h['cur_dev_from_hint'].item() == 0
    assert h['delay_delta_s'].item() is None
    assert ds['cur_dev_s'].null_count() == 1


def test_cur_dev_left_join_unlabeled_no_hint(mini):
    """Go-``cur_dev_s`` == 0 — валидное значение, фолбэк его не трогает."""
    ds = _run_cli(mini, 'ds_left_hint.parquet', ['--labels-join', 'left'])
    r = ds.filter(pl.col('sample_id') == '10002_1767666000')
    assert r['cur_dev_s'].item() == pytest.approx(0.0)  # Go-0 — не null, не трогать
    assert r['cur_dev_from_hint'].item() == 0
    # безметочный кадр при left-склейке остаётся, но хинта у него нет
    assert r['target_delay_s'].item() is None


# ------------------------- реальные артефакты -------------------------

ART = Path(__file__).resolve().parent.parent / 'artifacts'


def test_train_dataset_real(features_train_path):
    path = ART / 'dataset_train.parquet'
    if not path.exists():
        pytest.skip('dataset_train.parquet не собран (запусти python -m predictor.dataset)')
    ds = pl.read_parquet(path)
    assert ds.height == 4099  # все train-кадры покрыты labels_train
    assert ds['sample_id'].n_unique() == 4099
    # v1b: 377 кадров с null Go cur_dev_s импутированы хинтом labels_train
    assert 'cur_dev_from_hint' in ds.columns
    assert int(ds['cur_dev_from_hint'].sum()) == 377
    assert ds['cur_dev_from_hint'].dtype == pl.Int8
    assert ds['cur_dev_s'].null_count() == 0
    assert ds['delay_delta_s'].null_count() == 0
    ok = ds.filter(pl.col('delay_delta_s').is_not_null())
    assert ((ok['delay_delta_s'] - (ok['target_delay_s'] - ok['cur_dev_s'])).abs() < 1e-3).all()
    assert ds['target_delay_s'].null_count() == 0  # labels_train покрывает все кадры
    assert ds['speed_mean_10m'].null_count() <= 10  # почти все кадры имеют телеметрию до T
    # доли никогда не должны быть NaN (0/0 гасится в null)
    for m in (1, 3, 5, 10):
        assert not ds[f'zero_ratio_{m}m'].drop_nulls().is_nan().any()


def test_validate_dataset_real(features_validate_path):
    path = ART / 'dataset_validate.parquet'
    if not path.exists():
        pytest.skip('dataset_validate.parquet не собран')
    ds = pl.read_parquet(path)
    assert ds.height == 142  # left-склейка с points.csv сохраняет все кадры
    assert ds['target_time_begin'].null_count() == 0
    # меток target_delay для validate-сетки в раздаче нет — честные null
    assert ds['target_delay_s'].null_count() == 142
    assert ds['delay_delta_s'].null_count() == 142  # дельте не от чего считаться
    # Go-кадры validate без cur_dev_s (в schedule_plan.csv нет time_fact_begin):
    # весь столбец закрыт хинтом points.csv — источник истины для v1b-инференса
    assert ds['cur_dev_s'].null_count() == 0
    assert int(ds['cur_dev_from_hint'].sum()) == 142


def test_validate_has_zero_overlap_with_labels_test(repo_root, features_validate_path):
    """Зафиксированное расхождение покрытия: labels_test.csv — ДРУГАЯ сетка T.

    Поэтому dataset_validate собран не с labels/labels_test.csv (inner-склейка
    даёт ровно 0 строк), а с left-склейкой подсказки validate/points.csv.
    """
    ids = set(load_frames(features_validate_path)['sample_id'])
    labels = pl.read_csv(repo_root / 'labels' / 'labels_test.csv',
                         schema_overrides={'sample_id': pl.Utf8})
    assert len(ids) == 142
    assert len(set(labels['sample_id'])) == 353
    assert not (ids & set(labels['sample_id']))
