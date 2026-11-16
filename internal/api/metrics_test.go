package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/mdayat/go-template/internal/metrictest"
)

// Each request's time is recorded by route pattern, method and status, never by the path itself;
// the health check isn't recorded.
func TestMeasureRequests(t *testing.T) {
	t.Parallel()
	rec := metrictest.New()
	router := newBaseRouter(zerolog.Nop(), rec.Provider, nil)
	router.Get("/tasks/{id}", func(http.ResponseWriter, *http.Request) {})
	client := request{remoteAddr: "192.0.2.1:1234"}

	client.sendTo(t, router, "/tasks/1")
	client.sendTo(t, router, "/tasks/2")
	client.sendTo(t, router, "/nowhere")
	client.sendTo(t, router, healthPath)

	got := rec.Collect(t)
	const name = "http.server.request.duration"
	tasks := got.Find(name, map[string]string{
		"http.route": "/tasks/{id}", "http.request.method": "GET", "http.response.status_code": "200",
	})
	if len(tasks) != 1 || tasks[0].Count != 2 {
		t.Errorf("/tasks/{id} points = %+v, want one counting 2 requests; all: %+v", tasks, got[name])
	}
	if notFound := got.Find(name, map[string]string{"http.response.status_code": "404"}); len(notFound) != 1 {
		t.Errorf("404 points = %+v, want one", notFound)
	}
	var total int64
	for _, p := range got[name] {
		total += p.Count
		// The test requests' Host is example.com: a client's Host never becomes a label.
		if p.Attributes["server.address"] != "api" || p.Attributes["server.port"] != "" {
			t.Errorf("a point has server.address %q, server.port %q; want api and none",
				p.Attributes["server.address"], p.Attributes["server.port"])
		}
		if route := p.Attributes["http.route"]; route != "" && route != "/tasks/{id}" {
			t.Errorf("a point has http.route %q, want only route patterns", route)
		}
	}
	if total != 3 {
		t.Errorf("recorded %d requests, want 3: the health check isn't recorded", total)
	}
}

// A client that hung up before any reply is recorded as 499, as the request log has it, not as
// the 200 net/http assumes.
func TestMeasureRequestsClientGone(t *testing.T) {
	t.Parallel()
	rec := metrictest.New()
	router := newBaseRouter(zerolog.Nop(), rec.Provider, nil)
	router.Get("/slow", func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/slow", nil)

	router.ServeHTTP(httptest.NewRecorder(), req)

	got := rec.Collect(t).Find("http.server.request.duration", map[string]string{"http.route": "/slow"})
	if len(got) != 1 || got[0].Attributes["http.response.status_code"] != "499" {
		t.Errorf("points = %+v, want one with status 499", got)
	}
}

// A reply a panic cut off half-sent is still recorded, marked error.type aborted, and the
// connection is still cut (the abort reaches net/http).
func TestMeasureRequestsAborted(t *testing.T) {
	t.Parallel()
	rec := metrictest.New()
	router := newBaseRouter(zerolog.Nop(), rec.Provider, nil)
	router.Get("/half", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	})

	func() {
		defer func() {
			if p := recover(); p != http.ErrAbortHandler { //nolint:errorlint // net/http compares it so too
				t.Errorf("recovered %v, want http.ErrAbortHandler", p)
			}
		}()
		request{remoteAddr: "192.0.2.1:1234"}.sendTo(t, router, "/half")
	}()

	got := rec.Collect(t).Find("http.server.request.duration", map[string]string{"http.route": "/half"})
	if len(got) != 1 || got[0].Attributes["error.type"] != "aborted" {
		t.Errorf("points = %+v, want one marked error.type aborted", got)
	}
}

// With metrics off, nothing wraps the handler: otelhttp costs microseconds even recording nowhere.
func TestMeasureRequestsOff(t *testing.T) {
	t.Parallel()
	next := http.NewServeMux() // a pointer: comparable

	if got := measureRequests(noop.NewMeterProvider())(next); got != next {
		t.Error("measureRequests(off) wrapped the handler, want it unchanged")
	}
}
