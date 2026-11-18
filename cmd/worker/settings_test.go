package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/startup"
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
		dbLimits:    startup.DBLimits{Statement: 5 * time.Minute, IdleInTransaction: time.Minute},
		redisURL:    "redis://localhost:6379/0",

		purgeWebhookURL: "https://hooks.example.com/purged",
	}
	if got != want {
		t.Errorf("loadSettings() = %+v, want %+v", got, want)
	}
}

func TestLoadSettingsNamesEveryMissingOrInvalidSetting(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{"PURGE_WEBHOOK_URL": "hooks.example.com", "DB_STATEMENT_TIMEOUT": "0", "PPROF_ADDR": "0.0.0.0:6060"})
	if err == nil {
		t.Fatal("loadSettings() error = nil, want one naming every bad setting")
	}
	for _, name := range []string{"DATABASE_URL", "REDIS_URL", "PURGE_WEBHOOK_URL", "DB_STATEMENT_TIMEOUT", "PPROF_ADDR"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("loadSettings() error = %q, want it to name %s", err, name)
		}
	}
}
