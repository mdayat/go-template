// Package testenv reads test settings the way the app reads its own: the environment first, then
// the repository's .env file.
package testenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// Get returns the setting name from the environment, else from the .env file next to go.mod. It's
// an error if neither has it, so a missing setting fails the tests instead of skipping them.
func Get(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	env, err := godotenv.Read(filepath.Join(root, ".env"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read .env: %w", err)
	}
	if v := env[name]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is not set in the environment or .env (see .example.env)", name)
}

// RepoRoot is the nearest directory holding go.mod, from the package being tested upward.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the test's directory")
		}
		dir = parent
	}
}
