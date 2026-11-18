package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/api/handler"
	idempotencyvalkey "github.com/mdayat/go-template/internal/idempotency/valkey"
	"github.com/mdayat/go-template/internal/testredis"
	"github.com/mdayat/go-template/internal/user"
)

// sendKeyed sends a POST of body to target with an Idempotency-Key, as testAPI's user unless
// token says otherwise.
func sendKeyed(t *testing.T, router http.Handler, target, key, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyHeader, key)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func newKey() string { return "key-" + rand.Text() }

func countTasks(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM tasks").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A retried create gets the first reply back, marked as a replay, and makes no second row; the same
// key on another request is refused.
func TestIdempotentCreate(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	key := newKey()

	first := sendKeyed(t, router, "/web/tasks", key, `{"title":"buy milk"}`, "")
	if first.Code != http.StatusCreated || first.Header().Get(replayedHeader) != "" {
		t.Fatalf("first try = %d, replayed %q; want 201, not replayed", first.Code, first.Header().Get(replayedHeader))
	}
	retry := sendKeyed(t, router, "/web/tasks", key, `{"title":"buy milk"}`, "")
	if retry.Code != http.StatusCreated || retry.Header().Get(replayedHeader) != "true" {
		t.Fatalf("retry = %d, replayed %q; want 201, replayed", retry.Code, retry.Header().Get(replayedHeader))
	}
	for _, h := range []string{"Content-Type", "Location", "X-Content-Type-Options"} {
		if retry.Header().Get(h) != first.Header().Get(h) {
			t.Errorf("retry %s = %q, want the first try's %q", h, retry.Header().Get(h), first.Header().Get(h))
		}
	}
	if retry.Body.String() != first.Body.String() {
		t.Errorf("retry body = %s, want the first try's %s", retry.Body, first.Body)
	}
	if n := countTasks(t, pool); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}

	other := sendKeyed(t, router, "/web/tasks", key, `{"title":"buy eggs"}`, "")
	want := problemReply{Status: http.StatusUnprocessableEntity, Detail: "this Idempotency-Key was used for a different request; use a new key for a new action"}
	if diff := cmp.Diff(want, readProblem(t, other)); diff != "" {
		t.Errorf("same key, another body (-want +got):\n%s", diff)
	}
	if other := sendKeyed(t, router, "/mobile/v1/tasks", key, `{"title":"buy milk"}`, ""); other.Code != http.StatusUnprocessableEntity {
		t.Errorf("same key and body on another path = %d, want 422", other.Code)
	}
	if n := countTasks(t, pool); n != 1 {
		t.Errorf("tasks after reusing the key = %d, want 1", n)
	}
}

