package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotenvDoesNotOverrideEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("EXISTING=from-file\nQUOTED=\"hello world\"\nCOMMENTED=value # comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXISTING", "from-env")
	os.Unsetenv("QUOTED")
	os.Unsetenv("COMMENTED")
	t.Cleanup(func() {
		os.Unsetenv("QUOTED")
		os.Unsetenv("COMMENTED")
	})
	if err := loadDotenv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("EXISTING"); got != "from-env" {
		t.Fatalf("existing env overwritten: %q", got)
	}
	if got := os.Getenv("QUOTED"); got != "hello world" {
		t.Fatalf("quoted value = %q", got)
	}
	if got := os.Getenv("COMMENTED"); got != "value" {
		t.Fatalf("commented value = %q", got)
	}
}

func TestLoadUsesKawangDatabaseAndSecret(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kawang := t.TempDir()
	if err := os.WriteFile(filepath.Join(kawang, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(kawang, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kawang, ".env"), []byte("DATABASE_PATH=./data/kawang.db\nJWT_SECRET=shared-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KWMEMBERSHIP_PROJECT_ROOT", root)
	t.Setenv("KAWANG_PROJECT_ROOT", kawang)
	os.Unsetenv("DATABASE_PATH")
	os.Unsetenv("JWT_SECRET")
	t.Cleanup(func() {
		os.Unsetenv("DATABASE_PATH")
		os.Unsetenv("JWT_SECRET")
	})
	t.Setenv("WORKER_POLL_MS", "5000")
	t.Setenv("DEFAULT_REQUEST_TIMEOUT_MS", "15000")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != filepath.Join(kawang, "data", "kawang.db") {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.EncryptionKey != "shared-test-secret" {
		t.Fatalf("EncryptionKey did not come from kwRedeem")
	}
}

func TestLoadRejectsDefaultKawangSecret(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kawang := t.TempDir()
	if err := os.WriteFile(filepath.Join(kawang, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KWMEMBERSHIP_PROJECT_ROOT", root)
	t.Setenv("KAWANG_PROJECT_ROOT", kawang)
	t.Setenv("JWT_SECRET", defaultJWTSecret)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "default placeholder") {
		t.Fatalf("Load() error = %v, want default placeholder rejection", err)
	}
}
