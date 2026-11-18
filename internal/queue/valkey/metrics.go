package valkey

import (
	"context"
	"fmt"
	"slices"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RecordQueues measures each of queues to meters, read from Valkey each time metrics are sent:
// job.queue.jobs, its jobs by job.state (waiting: queued, not yet taken; running; retrying: failed,
// waiting for their next try), and job.queue.oldest_wait, how long its oldest waiting job has waited
// (0 when none waits). A worker that takes no jobs, or too few, shows as a wait that only grows.
// Every worker instance reports the same numbers: take the highest, not the sum.
//
// A queue that can't be read is logged to logger and left out: an error returned here would stop
// every metric of the program being sent. Give client short
// time limits (startup.RedisFailFast): asynq's reads can't be cancelled, and a slow Valkey would
// hold up each send. Call the returned func before closing client, as metrics are sent one last
// time on the way out.
func RecordQueues(client redis.UniversalClient, queues []string, meters metric.MeterProvider, logger zerolog.Logger) (func(), error) {
	meter := meters.Meter("github.com/mdayat/go-template/internal/queue/valkey")
	jobs, err := meter.Int64ObservableGauge("job.queue.jobs",
		metric.WithDescription("Jobs in each queue, waiting, running or waiting to retry."),
		metric.WithUnit("{job}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create job.queue.jobs: %w", err)
	}
	oldestWait, err := meter.Float64ObservableGauge("job.queue.oldest_wait",
		metric.WithDescription("How long the oldest job waiting in each queue has waited."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create job.queue.oldest_wait: %w", err)
	}
	// Nothing to close: it works through client, which the caller owns.
	inspector := asynq.NewInspectorFromRedisClient(client)
	registration, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		// asynq refuses to describe a queue nothing has been queued to yet; it has no jobs.
		existing, err := inspector.Queues()
		if err != nil {
			logger.Warn().Err(err).Msg("job queues not measured: can't list them")
			return nil
		}
		for _, name := range queues {
			info := &asynq.QueueInfo{}
			if slices.Contains(existing, name) {
				if info, err = inspector.GetQueueInfo(name); err != nil {
					logger.Warn().Err(err).Str("queue", name).Msg("job queue not measured")
					continue
				}
			}
			queue := attribute.String("job.queue", name)
			for state, n := range map[string]int{"waiting": info.Pending, "running": info.Active, "retrying": info.Retry} {
				o.ObserveInt64(jobs, int64(n), metric.WithAttributes(queue, attribute.String("job.state", state)))
			}
			o.ObserveFloat64(oldestWait, info.Latency.Seconds(), metric.WithAttributes(queue))
		}
		return nil
	}, jobs, oldestWait)
	if err != nil {
		return nil, fmt.Errorf("measure job queues: %w", err)
	}
	return func() { _ = registration.Unregister() }, nil
}
