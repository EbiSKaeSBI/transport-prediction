#!/usr/bin/env bash
# lazydocker в пейне: если TUI нет или докер-демон недоступен, пейн не
# закрываем — оставляем shell с диагностикой.
if ! command -v lazydocker >/dev/null 2>&1; then
    echo "lazydocker не установлен"
    exec bash -l
fi
if ! lazydocker; then
    echo "lazydocker завершился с кодом $? — проверьте, что докер-демон запущен (systemctl status docker)"
    exec bash -l
fi
