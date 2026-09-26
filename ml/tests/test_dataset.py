"""Сборка датасета: CLI end-to-end на мини-фикстуре + контроль реальных артефактов.

Мини-фикстура задаёт ручные ожидаемые значения оконных фич (окна, dwell-прокси,
граница as-of), фич движения #24 (trend_5/momentum по ряду отклонений,
dwell_p90_route_s по эпизодам у остановки, speed_deficit_ratio_5m против
train-профиля) и ловушку таргета: при непустом Go-``cur_dev_s`` дельта обязана
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
        _frame('10001_1767665700', 10001, '2026-01-06T02:15:00Z',
               {'cur_dev_s': 100, 'speed_current': 0, 'distance_to_target_m': 0}),
        _frame('10001_1767666000', 10001, '2026-01-06T02:20:00Z',
               {'cur_dev_s': 50, 'distance_to_target_m': 0}),
        _frame('10002_1767666000', 10002, '2026-01-06T02:20:00Z', {'cur_dev_s': 0}),
        # кадр БЕЗ cur_dev_s в values (null, как все validate-кадры) — под фолбэк
        _frame('10003_1767666300', 10003, '2026-01-06T02:25:00Z', {'speed_current': 10}),
    ]) + '\n', encoding='utf-8')
    traffic = tmp_path / 'traffic.csv'
    traffic.write_text('\n'.join([
        TRAFFIC_HEADER,
        # dwell-эпизоды для dwell_p90_route_s (обе серии <= 02:15, все кадры их видят)
        _traffic_row(10001, '2026-01-06 01:00:00', '0'),
        _traffic_row(10001, '2026-01-06 01:01:00', '0'),
        _traffic_row(10001, '2026-01-06 01:05:00', '20'),
        _traffic_row(10001, '2026-01-06 01:20:00', '0'),
        _traffic_row(10001, '2026-01-06 01:22:00', '0'),
        _traffic_row(10001, '2026-01-06 01:23:00', '40'),
        _traffic_row(10001, '2026-01-06 02:10:00', '0'),
        _traffic_row(10001, '2026-01-06 02:11:00', ''),  # пустой speed -> null
        _traffic_row(10001, '2026-01-06 02:12:00', '0'),
        _traffic_row(10001, '2026-01-06 02:16:00', '20'),
        _traffic_row(10001, '2026-01-06 02:18:00', '30'),
        _traffic_row(10001, '2026-01-06 02:19:59', '10'),
        _traffic_row(10001, '2026-01-06 02:20:30', '999'),  # строго в будущем для всех кадров
        _traffic_row(10002, '2026-01-06 02:19:00', '5'),
    ]) + '\n', encoding='utf-8')
    # график tr 10001: 7 остановок, отклонения [10,20,30,40,50,5,15];
    # 5 завершены к 02:15, все 7 — к 02:20. Остальные tr — без строк.
    # time_begin nanosecond-формат (ловушка train/schedule.csv).
    schedule = tmp_path / 'schedule.csv'
    schedule.write_text(
        'tt_action_item_id,time_begin,time_fact_begin,order_date,manual_fill,tr_id,geom,'
        'building_address\n'
        '11,2026-01-06 00:00:00.000000000,2026-01-06 00:00:10.000000000,2026-01-06,False,10001,'
        'POINT (37.6 55.7),A\n'
        '12,2026-01-06 00:01:00.000000000,2026-01-06 00:01:20.000000000,2026-01-06,False,10001,'
        'POINT (37.6 55.7),A\n'
        '13,2026-01-06 00:02:00.000000000,2026-01-06 00:02:30.000000000,2026-01-06,False,10001,'
        'POINT (37.6 55.7),A\n'
        '14,2026-01-06 00:03:00.000000000,2026-01-06 00:03:40.000000000,2026-01-06,False,10001,'
        'POINT (37.6 55.7),A\n'
        '53699018678,2026-01-06 00:04:00.000000000,2026-01-06 00:04:50.000000000,2026-01-06,False,'
        '10001,POINT (37.6 55.7),A\n'
        '16,2026-01-06 02:16:00.000000000,2026-01-06 02:16:05.000000000,2026-01-06,False,10001,'
        'POINT (37.6 55.7),A\n'
        '17,2026-01-06 02:18:00.000000000,2026-01-06 02:18:15.000000000,2026-01-06,False,10001,'
        'POINT (37.6 55.7),A\n',
        encoding='utf-8',
    )
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
    return {'frames': frames, 'traffic': traffic, 'labels': labels,
            'schedule': schedule, 'profile': traffic, 'dir': tmp_path}


def _run_cli(mini, out_name: str, extra: list[str] | None = None) -> pl.DataFrame:
    out = mini['dir'] / out_name
    rc = main([
        '--frames', str(mini['frames']), '--traffic', str(mini['traffic']),
        '--labels', str(mini['labels']), '--out', str(out), *(extra or []),
    ])
    assert rc == 0
    return pl.read_parquet(out)


def test_cli_inner_join_mini(mini):
    ds = _run_cli(mini, 'ds.parquet', extra=['--schedule', 'none'])
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


# --------------------- фичи движения (#24) на мини ---------------------

def _run_cli_motion(mini, out_name: str, extra: list[str] | None = None) -> pl.DataFrame:
    """CLI с --schedule/--profile: полный набор фич движения."""
    out = mini['dir'] / out_name
    rc = main([
        '--frames', str(mini['frames']), '--traffic', str(mini['traffic']),
        '--labels', str(mini['labels']),
        '--schedule', str(mini['schedule']), '--profile', str(mini['profile']),
        '--out', str(out), *(extra or []),
    ])
    assert rc == 0
    return pl.read_parquet(out)


def test_motion_features_mini_manual_values(mini):
    """trend_5/momentum/dwell_p90/deficit — ручные числа на 3-кадровой фикстуре.

    График tr 10001 даёт ряд dev [10,20,30,40,50,5,15] (5 завершено к 02:15,
    все 7 — к 02:20); dwell-эпизоды 300 с и 180 с у той же остановки;
    профиль (тот же traffic по всем tr_id, ненулевые скорости) — бакет
    (0 м, час 2): медиана [5,10,20,30,999] = 20 км/ч (строка 02:20:30 из
    «будущего» в неё входит — профиль по условию задачи считается из файла
    целиком).
    """
    ds = _run_cli_motion(mini, 'ds_motion.parquet', extra=['--labels-join', 'left'])
    assert ds.height == 4
    f1 = ds.filter(pl.col('sample_id') == '10001_1767665700')
    # trend_5: МНК по [10,20,30,40,50] => +10 с/остановку; momentum: 50-40=10
    assert f1['trend_5'].item() == pytest.approx(10.0, abs=1e-4)
    assert f1['momentum'].item() == pytest.approx(10.0)
    # dwell_p90: квантиль 0.9 (linear) по [180, 300] = 288
    assert f1['dwell_p90_route_s'].item() == pytest.approx(288.0, abs=0.5)
    # deficit: 1 - 0/20 = 1.0 (f1 стоит; профильный бакет (0м, 2ч))
    assert f1['speed_deficit_ratio_5m'].item() == pytest.approx(1.0)
    f2 = ds.filter(pl.col('sample_id') == '10001_1767666000')
    # к 02:20 завершены все 7; tail5 = [30,40,50,5,15]:
    # sx=10 sy=140 sxy=0*30+1*40+2*50+3*5+4*15=215 sx2=30 n=5
    # slope=(5*215-10*140)/(5*30-100)=(1075-1400)/50=-6.5
    assert f2['trend_5'].item() == pytest.approx(-6.5, abs=0.5)
    # momentum = dev_7 - dev_6 (последняя разность полного ряда) = 15-5
    assert f2['momentum'].item() == pytest.approx(10.0)
    # dwell-эпизоды тот же набор (все завершены до 02:20) — 288
    assert f2['dwell_p90_route_s'].item() == pytest.approx(288.0, abs=0.5)
    # deficit f2: 1 - 20/20 = 0.0 (speed_mean_5m равен медиане профиля)
    assert f2['speed_deficit_ratio_5m'].item() == pytest.approx(0.0, abs=0.01)
    # tr 10002/10003: нет строк графика => trend_5/momentum null; цель та же
    # остановка => dwell_p90 считается; deficit: f3 distance null => null
    f3 = ds.filter(pl.col('sample_id') == '10002_1767666000')
    assert f3['trend_5'].item() is None and f3['momentum'].item() is None
    assert f3['dwell_p90_route_s'].item() == pytest.approx(288.0, abs=0.5)
    assert f3['speed_deficit_ratio_5m'].item() is None
    f4 = ds.filter(pl.col('sample_id') == '10003_1767666300')
    assert f4['trend_5'].item() is None and f4['speed_deficit_ratio_5m'].item() is None


def test_motion_features_without_inputs_stay_null(mini):
    """--schedule none / без --profile: trend_5/momentum/dwell/deficit все-null (v1-поведение)."""
    ds = _run_cli(mini, 'ds_nomotion.parquet', extra=['--schedule', 'none'])
    for col in ('trend_5', 'momentum', 'dwell_p90_route_s', 'speed_deficit_ratio_5m'):
        assert ds[col].null_count() == ds.height


def test_plan_without_facts_gives_null_trend(mini):
    """schedule_plan без time_fact_begin (ловушка validate) => trend_5/momentum null."""
    plan = mini['dir'] / 'schedule_plan.csv'
    plan.write_text(
        'tt_action_item_id,time_begin,order_date,manual_fill,tr_id,geom,building_address\n'
        '11,2026-01-06 00:00:00,2026-01-06,False,10001,POINT (37.6 55.7),A\n'
        '53699018678,2026-01-06 00:04:00,2026-01-06,False,10001,POINT (37.6 55.7),A\n'
        '16,2026-01-06 02:16:00,2026-01-06,False,10001,POINT (37.6 55.7),A\n',
        encoding='utf-8',
    )
    out = mini['dir'] / 'ds_plan.parquet'
    rc = main([
        '--frames', str(mini['frames']), '--traffic', str(mini['traffic']),
        '--labels', str(mini['labels']), '--schedule', str(plan),
        '--profile', str(mini['profile']), '--out', str(out),
    ])
    assert rc == 0
    ds = pl.read_parquet(out)
    assert ds['trend_5'].null_count() == ds.height
    assert ds['momentum'].null_count() == ds.height
    # dwell/deficit считаются и на плане (эпизоды — из traffic, не из фактов)
    assert ds['dwell_p90_route_s'].null_count() < ds.height


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
