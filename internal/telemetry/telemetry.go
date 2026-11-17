// Package telemetry sets up a program's metrics and traces with OpenTelemetry: where they go is
// decided by the standard OTEL_* settings, read by OpenTelemetry itself, so changing where they're
// stored (a collector, Prometheus, Jaeger, a vendor) changes no code. Both are on unless set to
// "none".
//
//   - OTEL_METRICS_EXPORTER: "otlp" (the default) pushes them (OTEL_EXPORTER_OTLP_ENDPOINT, every
//     OTEL_METRIC_EXPORT_INTERVAL ms, default 60000); "prometheus" serves them for pulling at
//     http://OTEL_EXPORTER_PROMETHEUS_HOST:OTEL_EXPORTER_PROMETHEUS_PORT/metrics (default
//     localhost:9464); "none" turns metrics off.
//   - OTEL_TRACES_EXPORTER: "otlp" (the default) pushes traces to OTEL_EXPORTER_OTLP_ENDPOINT;
//     "none" turns them off. OTEL_TRACES_SAMPLER_ARG, from 0 to 1 (default 1), is the share of
//     traces kept.
//   - OTEL_SDK_DISABLED=true turns both off, as OpenTelemetry's other SDKs read it.
//   - OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES: override what Start names the program.
package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// FlushTimeout is how long Flush may take to send the last metrics and traces: little, as it runs
// after the program's own shutdown wait, and both must end before the platform's hard kill
// (Docker: 10 s).
const FlushTimeout = time.Second

// Program is what the metrics and traces say about the program sending them.
type Program struct {
	// Name is the program within the project ("api", "worker"); the service is named
	// "<project>-<Name>", the project being the last part of the module path.
	Name    string
	Version string
	Env     string // config.App's Env
}

// Providers are a program's meter and tracer providers: pass them to what records metrics and
// traces, and call Flush on the way out.
type Providers struct {
	Meters   metric.MeterProvider
	Tracers  trace.TracerProvider
	shutdown []func(context.Context) error
}

// Start sets up metrics and traces as the OTEL_* settings say, and starts recording the Go
// runtime's metrics (memory, goroutines, garbage collection). What's off records nothing, at next to
// no cost. OpenTelemetry's own errors (a send that failed) go to logger at warn, at most one line a
// minute: the program goes on, and nowhere to send to isn't worth a line every few seconds.
func Start(ctx context.Context, p Program, logger zerolog.Logger) (*Providers, error) {
	// autoexport would print them on stdout, among the JSON log lines.
	for _, name := range []string{"OTEL_METRICS_EXPORTER", "OTEL_TRACES_EXPORTER"} {
		if os.Getenv(name) == "console" {
			return nil, fmt.Errorf("%s=console isn't supported: it mixes telemetry into the log; use otlp or none", name)
		}
	}
	off := &Providers{Meters: noop.NewMeterProvider(), Tracers: tracenoop.NewTracerProvider()}
	// autoexport doesn't read it.
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return off, nil
	}
	// The sampler is ours (see newSampler), parent-based with OTEL_TRACES_SAMPLER_ARG as its
	// ratio: the two settings that mean that are accepted (platforms set them); any other would be
	// silently ignored, so it stops the program instead.
	switch os.Getenv("OTEL_TRACES_SAMPLER") {
	case "", "parentbased_traceidratio", "parentbased_always_on":
	default:
		return nil, errors.New("OTEL_TRACES_SAMPLER: only parentbased_traceidratio is supported; " +
			"set OTEL_TRACES_SAMPLER_ARG to the share of traces to keep, or OTEL_TRACES_EXPORTER=none")
	}
	ratio, err := sampleRatio()
	if err != nil {
		return nil, err
	}
	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("set up metrics exporter (OTEL_METRICS_EXPORTER): %w", err)
	}
	exporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("set up traces exporter (OTEL_TRACES_EXPORTER): %w", err), reader.Shutdown(ctx))
	}
	metricsOn, tracesOn := !autoexport.IsNoneMetricReader(reader), !autoexport.IsNoneSpanExporter(exporter)
	if !metricsOn && !tracesOn {
		return off, nil
	}
	providers := off

	res, err := newResource(ctx, p)
	if err != nil {
		return nil, errors.Join(err, reader.Shutdown(ctx), exporter.Shutdown(ctx))
	}
	// Process-wide: OpenTelemetry has one error handler.
	otel.SetErrorHandler(&limitedWarn{logger: logger, every: time.Minute})
	if metricsOn {
		meters := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res),
			sdkmetric.WithView(views()...))
		providers.Meters = meters
		providers.shutdown = append(providers.shutdown, meters.Shutdown)
		if err := runtime.Start(runtime.WithMeterProvider(meters)); err != nil {
			return nil, errors.Join(fmt.Errorf("record Go runtime metrics: %w", err), meters.Shutdown(ctx), exporter.Shutdown(ctx))
		}
	}
	if tracesOn {
		// Sends in the background, in batches, holding at most 2048 spans; past that, new ones are
		// dropped (OTEL_BSP_* change these).
		tracers := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res),
			sdktrace.WithSampler(newSampler(ratio)))
		providers.Tracers = tracers
		providers.shutdown = append(providers.shutdown, tracers.Shutdown)
	}
	return providers, nil
}

