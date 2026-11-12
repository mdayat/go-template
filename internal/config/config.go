// Package config loads the application's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
)

// Environment names accepted in APP_ENV.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

// Config holds the settings read from environment variables.
type Config struct {
	AppEnv      string
	HTTPAddr    string
	DatabaseURL string
	LogLevel    zerolog.Level
}

// Load reads a local .env file if there is one, then the environment. It reports every missing or
// invalid setting in one error. Values already set in the environment win over the .env file.
func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("read .env: %w", err)
	}

	cfg := Config{
		AppEnv:      getenv("APP_ENV", EnvProduction),
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
	}

	var errs []error
	if cfg.AppEnv != EnvDevelopment && cfg.AppEnv != EnvProduction {
		errs = append(errs, fmt.Errorf("APP_ENV must be %q or %q, got %q", EnvDevelopment, EnvProduction, cfg.AppEnv))
	}
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	logLevel := getenv("LOG_LEVEL", "info")
	// Only these five: zerolog also accepts "disabled", "panic", "fatal" and numbers, which would
	// silently drop error lines.
	switch level, _ := zerolog.ParseLevel(logLevel); level {
	case zerolog.TraceLevel, zerolog.DebugLevel, zerolog.InfoLevel, zerolog.WarnLevel, zerolog.ErrorLevel:
		cfg.LogLevel = level
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be trace, debug, info, warn or error, got %q", logLevel))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
