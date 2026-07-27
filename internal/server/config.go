package server

import (
	"errors"
	"fmt"
	"os"
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
	SecureCookies bool `toml:"secure_cookies"`
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
}

type AuthConfig struct {
	SessionSecret     string `toml:"session_secret"`
	AllowRegistration bool   `toml:"allow_registration"`
}

func (s ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

func (d *DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode,
	)
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
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

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("loading config %s: %w", path, err)
	}

	// Override secrets from environment variables
	if v := os.Getenv("TRACKKR_DB_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("TRACKKR_SESSION_SECRET"); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := os.Getenv("TRACKKR_TIMEZONE"); v != "" {
		cfg.Server.Timezone = v
	}

	return cfg, nil
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
	if c.Database.Name == "" {
		return errors.New("database.name must not be empty")
	}
	if _, err := c.Server.Location(); err != nil {
		return err
	}
	return nil
}
