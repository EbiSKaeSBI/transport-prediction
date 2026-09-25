"""Тесты для oracle_audit.py и make_submission.py.

Проверяет:
  1. oracle_audit.py — утечка, ground truth, базовые MAE, пороги
  2. make_submission.py — корректный submission.csv
  3. Инварианты из docs/data-audit.md

Запуск:  python3 -m pytest tests/test_phase0.py -v
Требует: Python 3.12+, файлы в репозитории (без установки зависимостей).
"""

from __future__ import annotations

import csv
import hashlib
import math
import statistics
import subprocess
import sys
from datetime import datetime
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent


# ═══════════════════════════════════════════════════════════════════════════
# Вспомогательные функции (дублируют логику oracle_audit.py для независимых проверок)
# ═══════════════════════════════════════════════════════════════════════════


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


# ═══════════════════════════════════════════════════════════════════════════
# 1. oracle_audit.py — интеграционные тесты
# ═══════════════════════════════════════════════════════════════════════════


class TestOracleAuditRuns:
    """Скрипт oracle_audit.py должен завершаться с кодом 0 и выводить ожидаемые числа."""

    def test_exit_code_zero(self) -> None:
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "oracle_audit.py")],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        assert result.returncode == 0, (
            f"Скрипт завершился с кодом {result.returncode}:\n{result.stderr}"
        )

    def test_output_contains_leak_proof(self) -> None:
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "oracle_audit.py")],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        out = result.stdout
        assert "файлы идентичны" in out, (
            "Вывод должен содержать результат сравнения md5"
        )
        assert "151 из 151" in out, "Все 151 точки должны быть найдены"
        assert "ground truth" in out.lower(), (
            "Должно быть утверждение о восстановлении ground truth"
        )

    def test_output_contains_baselines(self) -> None:
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "oracle_audit.py")],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        out = result.stdout
        assert "MAE при pred=0" in out, "Должен быть MAE при pred=0"
        assert "MAE при pred=cur_dev_s" in out, "Должен быть MAE при pred=cur_dev_s"

    def test_output_contains_validate_metrics(self) -> None:
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "oracle_audit.py")],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        out = result.stdout
        assert "mae_zero" in out, "Должен быть mae_zero"
        assert "score" in out, "Должен быть score"
        assert "32.4" in out, "Должен быть порог 6 баллов (32.4 с)"
        assert "43.3" in out, "Должен быть порог 5 баллов (43.3 с)"
        assert "54.1" in out, "Должен быть порог 4 балла (54.1 с)"


# ═══════════════════════════════════════════════════════════════════════════
# 2. Инварианты утечки (независимая проверка, не через вывод скрипта)
# ═══════════════════════════════════════════════════════════════════════════


class TestLeakInvariant:
    """Проверяет, что утечка в раздаче подтверждена данными, а не только скриптом."""

    def test_traffic_files_identical(self) -> None:
        h_test = md5(ROOT / "test" / "traffic.csv")
        h_val = md5(ROOT / "validate" / "traffic.csv")
        assert h_test == h_val, (
            f"test/traffic.csv != validate/traffic.csv: {h_test} vs {h_val}"
        )

    def test_schedule_rows_match(self) -> None:
        test_sched = read_csv(ROOT / "test" / "schedule.csv")
        val_sched = read_csv(ROOT / "validate" / "schedule_plan.csv")
        assert len(test_sched) == len(val_sched), "Количество строк должно совпадать"
        shared_cols = [c for c in test_sched[0] if c in val_sched[0]]
        only_test = [c for c in test_sched[0] if c not in val_sched[0]]
        assert "time_fact_begin" in only_test, (
            "time_fact_begin должна быть только в test"
        )
        assert len(only_test) == 1, "Одна колонка только в test"
        for a, b in zip(test_sched, val_sched):
            for c in shared_cols:
                assert a[c] == b[c], (
                    f"Строка {a['tt_action_item_id']}: колонка {c} не совпадает"
                )

    def test_all_points_resolvable(self) -> None:
        points = read_csv(ROOT / "validate" / "points.csv")
        test_sched = read_csv(ROOT / "test" / "schedule.csv")
        fact = {
            r["tt_action_item_id"]: r
            for r in test_sched
            if r["time_fact_begin"].strip()
        }
        matched = [p for p in points if p["target_stop_id"] in fact]
        assert len(matched) == len(points), (
            f"Найдено {len(matched)} из {len(points)} точек"
        )
        # plan-время должно совпадать с target_time_begin
        mismatches = [
            p
            for p in matched
            if parse_ts(fact[p["target_stop_id"]]["time_begin"])
            != parse_ts(p["target_time_begin"])
        ]
        assert len(mismatches) == 0, (
            f"{len(mismatches)} точек с план-временем != target_time_begin"
        )


