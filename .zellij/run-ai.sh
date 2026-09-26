#!/usr/bin/env bash
# AI-пейн: repowise server с web-UI для этого репозитория.
cd "$(dirname "$0")/.."
if ! command -v repowise >/dev/null 2>&1; then
    echo "repowise не установлен — fallback на shell"
    exec bash -l
fi
exec repowise serve
