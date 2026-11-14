package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// A bad deploy should show every wrong setting in one go, not one per restart.
func TestReaderReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	r := NewReader(Map{"APP_ENV": "staging", "LOG_LEVEL": "loud"})

	r.App()
	r.Required("DATABASE_URL")
	r.Required("REDIS_URL")

	err := r.Err()
	if err == nil {
		t.Fatal("Err() = nil, want one naming APP_ENV, DATABASE_URL, REDIS_URL and LOG_LEVEL")
	}
	for _, name := range []string{"APP_ENV", "DATABASE_URL", "REDIS_URL", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("Err() = %q, want it to name %s", err, name)
		}
	}
}

func TestReaderTakesTheFirstSourceWithAValue(t *testing.T) {
	t.Parallel()
	r := NewReader(
		Map{"BOTH": "first", "EMPTY_IN_FIRST": ""},
		Map{"BOTH": "second", "EMPTY_IN_FIRST": "second", "ONLY_SECOND": "second"},
	)

	tests := []struct {
		key, want string
	}{
		{"BOTH", "first"},
		{"EMPTY_IN_FIRST", "second"},
		{"ONLY_SECOND", "second"},
		{"NOWHERE", "fallback"},
	}
	for _, tt := range tests {
		if got := r.String(tt.key, "fallback"); got != tt.want {
			t.Errorf("String(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
	if got := r.Required("BOTH"); got != "first" {
		t.Errorf("Required(BOTH) = %q, want first", got)
	}
	if err := r.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

// Settings are often secrets: a problem names the setting and never prints a value.
func TestReaderErrorsNameSettingsNotValues(t *testing.T) {
	t.Parallel()
	r := NewReader(Map{"DATABASE_URL": ""})

	r.Required("DATABASE_URL")

	if err := r.Err(); err == nil || err.Error() != "invalid config: DATABASE_URL is required" {
		t.Errorf("Err() = %v, want exactly \"invalid config: DATABASE_URL is required\"", err)
	}
}

func TestAppDefaults(t *testing.T) {
	t.Parallel()
	r := NewReader(Map{})

	got := r.App()

	if want := (App{Env: EnvProduction, LogLevel: zerolog.InfoLevel}); got != want {
		t.Errorf("App() = %+v, want %+v", got, want)
	}
	if err := r.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

func TestAppAcceptsOnlyTheFiveLogLevels(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			r := NewReader(Map{"LOG_LEVEL": tt.logLevel})

			app := r.App()
			err := r.Err()

			if gotOK := err == nil; gotOK != tt.wantOK {
				t.Fatalf("Err() = %v, want ok = %v", err, tt.wantOK)
			}
			if tt.wantOK && app.LogLevel != tt.want {
				t.Errorf("LogLevel = %v, want %v", app.LogLevel, tt.want)
			}
		})
	}
}

// Not parallel: it changes the working directory and the process's environment.
func TestEnvLoadsDotEnvWithoutOverridingTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	dotEnv := "CONFIG_TEST_FROM_FILE=file\nCONFIG_TEST_IN_BOTH=file\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("CONFIG_TEST_IN_BOTH", "environment")
	// Unset now, and back to unset after the test: Env puts .env values into the environment.
	t.Setenv("CONFIG_TEST_FROM_FILE", "")
	if err := os.Unsetenv("CONFIG_TEST_FROM_FILE"); err != nil {
		t.Fatal(err)
	}

	env, err := Env()
	if err != nil {
		t.Fatalf("Env() error = %v", err)
	}

	for key, want := range map[string]string{"CONFIG_TEST_FROM_FILE": "file", "CONFIG_TEST_IN_BOTH": "environment"} {
		if got, _ := env.Lookup(key); got != want {
			t.Errorf("Lookup(%q) = %q, want %q", key, got, want)
		}
	}
}

// Not parallel: it changes the working directory.
func TestEnvWithoutDotEnv(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, err := Env(); err != nil {
		t.Errorf("Env() error = %v, want nil when there is no .env", err)
	}
}

// The parser's own messages quote the broken value or the rest of the file: secrets would land in
// the startup log line. Not parallel: it changes the working directory.
func TestEnvSyntaxErrorQuotesNoValues(t *testing.T) {
	for name, dotEnv := range map[string]string{
		"unterminated quote": "CONFIG_TEST_PASSWORD=\"s3cret\nCONFIG_TEST_TOKEN=s3cret\n",
		"bad name":           "BAD-NAME=x\nCONFIG_TEST_TOKEN=s3cret\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotEnv), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)

			_, err := Env()
			if err == nil || strings.Contains(err.Error(), "s3cret") {
				t.Errorf("Env() error = %v, want an error without the file's values", err)
			}
		})
	}
}

// Not parallel: it changes the working directory.
func TestEnvReportsAnUnreadableDotEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	_, err := Env()
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("Env() error = %v, want the file error", err)
	}
}

func TestOptionalHTTPURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value  string
		want   string
		wantOK bool
	}{
		{"", "", true},
		{"https://hooks.example.com/t/abc", "https://hooks.example.com/t/abc", true},
		{"http://localhost:9000/hook", "http://localhost:9000/hook", true},
		{"ftp://example.com/hook", "", false},
		{"hooks.example.com/hook", "", false},
		{"https://", "", false},
		{"https://exa mple.com/s3cret", "", false},
		{"https://hooks.example.com/s3cret ", "", false},
		{"https://:443/s3cret", "", false},
		{"HTTPS://hooks.example.com/hook", "HTTPS://hooks.example.com/hook", true},
		{"https://[::1]:9000/hook", "https://[::1]:9000/hook", true},
	}
	for _, tt := range tests {
		r := NewReader(Map{"HOOK_URL": tt.value})

		got := r.OptionalHTTPURL("HOOK_URL")
		err := r.Err()

		if got != tt.want || (err == nil) != tt.wantOK {
			t.Errorf("OptionalHTTPURL(%q) = %q, error %v; want %q, ok = %v", tt.value, got, err, tt.want, tt.wantOK)
		}
		if err != nil && (strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "HOOK_URL")) {
			t.Errorf("OptionalHTTPURL(%q) error = %q, want it to name HOOK_URL and quote no value", tt.value, err)
		}
	}
}
