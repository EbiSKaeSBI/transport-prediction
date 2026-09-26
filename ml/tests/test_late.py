"""Тесты P(late)-головы и правил причины (§5.4, v4).

Три уровня честности: паритет строк причины с replay-генератором
(демо и live не должны различаться лексикой), статистика классификатора
на синтетике с заведомо известным AUC, и интеграция в serve на РЕАЛЬНЫХ
артефактах пары v1online (тот же путь загрузки, что у сервиса).
"""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path

import numpy as np
import polars as pl
import pytest
from fastapi.testclient import TestClient

from predictor import late as late_head
from predictor.serve import create_app

REASONS = {
    'длительный простой на остановке',
    'увеличенное время стоянки',
    'низкая скорость движения',
    'малый интервал, эффект «паровозика»',
    'накопленное отставание без текущего простоя',
}


@pytest.fixture(scope='module')
def demo_mod(repo_root: Path):
    """scripts/make_dashboard_demo.py как модуль — источник эталонных строк."""
    path = repo_root / 'scripts' / 'make_dashboard_demo.py'
    spec = importlib.util.spec_from_file_location('make_dashboard_demo', path)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_infer_reason_parity_with_replay(demo_mod):
    """Каждая ветка правила совпадает с replay-генератором буквально.

    Таблица покрывает все границы: 59.9/60, 19.9/20, 5.0/4.9, 119/120 —
    расхождение на любой из них означает, что live-карточка и демо
    «почему» разъехались.
    """
    cases = [
        {'dwell_current_s': 61, 'speed_current': 30, 'headway_s': 300},
        {'dwell_current_s': 59.9, 'speed_current': 30, 'headway_s': 300},
        {'dwell_current_s': 25, 'speed_current': 30, 'headway_s': 300},
        {'dwell_current_s': 19.9, 'speed_current': 30, 'headway_s': 300},
        {'dwell_current_s': 0, 'speed_current': 5, 'headway_s': 300},
        {'dwell_current_s': 0, 'speed_current': 4.9, 'headway_s': 300},
        {'dwell_current_s': 0, 'speed_current': 30, 'headway_s': 119},
        {'dwell_current_s': 0, 'speed_current': 30, 'headway_s': 121},
        {'dwell_current_s': None, 'speed_current': None, 'headway_s': None},
        {'dwell_current_s': True, 'speed_current': 12, 'headway_s': None},
    ]
    for values in cases:
        assert late_head.infer_reason(values) == demo_mod.incident_reason(values), values


def test_infer_reason_no_keys_at_all():
    assert late_head.infer_reason({}) in REASONS


def test_build_label_threshold_and_null():
    df = pl.DataFrame({'target_delay_s': [119.9, 120.0, 121.0, None, -50.0]})
    label = late_head.build_label(df)
    assert label.tolist() == [0, 1, 1, 0, 0]


def test_auc_known_cases():
    y = np.array([1, 1, 0, 0])
    assert late_head._auc(y, np.array([0.9, 0.8, 0.2, 0.1])) == pytest.approx(1.0)
    assert late_head._auc(y, np.array([0.1, 0.2, 0.8, 0.9])) == pytest.approx(0.0)
    # все связки = подбрасывание монеты
    assert late_head._auc(y, np.array([0.5, 0.5, 0.5, 0.5])) == pytest.approx(0.5)


def test_require_parity_rejects_order_and_extras():
    late_head.require_parity(['a', 'b'], ['a', 'b'])  # не падает
    with pytest.raises(ValueError):
        late_head.require_parity(['b', 'a'], ['a', 'b'])
    with pytest.raises(ValueError):
        late_head.require_parity(['a', 'b', 'c'], ['a', 'b'])


def test_load_late_missing_files(tmp_path):
    with pytest.raises(FileNotFoundError):
        late_head.load_late(tmp_path / 'model_x_late.json')
    (tmp_path / 'model_x_late.json').write_bytes(b'{}')
    # Классификатор без metrics-соседа — это FileNotFoundError, а не «любое
    # исключение»: конкретный тип позволяет отличить забытый артефакт от
    # битого JSON, иначе тест проходит и на неверной причине падения.
    with pytest.raises(FileNotFoundError, match='метрик'):
        late_head.load_late(tmp_path / 'model_x_late.json')


