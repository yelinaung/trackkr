package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultServerHost = "0.0.0.0"
	defaultDatabase   = "trackkr"
	defaultSSLMode    = "disable"
)

type Config struct {
	Server   ServerConfig   `toml:"server"`
	Database DatabaseConfig `toml:"database"`
	Auth     AuthConfig     `toml:"auth"`
}

type ServerConfig struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
	// Timezone names the zone a "day" is measured in. Records are
	// TIMESTAMPTZ, so a dashboard day is meaningless without one.
	Timezone string `toml:"timezone"`
	// SecureCookies marks session and CSRF cookies Secure. Default
	// true; set false only for plain-HTTP local development.
	SecureCookies     bool     `toml:"secure_cookies"`
	TrustedProxyCIDRs []string `toml:"trusted_proxy_cidrs"`
}

// Location resolves the configured timezone.
func (s ServerConfig) Location() (*time.Location, error) {
	name := s.Timezone
	if name == "" {
		name = "Local"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("loading timezone %q: %w", name, err)
	}
	return loc, nil
}

type DatabaseConfig struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Name     string `toml:"name"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	SSLMode  string `toml:"sslmode"`
	URL      string `toml:"-"`
}

type AuthConfig struct {
	SessionSecret     string `toml:"session_secret"`
	AllowRegistration bool   `toml:"allow_registration"`
}

func (s ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

func (s ServerConfig) TrustedProxies() ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(s.TrustedProxyCIDRs))
	for _, raw := range s.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing trusted proxy CIDR %q: %w", raw, err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func (d *DatabaseConfig) DSN() (string, error) {
	if d.URL != "" {
		return normalizeDatabaseURL(d.URL)
	}

	if d.Host == "" {
		return "", errors.New("database.host must not be empty")
	}
	if d.Port <= 0 || d.Port > 65535 {
		return "", fmt.Errorf("database.port %d is out of range", d.Port)
	}
	if d.Name == "" {
		return "", errors.New("database.name must not be empty")
	}
	if d.User == "" {
		return "", errors.New("database.user must not be empty")
	}
	if d.SSLMode == "" {
		return "", errors.New("database.sslmode must not be empty")
	}

	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path:   d.Name,
	}
	if d.Password == "" {
		u.User = url.User(d.User)
	} else {
		u.User = url.UserPassword(d.User, d.Password)
	}
	q := u.Query()
	q.Set("sslmode", d.SSLMode)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func LoadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("loading config %s: %w", path, err)
	}
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfigOrDefault applies defaults and environment overrides when no TOML
// configuration is present. Production containers rely on this path.
func LoadConfigOrDefault(path string) (*Config, error) {
	cfg := defaultConfig()
	if _, err := toml.DecodeFile(path, cfg); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("loading config %s: %w", path, err)
	}
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:          defaultServerHost,
			Port:          8080,
			SecureCookies: true,
		},
		Database: DatabaseConfig{
			Host:    "localhost",
			Port:    5432,
			Name:    defaultDatabase,
			User:    defaultDatabase,
			SSLMode: defaultSSLMode,
		},
	}
}

func applyEnvOverrides(cfg *Config) error {
	if v := os.Getenv("PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parsing PORT: %w", err)
		}
		cfg.Server.Port = port
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("TRACKKR_DB_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("TRACKKR_SESSION_SECRET"); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := os.Getenv("TRACKKR_TIMEZONE"); v != "" {
		cfg.Server.Timezone = v
	}
	if err := applyBoolEnv("TRACKKR_ALLOW_REGISTRATION", &cfg.Auth.AllowRegistration); err != nil {
		return err
	}
	if err := applyBoolEnv("TRACKKR_SECURE_COOKIES", &cfg.Server.SecureCookies); err != nil {
		return err
	}
	if v := os.Getenv("TRACKKR_TRUSTED_PROXY_CIDRS"); v != "" {
		cfg.Server.TrustedProxyCIDRs = strings.Split(v, ",")
		for i := range cfg.Server.TrustedProxyCIDRs {
			cfg.Server.TrustedProxyCIDRs[i] = strings.TrimSpace(cfg.Server.TrustedProxyCIDRs[i])
		}
	}
	return nil
}

func applyBoolEnv(name string, dst *bool) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = parsed
	return nil
}

func normalizeDatabaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing DATABASE_URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", errors.New("DATABASE_URL must use postgres or postgresql")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("DATABASE_URL must include a host")
	}
	if u.Path == "" || u.Path == "/" {
		return "", errors.New("DATABASE_URL must include a database name")
	}
	if u.Fragment != "" {
		return "", errors.New("DATABASE_URL must not include a fragment")
	}

	q := u.Query()
	if q.Get("sslmode") == "" {
		if !isPrivateDatabaseHost(u.Hostname()) {
			return "", errors.New("DATABASE_URL for a non-private host must include sslmode")
		}
		q.Set("sslmode", defaultSSLMode)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func isPrivateDatabaseHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	// Docker assigns service links a single-label DNS name. A public database
	// host always uses a fully qualified name and must declare its SSL policy.
	return !strings.Contains(host, ".")
}

// Validate reports config that cannot serve HTTP.
//
// It is deliberately not called from LoadConfig: cmd/server loads the
// config before dispatching the create-user subcommand, and that command
// has no business demanding a session secret it never uses. New calls
// this instead, so the rule is "serving requires a secret".
func (c *Config) Validate() error {
	if len(c.Auth.SessionSecret) < minSecretLen {
		return fmt.Errorf(
			"auth.session_secret must be at least %d bytes; set TRACKKR_SESSION_SECRET",
			minSecretLen,
		)
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is out of range", c.Server.Port)
	}
	if _, err := c.Database.DSN(); err != nil {
		return fmt.Errorf("invalid database configuration: %w", err)
	}
	if _, err := c.Server.Location(); err != nil {
		return err
	}
	if _, err := c.Server.TrustedProxies(); err != nil {
		return err
	}
	return nil
}
