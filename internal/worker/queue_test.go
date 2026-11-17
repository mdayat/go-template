package worker

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/queue/valkey"
	"github.com/mdayat/go-template/internal/spantest"
	"github.com/mdayat/go-template/internal/testqueue"
	"github.com/mdayat/go-template/internal/testredis"
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
	off := tracenoop.NewTracerProvider()
	return runTracedOnQueue(t.Context(), t, off, off, handler, maxRetries, timeout)
}

// runTracedOnQueue is runOnQueue with the job queued in ctx's trace, traced to queueing, and run
// traced to running.
func runTracedOnQueue(
	ctx context.Context, t *testing.T, queueing, running trace.TracerProvider,
	handler queue.Handler, maxRetries int, timeout time.Duration,
) queuedRun {
	t.Helper()
	client := testredis.New(t)
	queueName := testqueue.NewQueue(t, client)
	lines := make(lineWriter, 10)

	mux := queue.NewMux()
	mux.Use(logJobs(zerolog.New(lines), noop.Float64Histogram{}, running.Tracer("")))
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
	id, err := valkey.NewEnqueuer(client, queueing).Enqueue(ctx, job)
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

// A job queued inside a request's trace runs in that trace: the request queues it ("send"), the
// worker runs it ("process") under that, and the run's log line names the trace. A failed run's
// span is marked failed.
func TestTraceFollowsTheJob(t *testing.T) {
	t.Parallel()
	spans := spantest.New()
	ctx, request := spans.Provider.Tracer("test").Start(t.Context(), "POST /task-purges",
		trace.WithSpanKind(trace.SpanKindServer))
	failing := func(context.Context, []byte) error { return errors.New("webhook down") }

	run := runTracedOnQueue(ctx, t, spans.Provider, spans.Provider, failing, 0, 10*time.Second)
	request.End()
	line := firstLine(t, run)

	process := waitForSpan(t, spans, "process test:job")
	send := spans.Named("send test:job")
	if len(send) != 1 || send[0].SpanKind() != trace.SpanKindProducer || send[0].Parent().SpanID() != request.SpanContext().SpanID() {
		t.Fatalf("spans = %q, want a producer send test:job under the request", spans.Names())
	}
	if process.SpanKind() != trace.SpanKindConsumer || process.Parent().SpanID() != send[0].SpanContext().SpanID() ||
		process.SpanContext().TraceID() != request.SpanContext().TraceID() {
		t.Errorf("process span: kind %v, parent %v; want a consumer under send, in the request's trace",
			process.SpanKind(), process.Parent())
	}
	if process.Status().Code != codes.Error || spantest.Attributes(process)["job.result"] != "failed" {
		t.Errorf("process span status %v, attributes %v; want failed", process.Status(), spantest.Attributes(process))
	}
	if trace, _ := line["trace"].(map[string]any); trace["id"] != request.SpanContext().TraceID().String() {
		t.Errorf("log line trace = %v, want the request's trace %s", line["trace"], request.SpanContext().TraceID())
	}
}

// A job queued outside any trace (a script, or a request whose trace wasn't recorded, which
// valkey's TestEnqueueCarriesOnlyKeptTraces shows sends none) starts its own when it runs.
func TestTraceStartsWithTheJob(t *testing.T) {
	t.Parallel()
	spans := spantest.New()
	succeed := func(context.Context, []byte) error { return nil }

	run := runTracedOnQueue(t.Context(), t, tracenoop.NewTracerProvider(), spans.Provider, succeed, 0, 10*time.Second)
	firstLine(t, run)

	if process := waitForSpan(t, spans, "process test:job"); process.Parent().IsValid() {
		t.Errorf("parent = %v, want none: nothing traced queued the job", process.Parent())
	}
}

// waitForSpan returns the span called name once it has ended: a run's span ends just after its
// log line.
//
//nolint:ireturn // the SDK hands out ended spans as an interface
func waitForSpan(t *testing.T, spans *spantest.Recorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if found := spans.Named(name); len(found) > 0 {
			return found[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no span %q; spans = %q", name, spans.Names())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With the worker's traces off, a job queued in a trace logs no trace: the IDs would be the
// queueing request's, not the run's.
func TestTraceOffLogsNoTrace(t *testing.T) {
	t.Parallel()
	spans := spantest.New()
	ctx, request := spans.Provider.Tracer("test").Start(t.Context(), "POST /task-purges",
		trace.WithSpanKind(trace.SpanKindServer))
	defer request.End()
	succeed := func(context.Context, []byte) error { return nil }

	run := runTracedOnQueue(ctx, t, spans.Provider, tracenoop.NewTracerProvider(), succeed, 0, 10*time.Second)

	if line := firstLine(t, run); line["trace"] != nil || line["span"] != nil {
		t.Errorf("log line has trace %v, span %v; want neither", line["trace"], line["span"])
	}
}
