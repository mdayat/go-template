package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/api"
	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/startup"
)

// Pins the names, defaults and required settings that deploys and .example.env rely on.
func TestLoadSettings(t *testing.T) {
	t.Parallel()
	got, err := loadSettings(config.Map{
		"DATABASE_URL":  "postgres://localhost/app",
		"AUTH_JWKS_URL": "https://keys.example.com/jwks.json",
		"AUTH_ISSUER":   "https://issuer.example.com",
		"AUTH_AUDIENCE": "app",
		"REDIS_URL":     "redis://localhost:6379/0",
	})
	if err != nil {
		t.Fatalf("loadSettings() error = %v", err)
	}

	want := settings{
		app:         config.App{Env: config.EnvProduction, LogLevel: zerolog.InfoLevel},
		httpAddr:    ":8080",
		databaseURL: "postgres://localhost/app",
		dbLimits:    startup.DBLimits{MaxConns: 10, Statement: 10 * time.Second, IdleInTransaction: 10 * time.Second},
		auth: jwks.Config{
			KeysURL: "https://keys.example.com/jwks.json", Issuer: "https://issuer.example.com", Audience: "app",
		},
		limits:   api.Limits{PerIP: 600, PerUser: 120},
		redisURL: "redis://localhost:6379/0",
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(settings{})); diff != "" {
		t.Errorf("loadSettings() mismatch (-want +got):\n%s", diff)
	}

	got, err = loadSettings(config.Map{
		"HTTP_ADDR":           ":9090",
		"DATABASE_URL":        "x",
		"AUTH_JWKS_URL":       "https://keys.example.com/jwks.json",
		"AUTH_ISSUER":         "i",
		"AUTH_AUDIENCE":       "a",
		"REDIS_URL":           "y",
		"TRUSTED_PROXIES":     "10.0.0.0/8,2001:db8::/32",
		"RATE_LIMIT_PER_IP":   "0",
		"RATE_LIMIT_PER_USER": "30",

		"DB_MAX_CONNS":                   "25",
		"DB_STATEMENT_TIMEOUT":           "30s",
		"DB_IDLE_IN_TRANSACTION_TIMEOUT": "2s",
		"PPROF_ADDR":                     "127.0.0.1:6060",
		"SHUTDOWN_DELAY":                 "5s",
		"CORS_ALLOWED_ORIGINS":           "https://app.example.com",
	})
	if err != nil || got.httpAddr != ":9090" {
		t.Errorf("loadSettings() httpAddr = %q, error = %v; want :9090, nil", got.httpAddr, err)
	}
	if want := (startup.DBLimits{MaxConns: 25, Statement: 30 * time.Second, IdleInTransaction: 2 * time.Second}); got.dbLimits != want {
		t.Errorf("loadSettings() dbLimits = %+v, want %+v", got.dbLimits, want)
	}
	if !cmp.Equal(got.allowedOrigins, []string{"https://app.example.com"}) {
		t.Errorf("loadSettings() allowedOrigins = %q, want [https://app.example.com]", got.allowedOrigins)
	}
	if got.shutdownDelay != 5*time.Second {
		t.Errorf("loadSettings() shutdownDelay = %s, want 5s", got.shutdownDelay)
	}
	if got.pprofAddr != "127.0.0.1:6060" {
		t.Errorf("loadSettings() pprofAddr = %q, want 127.0.0.1:6060", got.pprofAddr)
	}
	wantLimits := api.Limits{TrustedProxies: []string{"10.0.0.0/8", "2001:db8::/32"}, PerIP: 0, PerUser: 30}
	if diff := cmp.Diff(wantLimits, got.limits); diff != "" {
		t.Errorf("loadSettings() limits mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadSettingsNamesEveryMissingSetting(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{})
	if err == nil {
		t.Fatal("loadSettings() error = nil, want one naming every required setting")
	}
	required := []string{
		"DATABASE_URL", "AUTH_JWKS_URL", "AUTH_ISSUER", "AUTH_AUDIENCE", "REDIS_URL",
	}
	for _, name := range required {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("loadSettings() error = %q, want it to name %s", err, name)
		}
	}
}

// Outside production the keys may come from a local file (make dev-token) or a provider's emulator
// over http://; in production only https:// will do, or anyone able to write the file or sit on the
// network could sign in as anyone.
func TestLoadSettingsChecksTheKeysURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		env, keysURL string
		ok           bool
	}{
		{"production", "https://keys.example.com/jwks.json", true},
		{"production", "file://.dev/auth/keys.json", false},
		{"production", "http://localhost:9099/keys", false},
		{"development", "file://.dev/auth/keys.json", true},
		{"development", "http://localhost:9099/keys", true},
		{"development", "ftp://keys.example.com", false},
		{"development", "https:///jwks.json", false},
	}
	for _, tt := range tests {
		_, err := loadSettings(config.Map{
			"APP_ENV":       tt.env,
			"DATABASE_URL":  "x",
			"AUTH_JWKS_URL": tt.keysURL,
			"AUTH_ISSUER":   "i",
			"AUTH_AUDIENCE": "a",
			"REDIS_URL":     "y",
		})
		if (err == nil) != tt.ok {
			t.Errorf("APP_ENV=%s AUTH_JWKS_URL=%s: loadSettings() error = %v, want ok = %v", tt.env, tt.keysURL, err, tt.ok)
		}
		if err != nil && strings.Contains(err.Error(), tt.keysURL) {
			t.Errorf("loadSettings() error = %q, want it without the value", err)
		}
	}
}

