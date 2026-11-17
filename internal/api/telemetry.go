package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// observeRequests records each request to meters and tracers, as OpenTelemetry's HTTP conventions
// name them: its time (http.server.request.duration, by route, method and status) and a span
// ("GET /tasks/{id}") that the request's queries, commands, outside calls and queued jobs hang
// under. The span holds the route pattern as url.path, never the path itself, which can hold an
// ID, an email or a token. The health check isn't recorded: load balancers call it every few
// seconds. With both off it adds nothing: otelhttp costs a few microseconds a request even
// recording nowhere.
func observeRequests(meters metric.MeterProvider, tracers trace.TracerProvider) func(http.Handler) http.Handler {
	_, metricsOff := meters.(noop.MeterProvider)
	_, tracesOff := tracers.(tracenoop.TracerProvider)
	if metricsOff && tracesOff {
		return func(next http.Handler) http.Handler { return next }
	}
	// A client's traceparent is read only to link it (below). With traces off it isn't read at
	// all: a tracer that records nothing hands the caller's trace on as the request's own, and
	// its IDs would reach our logs and queued jobs.
	var propagator propagation.TextMapPropagator = propagation.TraceContext{}
	if tracesOff {
		propagator = propagation.NewCompositeTextMapPropagator()
	}
	observe := otelhttp.NewMiddleware("http.server",
		otelhttp.WithMeterProvider(meters),
		otelhttp.WithTracerProvider(tracers),
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != healthPath }),
		// Every caller is outside: a trace a client sends (its traceparent header) is linked, never
		// continued, or a client could make us keep every request it sends and choose the trace IDs
		// in our logs. Behind your own services, which do send theirs, drop this.
		otelhttp.WithPropagators(propagator),
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
		// The method until the route is known; then "GET /tasks/{id}" (below).
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return spanMethod(r.Method) }),
		// Fixed: otelhttp otherwise takes server.address and server.port from the request's Host
		// header, which a client can set to anything, making a new series each time. Port 0 leaves
		// server.port out.
		otelhttp.WithServerName("api:0"),
	)
	return func(next http.Handler) http.Handler {
		observed := observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			serveRecordingAbort(next, ww, r)
			labeler, _ := otelhttp.LabelerFromContext(r.Context())
			span := trace.SpanFromContext(r.Context())
			// chi sets the route on a copy of the request that otelhttp never sees, so it's read
			// from chi's routing context, which the copies share. The pattern (/tasks/{id}), never
			// the path: one series per route, not per task.
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route != "" {
				labeler.Add(semconv.HTTPRoute(route))
				span.SetName(spanMethod(r.Method) + " " + route)
				span.SetAttributes(semconv.HTTPRoute(route))
			}
			// Over otelhttp's url.path, the path as sent; empty when no route matched.
			span.SetAttributes(semconv.URLPath(route))
			// The client hung up before any reply: otelhttp would count the 200 net/http assumes.
			// Recorded as 499, as the request log has it; the client is gone, so nothing reaches it.
			if ww.Status() == 0 && r.Context().Err() != nil {
				ww.WriteHeader(statusClientClosed)
			}
			if aborted(r.Context()) {
				labeler.Add(semconv.ErrorTypeKey.String("aborted"))
				span.SetAttributes(semconv.ErrorTypeKey.String("aborted"))
				span.SetStatus(codes.Error, "reply cut off")
			}
		}))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var wasAborted bool
			observed.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), abortKey{}, &wasAborted)))
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

// spanMethod is method if HTTP defines it, else "HTTP": a client can send any method, and each
// would be a span name of its own.
func spanMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	}
	return "HTTP"
}

func aborted(ctx context.Context) bool {
	flag, _ := ctx.Value(abortKey{}).(*bool)
	return flag != nil && *flag
}

// logTraceIDs puts the request's trace and span IDs (trace.id, span.id) on every line logged for
// it, the summary line included, so a trace leads to its logs. Only for a span being recorded: an
// ID with no trace behind it leads nowhere, and a span that isn't ours may carry a caller's IDs.
func logTraceIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
			sc := span.SpanContext()
			hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
				return c.Dict("trace", zerolog.Dict().Str("id", sc.TraceID().String())).
					Dict("span", zerolog.Dict().Str("id", sc.SpanID().String()))
			})
		}
		next.ServeHTTP(w, r)
	})
}
