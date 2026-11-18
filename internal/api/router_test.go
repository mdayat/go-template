package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/queue/valkey"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/testredis"
)

func TestHealthzAnswersGetAndHead(t *testing.T) {
	t.Parallel()
	router := newBaseRouter(zerolog.Nop(), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), nil, nil)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, healthPath, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200", method, healthPath, rec.Code)
		}
	}
}

// Go's HTTP server reports its own problems through http.Server.ErrorLog; they must arrive as JSON
// error lines like everything else, not as plain text.
func TestServerErrorLogWritesJSONErrorLines(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer

	ServerErrorLog(zerolog.New(&logs)).Printf("http: superfluous response.WriteHeader call\n")

	var got map[string]any
	if err := json.Unmarshal(logs.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", logs.String(), err)
	}
	want := map[string]any{
		"level":   "error",
		"source":  "net/http",
		"message": "http: superfluous response.WriteHeader call",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("log line mismatch (-want +got):\n%s", diff)
	}
}

// Under each client's prefix, a request refused before its handler (here a 401) keeps its route in
// logs, metrics and traces, not just the prefix; paths and methods no route has get a 404 or 405
// without a sign-in, as anywhere else, labelled with the prefix (/web/*); GET routes answer HEAD.
func TestClientPrefixes(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	authn, issuer := testAuthenticator(t)
	var logs bytes.Buffer
	router := NewRouter(zerolog.New(&logs), noop.NewMeterProvider(), tracenoop.NewTracerProvider(),
		NewServices(
			db.New(pool),
			testReplies(t),
			// remove:worker
			valkey.NewEnqueuer(testredis.New(t), tracenoop.NewTracerProvider()),
			// remove:end
		), NewReadiness(pool.Ping, zerolog.Nop()), authn, Limits{}, nil)
	token := issuer.Token(testSubject)

	tests := []struct {
		method, target string
		signedIn       bool
		wantStatus     int
		wantRoute      string
		wantAllow      string
	}{
		{http.MethodGet, "/web/tasks/" + uuid.NewString(), false, 401, "/web/tasks/{id}", ""},
		{http.MethodGet, "/mobile/v1/tasks", false, 401, "/mobile/v1/tasks", ""},
		// remove:worker
		{http.MethodGet, "/admin/deleted-row-purges", false, 405, "/admin/*", "POST"},
		// remove:end
		{http.MethodGet, "/mobile/v1/nowhere", false, 404, "/mobile/v1/*", ""},
		{http.MethodGet, "/admin/nowhere", true, 404, "/admin/*", ""},
		{http.MethodGet, "/nowhere", false, 404, "", ""},
		// Profiles are only on their own local port (startup.ServeProfiles), never the API's.
		{http.MethodGet, "/debug/pprof/", false, 404, "", ""},
		{http.MethodGet, "/admin/debug/pprof/heap", true, 404, "/admin/*", ""},
		{http.MethodOptions, "/web/tasks", false, 405, "/web/*", "GET, HEAD, POST"},
		{http.MethodDelete, "/mobile/v1/tasks", false, 405, "/mobile/v1/*", "GET, HEAD, POST"},
		{http.MethodHead, "/web/tasks", true, 200, "/web/tasks", ""},
	}
	for _, tt := range tests {
		logs.Reset()
		req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.target, nil)
		if tt.signedIn {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		var line struct {
			HTTP struct {
				Route string `json:"route"`
			} `json:"http"`
		}
		if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
			t.Fatalf("%s %s: log line %q: %v", tt.method, tt.target, logs.String(), err)
		}
		if rec.Code != tt.wantStatus || line.HTTP.Route != tt.wantRoute || rec.Header().Get("Allow") != tt.wantAllow {
			t.Errorf("%s %s = %d, route %q, Allow %q; want %d, %q, %q", tt.method, tt.target,
				rec.Code, line.HTTP.Route, rec.Header().Get("Allow"), tt.wantStatus, tt.wantRoute, tt.wantAllow)
		}
	}
}

// No reply may be kept by a cache in between (a proxy, a CDN) unless its handler says so, and an
// error reply never.
func TestRepliesAreNotCached(t *testing.T) {
	t.Parallel()
	router := newBaseRouter(zerolog.Nop(), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), nil, nil)
	router.Get("/private", func(w http.ResponseWriter, _ *http.Request) {
		handler.WriteJSON(w, http.StatusOK, "application/json", struct{}{})
	})
	router.Get("/public", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		handler.WriteJSON(w, http.StatusOK, "application/json", struct{}{})
	})
	router.Get("/public-fails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		handler.WriteError(w, r, errors.New("broken"))
	})

	for target, want := range map[string]string{
		"/private":      "no-store",
		"/public":       "max-age=60",
		"/public-fails": "no-store",
		"/nowhere":      "no-store",
	} {
		if got := send(t, router, http.MethodGet, target, "").Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control = %q, want %q", target, got, want)
		}
	}
}
