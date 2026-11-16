// Package testredis gives tests a Valkey (Redis) connection, on a database the app never uses, so
// tests never touch the app's data.
//
// The server comes from TEST_REDIS_URL (the environment, else the repository's .env): a redis://
// URL to a local Valkey naming a database other than 0, which the app uses. Tests share the
// database, so each keeps to keys of its own and deletes what it made.
package testredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mdayat/go-template/internal/testenv"
)

const envURL = "TEST_REDIS_URL"

// shared is the one connection of a test binary (one binary per package), made by the first New.
//
//nolint:gochecknoglobals // one connection per test binary, shared by its tests
var shared struct {
	once   sync.Once
	client *redis.Client
	err    error
}

// New returns the connection to the test database. Don't close it: every test in the package
// shares it.
func New(tb testing.TB) *redis.Client {
	tb.Helper()
	shared.once.Do(func() { shared.client, shared.err = connect() })
	if shared.err != nil {
		tb.Fatalf("testredis: %v", shared.err)
	}
	return shared.client
}

func connect() (*redis.Client, error) {
	raw, err := testenv.Get(envURL)
	if err != nil {
		return nil, err
	}
	opts, err := localOptions(raw)
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to Valkey (is it running? `make db-up`): %w", err), client.Close())
	}
	return client, nil
}

// localOptions parses raw and refuses it unless it's a TCP address on this machine and a database
// other than 0.
func localOptions(raw string) (*redis.Options, error) {
	opts, err := redis.ParseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envURL, err)
	}
	if opts.Network != "tcp" {
		return nil, fmt.Errorf("%s must be a redis:// URL", envURL)
	}
	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envURL, err)
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
	default:
		return nil, fmt.Errorf("%s must point at localhost, got host %q: tests write there", envURL, host)
	}
	if opts.DB == 0 {
		return nil, fmt.Errorf("%s must name a database other than 0 (the app's), e.g. redis://localhost:6379/1", envURL)
	}
	return opts, nil
}
