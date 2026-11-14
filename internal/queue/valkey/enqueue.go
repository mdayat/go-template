// Package valkey implements internal/queue on Valkey (or Redis) with asynq: queueing jobs, running
// them, and queueing them on a timetable. Only cmd/ uses it; everything else goes through
// internal/queue.
package valkey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/mdayat/go-template/internal/queue"
)

// Enqueuer puts jobs on asynq queues.
type Enqueuer struct {
	client *asynq.Client
}

// NewEnqueuer returns an Enqueuer that queues jobs on client. It doesn't close client.
func NewEnqueuer(client redis.UniversalClient) *Enqueuer {
	return &Enqueuer{client: asynq.NewClientFromRedisClient(client)}
}

// Enqueue queues job and returns asynq's ID for it.
func (e *Enqueuer) Enqueue(ctx context.Context, job queue.Job) (string, error) {
	task, err := newTask(job)
	if err != nil {
		return "", err
	}
	info, err := e.client.EnqueueContext(ctx, task)
	if err != nil {
		return "", fmt.Errorf("queue %s: %w", job.Type, err)
	}
	return info.ID, nil
}

// newTask turns job into an asynq task, refusing what asynq would silently change. It checks what
// asynq itself checks only when queueing (a type, a queue name) too: the scheduler doesn't, and
// a scheduled job without them would fail only when it fires.
func newTask(job queue.Job) (*asynq.Task, error) {
	switch {
	case strings.TrimSpace(job.Type) == "":
		return nil, errors.New("a job needs a type")
	case strings.TrimSpace(job.Queue) == "":
		return nil, fmt.Errorf("job %s: no queue", job.Type)
	// asynq keeps timeouts in whole seconds, cutting off the rest, and replaces a zero one with its
	// 30-minute default.
	case job.Timeout < time.Second || job.Timeout%time.Second != 0:
		return nil, fmt.Errorf("job %s: timeout %s is not a whole number of seconds", job.Type, job.Timeout)
	case job.MaxRetries < 0:
		return nil, fmt.Errorf("job %s: max retries %d is negative", job.Type, job.MaxRetries)
	}
	return asynq.NewTask(job.Type, job.Payload,
		asynq.Queue(job.Queue),
		asynq.Timeout(job.Timeout),
		asynq.MaxRetry(job.MaxRetries),
	), nil
}