# ═══════════════════════════════════════════════════════════════════════════
# 3. Ground truth через оракул
# ═══════════════════════════════════════════════════════════════════════════


class TestOracleTruth:
    """Независимая проверка восстановления ground truth."""

    @pytest.fixture
    def truth(self) -> dict[str, float]:
        points = read_csv(ROOT / "validate" / "points.csv")
        test_sched = read_csv(ROOT / "test" / "schedule.csv")
        fact = {
            r["tt_action_item_id"]: r
            for r in test_sched
            if r["time_fact_begin"].strip()
        }
        truth: dict[str, float] = {}
        for p in points:
            row = fact[p["target_stop_id"]]
            delta = parse_ts(row["time_fact_begin"]) - parse_ts(row["time_begin"])
            truth[p["sample_id"]] = delta.total_seconds()
        return truth

    def test_truth_all_finite(self, truth: dict[str, float]) -> None:
        for sid, val in truth.items():
            assert math.isfinite(val), f"{sid}: нечисловое значение {val}"
            assert abs(val) < 1e4, f"{sid}: значение {val} подозрительно большое"

    def test_truth_range(self, truth: dict[str, float]) -> None:
        vals = list(truth.values())
        assert min(vals) >= -500, f"min {min(vals)} — подозрительно низкий"
        assert max(vals) <= 800, f"max {max(vals)} — подозрительно высокий"

    def test_truth_median_near_zero(self, truth: dict[str, float]) -> None:
        vals = list(truth.values())
        median = statistics.median(vals)
        # Документация указывает медиану дрейта ~0, но на validate через оракул
        # медиана может быть немного выше из-за состава 151 точки.
        # Достаточно, что она не нулевая — дрейф не смещён в одну сторону.
        assert abs(median) < 60, (
            f"Медиана {median:.1f} — слишком удалён от нуля (дрейф = target - cur_dev)"
        )


# ═══════════════════════════════════════════════════════════════════════════
# 4. Базовые MAE
# ═══════════════════════════════════════════════════════════════════════════


class TestBaselines:
    """MAE базовых прогнозов должны совпадать с заявленными в документации."""

    def test_train_baseline_zero(self) -> None:
        rows = read_csv(ROOT / "labels" / "labels_train.csv")
        pairs_zero = [(0.0, float(r["target_delay_s"])) for r in rows]
        m = mae(pairs_zero)
        assert abs(m - 97.17) < 0.5, (
            f"MAE при pred=0 на train: {m:.2f} (ожидается ~97.17)"
        )

    def test_train_baseline_cur_dev(self) -> None:
        rows = read_csv(ROOT / "labels" / "labels_train.csv")
        pairs_cur = [(float(r["cur_dev_s"]), float(r["target_delay_s"])) for r in rows]
        m = mae(pairs_cur)
        assert abs(m - 93.03) < 0.5, (
            f"MAE при pred=cur_dev_s на train: {m:.2f} (ожидается ~93.03)"
        )

    def test_test_baseline_zero(self) -> None:
        rows = read_csv(ROOT / "labels" / "labels_test.csv")
        pairs_zero = [(0.0, float(r["target_delay_s"])) for r in rows]
        m = mae(pairs_zero)
        assert abs(m - 103.34) < 0.5, (
            f"MAE при pred=0 на test: {m:.2f} (ожидается ~103.34)"
        )

    def test_test_baseline_cur_dev(self) -> None:
        rows = read_csv(ROOT / "labels" / "labels_test.csv")
        pairs_cur = [(float(r["cur_dev_s"]), float(r["target_delay_s"])) for r in rows]
        m = mae(pairs_cur)
        assert abs(m - 93.36) < 0.5, (
            f"MAE при pred=cur_dev_s на test: {m:.2f} (ожидается ~93.36)"
        )


