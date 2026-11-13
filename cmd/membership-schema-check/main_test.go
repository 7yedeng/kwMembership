package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kwmembership/internal/schemacheck"
)

func TestEmptySnapshotCLIIsReadOnlyAndDoesNotClaimReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PRIVATE_EMPTY.sqlite")
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAWANG_PROJECT_ROOT", "/PRIVATE_UNAVAILABLE_PROJECT")
	t.Setenv("JWT_SECRET", "PRIVATE_UNUSED")
	var stdout, stderr bytes.Buffer
	status := runCLI(context.Background(), []string{"--database", path}, &stdout, &stderr)
	var report schemacheck.Report
	if json.Unmarshal(stdout.Bytes(), &report) != nil || status != 1 || report.Status != "missing" ||
		report.ProductionReady || report.PaymentAllowed || report.RowValuesRead || len(report.Missing) == 0 || stderr.Len() != 0 {
		t.Fatal("empty database CLI result exceeded known metadata evidence")
	}
	if strings.Contains(stdout.String(), "PRIVATE") {
		t.Fatal("CLI echoed a private path/environment value")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatal("CLI initialized an empty file")
	}
}

func TestArgumentsAndDatabaseErrorsAreNotEchoed(t *testing.T) {
	for _, arguments := range [][]string{
		nil,
		{"--unknown=PRIVATE_TOKEN"},
		{"--timeout=PRIVATE_DURATION"},
		{"--database", "PRIVATE_PATH", "PRIVATE_POSITIONAL"},
		{"--database", "file:PRIVATE.sqlite?mode=rwc"},
		{"--database", filepath.Join(t.TempDir(), "PRIVATE_MISSING.sqlite")},
	} {
		var stdout, stderr bytes.Buffer
		if runCLI(context.Background(), arguments, &stdout, &stderr) != 2 || !json.Valid(stdout.Bytes()) ||
			strings.Contains(stdout.String()+stderr.String(), "PRIVATE") || stderr.Len() != 0 {
			t.Fatal("invalid input was accepted or leaked into diagnostics")
		}
	}
}
