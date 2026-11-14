package valkey

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/testqueue"
)

// seenRun is what a handler saw of its first run, and whether the run said it would be retried.
type seenRun struct {
	run       queue.Run // without WillRetry
	willRetry bool
}

// startedJob is a test job queued on a running server of its own.
type startedJob struct {
	server *Server
	seen   chan seenRun
	id     string
	queue  string
}

// startJob runs handler on a real server reading a queue of the test's own, and queues one job
// with maxRetries and timeout.
func startJob(t *testing.T, handler queue.Handler, maxRetries int, timeout time.Duration) startedJob {
	t.Helper()
	client := testqueue.New(t)
	queueName := testqueue.NewQueue(t, client)
	seen := make(chan seenRun, 10)

	mux := queue.NewMux()
	// Records the run the way the worker's log line reads it: a run whose context ended was cut
	// off, whatever the handler returned.
	mux.Use(func(next queue.Handler) queue.Handler {
		return func(ctx context.Context, payload []byte) error {
			err := next(ctx, payload)
			outcome := err
			if outcome == nil && ctx.Err() != nil {
				outcome = ctx.Err()
			}
			run, _ := queue.RunFrom(ctx)
			retries := run.Retries(outcome)
			run.WillRetry = nil
			seen <- seenRun{run: run, willRetry: retries}
			return err
		}
	})
	mux.Handle("test:job", handler)
	server, err := NewServer(client, mux, ServerOptions{Queues: []string{queueName}, ShutdownTimeout: time.Second, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(server.Shutdown)

	job := queue.Job{Type: "test:job", Queue: queueName, Timeout: timeout, MaxRetries: maxRetries}
	id, err := NewEnqueuer(client).Enqueue(t.Context(), job)
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	return startedJob{server: server, seen: seen, id: id, queue: queueName}
}

// firstRun waits for the job's first run to end.
func firstRun(t *testing.T, job startedJob) seenRun {
	t.Helper()
	select {
	case run := <-job.seen:
		return run
	case <-time.After(15 * time.Second):
		t.Fatal("no run ended 15 s after queueing the job")
		return seenRun{}
	}
}

// filedState is where asynq put the job after its run.
type filedState string

const (
	stateDeleted  filedState = "deleted (completed)"
	stateRetry    filedState = "retry"
	statePending  filedState = "pending"
	stateArchived filedState = "archived"
)

// jobState waits for asynq to file the job after its run, and returns where it went. A job that
// succeeded is deleted (no retention is set).
func jobState(t *testing.T, job startedJob) filedState {
	t.Helper()
	inspector := asynq.NewInspectorFromRedisClient(testqueue.New(t))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		info, err := inspector.GetTaskInfo(job.queue, job.id)
		switch {
		case errors.Is(err, asynq.ErrTaskNotFound):
			return stateDeleted
		case err != nil:
			t.Fatalf("get job %s: %v", job.id, err)
		case info.State == asynq.TaskStateActive && time.Now().Before(deadline):
			continue
		}
		return filedState(info.State.String())
	}
}

// The run in the handler's context describes the job, and its WillRetry matches what asynq does
// next, including the cases where asynq ignores what the handler returned.
func TestServerRunTellsWhetherAsynqRetries(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, []byte) error { return errors.New("smtp: connection refused") }
	waitForLimit := func(returned error) queue.Handler {
		return func(ctx context.Context, _ []byte) error {
			<-ctx.Done()
			return returned
		}
	}
	noRetry := fmt.Errorf("task not found: %w", queue.ErrNoRetry)

	tests := []struct {
		name       string
		handler    queue.Handler
		maxRetries int
		timeout    time.Duration
		wantRetry  bool
		wantState  filedState
	}{
		{"success", func(context.Context, []byte) error { return nil }, 3, time.Minute, false, stateDeleted},
		{"failure with retries left", failing, 3, time.Minute, true, stateRetry},
		{"failure on the last attempt", failing, 0, time.Minute, false, stateArchived},
		{"failure marked not to retry", func(context.Context, []byte) error { return noRetry }, 3, time.Minute, false, stateArchived},
		// Past the time limit asynq retries by count alone, whatever the handler returns.
		{"past the time limit, marked not to retry, retries left", waitForLimit(noRetry), 3, time.Second, true, stateRetry},
		{"past the time limit on the last attempt, returning nil", waitForLimit(nil), 0, time.Second, false, stateArchived},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			job := startJob(t, tt.handler, tt.maxRetries, tt.timeout)

			got := firstRun(t, job)

			wantRun := queue.Run{ID: job.id, Type: "test:job", Queue: job.queue, Attempt: 1, MaxAttempts: tt.maxRetries + 1}
			if diff := cmp.Diff(wantRun, got.run); diff != "" {
				t.Errorf("run mismatch (-want +got):\n%s", diff)
			}
			state := jobState(t, job)
			if got.willRetry != tt.wantRetry || state != tt.wantState {
				t.Errorf("WillRetry = %v and asynq filed the job as %s; want %v and %s", got.willRetry, state, tt.wantRetry, tt.wantState)
			}
		})
	}
}

// A job still running when the shutdown time runs out goes back in the queue without using an
// attempt, so its run says it will run again, even on its last attempt.
func TestServerRunCutOffByShutdownIsRetried(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	job := startJob(t, func(ctx context.Context, _ []byte) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, 0, time.Minute)

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("job didn't start 15 s after it was queued")
	}
	// Waits out the shutdown time (a second), then puts the job back and cancels its context.
	start := time.Now()
	job.server.Shutdown()
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("Shutdown() took %s, want about the 1 s ShutdownTimeout (asynq's default is 8 s)", waited)
	}
	got := firstRun(t, job)

	if state := jobState(t, job); !got.willRetry || state != statePending {
		t.Errorf("WillRetry = %v and asynq filed the job as %s; want true and %s", got.willRetry, state, statePending)
	}
}

func TestNewServerRefusesOptionsAsynqWouldReplace(t *testing.T) {
	t.Parallel()
	client := testqueue.New(t)
	valid := ServerOptions{Queues: []string{"default"}, ShutdownTimeout: time.Second}

	tests := []struct {
		name   string
		change func(*ServerOptions)
	}{
		{"no queues", func(o *ServerOptions) { o.Queues = nil }},
		{"blank queue name", func(o *ServerOptions) { o.Queues = []string{" "} }},
		{"no shutdown time", func(o *ServerOptions) { o.ShutdownTimeout = 0 }},
	}
	for _, tt := range tests {
		opts := valid
		tt.change(&opts)
		if _, err := NewServer(client, queue.NewMux(), opts); err == nil {
			t.Errorf("%s: NewServer(%+v) error = nil, want one", tt.name, opts)
		}
	}
	if _, err := NewServer(client, queue.NewMux(), valid); err != nil {
		t.Errorf("NewServer(%+v) error = %v, want nil", valid, err)
	}
}
