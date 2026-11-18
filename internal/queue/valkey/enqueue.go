// Package valkey implements internal/queue on Valkey (or Redis) with asynq: queueing jobs, running
// them, and queueing them on a timetable. Only cmd/ uses it; everything else goes through
// internal/queue.
package valkey

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/hlog"
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
	switch {
	case errors.Is(err, asynq.ErrTaskIDConflict):
		span.SetStatus(codes.Error, "queued already")
		return "", fmt.Errorf("queue %s %s: %w", job.Type, job.ID, queue.ErrDuplicate)
	case err != nil:
		span.SetStatus(codes.Error, "not queued")
		return "", fmt.Errorf("queue %s: %w", job.Type, err)
	}
	span.SetAttributes(semconv.MessagingMessageID(info.ID))
	return info.ID, nil
}

// requestIDHeader carries the ID of the request that queued a job (queue.Run.RequestID).
const requestIDHeader = "x-request-id"

// newTask turns job into an asynq task, refusing what asynq would silently change. It checks what
// asynq itself checks only when queueing (a type, a queue name) too: the scheduler doesn't, and
// a scheduled job without them would fail only when it fires. The span in ctx goes in the task's
// headers if it's being recorded: one that isn't is dropped (the run decides for itself instead of
// being dropped with it), or isn't ours (a caller's trace, passed on while traces are off).
func newTask(ctx context.Context, job queue.Job) (*asynq.Task, error) {
	return newTaskWithHeaders(ctx, job, nil)
}

// newTaskWithHeaders is newTask with extra headers on the task (scheduledHeader).
func newTaskWithHeaders(ctx context.Context, job queue.Job, extra map[string]string) (*asynq.Task, error) {
	// asynq keeps timeouts in whole seconds, cutting off the rest, and replaces a zero one with its
	// 30-minute default: Check refuses both.
	if err := job.Check(); err != nil {
		return nil, err
	}
	headers := propagation.MapCarrier{}
	maps.Copy(headers, extra)
	// The job's own (a job the outbox queues later, outside the request), else the context's: set
	// by the API's request log (hlog.RequestIDHandler).
	if job.RequestID != "" {
		headers[requestIDHeader] = job.RequestID
	} else if id, ok := hlog.IDFromCtx(ctx); ok {
		headers[requestIDHeader] = id.String()
	}
	if trace.SpanFromContext(ctx).IsRecording() {
		traceHeaders.Inject(ctx, headers)
	}
	opts := []asynq.Option{asynq.Queue(job.Queue), asynq.Timeout(job.Timeout), asynq.MaxRetry(job.MaxRetries)}
	if job.ID != "" {
		opts = append(opts, asynq.TaskID(job.ID))
	}
	return asynq.NewTaskWithHeaders(job.Type, job.Payload, headers, opts...), nil
}