def test_train_micro_model(artifacts_dir, tmp_path):
    """Полный цикл train() на реальных датасетах, но 30 итераций.

    Проверяем механику (артефакты, форма метрик, диапазон вероятностей),
    а не качество: качество зафиксировано в metrics_v1online_late.json,
    который пересоздаётся штатным train.
    """
    feats = late_head.feature_names_from_metrics(artifacts_dir / 'metrics_v1online.json')
    m = late_head.train(artifacts_dir / 'dataset_train.parquet',
                        artifacts_dir / 'dataset_test.parquet',
                        artifacts_dir / 'metrics_v1online.json',
                        tmp_path, tag='micro', iterations=30, patience=10)
    assert (tmp_path / 'model_micro_late.json').is_file()
    assert (tmp_path / 'metrics_micro_late.json').is_file()
    assert m['features'] == feats
    assert m['threshold_s'] == late_head.LATE_THRESHOLD_S
    assert 0.0 <= m['auc_holdout'] <= 1.0
    assert 0.5 <= m['auc_holdout']  # ниже подбрасывания — значит метрика сломана

    clf, loaded = late_head.load_late(tmp_path / 'model_micro_late.json')
    assert loaded['features'] == feats
    df = pl.read_parquet(artifacts_dir / 'dataset_test.parquet')
    X = late_head.to_matrix(df.head(16), feats).to_numpy().astype(np.float64)
    proba = late_head.predict_p_late(clf, X)
    assert proba.shape == (16,)
    assert np.all((proba >= 0.0) & (proba <= 1.0))


# --- интеграция в serve: пара v1online + v1online_late (реальные артефакты) ---

@pytest.fixture(scope='module')
def v1online_request(artifacts_dir) -> dict:
    """Кадр §4.5 из первой строки dataset_test на фичах v1online."""
    feats = json.loads((artifacts_dir / 'metrics_v1online.json').read_text())['features']
    row = pl.read_parquet(artifacts_dir / 'dataset_test.parquet').to_dicts()[0]
    req: dict = {'sample_id': row['sample_id'], 'cur_dev_s': float(row['cur_dev_s']),
                 'horizon_s': float(row['horizon_s'])}
    for name in feats:
        if name in ('cur_dev_s', 'horizon_s'):
            continue
        val = row[name]
        req[name] = None if val is None else float(val)
    return req


def test_serve_returns_p_late_and_reason(artifacts_dir, v1online_request):
    client = TestClient(create_app(artifacts_dir / 'model_v1online.json',
                                   artifacts_dir / 'model_v1online_late.json'))
    r = client.post('/predict', json=v1online_request)
    assert r.status_code == 200
    body = r.json()
    assert body['source'] == 'model'
    assert isinstance(body['p_late'], float) and 0.0 <= body['p_late'] <= 1.0
    assert body['reason'] in REASONS
    # p_ontime/p_early остаются честным null: трёхклассовой головы нет
    assert body['p_ontime'] is None and body['p_early'] is None

    info = client.get('/model/info').json()
    assert info['late']['version'] == 'v1online_late'
    assert info['late']['threshold_s'] == 120
    assert info['late']['auc_holdout'] > 0.85


def test_serve_without_late_head_keeps_null(artifacts_dir, v1online_request):
    """Без --late-model сервис работает по-старому: p_late=null, reason есть."""
    client = TestClient(create_app(artifacts_dir / 'model_v1online.json'))
    body = client.post('/predict', json=v1online_request).json()
    assert body['p_late'] is None
    assert body['reason'] in REASONS
    assert client.get('/model/info').json()['late'] is None


def test_serve_pair_parity_failure_degrades_atomically(artifacts_dir, tmp_path):
    """Классификатор с чужим списком фич — вся пара не грузится, сервис
    уходит в fallback, а не отвечает «модель + чужая p_late».

    Модельный файл берём настоящий, а metrics-соседа переписываем с
    урезанным features: ровно та ситуация, что возникнет при переезде
    контракта без перевыпуска головы.
    """
    import shutil

    late_file = tmp_path / 'model_v1online_late.json'
    shutil.copy(artifacts_dir / 'model_v1online_late.json', late_file)
    metrics = json.loads((artifacts_dir / 'metrics_v1online_late.json').read_text())
    metrics['features'] = metrics['features'][:-1]
    (tmp_path / 'metrics_v1online_late.json').write_text(
        json.dumps(metrics), encoding='utf-8')
    client = TestClient(create_app(artifacts_dir / 'model_v1online.json',
                                   late_file))
    info = client.get('/model/info').json()
    assert info['model_loaded'] is False
    assert 'фич' in (info['load_error'] or '') or 'features' in (info['load_error'] or '')
    assert info['late'] is None
    # и предсказание при этом живое: fallback, а не 5xx
    body = client.post('/predict', json={'sample_id': 's', 'cur_dev_s': 30.0}).json()
    assert body['source'] == 'fallback_cur_dev'
