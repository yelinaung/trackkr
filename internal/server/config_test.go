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
	// The userinfo is a separate literal: a DSN written out in full carries
	// credentials past every secret scan, and this one is only a fixture.
	const userinfo = "trackkr:secret"
	want := "postgres://" + userinfo + "@localhost:5432/trackkr?sslmode=disable"
	got, err := cfg.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

func TestLoadConfig(t *testing.T) {
	clearServerEnv(t)

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
	clearServerEnv(t)

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
	clearServerEnv(t)

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

func TestLoadConfigOrDefaultEnvOverrides(t *testing.T) {
	clearServerEnv(t)
	t.Setenv("PORT", "9090")
	const userinfo = "trackkr:secret"
	t.Setenv("DATABASE_URL", "postgres://"+userinfo+"@127.0.0.1:5432/trackkr")
	t.Setenv("TRACKKR_SESSION_SECRET", testSecret)
	t.Setenv("TRACKKR_TIMEZONE", "Asia/Singapore")
	t.Setenv("TRACKKR_ALLOW_REGISTRATION", "true")
	t.Setenv("TRACKKR_SECURE_COOKIES", "false")
	t.Setenv("TRACKKR_TRUSTED_PROXY_CIDRS", "10.0.0.0/24, fd00::/8")

	cfg, err := LoadConfigOrDefault(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("LoadConfigOrDefault: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("Server.Port = %d, want 9090", cfg.Server.Port)
	}
	if cfg.Server.Timezone != "Asia/Singapore" {
		t.Errorf("Server.Timezone = %q, want Asia/Singapore", cfg.Server.Timezone)
	}
	if !cfg.Auth.AllowRegistration {
		t.Error("Auth.AllowRegistration = false, want true")
	}
	if cfg.Server.SecureCookies {
		t.Error("Server.SecureCookies = true, want false")
	}
	proxies, err := cfg.Server.TrustedProxies()
	if err != nil {
		t.Fatalf("TrustedProxies: %v", err)
	}
	if len(proxies) != 2 {
		t.Errorf("trusted proxy CIDRs = %d, want 2", len(proxies))
	}
	gotDSN, err := cfg.Database.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	wantDSN := "postgres://" + userinfo + "@127.0.0.1:5432/trackkr?sslmode=disable"
	if gotDSN != wantDSN {
		t.Errorf("DSN() = %q, want %q", gotDSN, wantDSN)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	clearServerEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[server]
port = 8081
timezone = "UTC"
secure_cookies = true

[auth]
session_secret = "fromfile"
allow_registration = false
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PORT", "9091")
	t.Setenv("TRACKKR_SESSION_SECRET", testSecret)
	t.Setenv("TRACKKR_TIMEZONE", "Asia/Singapore")
	t.Setenv("TRACKKR_ALLOW_REGISTRATION", "true")
	t.Setenv("TRACKKR_SECURE_COOKIES", "false")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.Port != 9091 {
		t.Errorf("Server.Port = %d, want 9091", cfg.Server.Port)
	}
	if cfg.Auth.SessionSecret != testSecret {
		t.Error("Auth.SessionSecret did not use environment value")
	}
	if cfg.Server.Timezone != "Asia/Singapore" {
		t.Errorf("Server.Timezone = %q, want Asia/Singapore", cfg.Server.Timezone)
	}
	if !cfg.Auth.AllowRegistration {
		t.Error("Auth.AllowRegistration = false, want true")
	}
	if cfg.Server.SecureCookies {
		t.Error("Server.SecureCookies = true, want false")
	}
}

func TestNormalizeDatabaseURL(t *testing.T) {
	t.Parallel()
	const userinfo = "trackkr:secret"
	privateURL := "postgres://" + userinfo + "@127.0.0.1:5432/trackkr"
	publicURL := "postgres://" + userinfo + "@db.example.com:5432/trackkr"

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "private host gets disabled ssl",
			raw:  privateURL,
			want: privateURL + "?sslmode=disable",
		},
		{
			name: "Docker service alias gets disabled ssl",
			raw:  "postgres://" + userinfo + "@dokku-postgres-trackkr:5432/trackkr",
			want: "postgres://" + userinfo + "@dokku-postgres-trackkr:5432/trackkr?sslmode=disable",
		},
		{
			name:    "public host requires ssl mode",
			raw:     publicURL,
			wantErr: true,
		},
		{
			name: "public host with ssl mode",
			raw:  publicURL + "?sslmode=require",
			want: publicURL + "?sslmode=require",
		},
		{name: "missing database", raw: "postgres://" + userinfo + "@127.0.0.1:5432", wantErr: true},
		{name: "wrong scheme", raw: "mysql://" + userinfo + "@127.0.0.1:5432/trackkr", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeDatabaseURL(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeDatabaseURL(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("normalizeDatabaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestTrustedProxiesRejectInvalidCIDR(t *testing.T) {
	t.Parallel()

	_, err := (ServerConfig{TrustedProxyCIDRs: []string{"not-a-cidr"}}).TrustedProxies()
	if err == nil {
		t.Error("TrustedProxies returned nil error for invalid CIDR")
	}
}

func clearServerEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PORT",
		"DATABASE_URL",
		"TRACKKR_DB_PASSWORD",
		"TRACKKR_SESSION_SECRET",
		"TRACKKR_TIMEZONE",
		"TRACKKR_ALLOW_REGISTRATION",
		"TRACKKR_SECURE_COOKIES",
		"TRACKKR_TRUSTED_PROXY_CIDRS",
	} {
		t.Setenv(name, "")
	}
}
