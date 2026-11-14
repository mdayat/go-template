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

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/task"
)

const (
	healthPath = "/healthz"

	// requestTimeout is how long a request's context lives. The server's WriteTimeout (cmd/api) cuts
	// a slow reply's connection but doesn't cancel its context, so without this a query stuck on a
	// lock runs on and holds its database connection. Kept below WriteTimeout (10 s) so the 503
	// reply still reaches the client.
	requestTimeout = 8 * time.Second

	// defaultPageSize is a list's page size when the request has no ?limit=.
	defaultPageSize = 20
)

// remove:worker
// jobResponse is the reply of every endpoint that queues a job (202 Accepted): the job's ID, for
// finding it in the worker's logs.
type jobResponse struct {
	JobID string `json:"job_id"`
}

// remove:end

// Services holds what the routes call, one field per feature. A new feature adds its field here and
// builds it in NewServices.
type Services struct {
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
		Tasks: task.NewService(queries),
		// remove:worker
		Jobs: jobs,
		// remove:end
	}
}

// NewRouter returns the API's routes, each request logged to logger.
func NewRouter(logger zerolog.Logger, services Services) *chi.Mux {
	router := newBaseRouter(logger)
	// One decoder for every feature: the validator caches what it learns about each type.
	decoder := newBodyDecoder()
	taskRoutes{
		tasks: services.Tasks,
		// remove:worker
		jobs: services.Jobs,
		// remove:end
		decoder: decoder,
	}.mount(router)
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
