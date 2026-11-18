// Package api is the HTTP layer: routes, middleware and handlers.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mdayat/go-template/internal/api/admin"
	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/api/mobile"
	"github.com/mdayat/go-template/internal/api/web"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/task"
	"github.com/mdayat/go-template/internal/user"
)

const healthPath = "/healthz"

// RequestTimeout is how long a request's context lives. The server's WriteTimeout (cmd/api) cuts a
// slow reply's connection but doesn't cancel its context, so without this a query stuck on a lock
// runs on and holds its database connection. Kept below WriteTimeout (10 s) so the 503 reply still
// reaches the client. Exported for cmd/api, whose shutdown wait must not be shorter.
const RequestTimeout = 8 * time.Second

// Services holds what the routes call, one field per feature. A new feature adds its field here and
// builds it in NewServices.
type Services struct {
	// Users turns whoever signed in into our user (see authenticate).
	Users *user.Service
	Tasks *task.Service
	// remove:worker
	// Jobs queues background jobs for the worker (cmd/worker).
	Jobs queue.Enqueuer
	// remove:end
}

// NewServices builds every feature's service on queries. The app (cmd/api) and the endpoint tests
// (testAPI) both get their services here, so a feature can't be wired into one and forgotten in the
// other: its tests would pass while the app served it with a nil service.
func NewServices(
	queries *db.Queries,
	// remove:worker
	jobs queue.Enqueuer,
	// remove:end
) Services {
	return Services{
		Users: user.NewService(queries),
		Tasks: task.NewService(queries),
		// remove:worker
		Jobs: jobs,
		// remove:end
	}
}

// NewRouter returns the API's routes, each request logged to logger, measured to meters and traced
// to tracers. Every route but the health and readiness checks needs someone signed in, as authn
// says, and is rate limited as limits says. /readyz answers as ready says.
//
// Each client has its routes under its own prefix, in its own package, so one client's replies can
// change without touching another's (docs/standards/http-api.md): /mobile/v1 (internal/api/mobile:
// only grows, old app versions call it for years), /web (internal/api/web: changes with the web
// app) and /admin (internal/api/admin: admins only). All of them call the same services.
func NewRouter(
	logger zerolog.Logger, meters metric.MeterProvider, tracers trace.TracerProvider,
	services Services, ready *Readiness, authn Authenticator, limits Limits,
) *chi.Mux {
	router := newBaseRouter(logger, meters, tracers, limits.TrustedProxies)
	router.Get(readyPath, ready.serve)
	// One decoder for every client: the validator caches what it learns about each type.
	decoder := handler.NewDecoder()
	// The same check and counts for every client (one per-user limit across them all). In an inline
	// group inside each prefix, so they run once chi has matched the route: a refused request (401,
	// 429, 403) still carries its route ("/web/tasks/{id}") in logs, metrics and traces, and a path
	// no route matches is a plain 404 that needs no sign-in, like any other.
	signedIn := []func(http.Handler) http.Handler{
		authenticate(authn, services.Users, signedOutLimit(limits.PerIP, limits.Limiter), signedInLimit(limits.PerUser, limits.Limiter)),
	}
	router.Route("/mobile/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(signedIn...)
			mobile.Tasks{Tasks: services.Tasks, Decoder: decoder}.Mount(r)
		})
	})
	router.Route("/web", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(signedIn...)
			web.Tasks{Tasks: services.Tasks, Decoder: decoder}.Mount(r)
		})
	})
	router.Route("/admin", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(signedIn...)
			r.Use(admin.RequireAdmin)
			// remove:worker
			admin.Purges{Jobs: services.Jobs, Decoder: decoder}.Mount(r)
			// remove:end
		})
	})
	return router
}

// newBaseRouter returns a router with the middleware every route shares, the health check, and
// problem replies for unmatched paths and methods. Features add their routes to it. The client's
// IP is read as Limits.TrustedProxies says.
func newBaseRouter(
	logger zerolog.Logger, meters metric.MeterProvider, tracers trace.TracerProvider, trustedProxies []string,
) *chi.Mux {
	router := chi.NewRouter()
	router.Use(
		// First, so a request's time and span include all the rest.
		observeRequests(meters, tracers),
		hlog.NewHandler(logger),
		logTraceIDs,
		// Before logRequests, which logs the client's IP from the request this one passes on.
		clientIP(trustedProxies, logger),
		// A fresh ID per request, never one sent by the client, which could be forged.
		hlog.RequestIDHandler("request_id", "X-Request-Id"),
		noStore,
		logRequests,
		// After logRequests: its own context has no deadline, so a timed-out request is logged as
		// the 503 it got, not as a client that hung up (499).
		limitRequestTime(RequestTimeout),
		// HTTP requires HEAD wherever GET works; uptime checkers and load balancers use it.
		middleware.GetHead,
	)
	// The platform restarts the process if this doesn't answer (load balancers use /readyz).
	router.Get(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.NotFound(handler.NotFound)
	router.MethodNotAllowed(handler.MethodNotAllowed(router))
	return router
}

// noStore marks every reply as not to be kept by a cache: a proxy or CDN between the app and the
// API could otherwise keep one user's reply and hand it to another. A handler whose reply may be
// cached sets its own Cache-Control.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// limitRequestTime cancels each request's context after d, which stops its queries.
func limitRequestTime(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
