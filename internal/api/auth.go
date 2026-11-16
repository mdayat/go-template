package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/auth"
	"github.com/mdayat/go-template/internal/user"
)

// Authenticator says who a request's token belongs to: internal/auth/jwks, or another provider's
// package with the same method; cmd/api picks it. A token it refuses is an auth.ErrInvalidToken;
// any other error means it couldn't check tokens at all.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (auth.Identity, error)
}

// The same detail whatever went wrong with the token: saying "expired" or "bad signature" would
// help someone forging one. The log line has the reason.
const signInRequired = "a valid sign-in token is required"

// authenticate lets through only requests carrying a token authn accepts, sent as
// "Authorization: Bearer <token>", and turns who it is into our user through users, recording them
// on their first sign-in. Anything else gets a 401, or a 503 when authn can't check tokens at all:
// an outage on our side, which a client may retry. Each 401 counts against signedOut, which replies
// 429 instead once the client is past it.
func authenticate(authn Authenticator, users *user.Service, signedOut *rateLimit) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := identify(r, authn)
			if err != nil {
				if e, ok := errors.AsType[*apperr.Error](err); ok && e.Kind == apperr.KindUnauthenticated {
					if !signedOut.check(w, r) {
						return
					}
					// Tells the client which kind of credentials to send (RFC 6750).
					w.Header().Set("WWW-Authenticate", "Bearer")
				}
				writeError(w, r, err)
				return
			}
			u, err := users.SignIn(r.Context(), id.Subject)
			if err != nil {
				writeError(w, r, err)
				return
			}
			recordUser(r, u)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
		})
	}
}

// userKey holds the signed-in user in a request's context.
type userKey struct{}

// currentUser returns who signed in to make r, for handlers to pass to services. Only routes behind
// authenticate have one: calling it elsewhere is a bug, and panics.
func currentUser(r *http.Request) user.User {
	u, ok := r.Context().Value(userKey{}).(user.User)
	if !ok {
		panic("currentUser: the route isn't behind authenticate")
	}
	return u
}

func identify(r *http.Request, authn Authenticator) (auth.Identity, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return auth.Identity{}, fmt.Errorf("authenticate: no bearer token: %w", apperr.Unauthenticated(signInRequired))
	}
	id, err := authn.Authenticate(r.Context(), token)
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		return auth.Identity{}, fmt.Errorf("authenticate: %w: %w", err, apperr.Unauthenticated(signInRequired))
	case err != nil:
		return auth.Identity{}, fmt.Errorf("authenticate: %w: %w", err,
			&statusError{status: http.StatusServiceUnavailable, detail: "sign-in can't be checked right now"})
	}
	return id, nil
}
