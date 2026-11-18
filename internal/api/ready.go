package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/api/handler"
)

// readyPath says whether this instance (one running copy of the API) should get requests: a load
// balancer sends requests only to instances whose /readyz answers 200. /healthz only says the
// process runs, and the platform restarts an instance that stops answering it: if it checked the
// database too, a database outage would restart every instance, which fixes nothing.
const readyPath = "/readyz"

// readyTimeout is how long /readyz waits for the database; load balancers wait a few seconds for a
// reply and count no reply as a failure.
const readyTimeout = time.Second

// Readiness is what /readyz answers: ready while its check (a database ping) passes, until Drain.
// Valkey isn't checked: the API works without it (rate limits count in memory, and only requests
// with an Idempotency-Key fail), so taking every instance out when it's down would make things
// worse.
type Readiness struct {
	check    func(context.Context) error
	logger   zerolog.Logger
	draining atomic.Bool
	failing  atomic.Bool
}

// NewReadiness returns a Readiness that runs check for each /readyz and logs to logger when the
// answer changes.
func NewReadiness(check func(context.Context) error, logger zerolog.Logger) *Readiness {
	return &Readiness{check: check, logger: logger}
}

// Drain makes /readyz answer 503 from now on, so load balancers stop sending this instance new
// requests before it shuts down.
func (rd *Readiness) Drain() {
	rd.draining.Store(true)
}

// serve answers /readyz. Load balancers ask every few seconds, so the request log leaves it out
// (see logRequests); a change between ready and not ready is logged here instead.
func (rd *Readiness) serve(w http.ResponseWriter, r *http.Request) {
	if rd.draining.Load() {
		handler.WriteProblem(w, r, handler.NewProblem(http.StatusServiceUnavailable, "shutting down"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	err := rd.check(ctx)
	switch {
	case err != nil && r.Context().Err() != nil:
		// The caller hung up: that says nothing about the database.
		return
	case err != nil && !rd.failing.Swap(true):
		rd.logger.Warn().Err(err).Msg("not ready: the database didn't answer")
	case err == nil && rd.failing.Swap(false):
		rd.logger.Info().Msg("ready again: the database answers")
	}
	if err != nil {
		handler.WriteProblem(w, r, handler.NewProblem(http.StatusServiceUnavailable, "the database didn't answer"))
		return
	}
	w.WriteHeader(http.StatusOK)
}
