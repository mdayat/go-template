package handler

import (
	"context"
	"net/http"
	"time"
)

type shutdownKey struct{}

// WithShutdown returns ctx carrying shutdown, which Detach watches: the API's base context, ended
// when the server stops waiting for running requests on SIGTERM.
func WithShutdown(ctx, shutdown context.Context) context.Context {
	return context.WithValue(ctx, shutdownKey{}, shutdown)
}

// Detach returns a context for an endpoint that needs longer than the API's request limit (a large
// upload): not ended when the client leaves or at that limit, but after d, or when the server gives
// up waiting for running requests on shutdown. Not context.WithoutCancel alone: shutdown couldn't
// end it, and its database connection would keep the program from exiting.
func Detach(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), d)
	shutdown, ok := r.Context().Value(shutdownKey{}).(context.Context)
	if !ok {
		return ctx, cancel
	}
	stop := context.AfterFunc(shutdown, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
