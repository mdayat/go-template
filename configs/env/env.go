// Package env loads the application's settings from environment variables.
package env

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

// Env holds the settings read from environment variables.
type Env struct {
	DatabaseURL string
}

// Load reads the .env file into the environment and returns the settings.
func Load() (Env, error) {
	if err := godotenv.Load(); err != nil {
		return Env{}, fmt.Errorf("load .env: %w", err)
	}

	return Env{DatabaseURL: os.Getenv("DATABASE_URL")}, nil
}
