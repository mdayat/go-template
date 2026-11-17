// Package spantest records traces in memory for tests, so a test can check what code recorded:
// give Recorder.Provider to the code, then read Recorder.Ended. Every span is kept, whatever the
// program's sampler would decide (internal/telemetry tests that).
package spantest

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Recorder holds the spans ended through its Provider.
type Recorder struct {
	Provider *sdktrace.TracerProvider
	spans    *tracetest.SpanRecorder
}

// New returns an empty Recorder.
func New() *Recorder {
	spans := tracetest.NewSpanRecorder()
	return &Recorder{
		Provider: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans), sdktrace.WithSampler(sdktrace.AlwaysSample())),
		spans:    spans,
	}
}

// Ended returns the spans ended so far, in the order they ended.
func (r *Recorder) Ended() []sdktrace.ReadOnlySpan {
	return r.spans.Ended()
}

// Named returns the ended spans called name.
func (r *Recorder) Named(name string) []sdktrace.ReadOnlySpan {
	var found []sdktrace.ReadOnlySpan
	for _, s := range r.spans.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	return found
}

// Attributes returns s's attributes as strings, for comparing.
func Attributes(s sdktrace.ReadOnlySpan) map[string]string {
	m := map[string]string{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}

// Names returns the names of the spans ended so far, for a test's failure message.
func (r *Recorder) Names() []string {
	var names []string
	for _, s := range r.spans.Ended() {
		names = append(names, s.Name())
	}
	return names
}
