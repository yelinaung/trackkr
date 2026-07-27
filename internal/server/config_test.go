package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServerConfigAddr(t *testing.T) {
	cfg := ServerConfig{Host: defaultServerHost, Port: 8080}
	if got := cfg.Addr(); got != "0.0.0.0:8080" {
		t.Errorf("Addr() = %q, want %q", got, "0.0.0.0:8080")
	}
}

func TestDatabaseConfigDSN(t *testing.T) {
	cfg := DatabaseConfig{
		Host:     "localhost",
		Port:     5432,
		Name:     defaultDatabase,
		User:     defaultDatabase,
		Password: "secret",
		SSLMode:  defaultSSLMode,
	}
	want := "postgres://trackkr:secret@localhost:5432/trackkr?sslmode=disable" //nolint:gosec // test DSN
	if got := cfg.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

func TestLoadConfig(t *testing.T) {
	content := `
[server]
host = "127.0.0.1"
port = 9090

[database]
host = "dbhost"
port = 5433
name = "testdb"
user = "testuser"
password = "testpass"
sslmode = "require"

[auth]
session_secret = "supersecret"
allow_registration = true
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

	if cfg.Server.Host != testHost {
		t.Errorf("Server.Host = %q, want %q", cfg.Server.Host, testHost)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("Server.Port = %d, want %d", cfg.Server.Port, 9090)
	}
	if cfg.Database.Host != "dbhost" {
		t.Errorf("Database.Host = %q, want %q", cfg.Database.Host, "dbhost")
	}
	if cfg.Database.Port != 5433 {
		t.Errorf("Database.Port = %d, want %d", cfg.Database.Port, 5433)
	}
	if cfg.Database.Password != "testpass" {
		t.Errorf("Database.Password = %q, want %q", cfg.Database.Password, "testpass")
	}
	if cfg.Auth.SessionSecret != "supersecret" {
		t.Errorf("Auth.SessionSecret = %q, want %q", cfg.Auth.SessionSecret, "supersecret")
	}
	if !cfg.Auth.AllowRegistration {
		t.Error("Auth.AllowRegistration = false, want true")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("# empty\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Server.Host != defaultServerHost {
		t.Errorf("default Server.Host = %q, want %q", cfg.Server.Host, defaultServerHost)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("default Server.Port = %d, want %d", cfg.Server.Port, 8080)
	}
	if cfg.Database.Name != defaultDatabase {
		t.Errorf("default Database.Name = %q, want %q", cfg.Database.Name, defaultDatabase)
	}
	if cfg.Database.SSLMode != defaultSSLMode {
		t.Errorf("default Database.SSLMode = %q, want %q", cfg.Database.SSLMode, defaultSSLMode)
	}
}

func TestLoadConfigEnvOverrides(t *testing.T) {
	content := `
[database]
password = "fromfile"

[auth]
session_secret = "fromfile"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TRACKKR_DB_PASSWORD", "fromenv")
	t.Setenv("TRACKKR_SESSION_SECRET", "secretfromenv")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Database.Password != "fromenv" {
		t.Errorf("Database.Password = %q, want %q (env override)", cfg.Database.Password, "fromenv")
	}
	if cfg.Auth.SessionSecret != "secretfromenv" {
		t.Errorf("Auth.SessionSecret = %q, want %q (env override)", cfg.Auth.SessionSecret, "secretfromenv")
	}
}

func TestLoadConfigMissing(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.toml")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}
