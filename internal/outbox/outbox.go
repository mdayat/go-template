// Package outbox queues jobs through the database. A job is written to the outbox table, in the same
// transaction as the rows it's about when there are any, so the two are saved together or not at
// all; the worker's Relay then moves it to the job queue. Without it, saving a row and then queueing
// its job can half-fail: the row saved and the job lost, or the job run for a row never saved. And
// queueing works while the queue's own store (Valkey) is down: jobs wait in the table.
//
// A job reaches the queue within about a second (Relay's interval), under its outbox row's ID.
package outbox

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog/hlog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/queue"
)

// MaxPayload is the largest job payload Add takes. A payload names the rows a job works on, not
// their contents; a large one is slow to move to the queue and, with the relay's short time limit
// for each, could fail every time and hold up the jobs behind it.
const MaxPayload = 64 << 10

// Add writes job to the outbox through q and returns its ID, which it's queued and run under. Bind
// q to the transaction that saves the job's rows (postgres.InTx) and both are saved together. The
// ID of the API request on ctx, if any, goes with the job to its log lines.
func Add(ctx context.Context, q *db.Queries, job queue.Job) (string, error) {
	switch err := job.Check(); {
	case err != nil:
		return "", err
	case job.ID != "":
		return "", fmt.Errorf("job %s: has an ID; the outbox gives each job its own", job.Type)
	case job.Timeout > math.MaxInt32*time.Second || job.MaxRetries > math.MaxInt32:
		return "", fmt.Errorf("job %s: timeout or retries too large", job.Type)
	case len(job.Payload) > MaxPayload:
		return "", fmt.Errorf("job %s: payload of %d bytes, more than %d", job.Type, len(job.Payload), MaxPayload)
	}
	payload := job.Payload
	if payload == nil {
		payload = []byte{} // the driver writes nil as NULL, which the column refuses
	}
	requestID := job.RequestID
	if id, ok := hlog.IDFromCtx(ctx); ok && requestID == "" {
		requestID = id.String()
	}
	id, err := q.AddToOutbox(ctx, db.AddToOutboxParams{
		JobType:        job.Type,
		Queue:          job.Queue,
		Payload:        payload,
		TimeoutSeconds: int32(job.Timeout / time.Second), //nolint:gosec // checked to fit above
		MaxRetries:     int32(job.MaxRetries),            //nolint:gosec // checked to fit above
		RequestID:      nilIfEmpty(requestID),
	})
	if err != nil {
		return "", fmt.Errorf("add %s to the outbox: %w", job.Type, err)
	}
	return id.String(), nil
}

// Enqueuer is queue.Enqueuer through the outbox: each job in a transaction of its own, for code that
// only queues (Services.Jobs). Code that also saves rows calls Add in its own transaction instead.
type Enqueuer struct {
	queries *db.Queries
}

var _ queue.Enqueuer = (*Enqueuer)(nil)

// NewEnqueuer returns an Enqueuer writing through queries (on the pool, not a transaction).
func NewEnqueuer(queries *db.Queries) *Enqueuer {
	return &Enqueuer{queries: queries}
}

// Enqueue adds job to the outbox and returns its ID.
func (e *Enqueuer) Enqueue(ctx context.Context, job queue.Job) (string, error) {
	return Add(ctx, e.queries, job)
}

// jobFrom turns an outbox row back into the job to queue, under the row's ID.
func jobFrom(row db.Outbox) queue.Job {
	job := queue.Job{
		ID:         row.ID.String(),
		Type:       row.JobType,
		Queue:      row.Queue,
		Payload:    row.Payload,
		Timeout:    time.Duration(row.TimeoutSeconds) * time.Second,
		MaxRetries: int(row.MaxRetries),
	}
	if row.RequestID != nil {
		job.RequestID = *row.RequestID
	}
	return job
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
