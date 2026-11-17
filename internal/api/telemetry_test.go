package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/json"
	"github.com/mdayat/go-template/internal/metrictest"
	"github.com/mdayat/go-template/internal/spantest"
)

// Each request's time is recorded by route pattern, method and status, never by the path itself;
// the health check isn't recorded.
func TestObserveRequestsMetrics(t *testing.T) {
	t.Parallel()
	rec := metrictest.New()
	router := newBaseRouter(zerolog.Nop(), rec.Provider, tracenoop.NewTracerProvider(), nil)
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
func TestObserveRequestsClientGone(t *testing.T) {
	t.Parallel()
	rec := metrictest.New()
	router := newBaseRouter(zerolog.Nop(), rec.Provider, tracenoop.NewTracerProvider(), nil)
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

// A reply a panic cut off half-sent is still recorded, marked error.type aborted (its span failed
// too), and the connection is still cut (the abort reaches net/http).
func TestObserveRequestsAborted(t *testing.T) {
	t.Parallel()
	rec, spans := metrictest.New(), spantest.New()
	router := newBaseRouter(zerolog.Nop(), rec.Provider, spans.Provider, nil)
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
	span := spans.Named("GET /half")
	if len(span) != 1 || span[0].Status().Code != codes.Error || spantest.Attributes(span[0])["error.type"] != "aborted" {
		t.Errorf("spans = %v, want one GET /half failed with error.type aborted", spans.Names())
	}
}

// Each request is a server span named by its route, holding the client's IP as TRUSTED_PROXIES
// finds it and the route pattern as its path. A trace the client sends is only linked: the request
// starts its own. A method HTTP doesn't define isn't a name. The health check isn't traced.
func TestObserveRequestsTraces(t *testing.T) {
	t.Parallel()
	spans := spantest.New()
	router := newBaseRouter(zerolog.Nop(), noop.NewMeterProvider(), spans.Provider, nil)
	router.Get("/tasks/{id}", func(http.ResponseWriter, *http.Request) {})
	const theirs = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tasks/1", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9") // not a trusted proxy: ignored
	req.Header.Set("Traceparent", theirs)
	router.ServeHTTP(httptest.NewRecorder(), req)
	request{remoteAddr: "192.0.2.1:1234"}.sendTo(t, router, "/nowhere")
	request{remoteAddr: "192.0.2.1:1234"}.sendTo(t, router, healthPath)
	odd := httptest.NewRequestWithContext(t.Context(), "XYZZY", "/tasks/1", nil)
	router.ServeHTTP(httptest.NewRecorder(), odd)

	if names := spans.Names(); len(names) != 3 || names[0] != "GET /tasks/{id}" || names[1] != "GET" || names[2] != "HTTP" {
		t.Fatalf("spans = %q, want [GET /tasks/{id} GET HTTP]: route names, no health check", names)
	}
	span := spans.Ended()[0]
	if span.SpanKind() != trace.SpanKindServer {
		t.Errorf("kind = %v, want server", span.SpanKind())
	}
	attrs := spantest.Attributes(span)
	if attrs["http.route"] != "/tasks/{id}" || attrs["client.address"] != "192.0.2.1" || attrs["url.path"] != "/tasks/{id}" {
		t.Errorf("attributes = %v, want http.route and url.path /tasks/{id}, client.address 192.0.2.1", attrs)
	}
	if path := spantest.Attributes(spans.Ended()[1])["url.path"]; path != "" {
		t.Errorf("unmatched request's url.path = %q, want none", path)
	}
	if span.Parent().IsValid() {
		t.Errorf("parent = %v, want none: a client's trace is linked, never continued", span.Parent())
	}
	if links := span.Links(); len(links) != 1 || links[0].SpanContext.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("links = %+v, want the client's trace", links)
	}
}

// The request's log lines carry its trace.id and span.id, so a trace leads to its logs.
func TestObserveRequestsLogTraceIDs(t *testing.T) {
	t.Parallel()
	spans := spantest.New()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs), noop.NewMeterProvider(), spans.Provider, nil)
	router.Get("/tasks", func(_ http.ResponseWriter, r *http.Request) {
		zerolog.Ctx(r.Context()).Info().Msg("inside")
	})

	request{remoteAddr: "192.0.2.1:1234"}.sendTo(t, router, "/tasks")

	span := spans.Ended()[0].SpanContext()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %d lines, want the handler's and the summary: %s", len(lines), logs.String())
	}
	for _, line := range lines {
		var got struct {
			Trace struct{ ID string } `json:"trace"`
			Span  struct{ ID string } `json:"span"`
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatal(err)
		}
		if got.Trace.ID != span.TraceID().String() || got.Span.ID != span.SpanID().String() {
			t.Errorf("line %s has trace %q span %q, want %s %s", line, got.Trace.ID, got.Span.ID, span.TraceID(), span.SpanID())
		}
	}
}

