package worker

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/testqueue"
)

// lineWriter hands each log line to the test as it's written; zerolog writes a line per call.
type lineWriter chan []byte

func (w lineWriter) Write(p []byte) (int, error) {
	w <- append([]byte(nil), p...)
	return len(p), nil
}

// ranJob is a test job's first run: its summary line, and the ID and queue to find the job by.
type ranJob struct {
	line  map[string]any
	id    string
	queue string
}

// startedJob is a test job queued on a running server of its own.
type startedJob struct {
	server *asynq.Server
	lines  lineWriter
	id     string
	queue  string
}

// startJob runs handler as a real asynq server would, through logJobs, on a queue of the test's
// own, and queues one job with opts.
func startJob(t *testing.T, handler asynq.HandlerFunc, opts ...asynq.Option) startedJob {
	t.Helper()
	client := testqueue.New(t)
	queue := testqueue.NewQueue(t, client)
	lines := make(lineWriter, 10)

	mux := asynq.NewServeMux()
	mux.Use(logJobs(zerolog.New(lines)))
	mux.Handle("test:job", handler)
	server := newServer(client, zerolog.Nop(), queue)
	if err := server.Start(mux); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(server.Shutdown)

	// Retention keeps a succeeded job around, so the test can see it completed.
	opts = append([]asynq.Option{asynq.Queue(queue), asynq.Retention(time.Hour)}, opts...)
	info, err := asynq.NewClientFromRedisClient(client).EnqueueContext(t.Context(), asynq.NewTask("test:job", nil), opts...)
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	return startedJob{server: server, lines: lines, id: info.ID, queue: queue}
}

// firstRun waits for the job's first summary line.
func firstRun(t *testing.T, job startedJob) ranJob {
	t.Helper()
	select {
	case raw := <-job.lines:
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		return ranJob{line: line, id: job.id, queue: job.queue}
	case <-time.After(15 * time.Second):
		t.Fatal("no log line 15 s after queueing the job")
		return ranJob{}
	}
}

