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

	"github.com/mdayat/go-template/internal/task"
)

const (
	healthPath = "/healthz"

	// requestTimeout is how long a request's context lives. The server's WriteTimeout (cmd/api) cuts
	// a slow reply's connection but doesn't cancel its context, so without this a query stuck on a
	// lock runs on and holds its database connection. Kept below WriteTimeout (10 s) so the 503
	// reply still reaches the client.
	requestTimeout = 8 * time.Second
)

// NewRouter returns the API's routes, each request logged to logger.
func NewRouter(logger zerolog.Logger, tasks *task.Service) *chi.Mux {
	router := newBaseRouter(logger)
	taskRoutes{tasks: tasks, decoder: newBodyDecoder()}.mount(router)
	return router
}

// newBaseRouter returns a router with the middleware every route shares, the health check, and
// problem replies for unmatched paths and methods. Features add their routes to it.
func newBaseRouter(logger zerolog.Logger) *chi.Mux {
	router := chi.NewRouter()
	router.Use(
		hlog.NewHandler(logger),
		// A fresh ID per request, never one sent by the client, which could be forged.
		hlog.RequestIDHandler("request_id", "X-Request-Id"),
		logRequests,
		// After logRequests: its own context has no deadline, so a timed-out request is logged as
		// the 503 it got, not as a client that hung up (499).
		limitRequestTime(requestTimeout),
		// HTTP requires HEAD wherever GET works; uptime checkers and load balancers use it.
		middleware.GetHead,
	)
	// Load balancers and the deploy platform decide from this route whether the app is up.
	router.Get(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.NotFound(notFound)
	router.MethodNotAllowed(methodNotAllowed(router))
	return router
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
