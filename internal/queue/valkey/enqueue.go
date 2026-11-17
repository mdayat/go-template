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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/mdayat/go-template/internal/queue"
)

// traceHeaders carries a trace from where a job is queued to where it runs, in the task's headers
// (W3C traceparent), never its payload: a worker that doesn't read them runs the job all the same.
var traceHeaders = propagation.TraceContext{} //nolint:gochecknoglobals // stateless, like the type itself

// Enqueuer puts jobs on asynq queues.
type Enqueuer struct {
	client *asynq.Client
	tracer trace.Tracer
}

// Fails the build here, not only where cmd/ passes an Enqueuer in, if it stops implementing the
// interface.
var _ queue.Enqueuer = (*Enqueuer)(nil)

// NewEnqueuer returns an Enqueuer that queues jobs on client. It doesn't close client. Queueing a
// job inside a traced request is a span in tracers, and the job's run joins that trace.
func NewEnqueuer(client redis.UniversalClient, tracers trace.TracerProvider) *Enqueuer {
	return &Enqueuer{
		client: asynq.NewClientFromRedisClient(client),
		tracer: tracers.Tracer("github.com/mdayat/go-template/internal/queue/valkey"),
	}
}

// Enqueue queues job and returns asynq's ID for it.
func (e *Enqueuer) Enqueue(ctx context.Context, job queue.Job) (string, error) {
	ctx, span := e.tracer.Start(ctx, "send "+job.Type, trace.WithSpanKind(trace.SpanKindProducer),
		// As OpenTelemetry's messaging conventions name them.
		trace.WithAttributes(
			semconv.MessagingSystemKey.String("asynq"),
			semconv.MessagingOperationTypeKey.String("send"),
			semconv.MessagingDestinationName(job.Queue),
			attribute.String("job.type", job.Type),
		))
	defer span.End()

	task, err := newTask(ctx, job)
	if err != nil {
		span.SetStatus(codes.Error, "job refused")
		return "", err
	}
	info, err := e.client.EnqueueContext(ctx, task)
	if err != nil {
		span.SetStatus(codes.Error, "not queued")
		return "", fmt.Errorf("queue %s: %w", job.Type, err)
	}
	span.SetAttributes(semconv.MessagingMessageID(info.ID))
	return info.ID, nil
}

// newTask turns job into an asynq task, refusing what asynq would silently change. It checks what
// asynq itself checks only when queueing (a type, a queue name) too: the scheduler doesn't, and
// a scheduled job without them would fail only when it fires. The span in ctx goes in the task's
// headers if it's being recorded: one that isn't is dropped (the run decides for itself instead of
// being dropped with it), or isn't ours (a caller's trace, passed on while traces are off).
func newTask(ctx context.Context, job queue.Job) (*asynq.Task, error) {
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
	headers := propagation.MapCarrier{}
	if trace.SpanFromContext(ctx).IsRecording() {
		traceHeaders.Inject(ctx, headers)
	}
	return asynq.NewTaskWithHeaders(job.Type, job.Payload, headers,
		asynq.Queue(job.Queue),
		asynq.Timeout(job.Timeout),
		asynq.MaxRetry(job.MaxRetries),
	), nil
}
