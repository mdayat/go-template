package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/authtest"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/httpclient"
	idempotencyvalkey "github.com/mdayat/go-template/internal/idempotency/valkey"
	"github.com/mdayat/go-template/internal/queue/valkey"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/testredis"
)

func TestMain(m *testing.M) {
	// pgx returns times in the local zone. A zone other than UTC makes a reply time that wasn't
	// turned into UTC fail a test on every machine, CI (which runs on UTC) included.
	time.Local = time.FixedZone("UTC+7", 7*60*60)
	testdb.Main(m)
}

// testAPI returns the real router and every feature's service, built by NewServices as in the app,
// on a database of the test's own. Every endpoint test uses it. Requests are signed in as
// testSubject unless they carry their own Authorization header, through the real token check. No
// rate limits: ratelimit_test.go has those.
func testAPI(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	router, pool, issuer := testAPIWith(t, Limits{})
	return signedIn(router, issuer.Token(testSubject)), pool
}

// testAPIWith is testAPI with limits, and without signing requests in: each needs a token from
// issuer.
func testAPIWith(t *testing.T, limits Limits) (http.Handler, *pgxpool.Pool, *authtest.Issuer) {
	t.Helper()
	return testAPIFor(t, limits, nil)
}

// testAPIFor is testAPIWith with web pages from allowedOrigins allowed to call it.
func testAPIFor(t *testing.T, limits Limits, allowedOrigins []string) (http.Handler, *pgxpool.Pool, *authtest.Issuer) {
	t.Helper()
	pool := testdb.New(t)
	services := NewServices(
		db.New(pool),
		testReplies(t),
		// remove:worker
		valkey.NewEnqueuer(testredis.New(t), tracenoop.NewTracerProvider()), // the test job queue
		// remove:end
	)
	authn, issuer := testAuthenticator(t)
	return NewRouter(zerolog.Nop(), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), services, NewReadiness(pool.Ping, zerolog.Nop()), authn, limits, allowedOrigins), pool, issuer
}

// testSubject is who testAPI's requests are signed in as.
const testSubject = "test-user"

// testUserID returns the ID of testSubject's user, recording them first if needed, without
// changing whether they're an admin.
func testUserID(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(t.Context(),
		`INSERT INTO users (auth_subject) VALUES ($1)
		 ON CONFLICT (auth_subject) DO UPDATE SET auth_subject = EXCLUDED.auth_subject
		 RETURNING id`, testSubject).Scan(&id)
	if err != nil {
		t.Fatalf("seed test user: %v", err)
	}
	return id
}

// testUser records testSubject as a user straight in the database, an admin or not, and returns
// their ID: the same user their requests then act as.
func testUser(t *testing.T, pool *pgxpool.Pool, admin bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(t.Context(),
		`INSERT INTO users (auth_subject, is_admin) VALUES ($1, $2)
		 ON CONFLICT (auth_subject) DO UPDATE SET is_admin = EXCLUDED.is_admin
		 RETURNING id`, testSubject, admin).Scan(&id)
	if err != nil {
		t.Fatalf("seed test user: %v", err)
	}
	return id
}

// testAuthenticator returns the real token check (internal/auth/jwks), trusting a test issuer of
// the test's own.
func testAuthenticator(t *testing.T) (*jwks.Authenticator, *authtest.Issuer) {
	t.Helper()
	issuer := authtest.NewIssuer(t)
	authn, err := jwks.New(t.Context(), issuer.Config(), httpclient.New(5*time.Second, tracenoop.NewTracerProvider()), zerolog.Nop())
	if err != nil {
		t.Fatalf("jwks.New() error = %v", err)
	}
	return authn, issuer
}

// signedIn sends each request with token unless the request already has an Authorization header.
func signedIn(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		next.ServeHTTP(w, r)
	})
}

// Helpers shared by every feature's endpoint tests live here, not in a feature's test file, so
// removing one feature (the tasks example) can't break another's tests. Two tests of the router
// itself need real routes and use the example's: TestClientPrefixes and limitedRoute.

// seedTime is when seeded rows were made: in the past, so a change made by a test moves updated_at.
var seedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// send makes a request; a non-empty body is sent as JSON.
func send(t *testing.T, router http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeReply(t *testing.T, rec *httptest.ResponseRecorder, reply any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), reply); err != nil {
		t.Fatalf("reply is not JSON: %q: %v", rec.Body, err)
	}
}

// problemReply is the part of an error reply a test checks: the status and the problem's fields.
type problemReply struct {
	Status int
	Detail string
	Errors []handler.FieldProblem
}

// readProblem returns rec's status and problem body, for the test to compare.
func readProblem(t *testing.T, rec *httptest.ResponseRecorder) problemReply {
	t.Helper()
	var p handler.Problem
	decodeReply(t, rec, &p)
	return problemReply{Status: rec.Code, Detail: p.Detail, Errors: p.Errors}
}

// cursorOf encodes raw the way real cursors are encoded, so only its content is wrong.
func cursorOf(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// testReplies keeps the test's idempotency keys in the test Valkey. Keys are per user, and every
// test's users are new, so tests don't share them.
func testReplies(t *testing.T) *idempotencyvalkey.Store {
	t.Helper()
	return idempotencyvalkey.New(testredis.New(t), "test:", time.Second)
}
