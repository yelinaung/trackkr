package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestRunWithoutAPIKey(t *testing.T) {
	t.Setenv("TRACKKR_API_KEY", "")

	logger := zerolog.Nop()
	err := run(&logger, filepath.Join(t.TempDir(), "absent.toml"))
	if err == nil {
		t.Fatal("expected error when no api_key is configured")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error = %v, want it to mention api_key", err)
	}
}

// The config file is parsed before any env var is consulted, so this
// test needs no t.Setenv and can run in parallel.
func TestRunWithInvalidConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("not = valid toml ["), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := zerolog.Nop()
	err := run(&logger, path)
	if err == nil {
		t.Fatal("expected error for malformed config file")
	}
	if !strings.Contains(err.Error(), "loading config") {
		t.Errorf("error = %v, want it to mention loading config", err)
	}
}
