"""Конфигурация Sphinx: PyDoc по Python-коду ML-модуля (пакет predictor).

Сборка: `make docs` (или ml/.venv/bin/sphinx-build -b html
docs/sphinx/source docs/sphinx/build/html). autodoc импортирует настоящие
модули из ml/src, поэтому сборку надо вести venv'ом с установленными
зависимостями ML (uv sync в ml/).
"""

import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO / "ml" / "src"))

project = "Предиктор изменений в графике движения городского транспорта"
author = "команда NDTP-предиктора"
copyright = "2026, " + author

extensions = [
    "sphinx.ext.autodoc",
    "sphinx.ext.napoleon",  # Google/numpy-секции в докстрингах
    "sphinx.ext.intersphinx",
]

# Русский язык корпуса, но технические имена — латиница.
language = "ru"
exclude_patterns = ["build"]

autodoc_member_order = "bysource"
autodoc_default_options = {
    "members": True,
    "undoc-members": True,
    "show-inheritance": True,
}

intersphinx_mapping = {"python": ("https://docs.python.org/3", None)}

html_theme = "alabaster"
html_static_path: list[str] = []
