package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/queue"
)

// logJobs writes one summary line per job run: info when it succeeded, warn when it failed and
// will be retried, error when it failed for good or panicked. A handler's error or panic is on that
// line, so each run is logged exactly once. Payloads are never logged: they can hold personal data.
//
// It also marks an apperr error queue.ErrNoRetry: an expected failure (a bad payload, a row that's
// gone) fails the same way on every retry.
func logJobs(logger zerolog.Logger) queue.Middleware {
	return func(next queue.Handler) queue.Handler {
		return func(ctx context.Context, payload []byte) error {
			start := time.Now()
			run, _ := queue.RunFrom(ctx)
			jobLogger := logger.With().Str("job_type", run.Type).Str("job_id", run.ID).Logger()

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

			level := zerolog.InfoLevel
			switch {
			case stack != "" || (err != nil && !willRetry):
				level = zerolog.ErrorLevel
			case err != nil:
				level = zerolog.WarnLevel
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