func TestLoadSettingsChecksTrustedProxies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		ok    bool
	}{
		{"10.0.0.0/8,2001:db8::/32", true},
		{"0.0.0.0/0", false},
		{"10.0.0.0/8,::/0", false},
		{"::ffff:10.0.0.0/104", false},
	}
	for _, tt := range tests {
		_, err := loadSettings(config.Map{
			"DATABASE_URL":    "x",
			"AUTH_JWKS_URL":   "https://keys.example.com/jwks.json",
			"AUTH_ISSUER":     "i",
			"AUTH_AUDIENCE":   "a",
			"REDIS_URL":       "y",
			"TRUSTED_PROXIES": tt.value,
		})
		if (err == nil) != tt.ok {
			t.Errorf("TRUSTED_PROXIES=%s: loadSettings() error = %v, want ok = %v", tt.value, err, tt.ok)
		}
	}
}

// Profiles are never on the network (startup.CheckProfileAddr has the cases).
func TestLoadSettingsKeepsProfilesLocal(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{
		"DATABASE_URL":  "postgres://localhost/app",
		"AUTH_JWKS_URL": "https://keys.example.com/jwks.json",
		"AUTH_ISSUER":   "https://issuer.example.com",
		"AUTH_AUDIENCE": "app",
		"REDIS_URL":     "redis://localhost:6379/0",
		"PPROF_ADDR":    ":6060",
	})
	if err == nil || !strings.Contains(err.Error(), "PPROF_ADDR") {
		t.Errorf("loadSettings(PPROF_ADDR=:6060) error = %v, want one naming PPROF_ADDR", err)
	}
}

// Past a minute, the platform has likely killed the API before it stops: a typo, not a choice. Like
// every length of time, 0s is refused: no delay is the setting left unset.
func TestLoadSettingsChecksShutdownDelay(t *testing.T) {
	t.Parallel()
	for value, ok := range map[string]bool{"1m": true, "61s": false, "0s": false} {
		_, err := loadSettings(config.Map{
			"DATABASE_URL":   "x",
			"AUTH_JWKS_URL":  "https://keys.example.com/jwks.json",
			"AUTH_ISSUER":    "i",
			"AUTH_AUDIENCE":  "a",
			"REDIS_URL":      "y",
			"SHUTDOWN_DELAY": value,
		})
		if (err == nil) != ok {
			t.Errorf("SHUTDOWN_DELAY=%s: loadSettings() error = %v, want ok = %v", value, err, ok)
		}
	}
}

