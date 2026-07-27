package tracker

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

const unknownApp = "unknown"

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
	ServerURL     string   `toml:"server_url"`
	APIKey        string   `toml:"api_key"`
	DeviceName    string   `toml:"device_name"`
	PollInterval  Duration `toml:"poll_interval"`
	IdleThreshold Duration `toml:"idle_threshold"`
	FlushInterval Duration `toml:"flush_interval"`
	FlushSize     int      `toml:"flush_size"`
	DataDir       string   `toml:"data_dir"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		ServerURL:     "http://localhost:8080",
		DeviceName:    hostname(),
		PollInterval:  Duration{3 * time.Second},
		IdleThreshold: Duration{5 * time.Minute},
		FlushInterval: Duration{30 * time.Second},
		FlushSize:     20,
		DataDir:       DefaultDataDir(),
	}
}

// LoadConfig loads configuration from a TOML file, applying defaults
// first, then env var overrides.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("loading config %s: %w", path, err)
	}

	applyEnvOverrides(cfg)

	return cfg, nil
}

// LoadConfigOrDefault behaves like LoadConfig, but falls back to
// defaults plus env var overrides when the file does not exist. This
// lets the daemon run from environment variables alone.
func LoadConfigOrDefault(path string) (*Config, error) {
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading config %s: %w", path, err)
		}
		cfg := DefaultConfig()
		applyEnvOverrides(cfg)
		return cfg, nil
	}

	return LoadConfig(path)
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
