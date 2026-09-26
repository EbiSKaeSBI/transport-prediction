# transport-prediction — управление проектом.
#
# Быстрый старт:   make setup && make dev
# `make dev` поднимает Zellij-сессию `tpredict` с лейаутом .zellij/tpredict.kdl:
# вкладка «поток» — backend (NDTP :9201), дашборд (Vite :5173), эмулятор (:18080).
#
# Цель `help` перечисляет всё остальное.

SHELL  := /bin/bash
ROOT   := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
SESSION := tpredict
LAYOUT  := tpredict
# юниты для конфига эмулятора (id:интервал_мс), id взяты из train/traffic.csv
EMU_UNITS ?= --unit 664030:3000 --unit 794446:3000
# таргет обучения predictor: abs — target_delay_s (v1/v1b/v2), delta —
# delay_delta_s + сборка cur_dev+дельта (v3, §5.1 architecture.md)
ML_TARGET ?= abs
# артефакт для ML-сервиса (make ml-serve); пусто — fallback-режим cur_dev_s (§4.7)
ML_MODEL ?= ml/artifacts/model_v1online.json
#: Парный P(late)-классификатор (v4). Пусти ML_LATE_MODEL= — сервис
#: работает без головы вероятностей, p_late в ответах остаётся null.
ML_LATE_MODEL ?= ml/artifacts/model_v1online_late.json
ML_PORT ?= 8000

.DEFAULT_GOAL := help

.PHONY: help setup ml-setup build test lint fmt clean \
        dev attach stop status layout-install \
        serve dashboard emu-extract emu-up emu-config emu-down emu-logs \
        submission audit ml-train ml-serve ml-predict-validate capture ml-datasets

help: ## показать список целей
	@echo "transport-prediction — доступные команды:"
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Переменные: EMU_UNITS='$(EMU_UNITS)'  SESSION=$(SESSION)"

# ── Окружение ────────────────────────────────────────────────────────────

setup: ## установить зависимости (go, npm, эмулятор при наличии tar)
	cd backend && go mod download
	cd dashboard && npm install
	@if [ -f $(ROOT)/ndtp-telemetry-emulator.tar ]; then \
		python3 scripts/emu_native.py extract; \
	else \
		echo "пропускаю эмулятор: нет ndtp-telemetry-emulator.tar в корне (см. make emu-extract)"; \
	fi

ml-setup: ## поднять python-окружение ML-ядра (uv, тяжёлые зависимости)
	cd ml && uv sync

clean: ## убрать артефакты сборки и кэши тестов
	rm -f $(ROOT)/backend/transportctl $(ROOT)/observations.jsonl
	rm -rf $(ROOT)/dashboard/dist $(ROOT)/.pytest_cache
	find $(ROOT)/scripts $(ROOT)/tests -name __pycache__ -type d -exec rm -rf {} + 2>/dev/null || true

# ── Сборка и проверки ────────────────────────────────────────────────────

build: ## собрать transportctl и прод-сборку дашборда
	cd backend && go build -o transportctl ./cmd/transportctl
	cd dashboard && npm run build

test: ## тесты: Go + pytest (phase-0 аудит и submission)
	cd backend && go test ./...
	pytest tests -q

lint: ## статические проверки (go vet, ruff, eslint)
	cd backend && go vet ./...
	cd ml && uv run ruff check . 2>/dev/null || ruff check . 2>/dev/null || echo "ruff недоступен (сначала make ml-setup) — пропускаю"
	cd dashboard && npm run lint

fmt: ## привести Go-код к gofmt
	cd backend && gofmt -w $$(find . -name '*.go')

# ── Zellij: рабочий сеанс ────────────────────────────────────────────────

layout-install: ## render лейаута с путём этого клона в ~/.config/zellij/layouts/
	@mkdir -p ~/.config/zellij/layouts
	@rm -f ~/.config/zellij/layouts/$(LAYOUT).kdl
	@sed "s|@ROOT@|$(ROOT)|g" $(ROOT)/.zellij/$(LAYOUT).kdl.in \
		> ~/.config/zellij/layouts/$(LAYOUT).kdl
	@echo "Лейаут $(LAYOUT) собран из шаблона для $(ROOT)"

dev: layout-install ## запустить (или открыть) рабочий сеанс Zellij
	@if zellij list-sessions 2>/dev/null | grep -v EXITED | grep -q "^$(SESSION)"; then \
		zellij attach $(SESSION); \
	else \
		zellij delete-session $(SESSION) -f >/dev/null 2>&1 || true; \
		cd $(ROOT) && zellij -s $(SESSION) -n $(LAYOUT); \
	fi

attach: ## подключиться к последней активной сессии Zellij
	zellij attach $$(zellij list-sessions 2>/dev/null | grep -v EXITED | head -1 | awk '{print $$1}') || zellij

status: ## состояние: сессии Zellij, процессы, порты
	@echo "--- сессии zellij ---"
	@zellij list-sessions 2>/dev/null || echo "нет сессий"
	@echo "--- процессы ---"
	@pgrep -af "[t]ransportctl|[v]ite|[e]mu_native|[a]pp.jar" | grep -v pgrep || echo "сервисы не запущены"
	@echo "--- порты ---"
	@ss -ltn 2>/dev/null | grep -E ':(9201|5173|18080)\b' || echo "порты 9201/5173/18080 свободны"

