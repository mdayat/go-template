// Package telemetry sets up a program's metrics with OpenTelemetry: where they go is decided by the
// standard OTEL_* settings, read by OpenTelemetry itself, so changing where they're stored (a
// collector, Prometheus, a vendor) changes no code.
//
//   - OTEL_METRICS_EXPORTER: "otlp" pushes them (OTEL_EXPORTER_OTLP_ENDPOINT, every
//     OTEL_METRIC_EXPORT_INTERVAL ms, default 60000); "prometheus" serves them for pulling at
//     http://OTEL_EXPORTER_PROMETHEUS_HOST:OTEL_EXPORTER_PROMETHEUS_PORT/metrics (default
//     localhost:9464); unset or "none" turns metrics off.
//   - OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES: override what Start names the program.
package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// FlushTimeout is how long Flush may take to send the last metrics: little, as it runs after the
// program's own shutdown wait, and both must end before the platform's hard kill (Docker: 10 s).
const FlushTimeout = time.Second

// Program is what the metrics say about the program sending them.
type Program struct {
	// Name is the program within the project ("api", "worker"); the service is named
	// "<project>-<Name>", the project being the last part of the module path.
	Name    string
	Version string
	Env     string // config.App's Env
}

// Metrics is a program's meter provider: pass Provider to what records metrics, and call Flush on
// the way out.
type Metrics struct {
	Provider metric.MeterProvider
	shutdown func(context.Context) error
}

// Start sets up metrics as the OTEL_* settings say, and starts recording the Go runtime's (memory,
// goroutines, garbage collection). With metrics off, Provider records nothing, at next to no cost.
// OpenTelemetry's own errors (a push that failed) go to logger, at warn: the program goes on.
func Start(ctx context.Context, p Program, logger zerolog.Logger) (*Metrics, error) {
	off := &Metrics{Provider: noop.NewMeterProvider(), shutdown: func(context.Context) error { return nil }}
	// Unset is off, not autoexport's default (otlp): pushing to nothing logs an error every minute.
	switch os.Getenv("OTEL_METRICS_EXPORTER") {
	case "":
		return off, nil
	case "console":
		// autoexport would print the metrics on stdout, among the JSON log lines.
		return nil, errors.New("OTEL_METRICS_EXPORTER=console isn't supported: it mixes metrics into the log; use otlp or prometheus")
	}
	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("set up metrics exporter (OTEL_METRICS_EXPORTER): %w", err)
	}
	if autoexport.IsNoneMetricReader(reader) {
		return off, nil
	}
	// Process-wide: OpenTelemetry has one error handler.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn().Err(err).Str("source", "opentelemetry").Msg("metrics: OpenTelemetry reported an error")
	}))
	res, err := newResource(ctx, p)
	if err != nil {
		return nil, errors.Join(err, reader.Shutdown(ctx))
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res),
		sdkmetric.WithView(views()...))
	if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
		return nil, errors.Join(fmt.Errorf("record Go runtime metrics: %w", err), provider.Shutdown(ctx))
	}
	return &Metrics{Provider: provider, shutdown: provider.Shutdown}, nil
}

// Flush sends the metrics not yet sent and stops recording, giving up after FlushTimeout. A failure
// is logged to logger, not returned: the program has stopped cleanly or not by then, and losing the
// last numbers doesn't change that.
func (m *Metrics) Flush(logger zerolog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), FlushTimeout)
	defer cancel()
	if err := m.shutdown(ctx); err != nil {
		logger.Error().Err(err).Msg("metrics not flushed")
	}
}

// newResource describes the program. service.instance.id is new on every start: without it, copies
// of the program running at once would send the same series, each overwriting the others' numbers.
// The OTEL_* settings come last, so they win.
func newResource(ctx context.Context, p Program) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(
			semconv.ServiceName(serviceName(p.Name)),
			semconv.ServiceVersion(p.Version),
			semconv.ServiceInstanceID(rand.Text()),
			semconv.DeploymentEnvironmentNameKey.String(p.Env),
		),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("describe the program for metrics: %w", err)
	}
	return res, nil
}

// serviceName is name prefixed with the project: the last part of the module path (make rename
// changes it), so two projects' APIs aren't both "api" in one metrics store.
func serviceName(name string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return name
	}
	return projectName(info.Main.Path, name)
}

// majorVersion is a module path's major-version suffix (/v2), which isn't the project's name.
var majorVersion = regexp.MustCompile(`/v[0-9]+$`)

func projectName(modulePath, name string) string {
	if modulePath == "" {
		return name
	}
	return path.Base(majorVersion.ReplaceAllString(modulePath, "")) + "-" + name
}

// views fixes what the libraries record by default: time histograms whose default buckets miss the
// usual times, a key-value store client's command and connect times (db.client.connections.use_time,
// .create_time: milliseconds, mostly under 1) and database query times
// (db.client.operation.duration: seconds, mostly a few ms).
func views() []sdkmetric.View {
	buckets := func(name string, bounds ...float64) sdkmetric.View {
		return sdkmetric.NewView(sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: bounds}})
	}
	ms := []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000}
	return []sdkmetric.View{
		buckets("db.client.connections.use_time", ms...),
		buckets("db.client.connections.create_time", ms...),
		buckets("db.client.operation.duration",
			0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
		// remove:api
		// otelhttp's request and response body sizes: they'd triple the HTTP series, and no
		// dashboard reads them.
		sdkmetric.NewView(sdkmetric.Instrument{Name: "http.server.request.body.size"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}}),
		sdkmetric.NewView(sdkmetric.Instrument{Name: "http.server.response.body.size"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}}),
		// remove:end
	}
}