# ═══════════════════════════════════════════════════════════════════════════
# 5. Validate через оракул — точные числа
# ═══════════════════════════════════════════════════════════════════════════


class TestValidateOracle:
    """Проверяет точные значения MAE на validate через оракул."""

    @pytest.fixture
    def truth(self) -> dict[str, float]:
        points = read_csv(ROOT / "validate" / "points.csv")
        test_sched = read_csv(ROOT / "test" / "schedule.csv")
        fact = {
            r["tt_action_item_id"]: r
            for r in test_sched
            if r["time_fact_begin"].strip()
        }
        truth: dict[str, float] = {}
        for p in points:
            row = fact[p["target_stop_id"]]
            delta = parse_ts(row["time_fact_begin"]) - parse_ts(row["time_begin"])
            truth[p["sample_id"]] = delta.total_seconds()
        return truth

    def test_mae_zero(self, truth: dict[str, float]) -> None:
        points = read_csv(ROOT / "validate" / "points.csv")
        pairs_zero = [(0.0, truth[p["sample_id"]]) for p in points]
        m = mae(pairs_zero)
        assert abs(m - 108.13) < 0.5, f"mae_zero (validate): {m:.2f} (ожидается 108.13)"

    def test_mae_cur_dev(self, truth: dict[str, float]) -> None:
        points = read_csv(ROOT / "validate" / "points.csv")
        pairs_cur = [(float(p["cur_dev_s"]), truth[p["sample_id"]]) for p in points]
        m = mae(pairs_cur)
        assert abs(m - 88.72) < 0.5, (
            f"MAE при pred=cur_dev_s (validate): {m:.2f} (ожидается 88.72)"
        )

    def test_score_cur_dev(self, truth: dict[str, float]) -> None:
        points = read_csv(ROOT / "validate" / "points.csv")
        pairs_cur = [(float(p["cur_dev_s"]), truth[p["sample_id"]]) for p in points]
        m = mae(pairs_cur)
        mae_zero = mae([(0.0, truth[p["sample_id"]]) for p in points])
        score = max(0.0, 1.0 - m / mae_zero)
        assert abs(score - 0.1795) < 0.01, (
            f"score при pred=cur_dev_s: {score:.4f} (ожидается ~0.1795)"
        )


# ═══════════════════════════════════════════════════════════════════════════
# 6. Пороги баллов
# ═══════════════════════════════════════════════════════════════════════════


class TestScoreThresholds:
    """Пороги баллов рассчитаны из mae_zero = 108.13 и формулы score = 1 - MAE/mae_zero."""

    def test_thresholds(self) -> None:
        mae_zero = 108.13
        thresholds = {
            6: mae_zero * (1 - 0.70),  # 32.4
            5: mae_zero * (1 - 0.60),  # 43.3
            4: mae_zero * (1 - 0.50),  # 54.1
            3: mae_zero * (1 - 0.40),  # 64.9
        }
        assert abs(thresholds[6] - 32.4) < 0.2, (
            f"6 баллов: {thresholds[6]:.1f} (ожидается 32.4)"
        )
        assert abs(thresholds[5] - 43.3) < 0.2, (
            f"5 баллов: {thresholds[5]:.1f} (ожидается 43.3)"
        )
        assert abs(thresholds[4] - 54.1) < 0.2, (
            f"4 балла: {thresholds[4]:.1f} (ожидается 54.1)"
        )
        assert abs(thresholds[3] - 64.9) < 0.2, (
            f"3 балла: {thresholds[3]:.1f} (ожидается 64.9)"
        )


