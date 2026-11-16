package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// measureRequests records each request's time to meters (http.server.request.duration, with the
// route, method and status), as OpenTelemetry's HTTP conventions name it. The health check isn't
// measured: load balancers call it every few seconds. With metrics off it adds nothing: otelhttp
// costs a few microseconds a request even recording nowhere.
func measureRequests(meters metric.MeterProvider) func(http.Handler) http.Handler {
	if _, off := meters.(noop.MeterProvider); off {
		return func(next http.Handler) http.Handler { return next }
	}
	measure := otelhttp.NewMiddleware("http.server",
		otelhttp.WithMeterProvider(meters),
		otelhttp.WithTracerProvider(tracenoop.NewTracerProvider()), // no traces: metrics only
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != healthPath }),
		// Fixed: otelhttp otherwise takes server.address and server.port from the request's Host
		// header, which a client can set to anything, making a new series each time. Port 0 leaves
		// server.port out.
		otelhttp.WithServerName("api:0"),
	)
	return func(next http.Handler) http.Handler {
		measured := measure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			serveRecordingAbort(next, ww, r)
			labeler, _ := otelhttp.LabelerFromContext(r.Context())
			// chi sets the route on a copy of the request that otelhttp never sees, so it's read
			// from chi's routing context, which the copies share. The pattern (/tasks/{id}), never
			// the path: one series per route, not per task.
			if route := chi.RouteContext(r.Context()).RoutePattern(); route != "" {
				labeler.Add(semconv.HTTPRoute(route))
			}
			// The client hung up before any reply: otelhttp would count the 200 net/http assumes.
			// Recorded as 499, as the request log has it; the client is gone, so nothing reaches it.
			if ww.Status() == 0 && r.Context().Err() != nil {
				ww.WriteHeader(statusClientClosed)
			}
			if aborted(r.Context()) {
				labeler.Add(semconv.ErrorTypeKey.String("aborted"))
			}
		}))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var wasAborted bool
			measured.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), abortKey{}, &wasAborted)))
			if wasAborted {
				panic(http.ErrAbortHandler) // as next did, now that otelhttp has recorded the request
			}
		})
	}
}

// abortKey holds, in a request's context, where serveRecordingAbort marks the request cut off.
type abortKey struct{}

// serveRecordingAbort serves r, catching http.ErrAbortHandler (logRequests raises it to cut off a
// reply a panic left half sent): otelhttp records a request only when its handler returns, so the
// abort is marked in r's context and raised again after otelhttp has recorded the request.
func serveRecordingAbort(next http.Handler, w http.ResponseWriter, r *http.Request) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		err, isErr := p.(error)
		flag, _ := r.Context().Value(abortKey{}).(*bool)
		if !isErr || !errors.Is(err, http.ErrAbortHandler) || flag == nil {
			panic(p)
		}
		*flag = true
	}()
	next.ServeHTTP(w, r)
}

func aborted(ctx context.Context) bool {
	flag, _ := ctx.Value(abortKey{}).(*bool)
	return flag != nil && *flag
}