// With metrics and traces off, nothing wraps the handler: otelhttp costs microseconds even
// recording nowhere. Either on wraps it.
func TestObserveRequestsOff(t *testing.T) {
	t.Parallel()
	next := http.NewServeMux() // a pointer: comparable

	if got := observeRequests(noop.NewMeterProvider(), tracenoop.NewTracerProvider())(next); got != next {
		t.Error("observeRequests(off, off) wrapped the handler, want it unchanged")
	}
	if got := observeRequests(noop.NewMeterProvider(), spantest.New().Provider)(next); got == http.Handler(next) {
		t.Error("observeRequests(off, on) didn't wrap the handler")
	}
	if got := observeRequests(metrictest.New().Provider, tracenoop.NewTracerProvider())(next); got == http.Handler(next) {
		t.Error("observeRequests(on, off) didn't wrap the handler")
	}
}

// A trace that isn't kept leaves no ID in the logs: it would lead nowhere.
func TestObserveRequestsNoIDsForDroppedTraces(t *testing.T) {
	t.Parallel()
	dropAll := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs), noop.NewMeterProvider(), dropAll, nil)
	router.Get("/tasks", func(http.ResponseWriter, *http.Request) {})

	request{remoteAddr: "192.0.2.1:1234"}.sendTo(t, router, "/tasks")

	if strings.Contains(logs.String(), `"trace"`) || strings.Contains(logs.String(), `"span"`) {
		t.Errorf("logged %s, want no trace or span ID", logs.String())
	}
}

// With traces off and metrics on, a client's traceparent goes nowhere: a tracer that records
// nothing would hand it on as the request's own trace, into our logs.
func TestObserveRequestsTracesOffIgnoresTheCallersTrace(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs), metrictest.New().Provider, tracenoop.NewTracerProvider(), nil)
	router.Get("/tasks", func(_ http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			t.Errorf("handler's context holds trace %s, want none", sc.TraceID())
		}
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tasks", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")

	router.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logs.String(), "0af7651916cd43dd8448eb211c80319c") || strings.Contains(logs.String(), `"trace"`) {
		t.Errorf("logged %s, want no trace", logs.String())
	}
}

// logTraceIDs logs only a span being recorded: never one merely passed in, like a caller's.
func TestLogTraceIDsSkipsSpansNotRecorded(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	handler := hlog.NewHandler(zerolog.New(&logs))(logTraceIDs(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hlog.FromRequest(r).Info().Msg("inside")
	})))
	callers := trace.ContextWithRemoteSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled, Remote: true,
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(callers, http.MethodGet, "/", nil))

	if strings.Contains(logs.String(), `"trace"`) {
		t.Errorf("logged %s, want no trace", logs.String())
	}
}

// The span's client.address is never what X-Forwarded-For says unchecked: behind a trusted proxy
// whose header can't be read, it's the connection's address, as the request log has it.
func TestObserveRequestsClientAddressBehindProxy(t *testing.T) {
	t.Parallel()
	spans := spantest.New()
	router := newBaseRouter(zerolog.Nop(), noop.NewMeterProvider(), spans.Provider, []string{"192.0.2.0/24"})
	router.Get("/tasks", func(http.ResponseWriter, *http.Request) {})

	for _, xff := range []string{"203.0.113.9", "alice@example.com", "not an ip, 192.0.2.5"} {
		request{remoteAddr: "192.0.2.1:1234", xff: xff}.sendTo(t, router, "/tasks")
	}

	want := []string{"203.0.113.9", "192.0.2.1", "192.0.2.1"}
	for i, span := range spans.Ended() {
		if got := spantest.Attributes(span)["client.address"]; got != want[i] {
			t.Errorf("request %d: client.address = %q, want %q", i, got, want[i])
		}
	}
}

// A 5xx reply marks the request's span failed.
func TestObserveRequestsServerErrorFails(t *testing.T) {
	t.Parallel()
	spans := spantest.New()
	router := newBaseRouter(zerolog.Nop(), noop.NewMeterProvider(), spans.Provider, nil)
	router.Get("/tasks", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })

	request{remoteAddr: "192.0.2.1:1234"}.sendTo(t, router, "/tasks")

	if span := spans.Named("GET /tasks"); len(span) != 1 || span[0].Status().Code != codes.Error {
		t.Errorf("spans = %q, want GET /tasks failed", spans.Names())
	}
}
