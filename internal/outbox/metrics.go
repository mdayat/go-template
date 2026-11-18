package outbox

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/postgres"
)

// backlogTimeout is how long reading the backlog may take: it's read each time metrics are sent,
// and a slow read would hold up sending every other metric.
const backlogTimeout = time.Second

// RecordBacklog measures the outbox to meters, read from the database each time metrics are sent:
// outbox.jobs, the jobs waiting to be moved to the queue, and outbox.oldest_wait, how long the
// oldest has waited (0 when none waits). Jobs normally wait about a second; a wait that grows means
// no relay can move them (the queue is down, or no worker runs). Every worker instance reports the
// same numbers: take the highest, not the sum.
//
// A read that fails is left out, and logged to logger when reads start failing and when they work
// again, not on every send: an error returned here would stop every metric of the program being
// sent. Call the returned func before closing pool, as metrics are sent one last time on the way
// out.
func RecordBacklog(pool *postgres.Pool, meters metric.MeterProvider, logger zerolog.Logger) (func(), error) {
	return recordBacklog(pool, meters, logger, backlogTimeout)
}

// recordBacklog is RecordBacklog, each read limited to timeout (tests on a busy machine need more).
func recordBacklog(pool *postgres.Pool, meters metric.MeterProvider, logger zerolog.Logger, timeout time.Duration) (func(), error) {
	meter := meters.Meter("github.com/mdayat/go-template/internal/outbox")
	jobs, err := meter.Int64ObservableGauge("outbox.jobs",
		metric.WithDescription("Jobs in the outbox, waiting to be moved to the job queue."),
		metric.WithUnit("{job}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create outbox.jobs: %w", err)
	}
	oldestWait, err := meter.Float64ObservableGauge("outbox.oldest_wait",
		metric.WithDescription("How long the oldest job in the outbox has waited."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create outbox.oldest_wait: %w", err)
	}
	queries := db.New(pool)
	var failing atomic.Bool
	registration, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		backlog, err := queries.OutboxBacklog(ctx)
		switch {
		case err != nil && !failing.Swap(true):
			logger.Warn().Err(err).Msg("outbox not measured: can't read it")
		case err == nil && failing.Swap(false):
			logger.Info().Msg("outbox measured again")
		}
		if err != nil {
			return nil
		}
		o.ObserveInt64(jobs, backlog.Jobs)
		o.ObserveFloat64(oldestWait, backlog.OldestWaitSeconds)
		return nil
	}, jobs, oldestWait)
	if err != nil {
		return nil, fmt.Errorf("measure the outbox: %w", err)
	}
	return func() { _ = registration.Unregister() }, nil
}
