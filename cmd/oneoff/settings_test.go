package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/startup"
)

// Pins the names, defaults and required settings that runs and .example.env rely on.
func TestLoadSettings(t *testing.T) {
	t.Parallel()
	got, err := loadSettings(config.Map{"DATABASE_URL": "postgres://localhost/app"})
	if err != nil {
		t.Fatalf("loadSettings() error = %v", err)
	}

	want := settings{
		app:         config.App{Env: config.EnvProduction, LogLevel: zerolog.InfoLevel},
		databaseURL: "postgres://localhost/app",
		dbLimits:    startup.DBLimits{Statement: time.Minute, IdleInTransaction: time.Minute},
	}
	if got != want {
		t.Errorf("loadSettings() = %+v, want %+v", got, want)
	}
}

func TestLoadSettingsNamesEveryMissingOrInvalidSetting(t *testing.T) {
	t.Parallel()
	_, err := loadSettings(config.Map{"DB_STATEMENT_TIMEOUT": "0"})
	if err == nil {
		t.Fatal("loadSettings() error = nil, want one naming DATABASE_URL and DB_STATEMENT_TIMEOUT")
	}
	for _, name := range []string{"DATABASE_URL", "DB_STATEMENT_TIMEOUT"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("loadSettings() error = %q, want it to name %s", err, name)
		}
	}
}
