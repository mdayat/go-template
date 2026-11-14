package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/queue"
)

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

// logged runs handler through logJobs in run, and returns the one line it logged and the error it
// handed back to the queue.
func logged(ctx context.Context, t *testing.T, run queue.Run, handler queue.Handler) (map[string]any, error) {
	t.Helper()
	var logs bytes.Buffer
	mux := queue.NewMux()
	mux.Use(logJobs(zerolog.New(&logs)))
	mux.Handle(run.Type, handler)

	err := mux.Process(queue.WithRun(ctx, run), run.Type, nil)

	var line map[string]any
	if jsonErr := json.Unmarshal(logs.Bytes(), &line); jsonErr != nil {
		t.Fatalf("log is not one JSON line: %q: %v", logs.String(), jsonErr)
	}
	return line, err
}

// The log line comes from the run the queue describes; whether the run will be retried is the
// queue's call (its own tests check that it matches what the queue then does).
func TestLogJobs(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, []byte) error { return errors.New("smtp: connection refused") }
	notFound := func(context.Context, []byte) error { return apperr.NotFound("task not found") }
	// The first of four attempts, with the plain retry rule.
	firstOfFour := queue.Run{ID: "job-1", Type: "test:job", Queue: "default", Attempt: 1, MaxAttempts: 4}
	onlyAttempt := firstOfFour
	onlyAttempt.MaxAttempts = 1
	// A queue's own rule may retry a run marked not to be (asynq does past the time limit).
	queueRetries := firstOfFour
	queueRetries.WillRetry = func(error) bool { return true }
	base := map[string]any{
		"job_type": "test:job", "job_id": "job-1", "queue": "default", "duration_ms": anyNumber,
		"message": "job completed",
	}

	tests := []struct {
		name        string
		run         queue.Run
		handler     queue.Handler
		wantLine    map[string]any // on top of base
		wantNoRetry bool           // the error handed back wraps queue.ErrNoRetry
	}{
		{
			name: "success is info", run: firstOfFour,
			handler:  func(context.Context, []byte) error { return nil },
			wantLine: map[string]any{"level": "info", "attempt": 1.0, "max_attempts": 4.0},
		},
		{
			name: "failure with retries left is warn", run: firstOfFour, handler: failing,
			wantLine: map[string]any{
				"level": "warn", "attempt": 1.0, "max_attempts": 4.0,
				"error": map[string]any{"message": "smtp: connection refused"}, "will_retry": true,
			},
		},
		{
			name: "failure on the last attempt is error", run: onlyAttempt, handler: failing,
			wantLine: map[string]any{
				"level": "error", "attempt": 1.0, "max_attempts": 1.0,
				"error": map[string]any{"message": "smtp: connection refused"}, "will_retry": false,
			},
		},
		{
			name: "expected failure is error and not retried", run: firstOfFour, handler: notFound,
			wantLine: map[string]any{
				"level": "error", "attempt": 1.0, "max_attempts": 4.0,
				"error": map[string]any{"message": "task not found"}, "will_retry": false,
			},
			wantNoRetry: true,
		},
		{
			name: "panic is error, with its stack, and retried", run: firstOfFour,
			handler: func(context.Context, []byte) error { panic("boom") },
			wantLine: map[string]any{
				"level": "error", "attempt": 1.0, "max_attempts": 4.0,
				"error": map[string]any{"message": "panic: boom", "stack_trace": anyText}, "will_retry": true,
			},
		},
		{
			name: "the queue's own retry rule wins", run: queueRetries, handler: notFound,
			wantLine: map[string]any{
				"level": "warn", "attempt": 1.0, "max_attempts": 4.0,
				"error": map[string]any{"message": "task not found"}, "will_retry": true,
			},
			wantNoRetry: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			line, err := logged(t.Context(), t, tt.run, tt.handler)

			want := maps.Clone(base)
			maps.Copy(want, tt.wantLine)
			if diff := cmp.Diff(want, withStandIns(line)); diff != "" {
				t.Errorf("log line mismatch (-want +got):\n%s", diff)
			}
			if got := errors.Is(err, queue.ErrNoRetry); got != tt.wantNoRetry {
				t.Errorf("error handed back = %v, want it to wrap ErrNoRetry = %v", err, tt.wantNoRetry)
			}
		})
	}
}

// A handler that returns nil after its context ended (its time limit, or shutdown) didn't finish:
// the line says so, with the context's error.
func TestLogJobsRunCutOff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	run := queue.Run{ID: "job-1", Type: "test:job", Queue: "default", Attempt: 1, MaxAttempts: 4}

	line, err := logged(ctx, t, run, func(context.Context, []byte) error { return nil })

	want := map[string]any{
		"level": "warn", "job_type": "test:job", "job_id": "job-1", "queue": "default",
		"attempt": 1.0, "max_attempts": 4.0, "duration_ms": anyNumber, "message": "job completed",
		"error": map[string]any{"message": context.Canceled.Error()}, "will_retry": true,
	}
	if diff := cmp.Diff(want, withStandIns(line)); diff != "" {
		t.Errorf("log line mismatch (-want +got):\n%s", diff)
	}
	if err != nil {
		t.Errorf("error handed back = %v, want nil: the queue sees the context itself", err)
	}
}