stop: ## убить сеанс Zellij и все поднятые им сервисы
	-zellij delete-session $(SESSION) -f 2>/dev/null
	-pkill -f "[t]ransportctl serve" 2>/dev/null || true
	-pkill -f "[v]ite" 2>/dev/null || true
	-python3 $(ROOT)/scripts/emu_native.py stop
	@echo "Остановлено."

# ── Отдельные сервисы (без Zellij) ───────────────────────────────────────

serve: ## запустить NDTP-сервер :9201 (dry-run, если нет plan.csv/binding.csv)
	$(ROOT)/.zellij/run-backend.sh

dashboard: ## запустить Vite dev-сервер дашборда :5173
	$(ROOT)/.zellij/run-dashboard.sh

emu-extract: ## достать JRE и app.jar эмулятора из tar-образа в корне
	python3 scripts/emu_native.py extract

emu-up: ## поднять эмулятор NDTP (:18080) в фоне
	python3 scripts/emu_native.py serve

emu-config: ## залить конфиг юнитов на поднятый эмулятор (EMU_UNITS=...)
	python3 scripts/emu_native.py config $(EMU_UNITS)

emu-down: ## остановить эмулятор
	python3 scripts/emu_native.py stop

emu-logs: ## показать лог эмулятора
	@tail -n 50 $(ROOT)/.cache/ndtp-emu/emu.log 2>/dev/null || echo "лога нет — сначала make emu-up"

capture: ## принять 15 с телеметрии с эмулятора в golden-файлы
	cd backend && go run ./cmd/transportctl ndtp-capture --listen :9201 --duration 15s

# ── Пайплайн данных Kaggle ───────────────────────────────────────────────

submission: ## собрать submission.csv из train/validate
	python3 scripts/make_submission.py

audit: ## аудит датасета на утечки и базовые MAE
	python3 scripts/oracle_audit.py

# ── ML-пайплайн (этап 3: CatBoost v1) ────────────────────────────────────

ml-datasets: ## собрать датасеты parquet (replay-кадры должны быть в ml/artifacts/*.jsonl)
	# Go-реплей (make ml-replay / capture) уже выполнен — только python-склейка.
	# Профиль скорости для speed_deficit_ratio_5m всегда из train/traffic.csv.
	ml/.venv/bin/python -m predictor.dataset \
		--frames ml/artifacts/features_train.jsonl \
		--traffic train/traffic.csv --schedule train/schedule.csv \
		--labels labels/labels_train.csv \
		--profile train/traffic.csv \
		--out ml/artifacts/dataset_train.parquet
	ml/.venv/bin/python -m predictor.dataset \
		--frames ml/artifacts/features_test.jsonl \
		--traffic test/traffic.csv --schedule test/schedule.csv \
		--labels labels/labels_test.csv \
		--profile train/traffic.csv \
		--out ml/artifacts/dataset_test.parquet
	ml/.venv/bin/python -m predictor.dataset \
		--frames ml/artifacts/features_validate.jsonl \
		--traffic validate/traffic.csv --schedule validate/schedule_plan.csv \
		--labels validate/points.csv --labels-join left \
		--profile train/traffic.csv \
		--out ml/artifacts/dataset_validate.parquet

ml-train: ## обучить CatBoost на датасетах (ML_TARGET=abs|delta, по умолчанию abs)
	ml/.venv/bin/python -m predictor.train \
		--train ml/artifacts/dataset_train.parquet \
		--holdout ml/artifacts/dataset_test.parquet \
		--out-dir ml/artifacts \
		--target $(ML_TARGET)

ml-train-late: ## обучить P(late)-классификатор для той же матрицы, что и ML_MODEL (v4)
	ml/.venv/bin/python -m predictor.late \
		--train ml/artifacts/dataset_train.parquet \
		--holdout ml/artifacts/dataset_test.parquet \
		--metrics ml/artifacts/metrics_$(patsubst model_%,%,$(basename $(notdir $(ML_MODEL)))).json \
		--out-dir ml/artifacts \
		--tag $(patsubst model_%,%,$(basename $(notdir $(ML_MODEL))))

ml-serve: ## поднять ML-сервис FastAPI :$(ML_PORT) (ML_MODEL= — fallback-режим cur_dev_s)
	cd ml && .venv/bin/python -m predictor.serve \
		$(if $(ML_MODEL),--model $(ROOT)/$(ML_MODEL),--no-model) \
		$(if $(wildcard $(ML_LATE_MODEL)),--late-model $(ROOT)/$(ML_LATE_MODEL),) \
		--host 127.0.0.1 --port $(ML_PORT)

ml-predict-validate: ## прогнать v1 по validate и собрать submission.csv
	ml/.venv/bin/python -m predictor.predict \
		--model ml/artifacts/model_v1.json \
		--dataset ml/artifacts/dataset_validate.parquet \
		--out ml/artifacts/predictions_validate.csv
	python3 scripts/make_submission.py --model ml/artifacts/predictions_validate.csv
