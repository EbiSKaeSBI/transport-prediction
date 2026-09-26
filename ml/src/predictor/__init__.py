"""Пакет predictor: загрузка кадров Go-replay, оконные фичи и сборка датасета.

Единый источник имён фич — контракт `features/v1.yaml` (секции given /
schedule / go_state / window / context / quality). Go считает given,
schedule, go_state, quality; Python — window и context (см. window.py).
"""

from predictor.frames import load_frames
from predictor.window import build_window_features

__all__ = ['build_window_features', 'load_frames']
