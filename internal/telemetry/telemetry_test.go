package telemetry

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// These tests set OTEL_* variables, so none runs in parallel.

// "none": off, at no cost.
func TestStartOff(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")

	m, err := Start(t.Context(), Program{Name: "test"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, ok := m.Meters.(noop.MeterProvider); !ok {
		t.Errorf("Meters = %T, want the no-op one", m.Meters)
	}
	if _, ok := m.Tracers.(tracenoop.TracerProvider); !ok {
		t.Errorf("Tracers = %T, want the no-op one", m.Tracers)
	}
	m.Flush(zerolog.Nop())
}

// Unset: both on (TestStartPushesTraces and TestStartLogsFailedPushes show where to), so a deploy
// that forgets the settings still sends.
func TestStartOnByDefault(t *testing.T) {
	cleanEnv(t)
	// Somewhere nothing listens, not a local collector.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:"+strconv.Itoa(freePort(t)))

	m, err := Start(t.Context(), Program{Name: "test"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { m.Flush(zerolog.Nop()) })
	if _, ok := m.Meters.(*sdkmetric.MeterProvider); !ok {
		t.Errorf("Meters = %T, want the SDK's", m.Meters)
	}
	if _, ok := m.Tracers.(*sdktrace.TracerProvider); !ok {
		t.Errorf("Tracers = %T, want the SDK's", m.Tracers)
	}
}

// Traces are pushed with OTLP to the endpoint set, and Flush sends what's left.
func TestStartPushesTraces(t *testing.T) {
	cleanEnv(t)
	got := make(chan string, 10)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/v1/traces")

	m, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	_, span := m.Tracers.Tracer("test").Start(t.Context(), "GET /tasks", trace.WithSpanKind(trace.SpanKindServer))
	span.End()
	var logs bytes.Buffer
	m.Flush(zerolog.New(&logs))

	select {
	case req := <-got:
		if req != "POST /v1/traces" {
			t.Errorf("collector got %q, want POST /v1/traces", req)
		}
	default:
		t.Errorf("nothing pushed after Flush; logged %q", logs.String())
	}
}

// Only a request or a job run starts a trace; database, key-value store and outside calls are kept
// only inside a kept one. A job run queued in a trace follows that trace's decision.
// OTEL_TRACES_SAMPLER_ARG keeps a share of the traces started.
func TestSampler(t *testing.T) {
	t.Parallel()
	// Not span kinds: a parent from another program, kept or not.
	const remoteKept, remoteDropped = trace.SpanKind(100), trace.SpanKind(101)
	sampled := func(ratio float64, parent trace.SpanKind, kind trace.SpanKind) bool {
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSampler(newSampler(ratio))).Tracer("test")
		ctx := t.Context()
		if parent == remoteKept || parent == remoteDropped {
			flags := trace.TraceFlags(0)
			if parent == remoteKept {
				flags = trace.FlagsSampled
			}
			ctx = trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: flags, Remote: true,
			}))
		} else if parent != trace.SpanKindUnspecified {
			var span trace.Span
			ctx, span = tracer.Start(ctx, "parent", trace.WithSpanKind(parent))
			defer span.End()
		}
		_, span := tracer.Start(ctx, "span", trace.WithSpanKind(kind))
		defer span.End()
		return span.SpanContext().IsSampled()
	}
	none := trace.SpanKindUnspecified
	tests := []struct {
		name         string
		ratio        float64
		parent, kind trace.SpanKind
		want         bool
	}{
		{"a request", 1, none, trace.SpanKindServer, true},
		{"a job run", 1, none, trace.SpanKindConsumer, true},
		{"a query outside any", 1, none, trace.SpanKindClient, false},
		{"a job queued outside any", 1, none, trace.SpanKindProducer, false},
		{"code's own span outside any", 1, none, trace.SpanKindInternal, false},
		{"a query in a request", 1, trace.SpanKindServer, trace.SpanKindClient, true},
		{"a query in a dropped one", 1, trace.SpanKindClient, trace.SpanKindClient, false},
		{"a request, none kept", 0, none, trace.SpanKindServer, false},
		{"a job queued in a kept trace, none kept", 0, remoteKept, trace.SpanKindConsumer, true},
		{"a job queued in a dropped trace", 1, remoteDropped, trace.SpanKindConsumer, false},
	}
	for _, tt := range tests {
		if got := sampled(tt.ratio, tt.parent, tt.kind); got != tt.want {
			t.Errorf("%s: sampled = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestStartReadsSampleRatio(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	for value, wantErr := range map[string]bool{"0": false, "0.25": false, "1": false, "1.5": true, "-0.1": true, "half": true, "NaN": true} {
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", value)
		if _, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop()); (err != nil) != wantErr {
			t.Errorf("OTEL_TRACES_SAMPLER_ARG=%s: Start() error = %v, want an error: %t", value, err, wantErr)
		}
	}
}

