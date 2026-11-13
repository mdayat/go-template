// Package testqueue gives tests a Valkey (Redis) connection for queueing jobs, on a database the
// app never uses, so a worker running on the same machine can't pick up a test's jobs.
//
// The server comes from TEST_REDIS_URL (the environment, else the repository's .env): a redis://
// URL to a local Valkey naming a database other than 0, which the app uses. Tests share the
// database, so each one finds its own jobs by ID (Take) or uses a queue of its own (NewQueue), and
// deletes what it made.
package testqueue

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
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
		tb.Fatalf("testqueue: %v", shared.err)
	}
	return shared.client
}

// Take returns the job with id from queue and deletes it, so the shared database doesn't fill up
// with test jobs.
func Take(tb testing.TB, client *redis.Client, queue, id string) *asynq.TaskInfo {
	tb.Helper()
	inspector := asynq.NewInspectorFromRedisClient(client)
	info, err := inspector.GetTaskInfo(queue, id)
	if err != nil {
		tb.Fatalf("testqueue: get job %s: %v", id, err)
	}
	if err := inspector.DeleteTask(queue, id); err != nil {
		tb.Fatalf("testqueue: delete job %s: %v", id, err)
	}
	return info
}

// NewQueue returns a queue name for tb alone, for tests that run a job server, and deletes all that
// asynq keeps for the queue when tb ends. (asynq's own DeleteQueue leaves its counters and kept
// jobs behind.) Start the server after calling it, so the server stops before the queue goes.
func NewQueue(tb testing.TB, client *redis.Client) string {
	tb.Helper()
	queue := "test_" + strings.ToLower(rand.Text())
	tb.Cleanup(func() {
		// tb.Context() is already cancelled when cleanups run.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(tb.Context()), 5*time.Second)
		defer cancel()
		// Every key asynq writes for a queue starts with asynq:{<queue>}:.
		keys, err := client.Keys(ctx, "asynq:{"+queue+"}:*").Result()
		if err != nil {
			tb.Errorf("testqueue: list keys of queue %s: %v", queue, err)
			return
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				tb.Errorf("testqueue: delete queue %s: %v", queue, err)
			}
		}
		if err := client.SRem(ctx, "asynq:queues", queue).Err(); err != nil {
			tb.Errorf("testqueue: delete queue %s: %v", queue, err)
		}
	})
	return queue
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
		return nil, fmt.Errorf("%s must point at localhost, got host %q: tests write jobs there", envURL, host)
	}
	if opts.DB == 0 {
		return nil, fmt.Errorf("%s must name a database other than 0 (the app's), e.g. redis://localhost:6379/1", envURL)
	}
	return opts, nil
}
