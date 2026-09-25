"""Общие пути и тяжёлые фикстуры (реальные файлы репозитория)."""

from __future__ import annotations

import sys
from pathlib import Path

import polars as pl
import pytest

ML_DIR = Path(__file__).resolve().parent.parent
REPO_ROOT = ML_DIR.parent
# Пакет может быть не установлен — добавляем src в путь.
_SRC = str(ML_DIR / 'src')
if _SRC not in sys.path:
    sys.path.insert(0, _SRC)


@pytest.fixture(scope='session')
def repo_root() -> Path:
    return REPO_ROOT


@pytest.fixture(scope='session')
def features_train_path() -> Path:
    return ML_DIR / 'artifacts' / 'features_train.jsonl'


@pytest.fixture(scope='session')
def features_validate_path() -> Path:
    return ML_DIR / 'artifacts' / 'features_validate.jsonl'


@pytest.fixture(scope='session')
def train_traffic(repo_root) -> pl.DataFrame:
    from predictor.dataset import load_traffic
    return load_traffic(repo_root / 'train' / 'traffic.csv')


@pytest.fixture(scope='session')
def train_schedule_path(repo_root) -> Path:
    return repo_root / 'train' / 'schedule.csv'


@pytest.fixture(scope='session')
def validate_schedule_path(repo_root) -> Path:
    return repo_root / 'validate' / 'schedule_plan.csv'


@pytest.fixture(scope='session')
def validate_traffic(repo_root) -> pl.DataFrame:
    from predictor.dataset import load_traffic
    return load_traffic(repo_root / 'validate' / 'traffic.csv')
