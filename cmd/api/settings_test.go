package main

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/config"
)

// Pins the names, defaults and required settings that deploys and .example.env rely on.
func TestLoadSettings(t *testing.T) {
	t.Parallel()
	got, err := loadSettings(config.Map{
		"DATABASE_URL": "postgres://localhost/app",
		"REDIS_URL":    "redis://localhost:6379/0",
	})
	if err != nil {
		t.Fatalf("loadSettings() error = %v", err)
	}

	want := settings{
		app:         config.App{Env: config.EnvProduction, LogLevel: zerolog.InfoLevel},
		httpAddr:    ":8080",
		databaseURL: "postgres://localhost/app",
		redisURL:    "redis://localhost:6379/0",
	}
	if got != want {
		t.Errorf("loadSettings() = %+v, want %+v", got, want)
	}

	got, err = loadSettings(config.Map{"HTTP_ADDR": ":9090", "DATABASE_URL": "x", "REDIS_URL": "y"})
	if err != nil || got.httpAddr != ":9090" {
		t.Errorf("loadSettings() httpAddr = %q, error = %v; want :9090, nil", got.httpAddr, err)
	}
}

func TestLoadSettingsNamesEveryMissingSetting(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{})
	if err == nil {
		t.Fatal("loadSettings() error = nil, want one naming DATABASE_URL and REDIS_URL")
	}
	for _, name := range []string{"DATABASE_URL", "REDIS_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("loadSettings() error = %q, want it to name %s", err, name)
		}
	}
}
