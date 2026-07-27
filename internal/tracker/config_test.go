package tracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testServerURL   = "https://trackkr.example.com"
	testToken       = "tok"
	schemeHTTPTest  = "http"
	schemeHTTPSTest = "https"
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
	if cfg.ServerURL != defaultServerURL {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, defaultServerURL)
	}
	if cfg.DeviceName == "" {
		t.Error("DeviceName is empty")
	}
}

// clearTrackkrEnv unsets the TRACKKR_* overrides so a test asserting
// on file values is not affected by the caller's environment. It uses
// t.Setenv, so callers must not be parallel.
func clearTrackkrEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TRACKKR_SERVER_URL", "")
	t.Setenv("TRACKKR_API_KEY", "")
	t.Setenv("TRACKKR_DEVICE_NAME", "")
}

func TestLoadConfig(t *testing.T) {
	clearTrackkrEnv(t)
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

	if cfg.ServerURL != testServerURL {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, testServerURL)
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

func TestLoadConfigOrDefaultMissingFile(t *testing.T) {
	clearTrackkrEnv(t)
	t.Setenv("TRACKKR_API_KEY", "env_key")

	cfg, err := LoadConfigOrDefault(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("LoadConfigOrDefault: %v", err)
	}

	if cfg.APIKey != "env_key" {
		t.Errorf("APIKey = %q, want env override", cfg.APIKey)
	}
	if cfg.PollInterval.Duration != 3*time.Second {
		t.Errorf("PollInterval = %v, want default 3s", cfg.PollInterval)
	}
}

func TestLoadConfigOrDefaultExistingFile(t *testing.T) {
	clearTrackkrEnv(t)
	content := `
server_url = "https://from-file.example.com"
api_key = "from_file_key"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigOrDefault(path)
	if err != nil {
		t.Fatalf("LoadConfigOrDefault: %v", err)
	}

	if cfg.ServerURL != "https://from-file.example.com" {
		t.Errorf("ServerURL = %q, want value from file", cfg.ServerURL)
	}
	if cfg.APIKey != "from_file_key" {
		t.Errorf("APIKey = %q, want value from file", cfg.APIKey)
	}
}

func TestLoadConfigOrDefaultInvalidFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this is not = valid toml ["), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfigOrDefault(path); err == nil {
		t.Error("expected error for malformed config file")
	}
}

// A trailing slash would make Reporter build ".../api/v1/activity"
// as "...//api/v1/activity", which the server 404s on, so every
// batch would requeue forever.
func TestLoadConfigTrimsTrailingSlash(t *testing.T) {
	clearTrackkrEnv(t)
	content := `
server_url = "https://trackkr.example.com///"
api_key = "test_key"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ServerURL != testServerURL {
		t.Errorf("ServerURL = %q, want trailing slashes trimmed", cfg.ServerURL)
	}
}

func TestLoadConfigOrDefaultTrimsTrailingSlashFromEnv(t *testing.T) {
	clearTrackkrEnv(t)
	t.Setenv("TRACKKR_SERVER_URL", "https://trackkr.example.com/")
	t.Setenv("TRACKKR_API_KEY", "env_key")

	cfg, err := LoadConfigOrDefault(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("LoadConfigOrDefault: %v", err)
	}

	if cfg.ServerURL != testServerURL {
		t.Errorf("ServerURL = %q, want trailing slash trimmed", cfg.ServerURL)
	}
}

// Reporter appends "/api/v1/activity" to server_url, so anything that
// is not a plain http(s) origin either fails to send or hits the
// wrong endpoint.
func TestValidateServerURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{schemeHTTPTest, defaultServerURL, false},
		{schemeHTTPSTest, testServerURL, false},
		{"with path prefix", "https://example.com/trackkr", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"scheme only", "https:", true},
		{"no scheme", "trackkr.example.com", true},
		{"wrong scheme", "ftp://example.com", true},
		{"with query", "https://example.com?token=abc", true},
		{"with fragment", "https://example.com#frag", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			// Mirror the trimming finalize does before validating.
			cfg.ServerURL = strings.TrimRight(strings.TrimSpace(tt.url), "/")

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tt.url)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tt.url, err)
			}
		})
	}
}

