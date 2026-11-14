package queue

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestMuxRunsMiddlewareInTheOrderAdded(t *testing.T) {
	t.Parallel()
	var calls []string
	record := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(ctx context.Context, payload []byte) error {
				calls = append(calls, name)
				return next(ctx, payload)
			}
		}
	}
	mux := NewMux()
	mux.Use(record("first"), record("second"))
	mux.Handle("test:job", func(_ context.Context, payload []byte) error {
		calls = append(calls, "handler "+string(payload))
		return nil
	})

	if err := mux.Process(t.Context(), "test:job", []byte("p")); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if diff := cmp.Diff([]string{"first", "second", "handler p"}, calls); diff != "" {
		t.Errorf("calls mismatch (-want +got):\n%s", diff)
	}
}

// A job type without a handler fails through the middleware, so it's logged, and is retried: the
// worker that knows it may be on its way in a deploy.
func TestMuxUnknownTypeFailsAndIsRetried(t *testing.T) {
	t.Parallel()
	var seen error
	mux := NewMux()
	mux.Use(func(next Handler) Handler {
		return func(ctx context.Context, payload []byte) error {
			seen = next(ctx, payload)
			return seen
		}
	})

	err := mux.Process(t.Context(), "test:unknown", nil)

	if err == nil || !errors.Is(seen, err) {
		t.Errorf("Process() error = %v, middleware saw %v; want the same error", err, seen)
	}
	if run := (Run{Attempt: 1, MaxAttempts: 2}); !run.Retries(err) {
		t.Errorf("Retries(%v) = false, want true", err)
	}
}

func TestRunRetries(t *testing.T) {
	t.Parallel()
	failed := errors.New("smtp: connection refused")

	tests := []struct {
		name string
		run  Run
		err  error
		want bool
	}{
		{"success", Run{Attempt: 1, MaxAttempts: 3}, nil, false},
		{"failure with attempts left", Run{Attempt: 2, MaxAttempts: 3}, failed, true},
		{"failure on the last attempt", Run{Attempt: 3, MaxAttempts: 3}, failed, false},
		{"marked not to retry", Run{Attempt: 1, MaxAttempts: 3}, fmt.Errorf("x: %w", ErrNoRetry), false},
		{"the queue's own rule", Run{Attempt: 3, MaxAttempts: 3, WillRetry: func(error) bool { return true }}, failed, true},
	}
	for _, tt := range tests {
		if got := tt.run.Retries(tt.err); got != tt.want {
			t.Errorf("%s: Retries(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

func TestRunFrom(t *testing.T) {
	t.Parallel()
	if _, ok := RunFrom(t.Context()); ok {
		t.Error("RunFrom(a context without a run) ok = true, want false")
	}
	want := Run{ID: "job-1", Type: "test:job", Queue: "default", Attempt: 2, MaxAttempts: 3}
	got, ok := RunFrom(WithRun(t.Context(), want))
	if !ok || got.ID != want.ID || got.Attempt != want.Attempt {
		t.Errorf("RunFrom(WithRun(%+v)) = %+v, %v", want, got, ok)
	}
}

// Wiring bugs fail when the worker starts, not when a job runs (or never, for a second handler
// silently replacing the first).
func TestMuxHandleRefusesWiringBugs(t *testing.T) {
	t.Parallel()
	ok := func(context.Context, []byte) error { return nil }

	tests := []struct {
		name    string
		jobType string
		handler Handler
	}{
		{"empty type", "", ok},
		{"nil handler", "test:other", nil},
		{"second handler for a type", "test:job", ok},
	}
	for _, tt := range tests {
		mux := NewMux()
		mux.Handle("test:job", ok)
		if !panics(func() { mux.Handle(tt.jobType, tt.handler) }) {
			t.Errorf("%s: Handle(%q) didn't panic", tt.name, tt.jobType)
		}
	}
}

// panics reports whether f panics.
func panics(f func()) bool {
	panicked := true
	func() {
		defer func() { _ = recover() }()
		f()
		panicked = false
	}()
	return panicked
}

// Middleware reads the job type from the Run even when the implementation set none.
func TestMuxProcessSetsTheRunsType(t *testing.T) {
	t.Parallel()
	var got string
	mux := NewMux()
	mux.Use(func(next Handler) Handler {
		return func(ctx context.Context, payload []byte) error {
			run, _ := RunFrom(ctx)
			got = run.Type
			return next(ctx, payload)
		}
	})
	mux.Handle("test:job", func(context.Context, []byte) error { return nil })

	if err := mux.Process(t.Context(), "test:job", nil); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if got != "test:job" {
		t.Errorf("Run.Type in middleware = %q, want test:job", got)
	}
}
