package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/testdb"
)

func TestMain(m *testing.M) {
	// pgx returns times in the local zone. A zone other than UTC makes a reply time that wasn't
	// turned into UTC fail a test on every machine, CI (which runs on UTC) included.
	time.Local = time.FixedZone("UTC+7", 7*60*60)
	testdb.Main(m)
}

// testAPI returns the real router and every feature's service, built by NewServices as in the app,
// on a database of the test's own. Every endpoint test uses it.
func testAPI(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	return NewRouter(zerolog.Nop(), NewServices(db.New(pool))), pool
}

// Helpers shared by every feature's endpoint tests live here, not in a feature's test file, so
// removing one feature (the tasks example) can't break another's tests.

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
	Errors []fieldProblem
}

// readProblem returns rec's status and problem body, for the test to compare.
func readProblem(t *testing.T, rec *httptest.ResponseRecorder) problemReply {
	t.Helper()
	var p problem
	decodeReply(t, rec, &p)
	return problemReply{Status: rec.Code, Detail: p.Detail, Errors: p.Errors}
}

// cursorOf encodes raw the way real cursors are encoded, so only its content is wrong.
func cursorOf(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}
