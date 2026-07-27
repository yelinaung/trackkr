package tracker

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	unknownApp = "unknown"

	// defaultServerURL is the local dev server from docker-compose.
	defaultServerURL = "http://localhost:8080"

	// defaultExtensionAddr binds the browser listener to loopback only.
	defaultExtensionAddr = "127.0.0.1:7600"
)

// Duration wraps time.Duration for TOML unmarshalling.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(text []byte) error {
	dur, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", text, err)
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// Config holds the daemon's configuration.
type Config struct {
	ServerURL string `toml:"server_url"`
	APIKey    string `toml:"api_key"`
	// DeviceName is informational only. The server identifies the
	// device from the API key, so changing this does not affect
	// where records land.
	DeviceName    string   `toml:"device_name"`
	PollInterval  Duration `toml:"poll_interval"`
	IdleThreshold Duration `toml:"idle_threshold"`
	FlushInterval Duration `toml:"flush_interval"`
	FlushSize     int      `toml:"flush_size"`
	DataDir       string   `toml:"data_dir"`

	// ExtensionEnabled turns on the loopback listener the browser
	// extension reports to. Off by default: a daemon on a headless box
	// has no browser talking to it.
	ExtensionEnabled bool   `toml:"extension_enabled"`
	ExtensionAddr    string `toml:"extension_addr"`
	// ExtensionToken authenticates the extension. Generate one with
	// "trackkrd -print-extension-token"; it never rotates on its own,
	// so restarting the daemon does not invalidate what is already
	// pasted into the extension.
	ExtensionToken string `toml:"extension_token"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		ServerURL:     defaultServerURL,
		DeviceName:    hostname(),
		PollInterval:  Duration{3 * time.Second},
		IdleThreshold: Duration{5 * time.Minute},
		FlushInterval: Duration{30 * time.Second},
		FlushSize:     20,
		DataDir:       DefaultDataDir(),
		ExtensionAddr: defaultExtensionAddr,
	}
}

// LoadConfig loads configuration from a TOML file, applying defaults
// first, then env var overrides.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("loading config %s: %w", path, err)
	}

	if err := cfg.finalize(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}

	return cfg, nil
}

// LoadConfigOrDefault behaves like LoadConfig, but falls back to
// defaults plus env var overrides when the file does not exist. This
// lets the daemon run from environment variables alone.
func LoadConfigOrDefault(path string) (*Config, error) {
	cfg, err := LoadConfig(path)
	if errors.Is(err, fs.ErrNotExist) {
		cfg = DefaultConfig()
		if err := cfg.finalize(); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
		return cfg, nil
	}

	return cfg, err
}

// finalize applies env var overrides, normalizes values, and
// validates the result.
func (c *Config) finalize() error {
	applyEnvOverrides(c)

	// Reporter builds request URLs by concatenation, so a trailing
	// slash would produce a double slash the server 404s on.
	c.ServerURL = strings.TrimRight(strings.TrimSpace(c.ServerURL), "/")

	// A whitespace-only key would sail past the emptiness check and
	// then be rejected by the server on every flush.
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.DeviceName = strings.TrimSpace(c.DeviceName)
	c.ExtensionToken = strings.TrimSpace(c.ExtensionToken)
	c.ExtensionAddr = strings.TrimSpace(c.ExtensionAddr)

	return c.Validate()
}

// validateServerURL checks that the URL is one Reporter can append
// "/api/v1/activity" to. A query or fragment would end up before the
// path, so they are rejected rather than silently mangled.
func validateServerURL(raw string) error {
	if raw == "" {
		return errors.New("server_url must not be empty")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("server_url %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
		return fmt.Errorf("server_url %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("server_url %q must include a host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("server_url %q must not contain a query or fragment", raw)
	}

	return nil
}

// Validate rejects configurations the daemon cannot run with. The
// intervals feed time.NewTicker, which panics on non-positive
// durations, so a typo like poll_interval = "0s" must fail at load
// time rather than at the first tick.
func (c *Config) Validate() error {
	if err := validateServerURL(c.ServerURL); err != nil {
		return err
	}
	if c.PollInterval.Duration <= 0 {
		return fmt.Errorf("poll_interval must be positive, got %s", c.PollInterval)
	}
	if c.IdleThreshold.Duration <= 0 {
		return fmt.Errorf("idle_threshold must be positive, got %s", c.IdleThreshold)
	}
	if c.FlushInterval.Duration <= 0 {
		return fmt.Errorf("flush_interval must be positive, got %s", c.FlushInterval)
	}
	if c.FlushSize <= 0 {
		return fmt.Errorf("flush_size must be positive, got %d", c.FlushSize)
	}
	if c.ExtensionEnabled {
		if err := validateExtension(c); err != nil {
			return err
		}
	}
	return nil
}

// validateExtension checks the listener can only be reached from this
// machine and is actually authenticated.
func validateExtension(c *Config) error {
	if c.ExtensionToken == "" {
		return errors.New(
			"extension_token must be set when extension_enabled is true; " +
				"generate one with: trackkrd -print-extension-token",
		)
	}

	host, port, err := net.SplitHostPort(c.ExtensionAddr)
	if err != nil {
		return fmt.Errorf("extension_addr %q must be host:port: %w", c.ExtensionAddr, err)
	}
	if port == "" {
		return fmt.Errorf("extension_addr %q needs a port", c.ExtensionAddr)
	}

	// An unauthenticated-by-accident write endpoint on the LAN is a
	// typo away, so a non-loopback bind fails at startup rather than at
	// the first request. ParseIP returns nil for "localhost", which is
	// why the name is checked separately.
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("extension_addr %q must bind to loopback (127.0.0.1, ::1, or localhost)", c.ExtensionAddr)
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("TRACKKR_SERVER_URL"); v != "" {
		cfg.ServerURL = v
	}
	if v := os.Getenv("TRACKKR_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("TRACKKR_DEVICE_NAME"); v != "" {
		cfg.DeviceName = v
	}
	if v := os.Getenv("TRACKKR_EXTENSION_TOKEN"); v != "" {
		cfg.ExtensionToken = v
	}
}

// DefaultConfigPath returns ~/.config/trackkr/config.toml.
func DefaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "trackkr", "config.toml")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "trackkr", "config.toml")
}

// DefaultDataDir returns ~/.local/share/trackkr/.
func DefaultDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "trackkr")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "share", "trackkr")
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return unknownApp
	}
	return h
}
