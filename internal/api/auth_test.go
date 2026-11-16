package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/authtest"
	"github.com/mdayat/go-template/internal/httpclient"
)

// protectedRouter returns the shared middleware with one route, /private, behind authenticate,
// trusting cfg's issuer. It answers 204 to whoever gets in. No feature's routes: removing one can't
// break these tests.
func protectedRouter(t *testing.T, cfg jwks.Config) http.Handler {
	t.Helper()
	authn, err := jwks.New(t.Context(), cfg, httpclient.New(5*time.Second), zerolog.Nop())
	if err != nil {
		t.Fatalf("jwks.New() error = %v", err)
	}
	router := newBaseRouter(zerolog.Nop())
	router.With(authenticate(authn)).Get("/private", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return router
}

// sendWithAuth sends a GET with the given Authorization header ("" for none).
func sendWithAuth(t *testing.T, router http.Handler, target, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Without a token the provider accepts, a route answers 401 and asks for a Bearer token, with the
// same detail whatever was wrong (the reason is in the log line, not the reply).
func TestRoutesNeedSomeoneSignedIn(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	router := protectedRouter(t, issuer.Config())

	tests := []struct {
		name          string
		authorization string
	}{
		{"no token", ""},
		{"another scheme", "Basic dXNlcjpwYXNz"},
		{"an empty token", "Bearer "},
		{"a token the provider refuses", "Bearer " + issuer.Token("alice", authtest.Expired)},
	}
	for _, tt := range tests {
		rec := sendWithAuth(t, router, "/private", tt.authorization)

		want := problemReply{Status: http.StatusUnauthorized, Detail: signInRequired}
		if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
			t.Errorf("%s: GET /private reply mismatch (-want +got):\n%s", tt.name, diff)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s: WWW-Authenticate = %q, want Bearer", tt.name, got)
		}
	}
}

// The scheme's name is case-insensitive (RFC 9110), and a request signed in reaches the route.
func TestRoutesLetInSomeoneSignedIn(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	router := protectedRouter(t, issuer.Config())

	rec := sendWithAuth(t, router, "/private", "bearer "+issuer.Token("alice"))

	if rec.Code != http.StatusNoContent {
		t.Errorf("GET /private = %d %s, want 204", rec.Code, rec.Body)
	}
}

// When the provider's keys never arrived, it's our outage, not the caller's mistake: 503, which a
// client may retry, not 401.
func TestRoutesAnswer503WhenSignInCantBeChecked(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	down := httptest.NewServer(nil)
	down.Close()
	cfg := issuer.Config()
	cfg.KeysURL = down.URL
	router := protectedRouter(t, cfg)

	rec := sendWithAuth(t, router, "/private", "Bearer "+issuer.Token("alice"))

	want := problemReply{Status: http.StatusServiceUnavailable, Detail: "sign-in can't be checked right now"}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("GET /private reply mismatch (-want +got):\n%s", diff)
	}
}

// The health check stays open: a load balancer has no token.
func TestHealthCheckNeedsNoSignIn(t *testing.T) {
	t.Parallel()
	router := protectedRouter(t, authtest.NewIssuer(t).Config())

	if rec := sendWithAuth(t, router, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}