# ═══════════════════════════════════════════════════════════════════════════
# 7. make_submission.py — базовый режим (fallback на cur_dev_s)
# ═══════════════════════════════════════════════════════════════════════════


class TestMakeSubmissionBaseline:
    """Генерация submission.csv с fallback на cur_dev_s — должен совпадать с sample_submission.csv."""

    def test_generates_valid_file(self, tmp_path: Path) -> None:
        out = tmp_path / "submission.csv"
        result = subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        # Успех = файл создан, непустой и скрипт не упал
        assert result.returncode == 0, f"stderr: {result.stderr}"
        assert out.exists(), "submission.csv не создан"
        with out.open(newline="", encoding="utf-8") as f:
            rows = list(csv.DictReader(f, delimiter=";"))
        assert len(rows) > 0, "Файл пуст"

    def test_row_count(self, tmp_path: Path) -> None:
        out = tmp_path / "submission.csv"
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            cwd=ROOT,
            check=False,
        )
        with out.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f, delimiter=";")
            rows = list(reader)
        assert len(rows) == 151, f"Ожидалось 151 строк, получено {len(rows)}"

    def test_columns(self, tmp_path: Path) -> None:
        out = tmp_path / "submission.csv"
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            cwd=ROOT,
            check=False,
        )
        with out.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f, delimiter=";")
            rows = list(reader)
        assert set(rows[0].keys()) == {"sample_id", "prediction"}, (
            f"Неверные колонки: {list(rows[0].keys())}"
        )

    def test_delimiter_semicolon(self, tmp_path: Path) -> None:
        out = tmp_path / "submission.csv"
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            cwd=ROOT,
            check=False,
        )
        first_line = out.read_text(encoding="utf-8").splitlines()[0]
        assert ";" in first_line, (
            f"Разделитель должен быть ';', первая строка: {first_line!r}"
        )

    def test_predictions_match_cur_dev(self, tmp_path: Path) -> None:
        out = tmp_path / "submission.csv"
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            cwd=ROOT,
            check=False,
        )
        # Загружаем точки
        points = {
            r["sample_id"]: float(r["cur_dev_s"])
            for r in read_csv(ROOT / "validate" / "points.csv")
        }
        # Загружаем submission
        with out.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f, delimiter=";")
            sub = list(reader)
        for row in sub:
            sid = row["sample_id"]
            pred = float(row["prediction"])
            expected = points[sid]
            assert abs(pred - expected) < 0.01, (
                f"{sid}: prediction={pred}, cur_dev_s={expected}"
            )

    def test_sample_ids_match_template(self, tmp_path: Path) -> None:
        out = tmp_path / "submission.csv"
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            cwd=ROOT,
            check=False,
        )
        template = {
            r["sample_id"]
            for r in read_csv(ROOT / "sample_submission.csv", delimiter=";")
        }
        with out.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f, delimiter=";")
            generated = [r["sample_id"] for r in reader]
        assert set(generated) == template, (
            "Состав sample_id должен совпадать с шаблоном"
        )
        # Порядок генерируется по порядку sample_submission.csv — проверяем чётко
        template_order = [
            r["sample_id"]
            for r in read_csv(ROOT / "sample_submission.csv", delimiter=";")
        ]
        assert generated == template_order, "Порядок sample_id должен сохраняться"


# ═══════════════════════════════════════════════════════════════════════════
# 8. make_submission.py — режим с моделью
# ═══════════════════════════════════════════════════════════════════════════


