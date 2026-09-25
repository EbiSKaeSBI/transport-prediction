"""Локальный оракул для задачи «Предиктор изменений в графике движения».

Восстанавливает ground truth валидационной выборки через утечку, обнаруженную
в раздаче, и считает честный MAE в ходе разработки. Модуль намеренно изолирован:
он не импортируется ни backend, ни ml/src/predictor, и его результаты не
попадают в prediction-path. Используется только как измерительный прибор.

Запуск:  python3 scripts/oracle_audit.py
"""

from __future__ import annotations

import csv
import hashlib
import statistics
import sys
from datetime import datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SEP = "=" * 76


def md5(path: Path) -> str:
    digest = hashlib.md5()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def read_csv(path: Path, delimiter: str = ",") -> list[dict[str, str]]:
    with path.open(newline="", encoding="utf-8") as handle:
        return list(csv.DictReader(handle, delimiter=delimiter))


def parse_ts(value: str) -> datetime:
    text = value.strip()
    if "." in text:
        base, frac = text.split(".", 1)
        return datetime.fromisoformat(base).replace(
            microsecond=int((frac + "000000")[:6])
        )
    return datetime.fromisoformat(text)


def mae(pairs: list[tuple[float, float]]) -> float:
    if not pairs:
        return float("nan")
    return statistics.fmean(abs(actual - pred) for pred, actual in pairs)


def section(title: str) -> None:
    print()
    print(SEP)
    print(title)
    print(SEP)


def prove_leak() -> tuple[list[dict[str, str]], dict[str, dict[str, str]]]:
    section("1. ДОКАЗАТЕЛЬСТВО УТЕЧКИ В РАЗДАЧЕ")

    h_test = md5(ROOT / "test" / "traffic.csv")
    h_val = md5(ROOT / "validate" / "traffic.csv")
    print(f"  md5 test/traffic.csv      {h_test}")
    print(f"  md5 validate/traffic.csv  {h_val}")
    print(f"  файлы идентичны           {'ДА' if h_test == h_val else 'НЕТ'}")
    size_mb = (ROOT / "test" / "traffic.csv").stat().st_size / 1048576
    print(f"  размер                    {size_mb:.1f} МБ каждый")

    test_sched = read_csv(ROOT / "test" / "schedule.csv")
    val_sched = read_csv(ROOT / "validate" / "schedule_plan.csv")
    shared = [c for c in test_sched[0] if c in val_sched[0]]
    same_len = len(test_sched) == len(val_sched)
    same_rows = same_len and all(
        all(a[c] == b[c] for c in shared) for a, b in zip(test_sched, val_sched)
    )
    only_in_test = [c for c in test_sched[0] if c not in val_sched[0]]
    print()
    print(
        f"  test/schedule.csv          {len(test_sched)} строк, {len(test_sched[0])} колонок"
    )
    print(
        f"  validate/schedule_plan.csv {len(val_sched)} строк, {len(val_sched[0])} колонок"
    )
    print(f"  общие колонки              {len(shared)}: {', '.join(shared)}")
    print(f"  колонка только в test      {', '.join(only_in_test)}")
    print(f"  строки совпадают           {'ДА' if same_rows else 'НЕТ'}")

    fact = {
        r["tt_action_item_id"]: r for r in test_sched if r["time_fact_begin"].strip()
    }
    points = read_csv(ROOT / "validate" / "points.csv")
    matched = [p for p in points if p["target_stop_id"] in fact]
    print()
    print(f"  validate/points.csv        {len(points)} прогнозных точек")
    print(
        f"  target_stop_id найден в test/schedule.csv: {len(matched)} из {len(points)}"
    )

    plan_mismatch = [
        p
        for p in matched
        if parse_ts(fact[p["target_stop_id"]]["time_begin"])
        != parse_ts(p["target_time_begin"])
    ]
    print(
        f"  plan-время совпадает с target_time_begin: {len(points) - len(plan_mismatch)} из {len(points)}"
    )
    print()
    print("  ВЫВОД: ground truth всех точек validate восстанавливается точно.")
    print("         Утечка используется только здесь, вне prediction-path.")
    return points, fact


