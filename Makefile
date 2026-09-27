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
# ML_TAG — суффикс имени артефактов: predictor.train пишет model_v1$(ML_TAG).json,
# predictor.late — model_v1$(ML_TAG)_late.json. Источник имени один, иначе
# ML_MODEL ниже и .zellij/run-ml.sh ждут файла, которого ни одна цель не
# создаёт, и make dev молча уходит в baseline.
ML_TAG ?= online
# артефакт для ML-сервиса (make ml-serve); пусто — fallback-режим cur_dev_s (§4.7)
ML_MODEL ?= ml/artifacts/model_v1$(ML_TAG).json
#: Парный P(late)-классификатор (v4). Пусти ML_LATE_MODEL= — сервис
#: работает без головы вероятностей, p_late в ответах остаётся null.
ML_LATE_MODEL ?= ml/artifacts/model_v1$(ML_TAG)_late.json
# ML_FEATURES — признаки, на которых обучается ML_MODEL: ровно те, что Go
# отдаёт в кадре (/predict, #36). По умолчанию predictor.train берёт отбор v1
# целиком — 38 колонок, из которых 20 (rolling means, zero_ratio, календарь)
# считаются только питоновской секцией as-of T. Такая модель объявляет признаки,
# которых в кадре Go нет, и MLClient справедливо отказывается её грузить
# (checkNames), поэтому live-конвейер молча уходил в baseline.
# predictor.late наследует список из metrics-файла регрессии, так что
# классификатор остаётся с той же матрицей и serve.py принимает пару.
# Правку списка Go делай здесь, а не вручную в артефакте.
ML_FEATURES ?= consecutive_late_stops,cur_dev_s,distance_to_target_m,\
	drift_last3_slope,dwell_current_s,dwell_last_s,headway_s,horizon_s,\
	is_terminal_stop,manual_fill,plan_travel_s,route_progress,slack_s,\
	speed_current,speed_seg_avg,speed_seg_max,stops_remaining,\
	target_ambiguous,trip_index
ML_PORT ?= 8000

.DEFAULT_GOAL := help

.PHONY: help setup ml-setup build test lint fmt clean \
        dev attach stop status layout-install \
        serve dashboard emu-extract emu-up emu-config emu-down emu-logs \
        emu-capture emu-plan emu-replan replan-loop real-plan real-feed \
        submission audit ml-train ml-serve ml-predict-validate capture ml-datasets \
        ml-replay-frames ml-train-late \
        docker-up docker-feed docker-down docker-logs

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
	@if [ -x $(ROOT)/ml/.venv/bin/pytest ]; then \
		$(ROOT)/ml/.venv/bin/pytest tests -q; \
	else \
		pytest tests -q; \
	fi

lint: ## статические проверки (go vet, ruff, eslint)
	cd backend && go vet ./...
	@if command -v uv >/dev/null; then \
		cd ml && uv run ruff check .; \
	elif [ -x $(ROOT)/ml/.venv/bin/ruff ]; then \
		cd ml && ../ml/.venv/bin/ruff check .; \
	else \
		echo "ruff недоступен (сначала make ml-setup) — пропускаю"; \
	fi
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

docker-up: ## поднять весь контур в Docker (мл+бэкенд+дашборд, http://localhost:8088)
	docker compose up -d --build

docker-feed: ## то же + плеер real-датасета train/ в контур
	docker compose --profile feed up -d --build

docker-down: ## снять docker-контур (том с планом остаётся: -v — удалить)
	docker compose --profile feed down

docker-logs: ## свежие логи docker-контура
	docker compose --profile feed logs --tail 30

emu-logs: ## показать лог эмулятора
	@tail -n 50 $(ROOT)/.cache/ndtp-emu/emu.log 2>/dev/null || echo "лога нет — сначала make emu-up"

# Папка записи эмулятора. purposefully не в testdata/golden: та фикстура
# коммитится и используется Go-тестами, перезаписывать её нельзя.
EMU_CAPTURE ?= $(ROOT)/.cache/emu-capture
# повторов рейса в плане: расписание абсолютное, без повторов конвейер
# через один рейс честно отказывает (no_target_in_horizon)
EMU_REPEAT ?= 5

# Живой контур по умолчанию — реальные данные датасета (train/): телеметрию
# проигрывает dataset_feed, план берётся из настоящего schedule.csv.
# Эмулятор (случайные кривые) остаётся для стресс-проверок: EMULATOR=1.
REAL_DATASET ?= train
REAL_UNITS ?= 4
REAL_SPEED ?= 1
EMU_SECONDS ?= 240

emu-capture: ## снять EMU_SECONDS (240) телеметрии эмулятора в EMU_CAPTURE
	@mkdir -p $(EMU_CAPTURE)
	cd backend && go run ./cmd/transportctl ndtp-capture --listen :9201 \
		--duration $(EMU_SECONDS)s --out $(EMU_CAPTURE)

emu-plan: ## plan.csv/binding.csv по последней записи эмулятора (EMU_CAPTURE=...)
	@test -f $(EMU_CAPTURE)/packets.bin || { echo "нет записи в $(EMU_CAPTURE) — сначала make emu-capture"; exit 1; }
	python3 scripts/ndtp_feed.py --plan-from-capture --golden $(EMU_CAPTURE)/packets.bin \
		--plan-out $(ROOT)/plan.csv --binding-out $(ROOT)/binding.csv \
		--plan-repeat $(EMU_REPEAT)