// A 4xx is the same for the same request, so it's kept too; without a key nothing changes.
func TestIdempotentKeepsClientErrorsAndSkipsUnkeyed(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	key := newKey()

	first := sendKeyed(t, router, "/web/tasks", key, `{"title":""}`, "")
	retry := sendKeyed(t, router, "/web/tasks", key, `{"title":""}`, "")
	if first.Code != http.StatusUnprocessableEntity || retry.Code != http.StatusUnprocessableEntity || retry.Header().Get(replayedHeader) != "true" {
		t.Errorf("invalid body twice = %d then %d (replayed %q), want 422 then a replayed 422", first.Code, retry.Code, retry.Header().Get(replayedHeader))
	}

	plain := func() int { return send(t, router, http.MethodPost, "/web/tasks", `{"title":"no key"}`).Code }
	if a, b := plain(), plain(); a != http.StatusCreated || b != http.StatusConflict {
		t.Errorf("the same create twice without a key = %d, %d; want 201 then the duplicate title's 409", a, b)
	}
	if n := countTasks(t, pool); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

// A key is the signed-in user's: another user's same key is a request of its own.
func TestIdempotencyKeysArePerUser(t *testing.T) {
	t.Parallel()
	router, pool, issuer := testAPIWith(t, Limits{})
	key := newKey()
	for _, subject := range []string{"user-a", "user-b"} {
		rec := sendKeyed(t, router, "/web/tasks", key, `{"title":"same title"}`, issuer.Token(subject))
		if rec.Code != http.StatusCreated || rec.Header().Get(replayedHeader) != "" {
			t.Errorf("%s with the shared key = %d, replayed %q; want a fresh 201", subject, rec.Code, rec.Header().Get(replayedHeader))
		}
	}
	if n := countTasks(t, pool); n != 2 {
		t.Errorf("tasks = %d, want 2", n)
	}
}

func TestIdempotencyKeyMustBeValid(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	for _, key := range []string{strings.Repeat("k", 256), "has space", "tab\tkey", "ünïcode"} {
		rec := sendKeyed(t, router, "/web/tasks", key, `{"title":"x"}`, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("key %q = %d, want 400", key, rec.Code)
		}
	}
	if rec := sendKeyed(t, router, "/web/tasks", strings.Repeat("k", 255), `{"title":"x"}`, ""); rec.Code != http.StatusCreated {
		t.Errorf("a 255-character key = %d, want 201", rec.Code)
	}
	if n := countTasks(t, pool); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

// keyedRouter is a base router, logging to logs, whose routes are behind keepReplies, signed in as
// one user. runs counts the runs of the routes that count them.
type keyedRouter struct {
	http.Handler
	routes chi.Router
	runs   atomic.Int32
	logs   bytes.Buffer
}

func newKeyedRouter(t *testing.T, client *redis.Client) *keyedRouter {
	t.Helper()
	k := &keyedRouter{}
	base := newBaseRouter(zerolog.New(&k.logs), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), nil, nil)
	u := user.User{ID: uuid.New()}
	routes := base.With(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, handler.SignedIn(r, u)) })
		},
		keepReplies(idempotencyvalkey.New(client, "test:", time.Second)),
	)
	k.Handler, k.routes = base, routes
	return k
}

// A request that failed on our side, or panicked, frees its key: the retry runs again.
func TestIdempotencyRetriesAfterServerErrors(t *testing.T) {
	t.Parallel()
	router := newKeyedRouter(t, testredis.New(t))
	runs := &router.runs
	router.routes.Post("/flaky", func(w http.ResponseWriter, _ *http.Request) {
		switch runs.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			panic("boom")
		default:
			w.WriteHeader(http.StatusCreated)
		}
	})
	key := newKey()
	var codes []int
	for range 4 {
		codes = append(codes, sendKeyed(t, router, "/flaky", key, `{}`, "").Code)
	}
	if want := []int{500, 500, 201, 201}; !cmp.Equal(codes, want) || runs.Load() != 3 {
		t.Errorf("tries = %v with %d runs, want %v with 3 (the last a replay)", codes, runs.Load(), want)
	}
}

// While a request runs, a retry with its key is told to wait (409); once it's done, the retry gets
// its reply.
func TestIdempotencyWhileRunning(t *testing.T) {
	t.Parallel()
	router := newKeyedRouter(t, testredis.New(t))
	runs := &router.runs
	started, release := make(chan struct{}), make(chan struct{})
	router.routes.Post("/slow", func(w http.ResponseWriter, _ *http.Request) {
		runs.Add(1)
		close(started)
		<-release
		w.WriteHeader(http.StatusCreated)
	})
	key := newKey()
	done := make(chan int)
	go func() { done <- sendKeyed(t, router, "/slow", key, `{}`, "").Code }()
	<-started

	retry := sendKeyed(t, router, "/slow", key, `{}`, "")
	if retry.Code != http.StatusConflict || retry.Header().Get("Retry-After") != "1" {
		t.Errorf("retry while running = %d, Retry-After %q; want 409, 1", retry.Code, retry.Header().Get("Retry-After"))
	}
	close(release)
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("first try = %d, want 201", code)
	}
	if later := sendKeyed(t, router, "/slow", key, `{}`, ""); later.Code != http.StatusCreated || later.Header().Get(replayedHeader) != "true" || runs.Load() != 1 {
		t.Errorf("retry after = %d replayed %q with %d runs, want a replayed 201 and 1 run", later.Code, later.Header().Get(replayedHeader), runs.Load())
	}
}