def oracle_truth(
    points: list[dict[str, str]], fact: dict[str, dict[str, str]]
) -> dict[str, float]:
    truth: dict[str, float] = {}
    for point in points:
        row = fact[point["target_stop_id"]]
        delta = parse_ts(row["time_fact_begin"]) - parse_ts(row["time_begin"])
        truth[point["sample_id"]] = delta.total_seconds()
    return truth


def cross_check(truth: dict[str, float]) -> None:
    section("2. СВЕРКА ОРАКУЛА С ПРЕДОСТАВЛЕННОЙ РАЗМЕТКОЙ")
    labels = {r["sample_id"]: r for r in read_csv(ROOT / "labels" / "labels_test.csv")}
    shared = sorted(set(truth) & set(labels))
    if not shared:
        print("  Пересечения sample_id нет — сверка невозможна.")
        return
    diffs = [abs(truth[s] - float(labels[s]["target_delay_s"])) for s in shared]
    print(f"  общих sample_id: {len(shared)}")
    print(f"  макс. расхождение oracle vs labels_test: {max(diffs):.0f} с")
    print(f"  среднее расхождение:                     {statistics.fmean(diffs):.2f} с")
    print(
        "  Оракул корректен."
        if max(diffs) <= 1.0
        else "  ВНИМАНИЕ: оракул расходится с разметкой."
    )


def baselines() -> None:
    section("3. БАЗЕЛАЙНЫ (честная разметка организаторов)")
    for name in ("labels_train.csv", "labels_test.csv"):
        rows = read_csv(ROOT / "labels" / name)
        pairs_zero = [(0.0, float(r["target_delay_s"])) for r in rows]
        pairs_cur = [(float(r["cur_dev_s"]), float(r["target_delay_s"])) for r in rows]
        print(f"  {name}  n={len(rows)}")
        print(f"    MAE при pred=0          {mae(pairs_zero):7.2f} с")
        print(f"    MAE при pred=cur_dev_s  {mae(pairs_cur):7.2f} с")


def validate_report(points: list[dict[str, str]], truth: dict[str, float]) -> None:
    section("4. VALIDATE ЧЕРЕЗ ОРАКУЛ (151 точка, заземление для скоринга)")
    pairs_zero = [(0.0, truth[p["sample_id"]]) for p in points]
    pairs_cur = [(float(p["cur_dev_s"]), truth[p["sample_id"]]) for p in points]
    mae_zero = mae(pairs_zero)
    mae_cur = mae(pairs_cur)
    print(
        f"  mae_zero (pred=0)         {mae_zero:7.2f} с   <- делитель в формуле скоринга"
    )
    print(
        f"  MAE при pred=cur_dev_s    {mae_cur:7.2f} с   score = {max(0.0, 1 - mae_cur / mae_zero):.4f}"
    )
    print()
    print("  Пороги баллов (score = 1 - MAE / mae_zero):")
    for points_needed in (6, 5, 4, 3):
        target_score = 0.1 * (points_needed + 1)
        threshold = mae_zero * (1 - target_score)
        print(
            f"    {points_needed} баллов   score >= {target_score:.2f}   MAE <= {threshold:6.1f} с"
        )
    print()
    print("  Базовые гипотезы:")
    deltas = [truth[p["sample_id"]] - float(p["cur_dev_s"]) for p in points]
    print(f"    дрифт median {statistics.median(deltas):+8.1f} с")
    print(f"    дрифт std    {statistics.pstdev(deltas):8.1f} с")
    print(f"    дрифт min    {min(deltas):+8.1f} с")
    print(f"    дрифт max    {max(deltas):+8.1f} с")
    print()
    print(
        f"  Вывод: std дрифта {statistics.pstdev(deltas):.0f} с означает, что абсолютное"
    )
    print("  значение таргета предсказывать напрямую тяжело. Рабочая цель — дельта.")


def main() -> int:
    if not (ROOT / "validate" / "points.csv").exists():
        print(
            "Не найден validate/points.csv — запускайте из корня репозитория.",
            file=sys.stderr,
        )
        return 1
    points, fact = prove_leak()
    truth = oracle_truth(points, fact)
    cross_check(truth)
    baselines()
    validate_report(points, truth)
    print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
