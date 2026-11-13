package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
)

// logJobs writes one summary line per job run: info when it succeeded, warn when it failed and
// will be retried, error when it failed for good or panicked. A handler's error or panic is on that
// line, so each run is logged exactly once. Payloads are never logged: they can hold personal data.
//
// It also turns an apperr error into asynq.SkipRetry: an expected failure (a bad payload, a row
// that's gone) fails the same way on every retry.
func logJobs(logger zerolog.Logger) asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
			start := time.Now()
			id, _ := asynq.GetTaskID(ctx)
			queue, _ := asynq.GetQueueName(ctx)
			retried, _ := asynq.GetRetryCount(ctx)
			maxRetry, _ := asynq.GetMaxRetry(ctx)
			jobLogger := logger.With().Str("job_type", t.Type()).Str("job_id", id).Logger()

			var run jobRun
			run.process(jobLogger.WithContext(ctx), next, t)
			err, stack := run.err, run.stack
			toAsynq := err
			if _, ok := errors.AsType[*apperr.Error](err); ok {
				toAsynq = fmt.Errorf("%w: %w", err, asynq.SkipRetry)
			}
			// The rule asynq uses to decide between retrying and archiving the job.
			final := retried >= maxRetry || errors.Is(toAsynq, asynq.SkipRetry)
			// Once the job's context has ended, asynq has already decided without waiting for the
			// handler: past the job's time limit it retries by count alone; cut off by shutdown it
			// puts the job back in the queue without counting the attempt. (A lost lease also
			// cancels the context and is counted; it's rare and logged here as will retry.)
			if ctxErr := ctx.Err(); ctxErr != nil {
				if err == nil {
					err = ctxErr
				}
				final = errors.Is(ctxErr, context.DeadlineExceeded) && retried >= maxRetry
			}

			level := zerolog.InfoLevel
			switch {
			case stack != "" || (err != nil && final):
				level = zerolog.ErrorLevel
			case err != nil:
				level = zerolog.WarnLevel
			}
			line := jobLogger.WithLevel(level).
				Str("queue", queue).
				Int("attempt", retried+1).
				Int("max_attempts", maxRetry+1).
				Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond))
			if err != nil {
				errFields := zerolog.Dict().Str("message", err.Error())
				if stack != "" {
					errFields.Str("stack_trace", stack)
				}
				line.Dict("error", errFields).Bool("will_retry", !final)
			}
			line.Msg("job completed")
			return toAsynq
		})
	}
}

// jobRun is how a job run ended: its error, and the stack if it panicked.
type jobRun struct {
	err   error
	stack string
}

// process runs next and turns a panic into an error with its stack. asynq would recover it too, but
// log it on a line of its own.
func (jr *jobRun) process(ctx context.Context, next asynq.Handler, t *asynq.Task) {
	defer func() {
		if p := recover(); p != nil {
			jr.err = fmt.Errorf("panic: %v", p)
			jr.stack = string(debug.Stack())
		}
	}()
	jr.err = next.ProcessTask(ctx, t)
}
