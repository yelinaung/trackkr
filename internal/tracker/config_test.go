package tracker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()

	if cfg.PollInterval.Duration != 3*time.Second {
		t.Errorf("PollInterval = %v, want 3s", cfg.PollInterval)
	}
	if cfg.IdleThreshold.Duration != 5*time.Minute {
		t.Errorf("IdleThreshold = %v, want 5m", cfg.IdleThreshold)
	}
	if cfg.FlushInterval.Duration != 30*time.Second {
		t.Errorf("FlushInterval = %v, want 30s", cfg.FlushInterval)
	}
	if cfg.FlushSize != 20 {
		t.Errorf("FlushSize = %d, want 20", cfg.FlushSize)
	}
	if cfg.ServerURL != "http://localhost:8080" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "http://localhost:8080")
	}
	if cfg.DeviceName == "" {
		t.Error("DeviceName is empty")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	content := `
server_url = "https://trackkr.example.com"
api_key = "test_key_123"
device_name = "my-laptop"
poll_interval = "5s"
idle_threshold = "10m"
flush_interval = "1m"
flush_size = 50
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ServerURL != "https://trackkr.example.com" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "https://trackkr.example.com")
	}
	if cfg.APIKey != "test_key_123" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "test_key_123")
	}
	if cfg.DeviceName != "my-laptop" {
		t.Errorf("DeviceName = %q, want %q", cfg.DeviceName, "my-laptop")
	}
	if cfg.PollInterval.Duration != 5*time.Second {
		t.Errorf("PollInterval = %v, want 5s", cfg.PollInterval)
	}
	if cfg.IdleThreshold.Duration != 10*time.Minute {
		t.Errorf("IdleThreshold = %v, want 10m", cfg.IdleThreshold)
	}
	if cfg.FlushSize != 50 {
		t.Errorf("FlushSize = %d, want 50", cfg.FlushSize)
	}
}

func TestLoadConfigEnvOverrides(t *testing.T) {
	content := `
server_url = "https://from-file.example.com"
api_key = "from_file_key"
device_name = "from-file"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TRACKKR_SERVER_URL", "https://from-env.example.com")
	t.Setenv("TRACKKR_API_KEY", "env_key")
	t.Setenv("TRACKKR_DEVICE_NAME", "env-device")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ServerURL != "https://from-env.example.com" {
		t.Errorf("ServerURL = %q, want env override", cfg.ServerURL)
	}
	if cfg.APIKey != "env_key" {
		t.Errorf("APIKey = %q, want env override", cfg.APIKey)
	}
	if cfg.DeviceName != "env-device" {
		t.Errorf("DeviceName = %q, want env override", cfg.DeviceName)
	}
}

func TestLoadConfigMissing(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig("/nonexistent/config.toml")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestDurationUnmarshal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"3s", 3 * time.Second},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"500ms", 500 * time.Millisecond},
		{"1m30s", 90 * time.Second},
	}

	for _, tt := range tests {
		var d Duration
		if err := d.UnmarshalText([]byte(tt.input)); err != nil {
			t.Errorf("UnmarshalText(%q): %v", tt.input, err)
			continue
		}
		if d.Duration != tt.want {
			t.Errorf("UnmarshalText(%q) = %v, want %v", tt.input, d.Duration, tt.want)
		}
	}
}

func TestDurationUnmarshalInvalid(t *testing.T) {
	t.Parallel()
	var d Duration
	if err := d.UnmarshalText([]byte("not-a-duration")); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Parallel()
	path := DefaultConfigPath()
	if path == "" {
		t.Error("DefaultConfigPath returned empty string")
	}
	if filepath.Base(path) != "config.toml" {
		t.Errorf("DefaultConfigPath = %q, want config.toml basename", path)
	}
}

func TestDefaultDataDir(t *testing.T) {
	t.Parallel()
	dir := DefaultDataDir()
	if dir == "" {
		t.Error("DefaultDataDir returned empty string")
	}
	if filepath.Base(dir) != "trackkr" {
		t.Errorf("DefaultDataDir = %q, want trackkr basename", dir)
	}
}
