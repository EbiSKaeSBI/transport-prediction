"""P(late): бинарный классификатор опоздания и причины прогноза (v4, §5.1).

Регрессия отвечает «насколько опоздает», классификатор — «опоздает ли
существенно» (таргет ``target_delay_s >= LATE_THRESHOLD_S``), той же
матрицей признаков, что regression-модель: ``delay_s`` и ``p_late``
сервис считает по одному вектору, и порогам риска на gateway (0.3/0.6)
можно верить — они видят калиброванную Logloss-вероятность, а не
самодельную оценку из регрессии.

Причина прогноза (:func:`infer_reason`) — правила §5.4 architecture.md.
Классификатор мини-моделью здесь не делает: признаки ``dwell_*``/``speed_*``
/``headway_s`` дают правило детерминированно, а «мини-классификатор
поверх» на 4k строк означал бы четвёртую модель, объяснять которую
придётся уже не правилом. Строки причин совпадают с replay-генератором
``scripts/make_dashboard_demo.py::incident_reason`` буквально — карточка
в live и в демо не должна различаться лексикой (паритет проверяется
тестом tests/test_late.py, а не словами в доке).

CLI (от корня репозитория)::

    python -m predictor.late --train ml/artifacts/dataset_train.parquet \\
        --holdout ml/artifacts/dataset_test.parquet \\
        --metrics ml/artifacts/metrics_v1online.json \\
        --out-dir ml/artifacts --tag v1online

Артефакты: ``model_{tag}_late.json`` + ``metrics_{tag}_late.json``.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np
import polars as pl
from catboost import CatBoostClassifier

from predictor.features import to_matrix

#: Порог «существенного» опоздания, совпадает с красной зоной карты
#: (DelayRedS на gateway, §4.1) — класс и риск обязаны значить одно и то же.
LATE_THRESHOLD_S = 120.0

#: Признаки причины прогноза: только то, что реально пришло в кадре.
REASON_KEYS = ('dwell_current_s', 'speed_current', 'headway_s')


def build_label(df: pl.DataFrame) -> np.ndarray:
    """1, если фактическое опоздание к цели >= LATE_THRESHOLD_S.

    null в таргете трактуем как «не опоздал»: метки без факта в датасете
    быть не должно, но молча ронять выборку из-за одной дыры — хуже, чем
    посчитать её негативом (виден перекос в positive_rate).
    """
    delay = df.get_column('target_delay_s').cast(pl.Float64).fill_null(
        -np.inf).to_numpy()
    return (delay >= LATE_THRESHOLD_S).astype(np.int64)


def feature_names_from_metrics(metrics_path: Path) -> list[str]:
    """Список фич парной регрессии: классификатор учится ровно на нём."""
    metrics = json.loads(Path(metrics_path).read_text(encoding='utf-8'))
    feats = metrics.get('features')
    if not feats:
        raise ValueError(f'в {metrics_path} нет списка features — не с чем сверять')
    return [str(f) for f in feats]


def _auc(y_true: np.ndarray, score: np.ndarray) -> float:
    """ROC-AUC по рангам (Mann-Whitney).

    Не через clf.eval_metrics: у него формат возврата менялся между
    версиями CatBoost, а AUC по вероятностям — 10 строк честной статистики,
    которая к тому же проверяется на синтетике в тестах.
    """
    order = np.argsort(score)
    ranks = np.empty(len(score), dtype=np.float64)
    # Связки (одинаковые вероятности) получают средний ранг.
    ranks[order] = np.arange(1, len(score) + 1, dtype=np.float64)
    s = score[order]
    i = 0
    while i < len(s):
        j = i
        while j + 1 < len(s) and s[j + 1] == s[i]:
            j += 1
        if j > i:
            ranks[order[i:j + 1]] = (i + j + 2) / 2.0
        i = j + 1
    n_pos = int(np.sum(y_true == 1))
    n_neg = int(np.sum(y_true == 0))
    if n_pos == 0 or n_neg == 0:
        return float('nan')
    return float((ranks[y_true == 1].sum() - n_pos * (n_pos + 1) / 2.0)
                 / (n_pos * n_neg))


def train(train_path: str | Path, holdout_path: str | Path, metrics_src: str | Path,
          out_dir: str | Path, *, tag: str = 'v1online', iterations: int = 800,
          depth: int = 6, lr: float = 0.05, patience: int = 50,
          seed: int = 42) -> dict[str, Any]:
    """Обучить P(delay >= 120 c) на матрице регрессии; вернуть метрики."""
    features = feature_names_from_metrics(Path(metrics_src))
    df_tr = pl.read_parquet(train_path)
    df_ho = pl.read_parquet(holdout_path)
    missing = [f for f in features if f not in df_tr.columns]
    if missing:
        raise ValueError(f'в датасете нет фич классификатора: {missing}')
    y_tr, y_ho = build_label(df_tr), build_label(df_ho)
    # Матрица — через общий to_matrix: те же null→NaN и bool→float, что
    # видит сервис на inference. Свой способ конвертации дал бы drift
    # train/serve на тех же колонках, против которого написан контракт.
    X_tr = to_matrix(df_tr, features).to_numpy().astype(np.float64)
    X_ho = to_matrix(df_ho, features).to_numpy().astype(np.float64)

    clf = CatBoostClassifier(
        iterations=iterations, depth=depth, learning_rate=lr,
        loss_function='Logloss', eval_metric='AUC', random_seed=seed,
        early_stopping_rounds=patience, verbose=False,
    )
    t0 = time.perf_counter()
    clf.fit(X_tr, y_tr, eval_set=(X_ho, y_ho))
    elapsed = time.perf_counter() - t0

    proba = clf.predict_proba(X_ho)[:, 1]
    pred = (proba >= 0.5).astype(np.int64)
    tp = int(np.sum((pred == 1) & (y_ho == 1)))
    fp = int(np.sum((pred == 1) & (y_ho == 0)))
    fn = int(np.sum((pred == 0) & (y_ho == 1)))
    # Калибровка важна не сама по себе: gateway режет жёлтый/красный по
    # порогам 0.3/0.6 от этой вероятности, и кривая калибровки — то, на что
    # эти пороги операются.
    bins = np.digitize(proba, np.linspace(0, 1, 11)[1:-1])
    calibration = [
        {'bin': f'[{lo:.1f}; {hi:.1f})',
         'n': int(np.sum(bins == i)),
         'rate': float(np.mean(y_ho[bins == i])) if np.any(bins == i) else None}
        for i, (lo, hi) in enumerate(zip([0.0] + list(np.linspace(0, 1, 11)[1:-1]),
                                         list(np.linspace(0, 1, 11)[1:-1]) + [1.0]))
    ]
    metrics = {
        'task': 'p_late',
        'threshold_s': LATE_THRESHOLD_S,
        'features': features,
        'n_train_rows': df_tr.height,
        'n_holdout_rows': df_ho.height,
        'positive_rate_train': float(np.mean(y_tr)),
        'positive_rate_holdout': float(np.mean(y_ho)),
        'auc_holdout': _auc(y_ho, proba)
        if y_ho.sum() > 0 and y_ho.sum() < len(y_ho) else None,
        'accuracy_holdout': float(np.mean(pred == y_ho)),
        'precision_holdout': tp / (tp + fp) if tp + fp else None,
        'recall_holdout': tp / (tp + fn) if tp + fn else None,
        'base_accuracy_holdout': float(max(np.mean(y_ho), 1 - np.mean(y_ho))),
        'calibration_holdout': [c for c in calibration if c['n']],
        'best_iteration': int(clf.get_best_iteration() or 0),
        'train_seconds': round(elapsed, 2),
    }
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    model_path = out / f'model_{tag}_late.json'
    clf.save_model(str(model_path), format='json')
    (out / f'metrics_{tag}_late.json').write_text(
        json.dumps(metrics, ensure_ascii=False, indent=2), encoding='utf-8')
    return metrics


def load_late(model_path: str | Path) -> tuple[CatBoostClassifier, dict]:
    """Загрузить классификатор + его metrics-соседа; любая ошибка — исключение."""
    model_path = Path(model_path)
    if not model_path.is_file():
        raise FileNotFoundError(f'артефакт классификатора не найден: {model_path}')
    metrics_path = model_path.with_name(
        model_path.stem.replace('model_', 'metrics_') + '.json')
    if not metrics_path.is_file():
        raise FileNotFoundError(f'нет метрик классификатора рядом: {metrics_path}')
    metrics = json.loads(metrics_path.read_text(encoding='utf-8'))
    clf = CatBoostClassifier()
    clf.load_model(str(model_path), format='json')
    return clf, metrics


def require_parity(late_features: list[str], regressor_features: list[str]) -> None:
    """Один вектор на две модели — иначе p_late и delay_s считаются по разным кадрам."""
    if list(late_features) != list(regressor_features):
        raise ValueError(
            'классификатор обучен на другом списке фич, чем регрессия: '
            f'late={late_features} model={regressor_features}')


def predict_p_late(clf: CatBoostClassifier, X: np.ndarray) -> np.ndarray:
    return clf.predict_proba(X)[:, 1]


def infer_reason(values: dict[str, Any]) -> str:
    """Предполагаемая причина прогноза, правила §5.4.

    Порядок проверок значим: простой на остановке — то, что оператору
    делать нечего (ждём), а «малый интервал» — то, на что можно реагировать
    выпуском следующего ТС.

    Паритет со строками ``scripts/make_dashboard_demo.py::incident_reason``
    зафиксирован тестом — менять нужно обе копии одновременно.
    """
    # Не values.get(key, default): Go пишет пропуски явным JSON null,
    # ключ тогда присутствует, и None ломает сравнения ниже.
    def num(key: str, default: float) -> float:
        v = values.get(key)
        return default if v is None else float(v)

    dwell = num('dwell_current_s', 0.0)
    speed = num('speed_current', 99.0)
    headway = num('headway_s', 999.0)
    if dwell >= 60:
        return 'длительный простой на остановке'
    if dwell >= 20:
        return 'увеличенное время стоянки'
    if speed <= 5:
        return 'низкая скорость движения'
    if headway < 120:
        return 'малый интервал, эффект «паровозика»'
    return 'накопленное отставание без текущего простоя'


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description='Обучить P(late) (v4, §5.1)')
    ap.add_argument('--train', required=True)
    ap.add_argument('--holdout', required=True)
    ap.add_argument('--metrics', required=True,
                    help='metrics-файл парной регрессии (источник списка фич)')
    ap.add_argument('--out-dir', required=True)
    ap.add_argument('--tag', default='v1online')
    ap.add_argument('--iterations', type=int, default=800)
    ap.add_argument('--depth', type=int, default=6)
    ap.add_argument('--learning-rate', type=float, default=0.05)
    ap.add_argument('--patience', type=int, default=50)
    ap.add_argument('--seed', type=int, default=42)
    args = ap.parse_args(argv)
    m = train(args.train, args.holdout, args.metrics, args.out_dir,
              tag=args.tag, iterations=args.iterations, depth=args.depth,
              lr=args.learning_rate, patience=args.patience, seed=args.seed)
    print(f"P(late): порог {m['threshold_s']:.0f} с | фич {len(m['features'])} | "
          f"train {m['n_train_rows']} (позитивов {m['positive_rate_train']:.1%}), "
          f"holdout {m['n_holdout_rows']} (позитивов {m['positive_rate_holdout']:.1%})")
    if m['auc_holdout'] is not None:
        print(f"AUC holdout          : {m['auc_holdout']:.3f}")
    print(f"accuracy@0.5         : {m['accuracy_holdout']:.3f} "
          f"(базовая {m['base_accuracy_holdout']:.3f})")
    print(f"precision / recall   : {m['precision_holdout']} / {m['recall_holdout']}")
    print(f"лучшая итерация      : {m['best_iteration']}, за {m['train_seconds']} с")
    print(f"артефакты: model_{args.tag}_late.json, metrics_{args.tag}_late.json")
    return 0


if __name__ == '__main__':
    sys.exit(main())
