"""Генератор submission.csv — страховочный артефакт сдачи.

По умолчанию пишет базовый прогноз prediction = cur_dev_s, который даёт
MAE 88.72 с на validate (score 0.18) и гарантирует валидный файл, даже если
модель не обучена. Когда модель обучена, подставляем её вывод:

    python3 scripts/make_submission.py --model artifacts/predictions.csv

Ожидаемые колонки model-файла: sample_id, prediction (разделитель ; или ,).
"""

from __future__ import annotations

import argparse
import csv
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SAMPLE = ROOT / "sample_submission.csv"
POINTS = ROOT / "validate" / "points.csv"
EXPECTED_COLUMNS = ["sample_id", "prediction"]


def detect_delimiter(path: Path) -> str:
    head = path.open(encoding="utf-8", errors="replace").readline()
    return ";" if head.count(";") > head.count(",") else ","


def read_table(path: Path) -> list[dict[str, str]]:
    with path.open(newline="", encoding="utf-8") as handle:
        return list(csv.DictReader(handle, delimiter=detect_delimiter(path)))


def load_model(path: Path) -> dict[str, float]:
    rows = read_table(path)
    if not rows or not set(EXPECTED_COLUMNS).issubset(rows[0]):
        raise SystemExit(
            f"{path}: ожидаются колонки {EXPECTED_COLUMNS}, получено {list(rows[0]) if rows else 'пусто'}"
        )
    return {r["sample_id"]: float(r["prediction"]) for r in rows}


def display_path(path: Path) -> str:
    try:
        return str(path.relative_to(ROOT))
    except ValueError:
        return str(path)


def main() -> int:
    parser = argparse.ArgumentParser(description="Собрать submission.csv")
    parser.add_argument("--model", type=Path, help="CSV с предсказаниями модели")
    parser.add_argument("--out", type=Path, default=ROOT / "submission.csv")
    args = parser.parse_args()

    if not SAMPLE.exists():
        print(f"Нет {SAMPLE}", file=sys.stderr)
        return 1

    sample_rows = read_table(SAMPLE)
    points = {r["sample_id"]: r for r in read_table(POINTS)}
    predictions: dict[str, float] | None = None
    if args.model:
        predictions = load_model(args.model)
        print(f"Предсказания модели: {args.model} ({len(predictions)} строк)")

    missing = [r["sample_id"] for r in sample_rows if r["sample_id"] not in points]
    if missing:
        print(
            f"Нет телеметрии для {len(missing)} sample_id, например {missing[:3]}",
            file=sys.stderr,
        )
        return 1

    rows: list[dict[str, float | str]] = []
    fallback = 0
    for row in sample_rows:
        sid = row["sample_id"]
        point = points[sid]
        if predictions is not None and sid in predictions:
            value = predictions[sid]
        else:
            value = float(point["cur_dev_s"])
            fallback += 1
        rows.append({"sample_id": sid, "prediction": round(float(value), 3)})

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=EXPECTED_COLUMNS, delimiter=";")
        writer.writeheader()
        writer.writerows(rows)

    values = [float(r["prediction"]) for r in rows]
    print(f"Записано {len(rows)} строк в {display_path(args.out)}")
    print(f"Разделитель ';' | колонки {EXPECTED_COLUMNS}")
    if fallback:
        print(f"Базовая подстановка cur_dev_s: {fallback} из {len(rows)}")
    print(
        f"Прогноз: min {min(values):.1f} | max {max(values):.1f} | "
        f"mean {sum(values) / len(values):.1f} с"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
