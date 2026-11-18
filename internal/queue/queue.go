// Package queue is what our code knows about the job queue, without naming the library or server
// behind it. Features build Jobs and queue them through an Enqueuer; cmd/ picks the implementation
// (internal/queue/valkey), so moving to another queue means a new implementation, not edits across
// the features.
package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Job is one unit of background work.
type Job struct {
	// Type names the handler that runs the job, e.g. "deleted_rows:purge".
	Type string
	// Queue names the queue the job goes to; the worker reads the queues it's given.
	Queue string
	// Payload is the job's input, usually JSON. It sits in the queue in plain text: no secrets or
	// personal data.
	Payload []byte
	// Timeout is how long one run may take, in whole seconds (at least one); its context ends then.
	Timeout time.Duration
	// MaxRetries is how many times a failed run is tried again; 0 runs it once.
	MaxRetries int
	// ID, if set, is the job's ID in the queue, so queueing it twice is refused with ErrDuplicate
	// while the first is held (the outbox queues each job under its row's ID). Unset, the queue
	// makes one up.
	ID string
	// RequestID is the ID of the API request that queued it, for its log lines (Run.RequestID).
	// Unset, the queue takes it from the context, if a request's.
	RequestID string
}

// ErrDuplicate is Enqueue's error for a job whose ID the queue still holds: it's queued already.
var ErrDuplicate = errors.New("a job with this ID is queued already")

// Check refuses what a queue would silently change or fail on later: no type, no queue, a time
// limit that isn't a whole number of seconds (at least one), negative retries. Checked when
// queueing, and before a job is kept anywhere to be queued later.
func (j Job) Check() error {
	switch {
	case strings.TrimSpace(j.Type) == "":
		return errors.New("a job needs a type")
	case strings.TrimSpace(j.Queue) == "":
		return fmt.Errorf("job %s: no queue", j.Type)
	case j.Timeout < time.Second || j.Timeout%time.Second != 0:
		return fmt.Errorf("job %s: timeout %s is not a whole number of seconds", j.Type, j.Timeout)
	case j.MaxRetries < 0:
		return fmt.Errorf("job %s: max retries %d is negative", j.Type, j.MaxRetries)
	}
	return nil
}

// Limit is a queue the worker takes jobs from, and how many of its jobs one worker instance (one
// running copy of the worker) runs at once. Each queue's limit is its own: slow jobs filling one
// queue's slots don't hold up another's.
type Limit struct {
	Queue       string
	Concurrency int
}

// Enqueuer puts jobs on the queue for the worker (cmd/worker).
type Enqueuer interface {
	// Enqueue queues job and returns its ID, for finding the run in the worker's logs.
	Enqueue(ctx context.Context, job Job) (string, error)
}
