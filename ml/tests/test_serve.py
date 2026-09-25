"""Тесты ML-сервиса predictor.serve (FastAPI TestClient, задача #26).

Инференс идёт на РЕАЛЬНЫХ артефактах этапа 3 (v3b/v3c) и фичах из первой
строки dataset_validate.parquet: ожидаемый delay_s сверяется с
predictions_validate_v3b.csv (тот же пайплайн через общую композицию).
"""

from __future__ import annotations

import csv

import polars as pl
import pytest
from fastapi.testclient import TestClient

from predictor.serve import create_app

MODEL_V3B = 'model_v1v3b.json'
MODEL_V3C = 'model_v1v3c.json'


@pytest.fixture(scope='module')
def validate_rows(artifacts_dir) -> list[dict]:
    return pl.read_parquet(artifacts_dir / 'dataset_validate.parquet').to_dicts()


@pytest.fixture(scope='module')
def v3b_features(artifacts_dir) -> list[str]:
    import json
    return json.loads((artifacts_dir / 'metrics_v1v3b.json').read_text())['features']


@pytest.fixture(scope='module')
def v3b_and_v3c_features(artifacts_dir) -> list[str]:
    """Объединение фич v3b и v3c: запрос годен для обеих моделей (лишние
    ключи сервер игнорирует, недостающих ни у одной модели нет)."""
    import json
    union: list[str] = []
    for tag in ('v3b', 'v3c'):
        feats = json.loads((artifacts_dir / f'metrics_v1{tag}.json').read_text())['features']
        union.extend(f for f in feats if f not in union)
    return union


@pytest.fixture(scope='module')
def v3b_predictions(artifacts_dir) -> dict[str, float]:
    out = {}
    with (artifacts_dir / 'predictions_validate_v3b.csv').open(newline='') as fh:
        for row in csv.DictReader(fh, delimiter=';'):
            out[row['sample_id']] = float(row['prediction'])
    return out


def make_request(row: dict, features: list[str]) -> dict:
    """Feature vector контракта §4.5: выделенные поля + плоские имена фич."""
    req: dict = {
        'sample_id': row['sample_id'],
        'cur_dev_s': float(row['cur_dev_s']),
        'horizon_s': float(row['horizon_s']),
    }
    for name in features:
        if name in ('cur_dev_s', 'horizon_s'):
            continue
        val = row[name]
        req[name] = None if val is None else float(val)
    return req


@pytest.fixture(scope='module')
def client_model(artifacts_dir) -> TestClient:
    return TestClient(create_app(artifacts_dir / MODEL_V3B))


class TestPredictHappyPath:
    def test_first_row_matches_cli_pipeline(self, client_model, validate_rows,
                                            v3b_features, v3b_predictions):
        req = make_request(validate_rows[0], v3b_features)
        resp = client_model.post('/predict', json=req)
        assert resp.status_code == 200
        body = resp.json()
        assert body['source'] == 'model'
        assert body['model_version'] == 'v1v3b'
        # predictions_validate_v3b.csv округлён до 3 знаков (round в predict.py)
        assert body['delay_s'] == pytest.approx(v3b_predictions[req['sample_id']], abs=1e-3)
        assert body['horizon_min'] == pytest.approx(req['horizon_s'] / 60.0)
        # классификатор P(late) — только v4/#9; reason строит Go-шлюз
        assert body['p_late'] is None and body['p_ontime'] is None
        assert body['p_early'] is None and body['reason'] is None

    def test_batch_of_three(self, client_model, validate_rows, v3b_features, v3b_predictions):
        reqs = [make_request(r, v3b_features) for r in validate_rows[:3]]
        resp = client_model.post('/predict/batch', json=reqs)
        assert resp.status_code == 200
        answers = resp.json()
        assert len(answers) == 3
        for req, ans in zip(reqs, answers, strict=True):
            assert ans['sample_id'] == req['sample_id']
            assert ans['delay_s'] == pytest.approx(v3b_predictions[req['sample_id']], abs=1e-3)
            assert ans['source'] == 'model'

    def test_unknown_extra_keys_ignored(self, client_model, validate_rows, v3b_features):
        req = make_request(validate_rows[0], v3b_features)
        req['some_future_feature'] = 123.0
        resp = client_model.post('/predict', json=req)
        assert resp.status_code == 200
        assert resp.json()['source'] == 'model'

    def test_missing_feature_is_422(self, client_model, validate_rows, v3b_features):
        req = make_request(validate_rows[0], v3b_features)
        removed = next(f for f in v3b_features if f not in ('cur_dev_s', 'horizon_s'))
        del req[removed]
        resp = client_model.post('/predict', json=req)
        assert resp.status_code == 422
        assert removed in resp.json()['detail']


