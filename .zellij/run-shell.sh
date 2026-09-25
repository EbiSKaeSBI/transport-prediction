#!/usr/bin/env bash
# Shell-пейн: fish, если есть, иначе обычный bash.
if command -v fish >/dev/null 2>&1; then
    exec fish
fi
exec bash -l