// jobState waits for asynq to file the job after its run, and returns where it went.
func jobState(t *testing.T, job ranJob) asynq.TaskState {
	t.Helper()
	inspector := asynq.NewInspectorFromRedisClient(testqueue.New(t))
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := inspector.GetTaskInfo(job.queue, job.id)
		if err != nil {
			t.Fatalf("get job %s: %v", job.id, err)
		}
		if info.State != asynq.TaskStateActive || time.Now().After(deadline) {
			return info.State
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Stand-ins for values that change on every run, so a missing or wrong-typed one still fails.
const (
	anyNumber = "<a number>"
	anyText   = "<text>"
)

// withStandIns returns line with a numeric duration_ms and a non-empty stack_trace replaced by the
// stand-ins.
func withStandIns(line map[string]any) map[string]any {
	if _, ok := line["duration_ms"].(float64); ok {
		line["duration_ms"] = anyNumber
	}
	if e, ok := line["error"].(map[string]any); ok {
		if s, ok := e["stack_trace"].(string); ok && s != "" {
			e["stack_trace"] = anyText
		}
	}
	return line
}

func TestLogJobs(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, *asynq.Task) error { return errors.New("smtp: connection refused") }

	tests := []struct {
		name      string
		handler   asynq.HandlerFunc
		opts      []asynq.Option
		wantLine  map[string]any
		wantState asynq.TaskState
	}{
		{
			name:    "success is info",
			handler: func(context.Context, *asynq.Task) error { return nil },
			opts:    []asynq.Option{asynq.MaxRetry(3)},
			wantLine: map[string]any{
				"level": "info", "job_type": "test:job", "attempt": 1.0, "max_attempts": 4.0,
				"duration_ms": anyNumber, "message": "job completed",
			},
			wantState: asynq.TaskStateCompleted,
		},
		{
			name:    "failure with retries left is warn",
			handler: failing,
			opts:    []asynq.Option{asynq.MaxRetry(3)},
			wantLine: map[string]any{
				"level": "warn", "job_type": "test:job", "attempt": 1.0, "max_attempts": 4.0,
				"duration_ms": anyNumber, "message": "job completed",
				"error": map[string]any{"message": "smtp: connection refused"}, "will_retry": true,
			},
			wantState: asynq.TaskStateRetry,
		},
		{
			name:    "failure on the last attempt is error",
			handler: failing,
			opts:    []asynq.Option{asynq.MaxRetry(0)},
			wantLine: map[string]any{
				"level": "error", "job_type": "test:job", "attempt": 1.0, "max_attempts": 1.0,
				"duration_ms": anyNumber, "message": "job completed",
				"error": map[string]any{"message": "smtp: connection refused"}, "will_retry": false,
			},
			wantState: asynq.TaskStateArchived,
		},
		{
			name: "expected failure is not retried",
			handler: func(context.Context, *asynq.Task) error {
				return apperr.NotFound("task not found")
			},
			opts: []asynq.Option{asynq.MaxRetry(3)},
			wantLine: map[string]any{
				"level": "error", "job_type": "test:job", "attempt": 1.0, "max_attempts": 4.0,
				"duration_ms": anyNumber, "message": "job completed",
				"error": map[string]any{"message": "task not found"}, "will_retry": false,
			},
			wantState: asynq.TaskStateArchived,
		},
		{
			name:    "panic is error, with its stack, and retried",
			handler: func(context.Context, *asynq.Task) error { panic("boom") },
			opts:    []asynq.Option{asynq.MaxRetry(3)},
			wantLine: map[string]any{
				"level": "error", "job_type": "test:job", "attempt": 1.0, "max_attempts": 4.0,
				"duration_ms": anyNumber, "message": "job completed",
				"error": map[string]any{"message": "panic: boom", "stack_trace": anyText}, "will_retry": true,
			},
			wantState: asynq.TaskStateRetry,
		},
		{
			// asynq goes by the context's error once the time limit passes, so even an apperr
			// error is retried.
			name: "past its time limit with retries left is warn, whatever the handler returns",
			handler: func(ctx context.Context, _ *asynq.Task) error {
				<-ctx.Done()
				return apperr.NotFound("task not found")
			},
			opts: []asynq.Option{asynq.MaxRetry(3), asynq.Timeout(time.Second)},
			wantLine: map[string]any{
				"level": "warn", "job_type": "test:job", "attempt": 1.0, "max_attempts": 4.0,
				"duration_ms": anyNumber, "message": "job completed",
				"error": map[string]any{"message": "task not found"}, "will_retry": true,
			},
			wantState: asynq.TaskStateRetry,
		},
		{
			name: "past its time limit on the last attempt is error, even if the handler returns nil",
			handler: func(ctx context.Context, _ *asynq.Task) error {
				<-ctx.Done()
				return nil
			},
			opts: []asynq.Option{asynq.MaxRetry(0), asynq.Timeout(time.Second)},
			wantLine: map[string]any{
				"level": "error", "job_type": "test:job", "attempt": 1.0, "max_attempts": 1.0,
				"duration_ms": anyNumber, "message": "job completed",
				"error": map[string]any{"message": context.DeadlineExceeded.Error()}, "will_retry": false,
			},
			wantState: asynq.TaskStateArchived,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			job := firstRun(t, startJob(t, tt.handler, tt.opts...))

			want := maps.Clone(tt.wantLine)
			want["job_id"] = job.id
			want["queue"] = job.queue
			if diff := cmp.Diff(want, withStandIns(job.line)); diff != "" {
				t.Errorf("log line mismatch (-want +got):\n%s", diff)
			}
			if got := jobState(t, job); got != tt.wantState {
				t.Errorf("job state after the run = %v, want %v", got, tt.wantState)
			}
		})
	}
}

// A job still running when the worker's shutdown time runs out goes back in the queue without using
// an attempt, so its line says it will run again, even on its last attempt.
func TestLogJobsJobCutOffByShutdown(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	job := startJob(t, func(ctx context.Context, _ *asynq.Task) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, asynq.MaxRetry(0))

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("job didn't start 15 s after it was queued")
	}
	// Waits out shutdownTimeout, then puts the job back and cancels its context.
	job.server.Shutdown()
	run := firstRun(t, job)

	want := map[string]any{
		"level": "warn", "job_type": "test:job", "job_id": run.id, "queue": run.queue,
		"attempt": 1.0, "max_attempts": 1.0, "duration_ms": anyNumber, "message": "job completed",
		"error": map[string]any{"message": context.Canceled.Error()}, "will_retry": true,
	}
	if diff := cmp.Diff(want, withStandIns(run.line)); diff != "" {
		t.Errorf("log line mismatch (-want +got):\n%s", diff)
	}
	if got := jobState(t, run); got != asynq.TaskStatePending {
		t.Errorf("job state after shutdown = %v, want %v", got, asynq.TaskStatePending)
	}
}
