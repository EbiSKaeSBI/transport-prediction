package main

import (
	"flag"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseInterspersedFlagsAfterPositional(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	limit := fs.Int("limit", 5, "")

	paths, err := parseInterspersed(fs, []string{"/tmp/packets.bin", "--json", "--limit", "2"})
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/tmp/packets.bin" {
		t.Fatalf("paths %v, ожидался один путь", paths)
	}
	if !*asJSON {
		t.Error("--json после позиционного аргумента должен применяться")
	}
	if *limit != 2 {
		t.Errorf("--limit %d, ожидалось 2", *limit)
	}
}

func TestParseInterspersedFlagsBeforePositional(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	out := fs.String("out", "", "")

	paths, err := parseInterspersed(fs, []string{"--json", "--out", "/tmp/x", "/tmp/p.bin"})
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/tmp/p.bin" {
		t.Fatalf("paths %v, ожидался только /tmp/p.bin", paths)
	}
	if !*asJSON {
		t.Error("--json должен применяться")
	}
	if *out != "/tmp/x" {
		t.Errorf("--out %q, ожидалось /tmp/x", *out)
	}
}

func TestParseInterspersedMultiplePositionals(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	limit := fs.Int("limit", 5, "")

	paths, err := parseInterspersed(fs, []string{"a.bin", "--limit", "3", "b.bin"})
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	want := []string{"a.bin", "b.bin"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("paths %v, ожидалось %v", paths, want)
	}
	if *limit != 3 {
		t.Errorf("--limit %d, ожидалось 3", *limit)
	}
}

func TestParseInterspersedNoArgs(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	paths, err := parseInterspersed(fs, nil)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths %v, ожидалось пусто", paths)
	}
}

func TestRunInspectRejectsMissingPath(t *testing.T) {
	if err := runInspect([]string{"--json"}); err == nil {
		t.Fatal("ожидалась ошибка при отсутствии пути к файлу")
	}
}

func TestRunInspectReadsGolden(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "ndtp", "testdata", "golden", "packets.bin")
	if err := runInspect([]string{path, "--json", "--limit", "1"}); err != nil {
		t.Fatalf("разбор golden-файла: %v", err)
	}
}
