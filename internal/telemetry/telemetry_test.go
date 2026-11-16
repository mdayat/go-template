package telemetry

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// These tests set OTEL_* variables, so none runs in parallel.

// Unset or "none": metrics off, at no cost.
func TestStartOff(t *testing.T) {
	cleanEnv(t)
	for _, value := range []string{"", "none"} {
		t.Setenv("OTEL_METRICS_EXPORTER", value)

		m, err := Start(t.Context(), Program{Name: "test"}, zerolog.Nop())
		if err != nil {
			t.Fatalf("OTEL_METRICS_EXPORTER=%q: Start() error = %v", value, err)
		}
		if _, ok := m.Provider.(noop.MeterProvider); !ok {
			t.Errorf("OTEL_METRICS_EXPORTER=%q: Provider = %T, want the no-op one", value, m.Provider)
		}
		m.Flush(zerolog.Nop())
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
	counter, err := m.Provider.Meter("test").Int64Counter("test.things")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(t.Context(), 3)

	page := get(t, port)
	for _, want := range []string{
		`service_name="go-template-api"`,
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
	meter := m.Provider.Meter("test")
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
	bodySize, err := m.Provider.Meter("test").Int64Histogram("http.server.request.body.size", metric.WithUnit("By"))
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
	cleanEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "console")

	if _, err := Start(t.Context(), Program{Name: "api"}, zerolog.Nop()); err == nil {
		t.Error("Start() error = nil, want console refused")
	}
}

// cleanEnv unsets the OTEL_* variables a developer's shell may hold, which would change what the
// tests see.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_METRICS_EXPORTER", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_METRICS_PRODUCERS",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_METRIC_EXPORT_INTERVAL",
		"OTEL_EXPORTER_PROMETHEUS_HOST", "OTEL_EXPORTER_PROMETHEUS_PORT",
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
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1")
	t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", strconv.Itoa(port))
	return port
}