class TestMakeSubmissionWithModel:
    """Когда передан файл модели, используются его предсказания, не cur_dev_s."""

    def test_uses_model_predictions(self, tmp_path: Path) -> None:
        # Создаём модельный файл с предсказаниями = cur_dev_s + 10
        points = read_csv(ROOT / "validate" / "points.csv")
        model_path = tmp_path / "model.csv"
        with model_path.open("w", newline="", encoding="utf-8") as f:
            w = csv.DictWriter(f, fieldnames=["sample_id", "prediction"], delimiter=";")
            w.writeheader()
            for p in points:
                w.writerow(
                    {
                        "sample_id": p["sample_id"],
                        "prediction": float(p["cur_dev_s"]) + 10.0,
                    }
                )

        out = tmp_path / "submission.csv"
        result = subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--model",
                str(model_path),
                "--out",
                str(out),
            ],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        # Файл создан, скрипт завершился без ошибки
        assert result.returncode == 0, f"Скрипт упал: {result.stderr}"
        assert "Предсказания модели" in result.stdout, "Должно быть сообщение о модели"
        assert "Базовая подстановка" not in result.stdout, (
            "Базовая подстановка не должна использоваться"
        )

        with out.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f, delimiter=";")
            rows = list(reader)
        for row in rows:
            sid = row["sample_id"]
            pred = float(row["prediction"])
            expected = (
                float(next(p for p in points if p["sample_id"] == sid)["cur_dev_s"])
                + 10.0
            )
            assert abs(pred - expected) < 0.01, (
                f"{sid}: prediction={pred}, ожидалось {expected}"
            )

    def test_model_missing_column(self, tmp_path: Path) -> None:
        model_path = tmp_path / "bad_model.csv"
        model_path.write_text("sample_id,wrong_col\n1;2\n", encoding="utf-8")
        out = tmp_path / "submission.csv"
        result = subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--model",
                str(model_path),
                "--out",
                str(out),
            ],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        assert result.returncode != 0, (
            "Хотя бы одна ошибка ожидается при неверных колонках"
        )


# ═══════════════════════════════════════════════════════════════════════════
# 9. Инвариант: submission.csv не должен содержать future leak
# ═══════════════════════════════════════════════════════════════════════════


class TestNoFutureLeak:
    """submission.csv генерируется только из данных, доступных на момент T.
    Никакого time_fact_begin в признаках. Проверяем, что baseline submission
    не использует факты."""

    def test_baseline_only_uses_cur_dev(self, tmp_path: Path) -> None:
        """Baseline submission = cur_dev_s. Это не заимствует из future."""
        out = tmp_path / "submission.csv"
        subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            cwd=ROOT,
            check=False,
        )
        # Убеждаемся, что все значения = cur_dev_s (уже проверено выше, но явный ассерт)
        points = {
            r["sample_id"]: float(r["cur_dev_s"])
            for r in read_csv(ROOT / "validate" / "points.csv")
        }
        with out.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f, delimiter=";")
            for row in reader:
                sid = row["sample_id"]
                pred = float(row["prediction"])
                assert abs(pred - points[sid]) < 0.01, (
                    f"{sid}: prediction {pred} ≠ cur_dev_s {points[sid]}"
                )


# ═══════════════════════════════════════════════════════════════════════════
# 10. Документация data-audit.md — проверяемые утверждения
# ═══════════════════════════════════════════════════════════════════════════


