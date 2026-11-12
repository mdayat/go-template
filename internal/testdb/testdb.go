// Package testdb gives each test its own Postgres database, copied from a master database that has
// every migration in db/migrations applied. Tests don't share rows, so they can run in parallel and
// in any order.
//
// The server comes from TEST_DATABASE_URL (the environment, else the repository's .env): a
// postgres:// URL to a database where tests may create and drop databases, such as the server's
// "postgres" database. Any host but localhost is refused, so tests can't touch a shared server.
package testdb

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const (
	envURL = "TEST_DATABASE_URL"
	// Every database this package creates starts with it; `make test-db-clean` drops leftovers by it.
	namePrefix = "gotest_"
)

// server is the master database and the connection used to copy and drop databases.
type server struct {
	admin  *pgxpool.Pool
	base   *url.URL
	master string
}

// shared is built by the first New in a test binary (one binary per package) and torn down by Main.
//
//nolint:gochecknoglobals // one master per test binary, shared by its tests
var shared struct {
	once sync.Once
	srv  *server
	err  error
}

// New returns a pool on a new database holding every migration and no rows, for tb alone. The
// database is dropped when tb ends, or kept if tb failed, with its name logged for inspection.
func New(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	shared.once.Do(func() { shared.srv, shared.err = newServer() })
	if shared.err != nil {
		tb.Fatalf("testdb: %v", shared.err)
	}
	srv := shared.srv

	name := newName()
	// Postgres copies a database only while nobody is connected to it, so nothing ever stays
	// connected to the master.
	if _, err := srv.admin.Exec(tb.Context(), "CREATE DATABASE "+ident(name)+" TEMPLATE "+ident(srv.master)); err != nil {
		tb.Fatalf("testdb: create database: %v", err)
	}

	var pool *pgxpool.Pool
	tb.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		if tb.Failed() {
			tb.Logf("testdb: kept database %s", name)
			return
		}
		// tb.Context() is already cancelled when cleanups run.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(tb.Context()), 10*time.Second)
		defer cancel()
		if _, err := srv.admin.Exec(ctx, "DROP DATABASE "+ident(name)); err != nil {
			tb.Errorf("testdb: drop database %s: %v", name, err)
		}
	})

	// Assigns the pool declared above, which the cleanup closes.
	var err error
	pool, err = pgxpool.New(tb.Context(), srv.urlFor(name))
	if err != nil {
		tb.Fatalf("testdb: connect to %s: %v", name, err)
	}
	return pool
}

// Main runs the package's tests, then drops its master database. Call it from the package's
// TestMain, or a master is left behind on every run:
//
//	func TestMain(m *testing.M) { testdb.Main(m) }
func Main(m *testing.M) {
	code := m.Run()
	if shared.srv != nil {
		if err := shared.srv.close(); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		}
	}
	os.Exit(code)
}

func newServer() (*server, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	raw, err := databaseURL(root)
	if err != nil {
		return nil, err
	}
	base, err := localURL(raw)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envURL, err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		return nil, fmt.Errorf("connect to Postgres (is it running? `make db-up`): %w", err)
	}

	srv := &server{admin: admin, base: base, master: namePrefix + "master_" + randomSuffix()}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident(srv.master)); err != nil {
		admin.Close()
		return nil, fmt.Errorf("create master database: %w", err)
	}
	if err := migrate(ctx, root, srv.urlFor(srv.master)); err != nil {
		return nil, errors.Join(err, srv.close())
	}
	return srv, nil
}

// migrate applies db/migrations with Atlas, the tool that applies them everywhere else, so tests
// run on the schema production gets (atlas.sum checked, per-file settings honoured).
func migrate(ctx context.Context, root, dbURL string) error {
	cmd := exec.CommandContext(ctx, "atlas", "migrate", "apply",
		"--url", dbURL, "--dir", "file://"+filepath.Join(root, "db", "migrations"))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("atlas is not installed or not on PATH (brew install atlas)")
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %s: %w", bytes.TrimSpace(out), err)
	}
	return nil
}

func (s *server) close() error {
	defer s.admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.admin.Exec(ctx, "DROP DATABASE "+ident(s.master)); err != nil {
		return fmt.Errorf("drop master database %s: %w", s.master, err)
	}
	return nil
}

func (s *server) urlFor(database string) string {
	u := *s.base
	u.Path = "/" + database
	return u.String()
}

// localURL parses raw and refuses it unless every host it connects to is this machine. The hosts are
// the ones pgx connects to, so a ?host= parameter or a second host can't slip past.
func localURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, fmt.Errorf("%s must be a postgres:// URL", envURL)
	}
	// Each test's copy is reached by swapping the URL's path; a dbname parameter would win over the
	// path and send every test to that one database.
	if u.Query().Has("dbname") {
		return nil, fmt.Errorf("%s: put the database in the path, not in ?dbname=", envURL)
	}
	cfg, err := pgconn.ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envURL, err)
	}
	// Host is the first host, or the socket directory when the URL names none; Fallbacks lists every
	// host to try, the first one included.
	hosts := []string{cfg.Host}
	for _, fallback := range cfg.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	for _, host := range hosts {
		switch host {
		case "localhost", "127.0.0.1", "::1":
		default:
			return nil, fmt.Errorf("%s must point at localhost, got host %q: tests create and drop databases", envURL, host)
		}
	}
	return u, nil
}

func databaseURL(root string) (string, error) {
	if v := os.Getenv(envURL); v != "" {
		return v, nil
	}
	env, err := godotenv.Read(filepath.Join(root, ".env"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read .env: %w", err)
	}
	if v := env[envURL]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is not set in the environment or .env (see .example.env)", envURL)
}

// repoRoot is the nearest directory holding go.mod, from the package being tested upward.
func repoRoot() (string, error) {
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

func newName() string {
	return namePrefix + randomSuffix()
}

// randomSuffix is unique per call, across test binaries running at the same time too.
func randomSuffix() string {
	return strings.ToLower(rand.Text())
}

func ident(name string) string {
	return pgx.Identifier{name}.Sanitize()
}