// OTEL_TRACES_SAMPLER naming our sampler (parent-based, by ratio) is accepted, as platforms set
// it; any other would be ignored next to ours, so it's refused instead.
func TestStartChecksSampler(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	for value, wantErr := range map[string]bool{
		"parentbased_traceidratio": false, "parentbased_always_on": false,
		"always_on": true, "traceidratio": true, "always_off": true,
	} {
		t.Setenv("OTEL_TRACES_SAMPLER", value)
		if _, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop()); (err != nil) != wantErr {
			t.Errorf("OTEL_TRACES_SAMPLER=%s: Start() error = %v, want an error: %t", value, err, wantErr)
		}
	}
}

// OTEL_SDK_DISABLED=true, OpenTelemetry's own off switch, turns both off whatever else is set.
func TestStartDisabled(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")

	m, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, ok := m.Meters.(noop.MeterProvider); !ok {
		t.Errorf("Meters = %T, want the no-op one", m.Meters)
	}
	if _, ok := m.Tracers.(tracenoop.TracerProvider); !ok {
		t.Errorf("Tracers = %T, want the no-op one", m.Tracers)
	}
}

// An endpoint that's down fails every send: one warn line, then at most one a minute, counting
// what it skipped.
func TestLimitedWarn(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	l := &limitedWarn{logger: zerolog.New(&logs), every: time.Minute}

	for range 3 {
		l.Handle(errors.New("send failed"))
	}
	if n := strings.Count(logs.String(), "\n"); n != 1 {
		t.Fatalf("logged %d lines for 3 errors in a minute, want 1: %s", n, logs.String())
	}
	l.last = l.last.Add(-time.Minute)
	l.Handle(errors.New("send failed"))
	if n := strings.Count(logs.String(), "\n"); n != 2 || !strings.Contains(logs.String(), `"errors_skipped":2`) {
		t.Errorf("after a minute: %s, want a second line counting the 2 skipped", logs.String())
	}
}

func TestStartRefusesAnUnknownExporter(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "nowhere")

	if _, err := Start(t.Context(), Program{Name: "test"}, zerolog.Nop()); err == nil {
		t.Error("Start() error = nil, want one naming the setting")
	}
}

// With pulling on, the page Prometheus reads names the program (service.name, version,
// environment, a per-start instance ID), and holds what the program records and Go's runtime metrics.
func TestStartPrometheusPage(t *testing.T) {
	port := prometheusEnv(t)

	m, err := Start(t.Context(), Program{Name: "api", Version: "v1.2.3", Env: "development"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	counter, err := m.Meters.Meter("test").Int64Counter("test.things")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(t.Context(), 3)

	page := get(t, port)
	for _, want := range []string{
		// From the module path, so it's right after make rename too (TestProjectName has the rule).
		`service_name="` + serviceName("api") + `"`,
		`service_version="v1.2.3"`,
		`deployment_environment_name="development"`,
		`service_instance_id="`,
		"test_things_total",
		"go_goroutine_count",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("metrics page has no %s", want)
		}
	}

	var logs bytes.Buffer
	m.Flush(zerolog.New(&logs))
	if logs.Len() > 0 {
		t.Errorf("Flush() logged %q, want nothing", logs.String())
	}
}

// OTEL_SERVICE_NAME wins over the name Start gives, so a deploy can name the service its own way.
func TestStartServiceNameFromSetting(t *testing.T) {
	port := prometheusEnv(t)
	t.Setenv("OTEL_SERVICE_NAME", "billing-api")

	m, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { m.Flush(zerolog.Nop()) })

	if page := get(t, port); !strings.Contains(page, `service_name="billing-api"`) {
		t.Error(`metrics page doesn't name the service "billing-api"`)
	}
}

