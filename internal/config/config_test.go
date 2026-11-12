package config

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// A bad deploy should show every wrong setting in one go, not one per restart.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "loud")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want one naming APP_ENV, DATABASE_URL and LOG_LEVEL")
	}
	for _, name := range []string{"APP_ENV", "DATABASE_URL", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("Load() error = %q, want it to name %s", err, name)
		}
	}
}

// Not parallel: t.Setenv changes the process's environment.
func TestLoadAcceptsOnlyTheFiveLogLevels(t *testing.T) {
	tests := []struct {
		logLevel string
		want     zerolog.Level
		wantOK   bool
	}{
		{"trace", zerolog.TraceLevel, true},
		{"debug", zerolog.DebugLevel, true},
		{"info", zerolog.InfoLevel, true},
		{"warn", zerolog.WarnLevel, true},
		{"error", zerolog.ErrorLevel, true},
		{"disabled", 0, false},
		{"fatal", 0, false},
		{"panic", 0, false},
		{"5", 0, false},
		{"loud", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.logLevel, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/app")
			t.Setenv("LOG_LEVEL", tt.logLevel)

			cfg, err := Load()

			if gotOK := err == nil; gotOK != tt.wantOK {
				t.Fatalf("Load() error = %v, want ok = %v", err, tt.wantOK)
			}
			if tt.wantOK && cfg.LogLevel != tt.want {
				t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, tt.want)
			}
		})
	}
}