// With Valkey down a keyed request isn't run unguarded: 503, to try again. Without a key it runs.
func TestIdempotencyValkeyDown(t *testing.T) {
	t.Parallel()
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1}) // nothing listens
	t.Cleanup(func() { _ = down.Close() })
	router := newKeyedRouter(t, down)
	runs := &router.runs
	router.routes.Post("/create", func(w http.ResponseWriter, _ *http.Request) {
		runs.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	rec := sendKeyed(t, router, "/create", newKey(), `{}`, "")
	want := problemReply{Status: http.StatusServiceUnavailable, Detail: "the Idempotency-Key can't be checked right now; try again"}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" || runs.Load() != 0 {
		t.Errorf("keyed request with Valkey down (-want +got):\n%s\nruns = %d, want 0", diff, runs.Load())
	}
	if code := send(t, router, http.MethodPost, "/create", `{}`).Code; code != http.StatusCreated {
		t.Errorf("unkeyed request with Valkey down = %d, want 201", code)
	}
}

// A client that hangs up before any reply frees its key: the retry runs. One that hangs up after
// the handler replied doesn't: the work may be saved, so the retry gets that reply. A reply too
// large to keep never runs twice either: the retry is told it ran. A replay is marked on its log line.
func TestIdempotencyAfterCutOffsAndLargeReplies(t *testing.T) {
	t.Parallel()
	router := newKeyedRouter(t, testredis.New(t))
	router.routes.Post("/gone", func(w http.ResponseWriter, r *http.Request) {
		if router.runs.Add(1) == 1 {
			<-r.Context().Done() // the client leaves while this runs, before any reply
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	var savedRuns atomic.Int32
	router.routes.Post("/saved-then-gone", func(w http.ResponseWriter, r *http.Request) {
		savedRuns.Add(1)
		<-r.Context().Done() // the client leaves; the work is saved and replied to anyway
		w.Header().Set("Location", "/things/1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"1"}`))
	})
	var hugeRuns atomic.Int32
	router.routes.Post("/huge", func(w http.ResponseWriter, _ *http.Request) {
		hugeRuns.Add(1)
		w.Header().Set("Location", "/things/2")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxKeptReply+1))
	})

	// leave sends a request whose client hangs up while it runs: its context ends, and writes to it
	// fail, as on a real dropped connection.
	leave := func(target, key string) {
		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(`{}`))
		req.Header.Set(idempotencyHeader, key)
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		router.ServeHTTP(goneWriter{httptest.NewRecorder()}, req)
	}

	key := newKey()
	leave("/gone", key)
	if rec := sendKeyed(t, router, "/gone", key, `{}`, ""); rec.Code != http.StatusCreated || rec.Header().Get(replayedHeader) != "" {
		t.Errorf("retry after the client left before a reply = %d, replayed %q; want a fresh 201", rec.Code, rec.Header().Get(replayedHeader))
	}

	key = newKey()
	leave("/saved-then-gone", key)
	retry := sendKeyed(t, router, "/saved-then-gone", key, `{}`, "")
	if retry.Code != http.StatusCreated || retry.Header().Get(replayedHeader) != "true" || retry.Body.String() != `{"id":"1"}` || savedRuns.Load() != 1 {
		t.Errorf("retry after the client left a replied request = %d %s, replayed %q, %d runs; want the kept 201, 1 run",
			retry.Code, retry.Body, retry.Header().Get(replayedHeader), savedRuns.Load())
	}

	key = newKey()
	sendKeyed(t, router, "/huge", key, `{}`, "")
	rec := sendKeyed(t, router, "/huge", key, `{}`, "")
	want := problemReply{Status: http.StatusConflict, Detail: "the request with this Idempotency-Key already ran; its reply was too large to keep"}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" || hugeRuns.Load() != 1 || rec.Header().Get("Location") != "/things/2" {
		t.Errorf("retry of a reply too large to keep (-want +got):\n%s\nruns = %d, Location %q; want 1, /things/2", diff, hugeRuns.Load(), rec.Header().Get("Location"))
	}

	router.logs.Reset()
	replayKey := newKey()
	sendKeyed(t, router, "/gone", replayKey, `{}`, "")
	router.logs.Reset()
	replayed := sendKeyed(t, router, "/gone", replayKey, `{}`, "")
	if lines := logLines(t, &router.logs); len(lines) != 1 || lines[0]["idempotent_replay"] != true {
		t.Errorf("replay's log lines = %v, want one with idempotent_replay true", lines)
	}
	if got := replayed.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("replay's Cache-Control = %q, want no-store", got)
	}
}

