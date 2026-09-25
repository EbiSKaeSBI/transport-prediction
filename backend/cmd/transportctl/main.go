package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

const usage = `transportctl — предиктор изменений в графике движения

Использование:
  transportctl <команда> [флаги]

Команды:
  serve           принять NDTP-телематику и вести состояние устройств
  features        построить кадры прогноза по сохранённой телеметрии
  replay          переиграть размеченную выборку и сверить с метками
  ndtp-capture   принять NDTP-пакеты от эмулятора и сохранить в golden-файлы
  ndtp-inspect   разобрать сохранённые NDTP-пакеты (hexdump + JSON)

Примеры:
  transportctl serve --listen :9201 --plan plan.csv --binding binding.csv
  transportctl features --plan plan.csv --binding binding.csv \
      --input observations.jsonl --frames
  transportctl ndtp-capture --listen :9201 --duration 15s
  transportctl ndtp-inspect internal/ndtp/testdata/golden/packets.bin
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("не указана команда")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "features":
		return runFeatures(args[1:])
	case "replay":
		return runReplay(args[1:])
	case "ndtp-capture":
		return runCapture(args[1:])
	case "ndtp-inspect":
		return runInspect(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("неизвестная команда %q", args[0])
	}
}
