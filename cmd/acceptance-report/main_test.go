package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/acceptance"
)

func TestRunEmitsRejectedJSONAndNonzeroExit(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{
			"-db", filepath.Join(t.TempDir(), "acceptance.db"),
			"-runs", "1",
		},
		&stdout,
		&stderr,
	)
	if code == 0 {
		t.Fatal("пустая база ошибочно принята")
	}
	if stderr.Len() != 0 {
		t.Fatalf("неожиданный stderr: %s", stderr.String())
	}
	var report acceptance.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout не является JSON-отчётом: %v; вывод: %s", err, stdout.String())
	}
	if report.Accepted || report.RequiredRuns != 1 || len(report.Reasons) == 0 {
		t.Fatalf("неверный отчёт для пустой базы: %+v", report)
	}
}

func TestRunEmitsJSONForInvalidSince(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{"-since", "не-время"},
		&stdout,
		&stderr,
	)
	if code == 0 {
		t.Fatal("некорректный -since ошибочно принят")
	}
	if stderr.Len() != 0 {
		t.Fatalf("неожиданный stderr: %s", stderr.String())
	}
	var report acceptance.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("ошибка параметров выведена не в JSON: %v; вывод: %s", err, stdout.String())
	}
	if report.Accepted || len(report.Reasons) != 1 {
		t.Fatalf("неверный JSON ошибки параметров: %+v", report)
	}
}
