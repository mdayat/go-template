package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/queue"
)

// logJobs writes one summary line per job run: info when it succeeded, warn when it failed and
// will be retried, error when it failed for good or panicked. A handler's error or panic is on that
// line, so each run is logged exactly once. Payloads are never logged: they can hold personal data.
// Each run's time also goes to durations (see newJobDurations), with the same outcome, and each run
// is a span in tracer ("process deleted_rows:purge"), in the trace of the request that queued it if
// that trace is kept; the handler's queries, commands and calls hang under it. The line carries the
// span's trace.id and span.id, as does every line the handler logs.
//
// It also marks an apperr error queue.ErrNoRetry: an expected failure (a bad payload, a row that's
// gone) fails the same way on every retry.
func logJobs(logger zerolog.Logger, durations metric.Float64Histogram, tracer trace.Tracer) queue.Middleware {
	return func(next queue.Handler) queue.Handler {
		return func(ctx context.Context, payload []byte) error {
			start := time.Now()
			run, _ := queue.RunFrom(ctx)
			ctx, span := tracer.Start(ctx, "process "+run.Type, trace.WithSpanKind(trace.SpanKindConsumer),
				trace.WithAttributes(
					semconv.MessagingOperationTypeKey.String("process"),
					semconv.MessagingDestinationName(run.Queue),
					semconv.MessagingMessageID(run.ID),
					attribute.String("job.type", run.Type),
					attribute.Int("job.attempt", run.Attempt),
				))
			defer span.End()
			fields := logger.With().Str("job_type", run.Type).Str("job_id", run.ID)
			// Not when the span isn't being recorded: its IDs would be the queueing request's.
			if sc := span.SpanContext(); span.IsRecording() {
				fields = fields.Dict("trace", zerolog.Dict().Str("id", sc.TraceID().String())).
					Dict("span", zerolog.Dict().Str("id", sc.SpanID().String()))
			}
			jobLogger := fields.Logger()

			var jr jobRun
			jr.process(jobLogger.WithContext(ctx), next, payload)
			err, stack := jr.err, jr.stack
			toQueue := err
			if _, ok := errors.AsType[*apperr.Error](err); ok {
				toQueue = fmt.Errorf("%w: %w", err, queue.ErrNoRetry)
			}
			// A run whose context ended (its time limit, or shutdown) was cut off, whatever the
			// handler returned.
			outcome := toQueue
			if ctxErr := ctx.Err(); ctxErr != nil && err == nil {
				err, outcome = ctxErr, ctxErr
			}
			willRetry := run.Retries(outcome)

			level, result := zerolog.InfoLevel, "succeeded"
			switch {
			case stack != "" || (err != nil && !willRetry):
				level, result = zerolog.ErrorLevel, "failed"
				if willRetry {
					result = "retrying" // a panic, which is retried like any failure
				}
			case err != nil:
				level, result = zerolog.WarnLevel, "retrying"
			}
			durations.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
				attribute.String("job.type", run.Type),
				attribute.String("job.queue", run.Queue),
				attribute.String("job.result", result),
			))
			span.SetAttributes(attribute.String("job.result", result))
			if err != nil {
				// The message, as on the log line; never the stack.
				span.RecordError(err)
				span.SetStatus(codes.Error, result)
			}
			line := jobLogger.WithLevel(level).
				Str("queue", run.Queue).
				Int("attempt", run.Attempt).
				Int("max_attempts", run.MaxAttempts).
				Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond))
			if err != nil {
				errFields := zerolog.Dict().Str("message", err.Error())
				if stack != "" {
					errFields.Str("stack_trace", stack)
				}
				line.Dict("error", errFields).Bool("will_retry", willRetry)
			}
			line.Msg("job completed")
			return toQueue
		}
	}
}

// jobRun is how a job run ended: its error, and the stack if it panicked.
type jobRun struct {
	err   error
	stack string
}

// process runs next and turns a panic into an error with its stack, so the panic is on the run's
// one line and the job is retried like any failure.
func (jr *jobRun) process(ctx context.Context, next queue.Handler, payload []byte) {
	defer func() {
		if p := recover(); p != nil {
			jr.err = fmt.Errorf("panic: %v", p)
			jr.stack = string(debug.Stack())
		}
	}()
	jr.err = next(ctx, payload)
}

// newJobDurations returns the histogram of job run times, job.run.duration, in seconds, by job.type,
// job.queue and job.result (succeeded, retrying, failed). Its count is the number of runs, so it
// answers both "how many failed" and "how long did they take". Our own metric: OpenTelemetry has no
// automatic one for asynq.
//
//nolint:ireturn // OpenTelemetry's metric API hands out its instruments as interfaces
func newJobDurations(meters metric.MeterProvider) (metric.Float64Histogram, error) {
	h, err := meters.Meter("github.com/mdayat/go-template/internal/worker").Float64Histogram("job.run.duration",
		metric.WithDescription("How long each job run took, by job type and result."),
		metric.WithUnit("s"),
		// The SDK's default buckets are made for milliseconds; these span 5 ms to 10 min.
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600),
	)
	if err != nil {
		return nil, fmt.Errorf("create job.run.duration: %w", err)
	}
	return h, nil
}
