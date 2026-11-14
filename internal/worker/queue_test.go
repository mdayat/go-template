package worker

import (
	"context"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/queue/valkey"
	"github.com/mdayat/go-template/internal/testqueue"
)

// lineWriter hands each log line to the test as it's written; zerolog writes a line per call.
type lineWriter chan []byte

func (w lineWriter) Write(p []byte) (int, error) {
	w <- append([]byte(nil), p...)
	return len(p), nil
}

// queuedRun is a test job queued on a running server of the queue cmd/worker runs on, its runs
// logged by logJobs.
type queuedRun struct {
	server *valkey.Server
	lines  lineWriter
	id     string
	queue  string
}

// runOnQueue runs handler through logJobs on a real server reading a queue of the test's own, and
// queues one job.
func runOnQueue(t *testing.T, handler queue.Handler, maxRetries int, timeout time.Duration) queuedRun {
	t.Helper()
	client := testqueue.New(t)
	queueName := testqueue.NewQueue(t, client)
	lines := make(lineWriter, 10)

	mux := queue.NewMux()
	mux.Use(logJobs(zerolog.New(lines)))
	mux.Handle("test:job", handler)
	server, err := valkey.NewServer(client, mux, valkey.ServerOptions{
		Queues: []string{queueName}, ShutdownTimeout: time.Second, Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(server.Shutdown)

	job := queue.Job{Type: "test:job", Queue: queueName, Timeout: timeout, MaxRetries: maxRetries}
	id, err := valkey.NewEnqueuer(client).Enqueue(t.Context(), job)
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	return queuedRun{server: server, lines: lines, id: id, queue: queueName}
}

// firstLine waits for the job's first summary line.
func firstLine(t *testing.T, run queuedRun) map[string]any {
	t.Helper()
	select {
	case raw := <-run.lines:
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		return line
	case <-time.After(15 * time.Second):
		t.Fatal("no log line 15 s after queueing the job")
		return nil
	}
}

// The cases where the queue, not the handler, decides what happens: the line must say what the
// queue does (its own tests check that it does).
func TestLogJobsOnTheQueue(t *testing.T) {
	t.Parallel()
	waitForLimit := func(returned error) queue.Handler {
		return func(ctx context.Context, _ []byte) error {
			<-ctx.Done()
			return returned
		}
	}

	tests := []struct {
		name       string
		handler    queue.Handler
		maxRetries int
		wantLine   map[string]any
	}{
		{
			name:    "past the time limit with retries left is warn, whatever the handler returns",
			handler: waitForLimit(apperr.NotFound("task not found")), maxRetries: 3,
			wantLine: map[string]any{
				"level": "warn", "attempt": 1.0, "max_attempts": 4.0,
				"error": map[string]any{"message": "task not found"}, "will_retry": true,
			},
		},
		{
			name:    "past the time limit on the last attempt is error, even if the handler returns nil",
			handler: waitForLimit(nil), maxRetries: 0,
			wantLine: map[string]any{
				"level": "error", "attempt": 1.0, "max_attempts": 1.0,
				"error": map[string]any{"message": context.DeadlineExceeded.Error()}, "will_retry": false,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := runOnQueue(t, tt.handler, tt.maxRetries, time.Second)

			got := firstLine(t, run)

			want := map[string]any{
				"job_type": "test:job", "job_id": run.id, "queue": run.queue, "duration_ms": anyNumber,
				"message": "job completed",
			}
			maps.Copy(want, tt.wantLine)
			if diff := cmp.Diff(want, withStandIns(got)); diff != "" {
				t.Errorf("log line mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A job still running when the worker's shutdown time runs out goes back in the queue without using
// an attempt, so its line says it will run again, even on its last attempt.
func TestLogJobsOnTheQueueCutOffByShutdown(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	run := runOnQueue(t, func(ctx context.Context, _ []byte) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, 0, time.Minute)

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("job didn't start 15 s after it was queued")
	}
	run.server.Shutdown()
	got := firstLine(t, run)

	want := map[string]any{
		"level": "warn", "job_type": "test:job", "job_id": run.id, "queue": run.queue,
		"attempt": 1.0, "max_attempts": 1.0, "duration_ms": anyNumber, "message": "job completed",
		"error": map[string]any{"message": context.Canceled.Error()}, "will_retry": true,
	}
	if diff := cmp.Diff(want, withStandIns(got)); diff != "" {
		t.Errorf("log line mismatch (-want +got):\n%s", diff)
	}
}
