package main

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/config"
)

// Pins the names, defaults and required settings that deploys and .example.env rely on.
func TestLoadSettings(t *testing.T) {
	t.Parallel()
	got, err := loadSettings(config.Map{
		"DATABASE_URL":  "postgres://localhost/app",
		"AUTH_JWKS_URL": "https://keys.example.com/jwks.json",
		"AUTH_ISSUER":   "https://issuer.example.com",
		"AUTH_AUDIENCE": "app",
		// remove:worker
		"REDIS_URL": "redis://localhost:6379/0",
		// remove:end
	})
	if err != nil {
		t.Fatalf("loadSettings() error = %v", err)
	}

	want := settings{
		app:         config.App{Env: config.EnvProduction, LogLevel: zerolog.InfoLevel},
		httpAddr:    ":8080",
		databaseURL: "postgres://localhost/app",
		auth: jwks.Config{
			KeysURL: "https://keys.example.com/jwks.json", Issuer: "https://issuer.example.com", Audience: "app",
		},
		// remove:worker
		redisURL: "redis://localhost:6379/0",
		// remove:end
	}
	if got != want {
		t.Errorf("loadSettings() = %+v, want %+v", got, want)
	}

	got, err = loadSettings(config.Map{
		"HTTP_ADDR":     ":9090",
		"DATABASE_URL":  "x",
		"AUTH_JWKS_URL": "https://keys.example.com/jwks.json",
		"AUTH_ISSUER":   "i",
		"AUTH_AUDIENCE": "a",
		// remove:worker
		"REDIS_URL": "y",
		// remove:end
	})
	if err != nil || got.httpAddr != ":9090" {
		t.Errorf("loadSettings() httpAddr = %q, error = %v; want :9090, nil", got.httpAddr, err)
	}
}

func TestLoadSettingsNamesEveryMissingSetting(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{})
	if err == nil {
		t.Fatal("loadSettings() error = nil, want one naming every required setting")
	}
	required := []string{
		"DATABASE_URL", "AUTH_JWKS_URL", "AUTH_ISSUER", "AUTH_AUDIENCE",
		// remove:worker
		"REDIS_URL",
		// remove:end
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
			// remove:worker
			"REDIS_URL": "y",
			// remove:end
		})
		if (err == nil) != tt.ok {
			t.Errorf("APP_ENV=%s AUTH_JWKS_URL=%s: loadSettings() error = %v, want ok = %v", tt.env, tt.keysURL, err, tt.ok)
		}
		if err != nil && strings.Contains(err.Error(), tt.keysURL) {
			t.Errorf("loadSettings() error = %q, want it without the value", err)
		}
	}
}
