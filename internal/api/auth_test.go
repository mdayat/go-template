package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/authtest"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/httpclient"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/user"
)

// protectedRouter returns the shared middleware with one route, /private, behind authenticate,
// trusting cfg's issuer and with users on a database of the test's own, and the buffer the logs
// go to. It answers 204 to whoever gets in. No feature's routes: removing one can't break these
// tests.
func protectedRouter(t *testing.T, cfg jwks.Config) (http.Handler, *pgxpool.Pool, *bytes.Buffer) {
	t.Helper()
	authn, err := jwks.New(t.Context(), cfg, httpclient.New(5*time.Second, tracenoop.NewTracerProvider()), zerolog.Nop())
	if err != nil {
		t.Fatalf("jwks.New() error = %v", err)
	}
	pool := testdb.New(t)
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), nil, nil)
	router.With(authenticate(authn, user.NewService(db.New(pool)), nil, nil)).Get("/private", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return router, pool, &logs
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
	router, _, _ := protectedRouter(t, issuer.Config())

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

// The scheme's name is case-insensitive (RFC 9110). A request signed in reaches the route as our
// user, recorded on the first sign-in, and the log line names that user by our ID only.
func TestRoutesLetInSomeoneSignedIn(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	router, pool, logs := protectedRouter(t, issuer.Config())

	for range 2 {
		if rec := sendWithAuth(t, router, "/private", "bearer "+issuer.Token("alice")); rec.Code != http.StatusNoContent {
			t.Fatalf("GET /private = %d %s, want 204", rec.Code, rec.Body)
		}
	}

	var id string
	if err := pool.QueryRow(t.Context(), "SELECT id FROM users WHERE auth_subject = 'alice'").Scan(&id); err != nil {
		t.Fatalf("alice's user row: %v (want exactly one)", err)
	}
	lines := logLines(t, logs)
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2", len(lines))
	}
	for _, line := range lines {
		if diff := cmp.Diff(map[string]any{"id": id}, line["user"]); diff != "" {
			t.Errorf("log line's user mismatch (-want +got):\n%s", diff)
		}
		if strings.Contains(fmt.Sprint(line), "alice") {
			t.Errorf("log line = %v, want it without the provider's ID for the user", line)
		}
	}
}

// A database that's down while signing someone in is an unexpected failure (500), and the log line
// still never names the provider's ID for them.
func TestSignInWithTheDatabaseDownIs500(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	router, pool, logs := protectedRouter(t, issuer.Config())
	pool.Close()

	rec := sendWithAuth(t, router, "/private", "Bearer "+issuer.Token("alice"))

	want := problemReply{Status: http.StatusInternalServerError, Detail: handler.InternalErrorDetail}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("GET /private reply mismatch (-want +got):\n%s", diff)
	}
	if strings.Contains(logs.String(), "alice") {
		t.Errorf("log = %s, want it without the provider's ID for the user", logs)
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
	router, _, _ := protectedRouter(t, cfg)

	rec := sendWithAuth(t, router, "/private", "Bearer "+issuer.Token("alice"))

	want := problemReply{Status: http.StatusServiceUnavailable, Detail: "sign-in can't be checked right now"}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("GET /private reply mismatch (-want +got):\n%s", diff)
	}
}

// The health check stays open: a load balancer has no token.
func TestHealthCheckNeedsNoSignIn(t *testing.T) {
	t.Parallel()
	router, _, _ := protectedRouter(t, authtest.NewIssuer(t).Config())

	if rec := sendWithAuth(t, router, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}
