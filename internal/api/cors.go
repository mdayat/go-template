package api

import (
	"net/http"

	"github.com/rs/cors"
)

// corsMaxAge is how long, in seconds, a browser may reuse a preflight's answer instead of asking
// again before each request: two hours, the most Chrome allows.
const corsMaxAge = 2 * 60 * 60

// allowOrigins lets web pages from origins (sites: https://app.example.com) call the API from a
// browser, which otherwise refuses them the reply: CORS. It answers the browser's preflight (an
// OPTIONS request asking first) itself, and marks every reply, error replies included, so the page
// can read them. No origins: nothing is added, and browsers keep pages from other sites out.
//
// Sign-in is a bearer token, not a cookie, so credentials aren't allowed: a page can't make the
// browser send a cookie of the user's. Sign-in by cookie would need them allowed, and protection
// against requests forged from other sites (CSRF).
//
// Not needed: leave CORS_ALLOWED_ORIGINS unset. To take the code out too: delete this file and
// cors_test.go, fix what no longer compiles (the allowedOrigins argument, readOrigins in cmd/api),
// then go mod tidy.
func allowOrigins(origins []string) func(http.Handler) http.Handler {
	if len(origins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return cors.New(cors.Options{
		AllowedOrigins: origins,
		AllowedMethods: []string{
			http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		},
		// A header a page sends that isn't here fails its preflight: add it (a browser tracing
		// library's traceparent, say).
		AllowedHeaders: []string{"Authorization", "Content-Type"},
		// What a page may read besides the basic headers: where a created row is, the request's ID
		// for a support ticket, and the rate limits, to wait before trying again.
		ExposedHeaders: []string{
			"Location", "X-Request-Id", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset",
			"Retry-After",
		},
		MaxAge:               corsMaxAge,
		OptionsSuccessStatus: http.StatusNoContent,
	}).Handler
}
