// Package api is the HTTP layer: routes, middleware and handlers.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/mdayat/go-template/internal/task"
)

const healthPath = "/healthz"

// NewRouter returns the API's routes, each request logged to logger.
func NewRouter(logger zerolog.Logger, tasks *task.Service) *chi.Mux {
	router := newBaseRouter(logger)
	router.Get(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	taskRoutes{tasks: tasks, decoder: newBodyDecoder()}.mount(router)
	return router
}

// newBaseRouter returns a router with the middleware every route shares, problem replies for
// unmatched paths and methods, and no routes yet.
func newBaseRouter(logger zerolog.Logger) *chi.Mux {
	router := chi.NewRouter()
	router.Use(
		hlog.NewHandler(logger),
		// A fresh ID per request, never one sent by the client, which could be forged.
		hlog.RequestIDHandler("request_id", "X-Request-Id"),
		logRequests,
	)
	router.NotFound(notFound)
	router.MethodNotAllowed(methodNotAllowed(router))
	return router
}
