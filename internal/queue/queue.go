// Package queue is what our code knows about the job queue, without naming the library or server
// behind it. Features build Jobs and queue them through an Enqueuer; cmd/ picks the implementation
// (internal/queue/valkey), so moving to another queue means a new implementation, not edits across
// the features.
package queue

import (
	"context"
	"time"
)

// Job is one unit of background work.
type Job struct {
	// Type names the handler that runs the job, e.g. "tasks:purge_done".
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
}

// Enqueuer puts jobs on the queue for the worker (cmd/worker).
type Enqueuer interface {
	// Enqueue queues job and returns its ID, for finding the run in the worker's logs.
	Enqueue(ctx context.Context, job Job) (string, error)
}
