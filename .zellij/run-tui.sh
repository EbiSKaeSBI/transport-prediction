#!/usr/bin/env bash
# Git-пейн: lazygit, если есть, иначе shell.
if command -v lazygit >/dev/null 2>&1; then
    exec lazygit
fi
echo "lazygit не установлен — fallback на shell"
exec bash -l
