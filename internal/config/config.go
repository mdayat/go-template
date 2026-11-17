// Package config reads settings from a list of sources: the environment (with a local .env file)
// and any other a project adds, such as a secret manager. Each program declares the settings it
// needs in its own cmd/<name>/settings.go.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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

// Fail the build here, not only where a program passes a source in, if one stops implementing
// Source.
var (
	_ Source = EnvSource{}
	_ Source = Map(nil)
)

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

// OptionalHTTPURL returns the setting key, an absolute http:// or https:// URL, or "" if no source
// has it. A value that isn't one is recorded as a problem; the problem doesn't quote it, since a
// URL can hold a token.
func (r *Reader) OptionalHTTPURL(key string) string {
	v := r.lookup(key)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || strings.TrimSpace(v) != v || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		r.errs = append(r.errs, fmt.Errorf("%s must be an http:// or https:// URL", key))
		return ""
	}
	return v
}

// Count returns the setting key, a whole number of 0 or more, or fallback if no source has it. A
// value that isn't one is recorded as a problem.
func (r *Reader) Count(key string, fallback int) int {
	v := r.lookup(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		r.errs = append(r.errs, fmt.Errorf("%s must be a whole number, 0 or more", key))
		return fallback
	}
	return n
}

// Duration returns the setting key, a length of time above 0 in whole milliseconds, written as Go
// writes durations (500ms, 10s, 5m, 1h30m), or fallback if no source has it. A value that isn't one
// is recorded as a problem.
func (r *Reader) Duration(key string, fallback time.Duration) time.Duration {
	v := r.lookup(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d%time.Millisecond != 0 {
		r.errs = append(r.errs, fmt.Errorf("%s must be a length of time above 0 like 500ms, 10s or 5m, in whole milliseconds", key))
		return fallback
	}
	return d
}

// IPRanges returns the setting key, a comma-separated list of IP ranges in CIDR form (10.0.0.0/8,
// 2001:db8::/32, 203.0.113.7/32 for one address), or nil if no source has it. A list with an entry
// that isn't one is recorded as a problem.
func (r *Reader) IPRanges(key string) []string {
	v := r.lookup(key)
	if v == "" {
		return nil
	}
	var ranges []string
	for entry := range strings.SplitSeq(v, ",") {
		entry = strings.TrimSpace(entry)
		if _, err := netip.ParsePrefix(entry); err != nil {
			r.errs = append(r.errs, fmt.Errorf("%s must be comma-separated IP ranges like 10.0.0.0/8", key))
			return nil
		}
		ranges = append(ranges, entry)
	}
	return ranges
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
