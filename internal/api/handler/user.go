package handler

import (
	"context"
	"net/http"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/user"
)

// userKey holds the signed-in user in a request's context.
type userKey struct{}

// SignedIn returns r carrying u as who signed in to make it, and puts u on the request's summary
// log line. The router's sign-in check calls it.
func SignedIn(r *http.Request, u user.User) *http.Request {
	recordUser(r, u)
	return r.WithContext(context.WithValue(r.Context(), userKey{}, u))
}

// CurrentUser returns who signed in to make r, for handlers to pass to services. Only routes behind
// the sign-in check have one: calling it elsewhere is a bug, and panics.
func CurrentUser(r *http.Request) user.User {
	u, ok := r.Context().Value(userKey{}).(user.User)
	if !ok {
		panic("CurrentUser: the route isn't behind the sign-in check")
	}
	return u
}

// recordUser puts who made the request on its summary log line: our user ID only, never the
// provider's ID for them or anything else about them. A request without its own logger has no
// summary line, so there's nothing to add to (see recordError).
func recordUser(r *http.Request, u user.User) {
	logger := zerolog.Ctx(r.Context())
	if logger == zerolog.DefaultContextLogger {
		return
	}
	logger.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Dict("user", zerolog.Dict().Str("id", u.ID.String()))
	})
}