# Перепривязка на ходу: пересобрать план из свежей записи и подложить его
# работающему серверу. Запись ведёт сам сервер (--capture-out), поэтому второй
# NDTP-слушатель не нужен: на :9201 он бы и не встал, порт занят сервером.
# Сервер замечает изменение файла плана за секунду (--plan-watch), так что ни
# перезапуска, ни разрыва потока, ни потери накопленной телеметрии.
# Перепривязка нужна потому, что план описывает дорогу там, где машина была:
# без неё запись устаревает и привязка к маршруту теряется.
emu-replan: ## пересобрать plan.csv/binding.csv по записи сервера и перепривязать его
	@test -f $(EMU_CAPTURE)/packets.bin || { echo "нет записи в $(EMU_CAPTURE) — сервер запущен без --capture-out?"; exit 1; }
	python3 scripts/ndtp_feed.py --plan-from-capture --golden $(EMU_CAPTURE)/packets.bin \
		--plan-out $(ROOT)/plan.csv --binding-out $(ROOT)/binding.csv \
		--plan-repeat $(EMU_REPEAT)
	@echo "план перезаписан; работающий сервер подхватит его за секунду (--plan-watch)"

# Ездить по неписаной дороге план не умеет: экстраполяция по касательной в
# position_at разъезжается с реальным поворотом за минуты, и ТС на карте
# «сходит с маршрута». Пока сервер пишет capture непрерывно, цикл держит
# план у головы записи — привязка не теряется между перегенерациями.
REPLAN_EVERY ?= 60
replan-loop: ## live-цикл для ЭМУЛЯТОРА (EMULATOR=1): перезабирать план каждые REPLAN_EVERY с
	@echo "replan-цикл: каждые $(REPLAN_EVERY) с (Ctrl-C — остановить)"; \
	while sleep $(REPLAN_EVERY); do $(MAKE) --no-print-directory emu-replan >/dev/null || true; done

# --- реальный контур -------------------------------------------------------
# dataset_feed играет train/ как есть: телеметрия из traffic.csv, план из
# schedule.csv, синхронно на одной оси времени. Линии на дашборде — настоящие
# маршруты, стабильные между перезагрузками; задержки — настоящие план-факт.

real-plan: ## только собрать plan.csv/binding.csv из реального расписания $(REAL_DATASET)/
	python3 scripts/dataset_feed.py --dataset-dir $(REAL_DATASET) --units $(REAL_UNITS) \
		--plan-out $(ROOT)/plan.csv --binding-out $(ROOT)/binding.csv --generate-only

real-feed: ## реальный контур: проиграть $(REAL_DATASET) (телеметрия + план) в NDTP-гейтвей
	python3 scripts/dataset_feed.py --dataset-dir $(REAL_DATASET) --units $(REAL_UNITS) \
		--speed $(REAL_SPEED) --plan-out $(ROOT)/plan.csv --binding-out $(ROOT)/binding.csv


capture: ## принять 15 с телеметрии с эмулятора в EMU_CAPTURE
	@mkdir -p $(EMU_CAPTURE)
	cd backend && go run ./cmd/transportctl ndtp-capture --listen :9201 --duration 15s \
		--out $(EMU_CAPTURE)

# ── Пайплайн данных Kaggle ───────────────────────────────────────────────

submission: ## собрать submission.csv из train/validate
	python3 scripts/make_submission.py

audit: ## аудит датасета на утечки и базовые MAE
	python3 scripts/oracle_audit.py

# ── ML-пайплайн (этап 3: CatBoost v1) ────────────────────────────────────

ml-replay-frames: ## выгрузить Go-признаки кадров в ml/artifacts/features_*.jsonl
	# Тот же Go-код, что в онлайне (docs/how-it-works.md §«шаг 1»): признаки
	# считаются один раз и для обучения, и для рантайма, иначе модель и
	# конвейер работают с разными фичами.
	mkdir -p ml/artifacts
	cd backend && go run ./cmd/transportctl replay \
		-plan ../train/schedule.csv --traffic ../train/traffic.csv \
		--labels ../labels/labels_train.csv \
		--out ../ml/artifacts/features_train.jsonl
	cd backend && go run ./cmd/transportctl replay \
		-plan ../test/schedule.csv --traffic ../test/traffic.csv \
		--labels ../labels/labels_test.csv \
		--out ../ml/artifacts/features_test.jsonl
	cd backend && go run ./cmd/transportctl replay \
		-plan ../validate/schedule_plan.csv --traffic ../validate/traffic.csv \
		--labels ../validate/points.csv \
		--out ../ml/artifacts/features_validate.jsonl

ml-datasets: ## собрать датасеты parquet (предыдущая цель: ml-replay-frames)
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
		--target $(ML_TARGET) \
		--features "$(ML_FEATURES)" \
		--tag $(ML_TAG)

ml-train-late: ## обучить P(late)-классификатор для той же матрицы, что и ML_MODEL (v4)
	# predictor.late называет артефакты model_<tag>_late.json, а predictor.train —
	# model_v1<tag>.json, поэтому тег у late.py на один уровень длиннее.
	# Раньше он выводился разбором имени ML_MODEL, и смена ML_MODEL молча
	# ломала сверку метрик; здесь имя выводится из того же ML_TAG.
	ml/.venv/bin/python -m predictor.late \
		--train ml/artifacts/dataset_train.parquet \
		--holdout ml/artifacts/dataset_test.parquet \
		--metrics ml/artifacts/metrics_v1$(ML_TAG).json \
		--out-dir ml/artifacts \
		--tag v1$(ML_TAG)

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