func TestFinalizeTrimsWhitespace(t *testing.T) {
	clearTrackkrEnv(t)
	content := `
server_url = "  https://trackkr.example.com  "
api_key = "  spaced_key  "
device_name = "  laptop  "
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ServerURL != testServerURL {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, testServerURL)
	}
	if cfg.APIKey != "spaced_key" {
		t.Errorf("APIKey = %q, want trimmed", cfg.APIKey)
	}
	if cfg.DeviceName != "laptop" {
		t.Errorf("DeviceName = %q, want trimmed", cfg.DeviceName)
	}
}

// A whitespace-only key must not pass the daemon's emptiness check.
func TestFinalizeBlanksWhitespaceOnlyAPIKey(t *testing.T) {
	clearTrackkrEnv(t)
	content := `
server_url = "https://trackkr.example.com"
api_key = "   "
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, want empty so startup rejects it", cfg.APIKey)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults", func(*Config) {}, false},
		{"empty server url", func(c *Config) { c.ServerURL = "" }, true},
		{"zero poll interval", func(c *Config) { c.PollInterval = Duration{} }, true},
		{"negative poll interval", func(c *Config) { c.PollInterval = Duration{-time.Second} }, true},
		{"zero idle threshold", func(c *Config) { c.IdleThreshold = Duration{} }, true},
		{"zero flush interval", func(c *Config) { c.FlushInterval = Duration{} }, true},
		{"negative flush interval", func(c *Config) { c.FlushInterval = Duration{-time.Minute} }, true},
		{"zero flush size", func(c *Config) { c.FlushSize = 0 }, true},
		{"negative flush size", func(c *Config) { c.FlushSize = -1 }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			tt.mutate(cfg)

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

// A non-positive interval reaches time.NewTicker and panics, so it
// must be rejected at load time.
func TestLoadConfigRejectsNonPositiveIntervals(t *testing.T) {
	clearTrackkrEnv(t)
	content := `
api_key = "test_key"
poll_interval = "0s"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfigOrDefault(path); err == nil {
		t.Error("expected error for zero poll_interval")
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

// A typo of 0.0.0.0 would publish a write endpoint to the local network,
// so a non-loopback bind must fail at startup rather than at the first
// request.
func TestValidateExtensionListener(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"disabled needs nothing", func(c *Config) { c.ExtensionEnabled = false }, false},
		{
			"enabled with token and loopback",
			func(c *Config) { c.ExtensionEnabled = true; c.ExtensionToken = testToken },
			false,
		},
		{
			"localhost by name",
			func(c *Config) {
				c.ExtensionEnabled = true
				c.ExtensionToken = testToken
				c.ExtensionAddr = "localhost:7600"
			},
			false,
		},
		{
			"ipv6 loopback",
			func(c *Config) {
				c.ExtensionEnabled = true
				c.ExtensionToken = testToken
				c.ExtensionAddr = "[::1]:7600"
			},
			false,
		},
		{
			"enabled without a token",
			func(c *Config) { c.ExtensionEnabled = true },
			true,
		},
		{
			"all interfaces",
			func(c *Config) {
				c.ExtensionEnabled = true
				c.ExtensionToken = testToken
				c.ExtensionAddr = "0.0.0.0:7600"
			},
			true,
		},
		{
			"routable address",
			func(c *Config) {
				c.ExtensionEnabled = true
				c.ExtensionToken = testToken
				c.ExtensionAddr = "192.168.1.10:7600"
			},
			true,
		},
		{
			"missing port",
			func(c *Config) {
				c.ExtensionEnabled = true
				c.ExtensionToken = testToken
				c.ExtensionAddr = "127.0.0.1"
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			tt.mutate(cfg)

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected a validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestGenerateExtensionToken(t *testing.T) {
	t.Parallel()

	first, err := GenerateExtensionToken()
	if err != nil {
		t.Fatalf("GenerateExtensionToken: %v", err)
	}
	if len(first) != 64 {
		t.Errorf("token is %d hex chars, want 64 (32 bytes)", len(first))
	}

	second, err := GenerateExtensionToken()
	if err != nil {
		t.Fatalf("GenerateExtensionToken: %v", err)
	}
	if first == second {
		t.Error("two calls produced the same token")
	}
}
