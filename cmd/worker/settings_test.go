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
		"DATABASE_URL":      "postgres://localhost/app",
		"REDIS_URL":         "redis://localhost:6379/0",
		"PURGE_WEBHOOK_URL": "https://hooks.example.com/purged",
	})
	if err != nil {
		t.Fatalf("loadSettings() error = %v", err)
	}

	want := settings{
		app:         config.App{Env: config.EnvProduction, LogLevel: zerolog.InfoLevel},
		databaseURL: "postgres://localhost/app",
		redisURL:    "redis://localhost:6379/0",

		purgeWebhookURL: "https://hooks.example.com/purged",
	}
	if got != want {
		t.Errorf("loadSettings() = %+v, want %+v", got, want)
	}
}

func TestLoadSettingsNamesEveryMissingOrInvalidSetting(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{"PURGE_WEBHOOK_URL": "hooks.example.com"})
	if err == nil {
		t.Fatal("loadSettings() error = nil, want one naming DATABASE_URL, REDIS_URL and PURGE_WEBHOOK_URL")
	}
	for _, name := range []string{"DATABASE_URL", "REDIS_URL", "PURGE_WEBHOOK_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("loadSettings() error = %q, want it to name %s", err, name)
		}
	}
}
