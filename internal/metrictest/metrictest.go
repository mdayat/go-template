// Package metrictest records metrics in memory for tests, so a test can check what code recorded:
// give Recorder.Provider to the code, then read Recorder.Collect.
package metrictest

import (
	"fmt"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Recorder holds what's recorded through its Provider.
type Recorder struct {
	Provider *sdkmetric.MeterProvider
	reader   *sdkmetric.ManualReader
}

// New returns an empty Recorder.
func New() *Recorder {
	reader := sdkmetric.NewManualReader()
	return &Recorder{Provider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), reader: reader}
}

// Point is one data point: its attributes, and its count (a histogram's number of measurements, or
// a sum's or gauge's value).
type Point struct {
	Attributes map[string]string
	Count      int64
}

// Metrics maps each metric's name to its data points.
type Metrics map[string][]Point

// Collect returns what's been recorded so far.
func (r *Recorder) Collect(tb testing.TB) Metrics {
	tb.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(tb.Context(), &rm); err != nil {
		tb.Fatalf("metrictest: collect: %v", err)
	}
	got := Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = append(got[m.Name], points(m.Data)...)
		}
	}
	return got
}

// Find returns name's data points whose attributes include every key and value in attrs.
func (m Metrics) Find(name string, attrs map[string]string) []Point {
	var found []Point
	for _, p := range m[name] {
		if matches(p.Attributes, attrs) {
			found = append(found, p)
		}
	}
	return found
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Names returns the metric names recorded, sorted, for a test's failure message.
func (m Metrics) Names() []string {
	var names []string
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func points(data metricdata.Aggregation) []Point {
	var ps []Point
	switch d := data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			ps = append(ps, Point{attributes(p.Attributes), int64(p.Count)}) //nolint:gosec // a test's few points
		}
	case metricdata.Histogram[int64]:
		for _, p := range d.DataPoints {
			ps = append(ps, Point{attributes(p.Attributes), int64(p.Count)}) //nolint:gosec // a test's few points
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			ps = append(ps, Point{attributes(p.Attributes), p.Value})
		}
	case metricdata.Sum[float64]:
		for _, p := range d.DataPoints {
			ps = append(ps, Point{attributes(p.Attributes), int64(p.Value)})
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			ps = append(ps, Point{attributes(p.Attributes), p.Value})
		}
	case metricdata.Gauge[float64]:
		for _, p := range d.DataPoints {
			ps = append(ps, Point{attributes(p.Attributes), int64(p.Value)})
		}
	default:
		panic(fmt.Sprintf("metrictest: %T not supported", data))
	}
	return ps
}

func attributes(set attribute.Set) map[string]string {
	m := map[string]string{}
	for _, kv := range set.ToSlice() {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}
