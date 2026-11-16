// Package testqueue helps tests that queue jobs: finding and deleting a test's own jobs, and queues
// of their own. Their connection comes from testredis.New, on a database the app never uses, so a
// worker running on the same machine can't pick up a test's jobs. Tests share that database, so
// each one finds its own jobs by ID (Take) or uses a queue of its own (NewQueue), and deletes what
// it made.
package testqueue

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

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
