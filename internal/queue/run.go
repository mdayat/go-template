package queue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrNoRetry marks a failure a retry can't fix: wrap it into a run's error and the job fails for
// good instead of being tried again.
var ErrNoRetry = errors.New("not retried")

// Handler runs one job, given its payload. Its context ends at the job's Timeout, or when the
// worker shuts down.
type Handler func(ctx context.Context, payload []byte) error

// Middleware wraps every handler of a Mux, e.g. to log each run.
type Middleware func(next Handler) Handler

// Mux holds the handler for each job type and the middleware they share. An implementation's
// worker passes each job it takes to Process.
type Mux struct {
	handlers   map[string]Handler
	middleware []Middleware
}

// NewMux returns a Mux with no handlers.
func NewMux() *Mux {
	return &Mux{handlers: map[string]Handler{}}
}

// Use adds middleware; the first added runs first. Add it before Process is called.
func (m *Mux) Use(middleware ...Middleware) {
	m.middleware = append(m.middleware, middleware...)
}

// Handle sets the handler for jobs of jobType. Set them all before Process is called. It panics on
// an empty type, a nil handler or a type that already has one: wiring bugs that would otherwise
// show only when a job runs, or never (the second handler silently replacing the first).
func (m *Mux) Handle(jobType string, handler Handler) {
	switch _, taken := m.handlers[jobType]; {
	case jobType == "":
		panic("queue: Handle with an empty job type")
	case handler == nil:
		panic(fmt.Sprintf("queue: Handle(%q) with a nil handler", jobType))
	case taken:
		panic(fmt.Sprintf("queue: a second handler for job type %q", jobType))
	}
	m.handlers[jobType] = handler
}

// Process runs the handler for jobType through the middleware. An implementation puts the Run in
// ctx first (WithRun); its Type is jobType even if it doesn't. A type without a handler fails and
// is retried, through the middleware too: during a deploy a new API may queue a job only the new
// worker knows.
func (m *Mux) Process(ctx context.Context, jobType string, payload []byte) error {
	if run, _ := RunFrom(ctx); run.Type == "" {
		run.Type = jobType
		ctx = WithRun(ctx, run)
	}
	handler, ok := m.handlers[jobType]
	if !ok {
		handler = func(context.Context, []byte) error {
			return fmt.Errorf("no handler for job type %q", jobType)
		}
	}
	for _, mw := range slices.Backward(m.middleware) {
		handler = mw(handler)
	}
	return handler(ctx, payload)
}

// Run describes the job run a handler is in. An implementation puts it in the handler's context
// (WithRun); RunFrom reads it.
type Run struct {
	ID          string
	Type        string
	Queue       string
	Attempt     int // 1 on the first run
	MaxAttempts int // 1 + the job's MaxRetries

	// WillRetry reports whether the run, ending with err, will be tried again. Each queue decides
	// in its own way (asynq retries a run past its time limit by its count alone, whatever err is),
	// so the implementation sets it; nil means the plain rule (see Run.Retries).
	WillRetry func(err error) bool
}

// Retries reports whether the run, ending with err, will be tried again: by WillRetry if set,
// otherwise when err is not nil, isn't ErrNoRetry, and attempts are left.
func (r Run) Retries(err error) bool {
	if r.WillRetry != nil {
		return r.WillRetry(err)
	}
	return err != nil && !errors.Is(err, ErrNoRetry) && r.Attempt < r.MaxAttempts
}

type runKey struct{}

// WithRun returns ctx carrying run.
func WithRun(ctx context.Context, run Run) context.Context {
	return context.WithValue(ctx, runKey{}, run)
}

// RunFrom returns the run in ctx, and false if there is none (a handler called by a test).
func RunFrom(ctx context.Context) (Run, bool) {
	run, ok := ctx.Value(runKey{}).(Run)
	return run, ok
}

// Schedule queues Job on a timetable.
type Schedule struct {
	// Spec is a cron spec in UTC ("0 3 * * *") or a shorthand ("@daily", "@every 1h").
	Spec string
	Job  Job
	// Unique is how long, from the moment one copy is queued, other copies are dropped, in whole
	// seconds. Every worker runs a scheduler, so without it each worker queues its own copy. Keep
	// it shorter than the time between runs, or it drops the next run too.
	Unique time.Duration
}