// A pool size ConnectDB would refuse is reported with the other settings, before connecting.
func TestLoadSettingsChecksPoolSize(t *testing.T) {
	t.Parallel()
	for value, ok := range map[string]bool{"1": true, "1000": true, "0": false, "1001": false} {
		_, err := loadSettings(config.Map{
			"DATABASE_URL":  "x",
			"AUTH_JWKS_URL": "https://keys.example.com/jwks.json",
			"AUTH_ISSUER":   "i",
			"AUTH_AUDIENCE": "a",
			"REDIS_URL":     "y",
			"DB_MAX_CONNS":  value,
		})
		if (err == nil) != ok {
			t.Errorf("DB_MAX_CONNS=%s: loadSettings() error = %v, want ok = %v", value, err, ok)
		}
	}
}

// Each origin is a site exactly as a browser sends it in Origin; anything else would never match,
// or would open the API to pages it wasn't meant for.
func TestReadOrigins(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value, env string
		want       []string // nil with ok false: refused
		ok         bool
	}{
		{"", "production", nil, true},
		{"https://app.example.com", "production", []string{"https://app.example.com"}, true},
		{" https://App.Example.com , https://admin.example.com:8443", "production", []string{"https://app.example.com", "https://admin.example.com:8443"}, true},
		{"http://localhost:5173", "development", []string{"http://localhost:5173"}, true},
		{"http://localhost:5173", "production", nil, false},
		{"*", "development", nil, false},
		{"https://app.example.com/", "production", nil, false},
		{"https://app.example.com/web", "production", nil, false},
		{"https://app.example.com?", "production", nil, false},
		{"https://user@app.example.com", "production", nil, false},
		{"app.example.com", "production", nil, false},
		{"ftp://app.example.com", "production", nil, false},
		{"https://app.example.com,", "production", nil, false},
		{"https://*.example.com", "production", nil, false},
		{"https://*", "production", nil, false},
		{"https://*example.com", "production", nil, false},
		{"https://app.example.com:443", "production", nil, false},
		{"http://localhost:80", "development", nil, false},
		{"https://app.example.com:", "production", nil, false},
		{"https://app.example.com:0443", "production", nil, false},
		{"https://bücher.example", "production", nil, false},
		{"https://app_name.example.com", "production", nil, false},
		{"https://xn--bcher-kva.example", "production", []string{"https://xn--bcher-kva.example"}, true},
		{"http://[::1]:5173", "development", []string{"http://[::1]:5173"}, true},
		{"http://127.0.0.1:5173", "development", []string{"http://127.0.0.1:5173"}, true},
		{"http://localhost", "development", []string{"http://localhost"}, true},
		{"http://127.1:5173", "development", nil, false},
		{"http://127.000.000.001:5173", "development", nil, false},
		{"http://2130706433:5173", "development", nil, false},
		{"http://0x7f.0.0.1:5173", "development", nil, false},
		{"https://app.123", "production", nil, false},
		{"http://[0:0:0:0:0:0:0:1]:5173", "development", nil, false},
		{"http://[::ffff:127.0.0.1]:5173", "development", nil, false},
		{"http://localhost:65536", "development", nil, false},
		{"http://localhost:65535", "development", []string{"http://localhost:65535"}, true},
		{"https://.example.com", "production", nil, false},
		{"https://app..example.com", "production", nil, false},
		{"https://app.example.com.", "production", nil, false},
	}
	for _, tt := range tests {
		got, err := readOrigins(tt.value, tt.env)
		if (err == nil) != tt.ok || !cmp.Equal(got, tt.want) {
			t.Errorf("readOrigins(%q, %s) = %q, %v; want %q, ok = %v", tt.value, tt.env, got, err, tt.want, tt.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
			t.Errorf("readOrigins(%q) error = %q, want it to name CORS_ALLOWED_ORIGINS", tt.value, err)
		}
	}
}

// A bad CORS_ALLOWED_ORIGINS stops the API at start, with the other settings' problems.
func TestLoadSettingsChecksOrigins(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{
		"DATABASE_URL":         "x",
		"AUTH_JWKS_URL":        "https://keys.example.com/jwks.json",
		"AUTH_ISSUER":          "i",
		"AUTH_AUDIENCE":        "a",
		"REDIS_URL":            "y",
		"CORS_ALLOWED_ORIGINS": "*",
	})
	if err == nil || !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
		t.Errorf("loadSettings() error = %v, want one naming CORS_ALLOWED_ORIGINS", err)
	}
}
