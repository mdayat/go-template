package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/postgres"
	"github.com/mdayat/go-template/internal/queue"
)

const (
	// relayInterval is how often Relay looks for jobs: the most a job waits in the outbox while the
	// queue takes jobs.
	relayInterval = time.Second
	// relayBatch is the most jobs one transaction moves; a full one is followed at once by the next.
	relayBatch = 100
)

// Relay moves jobs from the outbox to the job queue: every worker instance runs one, and each job is
// taken by one of them (rows another has locked are skipped).
type Relay struct {
	pool     *postgres.Pool
	queue    queue.Enqueuer
	logger   zerolog.Logger
	interval time.Duration // relayInterval; tests set another
	// stuck says the last move failed, so only a change between moving and stuck is logged.
	stuck bool
}

// NewRelay returns a Relay moving jobs from pool's outbox to queue, logging to logger.
func NewRelay(pool *postgres.Pool, queue queue.Enqueuer, logger zerolog.Logger) *Relay {
	return &Relay{pool: pool, queue: queue, logger: logger, interval: relayInterval}
}

// Run moves jobs until ctx ends. A job taken when it ends stays in the outbox for the next run.
func (r *Relay) Run(ctx context.Context) {
	for {
		moved, err := r.moveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		r.report(err)
		if err == nil && moved == relayBatch {
			continue // more are waiting
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.interval):
		}
	}
}

// relayTimeout bounds one batch's transaction, which holds its rows' locks and a connection. A batch
// stops taking jobs at half of it, leaving the rest time to delete and commit.
const relayTimeout = 30 * time.Second

// moveOnce queues the oldest jobs, each under its row's ID, and deletes those queued, in one
// transaction: a job is deleted only once the queue has it. If the transaction then fails, its jobs
// are queued again next time and refused as duplicates (queue.ErrDuplicate) while the queue holds
// them. It stops at the first job the queue won't take (likely down: the rest would fail too),
// keeping those moved before it.
//
// The transaction doesn't end with ctx: a batch cut off by shutdown would roll back the deletes of
// jobs already queued, and one finished and dropped by the queue meanwhile would then run again.
// When ctx ends it stops taking jobs and saves what it moved.
func (r *Relay) moveOnce(ctx context.Context) (int, error) {
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayTimeout)
	defer cancel()
	start := time.Now()
	var moved int
	var queueErr error
	err := postgres.InTx(txCtx, r.pool, func(q *db.Queries) error {
		moved, queueErr = 0, nil // InTx may run this again
		rows, err := q.TakeFromOutbox(txCtx, relayBatch)
		if err != nil {
			return err
		}
		var done []uuid.UUID
		for _, row := range rows {
			if ctx.Err() != nil || time.Since(start) > relayTimeout/2 {
				break
			}
			job := jobFrom(row)
			if err := job.Check(); err != nil {
				// The queue would never take it (written by other code, or before a check got
				// stricter): kept, it would block every job behind it, as rows go oldest first.
				r.logger.Error().Err(err).Str("outbox_id", row.ID.String()).Str("job_type", row.JobType).
					Msg("outbox: job dropped, as the queue would never take it")
				done = append(done, row.ID)
				continue
			}
			_, err := r.queue.Enqueue(txCtx, job)
			if err != nil && !errors.Is(err, queue.ErrDuplicate) {
				queueErr = err
				break
			}
			done = append(done, row.ID)
		}
		if len(done) > 0 {
			if err := q.DeleteFromOutbox(txCtx, done); err != nil {
				return err
			}
		}
		moved = len(done)
		return nil // commits the deletes even when the queue failed partway
	})
	if err != nil {
		return 0, err
	}
	return moved, queueErr
}

// report logs when jobs stop moving and when they move again, not on every try: the outbox's metrics
// (RecordBacklog) show how far behind it is meanwhile.
func (r *Relay) report(err error) {
	switch {
	case err != nil && !r.stuck:
		r.stuck = true
		r.logger.Warn().Err(err).Msg("outbox: jobs aren't moving to the queue; they wait in the outbox")
	case err == nil && r.stuck:
		r.stuck = false
		r.logger.Info().Msg("outbox: jobs moving to the queue again")
	}
}
