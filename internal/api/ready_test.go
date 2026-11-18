package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// A load balancer has no token and checks often, from few IPs: /readyz needs no sign-in and is
// never limited. It answers 200 while the database does, and 503 once it doesn't.
func TestReadyzChecksTheDatabase(t *testing.T) {
	t.Parallel()
	router, pool, _ := testAPIWith(t, Limits{PerIP: 1, Limiter: valkeyLimiter(t)})
	client := request{remoteAddr: "192.0.2.1:1234"}

	for i := range 3 {
		if got := client.sendTo(t, router, readyPath).Code; got != http.StatusOK {
			t.Fatalf("readiness check %d = %d, want 200", i+1, got)
		}
	}

	pool.Close()
	rec := client.sendTo(t, router, readyPath)
	want := problemReply{Status: http.StatusServiceUnavailable, Detail: "the database didn't answer"}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("GET /readyz with the database gone (-want +got):\n%s", diff)
	}
}

// After Drain, /readyz says 503 whatever the database says, so load balancers stop sending
// requests; everything else is still served.
func TestReadyzAfterDrain(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	ready := NewReadiness(func(context.Context) error { return nil }, zerolog.Nop())
	router := readyRouter(zerolog.New(&logs), ready)
	router.Get("/ok", func(http.ResponseWriter, *http.Request) {})

	ready.Drain()
	rec := send(t, router, http.MethodGet, readyPath, "")
	want := problemReply{Status: http.StatusServiceUnavailable, Detail: "shutting down"}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("GET /readyz after Drain (-want +got):\n%s", diff)
	}
	if got := send(t, router, http.MethodGet, "/ok", "").Code; got != http.StatusOK {
		t.Errorf("GET /ok after Drain = %d, want 200", got)
	}
	// Only /ok's line: /readyz is never in the request log, even when it fails.
	lines := logLines(t, &logs)
	var paths []any
	for _, line := range lines {
		url, _ := line["url"].(map[string]any)
		paths = append(paths, url["path"])
	}
	if !cmp.Equal(paths, []any{"/ok"}) {
		t.Errorf("request log = %v, want only GET /ok's line", lines)
	}
}

// A load balancer asks every few seconds: a database outage is logged once when it starts and once
// when it ends, not on every check.
func TestReadyzLogsOnlyChanges(t *testing.T) {
	t.Parallel()
	var down atomic.Bool
	var logs bytes.Buffer
	ready := NewReadiness(func(context.Context) error {
		if down.Load() {
			return errors.New("connection refused")
		}
		return nil
	}, zerolog.New(&logs))
	router := readyRouter(zerolog.Nop(), ready)

	wantCodes := func(want int, n int) {
		t.Helper()
		for range n {
			if got := send(t, router, http.MethodGet, readyPath, "").Code; got != want {
				t.Fatalf("GET /readyz = %d, want %d", got, want)
			}
		}
	}
	wantCodes(http.StatusOK, 2)
	down.Store(true)
	wantCodes(http.StatusServiceUnavailable, 3)
	down.Store(false)
	wantCodes(http.StatusOK, 2)

	var got []string
	for line := range strings.Lines(logs.String()) {
		got = append(got, strings.TrimSpace(line))
	}
	want := []string{
		`{"level":"warn","error":"connection refused","message":"not ready: the database didn't answer"}`,
		`{"level":"info","message":"ready again: the database answers"}`,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("readiness log mismatch (-want +got):\n%s", diff)
	}
}

// A caller that hangs up mid-check says nothing about the database: no "not ready" line.
func TestReadyzIgnoresACallerThatLeft(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	ready := NewReadiness(func(ctx context.Context) error { return ctx.Err() }, zerolog.New(&logs))
	router := readyRouter(zerolog.Nop(), ready)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	router.ServeHTTP(httptest.NewRecorder(), req)
	if logs.Len() != 0 {
		t.Errorf("readiness log = %q, want nothing", logs.String())
	}
}

// readyRouter is the base router, logging requests to logger, with /readyz answered by ready.
func readyRouter(logger zerolog.Logger, ready *Readiness) *chi.Mux {
	router := newBaseRouter(logger, noop.NewMeterProvider(), tracenoop.NewTracerProvider(), nil)
	router.Get(readyPath, ready.serve)
	return router
}
