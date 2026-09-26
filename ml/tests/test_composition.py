"""Тесты общей композиции предсказания (predictor.composition, задача #26/#27).

Гарантия: CLI :mod:`predictor.predict` и ML-сервис :mod:`predictor.serve`
собирают delay из режима таргета одним кодом — проверяем саму функцию и
вывод режима из метрик (включая исторические abs-файлы без поля target_mode).
"""

from __future__ import annotations

import numpy as np
import pytest

from predictor.composition import (
    compose_prediction,
    metrics_for_model,
    target_mode_for,
)


def test_abs_mode_passthrough():
    raw = np.array([1.5, -2.0])
    assert np.array_equal(compose_prediction(raw, np.array([100.0, 200.0]), 'abs'), raw)


def test_delta_mode_adds_cur_dev():
    raw = np.array([1.5, -2.0])
    out = compose_prediction(raw, np.array([100.0, 200.0]), 'delta')
    assert np.allclose(out, [101.5, 198.0])


def test_delta_mode_requires_cur():
    with pytest.raises(ValueError, match='cur_dev_s'):
        compose_prediction(np.array([1.0]), None, 'delta', context='req')


def test_delta_mode_rejects_null_cur():
    with pytest.raises(ValueError, match='ADR-0007'):
        compose_prediction(
            np.array([1.0, 2.0]), np.array([np.nan, 5.0]), 'delta', context='req',
        )


def test_target_mode_from_metrics():
    assert target_mode_for({'target_mode': 'delta'}) == 'delta'
    assert target_mode_for({'target': 'delay_delta_s'}) == 'delta'
    assert target_mode_for({'target': 'target_delay_s'}) == 'abs'
    assert target_mode_for(None) == 'abs'


def test_metrics_for_model_finds_sibling(artifacts_dir):
    metrics = metrics_for_model(artifacts_dir / 'model_v1v3b.json')
    assert metrics is not None
    assert metrics['target_mode'] == 'delta'
