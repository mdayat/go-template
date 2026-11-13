// Package config reads settings from a list of sources: the environment (with a local .env file)
// and any other a project adds, such as a secret manager. Each program declares the settings it
// needs in its own cmd/<name>/settings.go.
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

// Source is somewhere settings come from. Lookup returns a setting's value and whether the source
// has it.
type Source interface {
	Lookup(key string) (string, bool)
}

// EnvSource reads settings from the process environment.
type EnvSource struct{}

// Env loads a local .env file into the process environment if there is one, then returns the
// environment as a source. Values already set in the environment win over the .env file. The .env
// values go into the environment itself, not only into this source, because some libraries read
// their own variables there (pgx reads PGPASSWORD).
func Env() (EnvSource, error) {
	err := godotenv.Load()
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return EnvSource{}, nil
	case errors.As(err, new(*fs.PathError)):
		return EnvSource{}, fmt.Errorf("read .env: %w", err) // can't open or read it
	default:
		// A syntax error: the parser's message quotes the broken value or the rest of the file,
		// secrets included, so it's left out.
		return EnvSource{}, errors.New("read .env: a line can't be parsed (check quotes and names)")
	}
}

// Lookup returns the environment variable key.
func (EnvSource) Lookup(key string) (string, bool) {
	return os.LookupEnv(key)
}

// Map is a source holding its settings in memory: tests use it, and a secret manager's source can
// fetch every secret once at startup into one.
type Map map[string]string

// Lookup returns m[key].
func (m Map) Lookup(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// Reader reads settings from its sources in order: the first source with a non-empty value wins,
// so an empty value falls through to the next source, then to the default. It collects every
// problem instead of stopping at the first, so a bad deploy shows them all at once: check Err after
// reading. Problems name the setting, never its value, which may be a secret.
type Reader struct {
	sources []Source
	errs    []error
}

// NewReader returns a reader over sources, the first one winning.
func NewReader(sources ...Source) *Reader {
	return &Reader{sources: sources}
}

func (r *Reader) lookup(key string) string {
	for _, s := range r.sources {
		if v, ok := s.Lookup(key); ok && v != "" {
			return v
		}
	}
	return ""
}

// String returns the setting key, or fallback if no source has it.
func (r *Reader) String(key, fallback string) string {
	if v := r.lookup(key); v != "" {
		return v
	}
	return fallback
}

// Required returns the setting key, recording a problem if no source has it.
func (r *Reader) Required(key string) string {
	v := r.lookup(key)
	if v == "" {
		r.errs = append(r.errs, fmt.Errorf("%s is required", key))
	}
	return v
}

// App holds the settings every program reads.
type App struct {
	Env      string // EnvDevelopment or EnvProduction
	LogLevel zerolog.Level
}

// App reads APP_ENV (default production) and LOG_LEVEL (default info).
func (r *Reader) App() App {
	app := App{Env: r.String("APP_ENV", EnvProduction)}
	if app.Env != EnvDevelopment && app.Env != EnvProduction {
		// Not a secret, and the wrong value is what the reader needs to see.
		r.errs = append(r.errs, fmt.Errorf("APP_ENV must be %q or %q, got %q", EnvDevelopment, EnvProduction, app.Env))
	}

	logLevel := r.String("LOG_LEVEL", "info")
	// Only these five: zerolog also accepts "disabled", "panic", "fatal" and numbers, which would
	// silently drop error lines.
	switch level, _ := zerolog.ParseLevel(logLevel); level {
	case zerolog.TraceLevel, zerolog.DebugLevel, zerolog.InfoLevel, zerolog.WarnLevel, zerolog.ErrorLevel:
		app.LogLevel = level
	default:
		r.errs = append(r.errs, fmt.Errorf("LOG_LEVEL must be trace, debug, info, warn or error, got %q", logLevel))
	}
	return app
}

// Err returns every problem found so far, in one error, or nil.
func (r *Reader) Err() error {
	if err := errors.Join(r.errs...); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return nil
}
