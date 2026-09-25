#!/usr/bin/env bash
# Дашборд (Vite dev-сервер) в пейне Zellij.
set -u
cd "$(dirname "$0")/../dashboard"
[ -d node_modules ] || npm install
exec npm run dev
