package env

import (
	"os"

	"github.com/joho/godotenv"
)

var (
	DatabaseURL string
)

func Load() error {
	if err := godotenv.Load(); err != nil {
		return err
	}

	DatabaseURL = os.Getenv("DATABASE_URL")

	return nil
}