// Who's signed in, and what they may do, can change: a 401 or 403 isn't kept.
func TestIdempotencyDoesntKeepAuthReplies(t *testing.T) {
	t.Parallel()
	router := newKeyedRouter(t, testredis.New(t))
	router.routes.Post("/admin-only", func(w http.ResponseWriter, _ *http.Request) {
		if router.runs.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	key := newKey()
	first, retry := sendKeyed(t, router, "/admin-only", key, `{}`, ""), sendKeyed(t, router, "/admin-only", key, `{}`, "")
	if first.Code != http.StatusForbidden || retry.Code != http.StatusCreated || retry.Header().Get(replayedHeader) != "" {
		t.Errorf("403 then retry = %d, %d (replayed %q); want 403, then a fresh 201", first.Code, retry.Code, retry.Header().Get(replayedHeader))
	}
}

// A keyed body is read whole to compare retries, so it has the JSON limit.
func TestIdempotencyBodyLimit(t *testing.T) {
	t.Parallel()
	router := newKeyedRouter(t, testredis.New(t))
	router.routes.Post("/create", func(http.ResponseWriter, *http.Request) { router.runs.Add(1) })
	rec := sendKeyed(t, router, "/create", newKey(), strings.Repeat("x", handler.MaxBodyBytes+1), "")
	if rec.Code != http.StatusRequestEntityTooLarge || router.runs.Load() != 0 {
		t.Errorf("keyed body over 1 MB = %d with %d runs, want 413 and 0", rec.Code, router.runs.Load())
	}
}

// goneWriter is a reply to a client that has hung up: every write fails.
type goneWriter struct{ *httptest.ResponseRecorder }

func (g goneWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// Only a POST is keyed: other methods give the same result twice already, and pass straight
// through, even with a key that isn't one. On a POST the query is part of the request.
func TestIdempotencyOnlyForPOST(t *testing.T) {
	t.Parallel()
	router := newKeyedRouter(t, testredis.New(t))
	router.routes.Put("/thing", func(http.ResponseWriter, *http.Request) { router.runs.Add(1) })
	router.routes.Post("/search", func(http.ResponseWriter, *http.Request) {})
	for range 2 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/thing", strings.NewReader(`{}`))
		req.Header.Set(idempotencyHeader, "not a valid key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get(replayedHeader) != "" {
			t.Errorf("keyed PUT = %d, replayed %q; want 200, not replayed", rec.Code, rec.Header().Get(replayedHeader))
		}
	}
	if router.runs.Load() != 2 {
		t.Errorf("keyed PUT runs = %d, want 2", router.runs.Load())
	}

	key := newKey()
	sendKeyed(t, router, "/search?page=1", key, `{}`, "")
	if rec := sendKeyed(t, router, "/search?page=2", key, `{}`, ""); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("same key, another query = %d, want 422", rec.Code)
	}
}