// A push that fails is logged at warn in the program's own log, not as plain text on stderr.
func TestStartLogsFailedPushes(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:"+strconv.Itoa(freePort(t))+"/v1/metrics")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "50")
	logs := &syncBuffer{}

	m, err := Start(t.Context(), Program{Name: "api"}, zerolog.New(logs))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { m.Flush(zerolog.Nop()) })

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `"source":"opentelemetry"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no warn line from OpenTelemetry after a failed push; logs: %q", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) {
		t.Errorf("logs = %q, want the failure at warn", logs.String())
	}
}

// syncBuffer is a bytes.Buffer OpenTelemetry's export goroutine can write while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", l.Addr())
	}
	_ = l.Close()
	return addr.Port
}

// get returns the metrics page on port, waiting briefly for its server to start.
func get(t *testing.T, port int) string {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/metrics"
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			return string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("get %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestProjectName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"github.com/acme/billing":    "billing-api",
		"github.com/acme/billing/v2": "billing-api",
		"":                           "api",
	}
	for modulePath, want := range tests {
		if got := projectName(modulePath, "api"); got != want {
			t.Errorf("projectName(%q) = %q, want %q", modulePath, got, want)
		}
	}
}

// Times have buckets that fit them: a sub-millisecond command lands below 1 ms, not in 0-5 ms.
func TestStartViews(t *testing.T) {
	port := prometheusEnv(t)

	m, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { m.Flush(zerolog.Nop()) })
	meter := m.Meters.Meter("test")
	useTime, err := meter.Float64Histogram("db.client.connections.use_time", metric.WithUnit("ms"))
	if err != nil {
		t.Fatal(err)
	}
	useTime.Record(t.Context(), 0.3)

	page := get(t, port)
	if !strings.Contains(page, `db_client_connections_use_time_milliseconds_bucket{`) ||
		!strings.Contains(page, `le="0.5"} 1`) {
		t.Error("use_time has no 0.5 ms bucket holding the 0.3 ms command")
	}
}

// remove:api

// otelhttp's body sizes aren't recorded: they'd triple the HTTP series.
func TestStartDropsHTTPBodySizes(t *testing.T) {
	port := prometheusEnv(t)
	m, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { m.Flush(zerolog.Nop()) })
	bodySize, err := m.Meters.Meter("test").Int64Histogram("http.server.request.body.size", metric.WithUnit("By"))
	if err != nil {
		t.Fatal(err)
	}
	bodySize.Record(t.Context(), 100)

	if strings.Contains(get(t, port), "http_server_request_body_size") {
		t.Error("http.server.request.body.size is recorded, want it dropped")
	}
}

// remove:end

func TestStartRefusesConsole(t *testing.T) {
	for _, name := range []string{"OTEL_METRICS_EXPORTER", "OTEL_TRACES_EXPORTER"} {
		cleanEnv(t)
		t.Setenv(name, "console")

		if _, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop()); err == nil {
			t.Errorf("%s=console: Start() error = nil, want it refused", name)
		}
	}
}

// cleanEnv unsets the OTEL_* variables a developer's shell may hold, which would change what the
// tests see.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_METRICS_EXPORTER", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_METRICS_PRODUCERS",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_METRIC_EXPORT_INTERVAL", "OTEL_EXPORTER_PROMETHEUS_HOST", "OTEL_EXPORTER_PROMETHEUS_PORT",
		"OTEL_TRACES_EXPORTER", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", "OTEL_SDK_DISABLED",
	} {
		t.Setenv(name, "")
	}
}

// prometheusEnv sets up pulling on a free local port, and returns the port.
func prometheusEnv(t *testing.T) int {
	t.Helper()
	cleanEnv(t)
	port := freePort(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "prometheus")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", strconv.Itoa(port))
	return port
}

// OTEL_TRACES_SAMPLER_ARG reaches the sampler: at 0 a request starts no kept trace.
func TestStartSamplesByTheRatio(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:"+strconv.Itoa(freePort(t))+"/v1/traces")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")

	m, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { m.Flush(zerolog.Nop()) })
	_, span := m.Tracers.Tracer("test").Start(t.Context(), "GET /tasks", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	if span.SpanContext().IsSampled() {
		t.Error("a request's trace was kept at OTEL_TRACES_SAMPLER_ARG=0")
	}
}
