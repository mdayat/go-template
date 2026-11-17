// Package worker runs background jobs: the handler for each job type, the middleware every job
// shares, and the schedule of jobs that run on a timetable. It sees the queue only through
// internal/queue; cmd/worker picks the implementation that runs them.
package worker

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/task"
)

// ShutdownTimeout is how long running jobs get to finish after SIGTERM; the queue then puts the
// ones still running back. Below Docker's default 10 s between SIGTERM and a hard kill, the
// shortest common wait (Kubernetes and ECS: 30 s).
const ShutdownTimeout = 8 * time.Second

// Services holds what the job handlers call, one field per feature. A new feature adds its field
// here and builds it in NewServices.
type Services struct {
	Tasks *task.Service
	// PurgeReports is told when deleted rows are purged (webhook.Discard when nobody listens).
	PurgeReports EventSender
}

// EventSender tells another system about an event: body is the event as JSON, and idempotencyKey
// is the same on every try of one job, so the receiver can drop repeats. cmd/worker passes in
// webhook.HTTPSender, or webhook.Discard when nobody listens.
type EventSender interface {
	Send(ctx context.Context, idempotencyKey string, body []byte) error
}

// NewServices builds every feature's service on queries, and reports purges to purgeReports. The
// worker (cmd/worker) and the job tests both get their services here, so a feature can't be wired
// into one and forgotten in the other.
func NewServices(queries *db.Queries, purgeReports EventSender) Services {
	return Services{
		Tasks:        task.NewService(queries),
		PurgeReports: purgeReports,
	}
}

// NewMux returns the handler for every job type, each job logged to logger, measured to meters and
// traced to tracers.
func NewMux(logger zerolog.Logger, meters metric.MeterProvider, tracers trace.TracerProvider, services Services) (*queue.Mux, error) {
	durations, err := newJobDurations(meters)
	if err != nil {
		return nil, err
	}
	mux := queue.NewMux()
	mux.Use(logJobs(logger, durations, tracers.Tracer("github.com/mdayat/go-template/internal/worker")))
	purgeJobs{tables: tablePurges(services), reports: services.PurgeReports}.mount(mux)
	return mux, nil
}

// Schedules returns every job that runs on a timetable. A feature with jobs of its own adds its
// schedule function here.
func Schedules() ([]queue.Schedule, error) {
	var all []queue.Schedule
	for _, schedules := range []func() ([]queue.Schedule, error){purgeSchedules} {
		s, err := schedules()
		if err != nil {
			return nil, err
		}
		all = append(all, s...)
	}
	return all, nil
}