// Flush sends the metrics and traces not yet sent and stops recording, giving up after
// FlushTimeout. A failure is logged to logger at warn, like any failed send, not returned: the
// program has stopped cleanly or not by then, and losing the last numbers doesn't change that.
func (p *Providers) Flush(logger zerolog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), FlushTimeout)
	defer cancel()
	// At once, so both get the whole FlushTimeout.
	errs := make([]error, len(p.shutdown))
	var wg sync.WaitGroup
	for i, shutdown := range p.shutdown {
		wg.Go(func() { errs[i] = shutdown(ctx) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		logger.Warn().Err(err).Str("source", "opentelemetry").Msg("telemetry not flushed")
	}
}

// sampleRatio is OTEL_TRACES_SAMPLER_ARG: the share of traces kept, 1 when unset.
func sampleRatio() (float64, error) {
	value := os.Getenv("OTEL_TRACES_SAMPLER_ARG")
	if value == "" {
		return 1, nil
	}
	ratio, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
		return 0, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG=%q: want a number from 0 to 1", value)
	}
	return ratio, nil
}

// newSampler decides which spans are kept. Only what serves a request or runs a job (a server or
// consumer span) starts a trace, a ratio of them kept; everything else is kept when, and only
// when, the trace it's in is. Database and key-value store clients start a span for every call,
// even outside a request or job: a job queue polls every second, which would be a trace each.
func newSampler(ratio float64) sdktrace.Sampler { //nolint:ireturn // the SDK takes samplers as an interface
	return sdktrace.ParentBased(entryPoints{keep: sdktrace.TraceIDRatioBased(ratio)})
}

// entryPoints is the sampler for spans with no parent: server and consumer spans by keep, others
// dropped.
type entryPoints struct {
	keep sdktrace.Sampler
}

func (s entryPoints) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if p.Kind == trace.SpanKindServer || p.Kind == trace.SpanKindConsumer {
		return s.keep.ShouldSample(p)
	}
	return sdktrace.SamplingResult{
		Decision:   sdktrace.Drop,
		Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState(),
	}
}

func (s entryPoints) Description() string {
	return "EntryPoints{" + s.keep.Description() + "}"
}

// limitedWarn logs OpenTelemetry's errors at warn, at most one line every so often, with how many
// it skipped since the last: an unreachable endpoint fails every send.
type limitedWarn struct {
	logger  zerolog.Logger
	every   time.Duration
	mu      sync.Mutex
	last    time.Time
	skipped int
}

func (l *limitedWarn) Handle(err error) {
	l.mu.Lock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.skipped++
		l.mu.Unlock()
		return
	}
	skipped := l.skipped
	l.last, l.skipped = now, 0
	l.mu.Unlock()

	line := l.logger.Warn().Err(err).Str("source", "opentelemetry")
	if skipped > 0 {
		line.Int("errors_skipped", skipped)
	}
	line.Msg("telemetry: OpenTelemetry reported an error")
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
		return nil, fmt.Errorf("describe the program for telemetry: %w", err)
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