class TestDataAuditDocumentedClaims:
    """Независимая проверка числовых утверждений из docs/data-audit.md."""

    def test_md5_match(self) -> None:
        """Утверждение: md5 test/traffic.csv == validate/traffic.csv == 7c59b911..."""
        h = md5(ROOT / "test" / "traffic.csv")
        assert h == "7c59b911c7ec057dc589fb594299378b", f"md5 изменился: {h}"

    def test_train_rows_count(self) -> None:
        """Утверждение: train/traffic.csv ~287849 строк."""
        with (ROOT / "train" / "traffic.csv").open(newline="", encoding="utf-8") as f:
            rows = sum(1 for _ in csv.reader(f)) - 1  # заголовок
        assert rows > 280000, f"Слишком мало строк в train/traffic.csv: {rows}"

    def test_validate_points_count(self) -> None:
        """Утверждение: validate/points.csv = 151 точка."""
        points = read_csv(ROOT / "validate" / "points.csv")
        assert len(points) == 151, f"Ожидалось 151 точка, получено {len(points)}"

    def test_labels_train_count(self) -> None:
        """Утверждение: labels_train.csv = 4434 строки."""
        rows = read_csv(ROOT / "labels" / "labels_train.csv")
        assert len(rows) == 4434, f"Ожидалось 4434, получено {len(rows)}"

    def test_labels_test_count(self) -> None:
        """Утверждение: labels_test.csv = 353 строки."""
        rows = read_csv(ROOT / "labels" / "labels_test.csv")
        assert len(rows) == 353, f"Ожидалось 353, получено {len(rows)}"

    def test_submission_template_rows(self) -> None:
        """Утверждение: sample_submission.csv = 151 строка."""
        rows = read_csv(ROOT / "sample_submission.csv", delimiter=";")
        assert len(rows) == 151, f"Ожидалось 151, получено {len(rows)}"

    def test_target_delay_range_train(self) -> None:
        """Утверждение: target_delay_s на train: min -372, max +672."""
        rows = read_csv(ROOT / "labels" / "labels_train.csv")
        vals = [float(r["target_delay_s"]) for r in rows]
        assert min(vals) >= -400, f"min {min(vals)} — подозрительно низкий"
        assert max(vals) <= 700, f"max {max(vals)} — подозрительно высокий"

    def test_cur_dev_correlation(self) -> None:
        """Утверждение: corr(cur_dev_s, target_delay_s) ~0.51 на train."""
        rows = read_csv(ROOT / "labels" / "labels_train.csv")
        x = [float(r["cur_dev_s"]) for r in rows]
        y = [float(r["target_delay_s"]) for r in rows]
        n = len(x)
        mx = statistics.fmean(x)
        my = statistics.fmean(y)
        cov = sum((xi - mx) * (yi - my) for xi, yi in zip(x, y)) / n
        sx = (sum((xi - mx) ** 2 for xi in x) / n) ** 0.5
        sy = (sum((yi - my) ** 2 for yi in y) / n) ** 0.5
        corr = cov / (sx * sy) if sx > 0 and sy > 0 else 0.0
        assert 0.45 < corr < 0.58, (
            f"corr(cur_dev, target) = {corr:.3f} (ожидается ~0.51)"
        )


# ═══════════════════════════════════════════════════════════════════════════
# 11. Запуск make_submission.py из корня (как в инструкции)
# ═══════════════════════════════════════════════════════════════════════════


class TestEndToEndSubmission:
    """Запуск make_submission.py как в инструкции из data-audit.md."""

    def test_help(self) -> None:
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "make_submission.py"), "--help"],
            capture_output=True,
            text=True,
            cwd=ROOT,
            check=False,
        )
        assert result.returncode == 0
        assert "--model" in result.stdout
        assert "--out" in result.stdout

    def test_creates_submission_in_root(self, tmp_path: Path) -> None:
        """Создаём submission.csv в корне репозитория (как на сдачу)."""
        # Не пишем в реальный корень, используем tmp_path как 'root'
        sandbox = tmp_path / "sandbox"
        sandbox.mkdir()
        # Копируем нужные файлы
        import shutil

        for fname in ["sample_submission.csv", "validate/points.csv"]:
            src = ROOT / fname
            dst = sandbox / fname
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, dst)

        out = sandbox / "submission.csv"
        result = subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "make_submission.py"),
                "--out",
                str(out),
            ],
            capture_output=True,
            text=True,
            cwd=sandbox,
            check=False,
        )
        # Файл создан, скрипт завершился без ошибки
        assert result.returncode == 0, f"stderr: {result.stderr}"
        assert out.exists(), "submission.csv не создан"
        with out.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f, delimiter=";")
            rows = list(reader)
        assert len(rows) == 151, f"Ожидалось 151 строк, получено {len(rows)}"