class TestModelReload:
    def test_reload_swaps_version_and_back(self, artifacts_dir, validate_rows,
                                           v3b_and_v3c_features):
        client = TestClient(create_app(artifacts_dir / MODEL_V3B))
        req = make_request(validate_rows[0], v3b_and_v3c_features)
        before = client.post('/predict', json=req).json()

        resp = client.post('/model/reload', json={'model': str(artifacts_dir / MODEL_V3C)})
        assert resp.status_code == 200
        assert resp.json() == {
            'status': 'reloaded', 'old_model': 'v1v3b', 'active_model': 'v1v3c',
            'target_mode': 'delta', 'feature_count': 35,
        }
        # ответ другой модели обязан измениться (разные списки фич/итерации)
        after = client.post('/predict', json=req)
        assert after.status_code == 200  # лишние фичи v3b игнорируются, v3c-фичи в запросе есть
        assert after.json()['model_version'] == 'v1v3c'
        assert after.json()['delay_s'] != pytest.approx(before['delay_s'], abs=1e-6)

        back = client.post('/model/reload', json={'model': str(artifacts_dir / MODEL_V3B)})
        assert back.json()['active_model'] == 'v1v3b'
        restored = client.post('/predict', json=req).json()
        assert restored['delay_s'] == pytest.approx(before['delay_s'], abs=1e-6)

    def test_reload_broken_path_keeps_old_model(self, artifacts_dir, validate_rows, v3b_features):
        client = TestClient(create_app(artifacts_dir / MODEL_V3B))
        resp = client.post('/model/reload', json={'model': '/nonexistent/model.json'})
        assert resp.status_code == 400
        assert 'FileNotFoundError' in resp.json()['detail']
        assert resp.json()['active_model'] == 'v1v3b'
        # СТАРАЯ модель продолжает отвечать
        ok = client.post('/predict', json=make_request(validate_rows[0], v3b_features))
        assert ok.status_code == 200 and ok.json()['source'] == 'model'


class TestFallback:
    def test_no_model_returns_cur_dev(self, validate_rows, v3b_features):
        client = TestClient(create_app(None))
        req = make_request(validate_rows[0], v3b_features)
        resp = client.post('/predict', json=req)
        assert resp.status_code == 200
        body = resp.json()
        assert body['source'] == 'fallback_cur_dev'
        assert body['delay_s'] == req['cur_dev_s']
        assert body['model_version'] is None
        assert body['p_late'] is None
        assert client.get('/healthz').json()['status'] == 'degraded_fallback'

    def test_broken_model_at_startup_serves_fallback(self, artifacts_dir, validate_rows,
                                                     v3b_features):
        client = TestClient(create_app(artifacts_dir / 'model_v1НЕСУЩЕСТВУЕТ.json'))
        req = make_request(validate_rows[0], v3b_features)
        resp = client.post('/predict', json=req)
        assert resp.status_code == 200  # не 5xx — явная деградация §4.7
        assert resp.json()['source'] == 'fallback_cur_dev'
        info = client.get('/model/info').json()
        assert info['model_loaded'] is False and info['load_error']


class TestInfoAndMetrics:
    def test_model_info_complete(self, client_model, v3b_features):
        info = client_model.get('/model/info').json()
        assert info['model_loaded'] is True
        assert info['version'] == 'v1v3b'
        assert info['target_mode'] == 'delta'
        assert info['features'] == v3b_features
        assert info['feature_count'] == 36
        assert info['trained_at'] and info['loss'] == 'MAE'
        assert 'mae_holdout_model' in info['mae'] and 'mae_train_all' in info['mae']
        assert info['best_iteration'] == 1501

    def test_metrics_expose_latency_and_degradation(self, artifacts_dir, validate_rows,
                                                    v3b_features):
        client = TestClient(create_app(artifacts_dir / MODEL_V3B))
        for row in validate_rows[:5]:
            client.post('/predict', json=make_request(row, v3b_features))
        # деградация обязана быть видима в /metrics (критерий 5, §4.7)
        client.post('/predict', json={'sample_id': 'x', 'cur_dev_s': 1.0})  # 422
        body = client.get('/metrics').text
        assert 'predictor_inference_latency_seconds{quantile="0.5"}' in body
        assert 'predictor_inference_latency_seconds{quantile="0.95"}' in body
        assert 'predictor_inference_latency_seconds{quantile="0.99"}' in body
        assert 'predictor_model_loaded 1' in body
        assert 'predictor_invalid_requests_total 1' in body
        assert 'predictor_requests_total{endpoint="/predict"} 6' in body
