package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const allowedSite = "https://app.example.com"

// corsRequest sends method to target as a page on origin would, from its own IP; a preflight also
// says which method and headers the real request will use.
func corsRequest(t *testing.T, router http.Handler, method, target, origin, token string, preflight bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("Origin", origin)
	if preflight {
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,idempotency-key")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// corsHeaders is what a reply tells the browser about CORS.
func corsHeaders(rec *httptest.ResponseRecorder) map[string]string {
	got := map[string]string{}
	for _, h := range []string{
		"Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers",
		"Access-Control-Max-Age", "Access-Control-Expose-Headers", "Access-Control-Allow-Credentials",
	} {
		if v := rec.Header().Get(h); v != "" {
			got[h] = v
		}
	}
	return got
}

// The browser asks first (a preflight, without a token): an allowed site is told what it may send,
// without the request reaching sign-in or the rate limits; another site is told nothing, so the
// browser refuses its page.
func TestCORSPreflight(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIFor(t, Limits{PerIP: 1, Limiter: valkeyLimiter(t)}, []string{allowedSite})

	for range 3 { // past the per-IP limit of 1: a preflight isn't counted
		rec := corsRequest(t, router, http.MethodOptions, limitedRoute, allowedSite, "", true)
		want := map[string]string{
			"Access-Control-Allow-Origin":  allowedSite,
			"Access-Control-Allow-Methods": http.MethodPost,
			"Access-Control-Allow-Headers": "authorization,content-type,idempotency-key",
			"Access-Control-Max-Age":       "7200",
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("preflight from %s = %d, want 204", allowedSite, rec.Code)
		}
		if diff := cmp.Diff(want, corsHeaders(rec)); diff != "" {
			t.Fatalf("preflight from %s headers (-want +got):\n%s", allowedSite, diff)
		}
	}

	rec := corsRequest(t, router, http.MethodOptions, limitedRoute, "https://evil.example.com", "", true)
	if got := corsHeaders(rec); rec.Code != http.StatusNoContent || len(got) != 0 {
		t.Errorf("preflight from another site = %d with %v, want 204 and no CORS headers", rec.Code, got)
	}
	// Answered before routing: a path no route serves gets the same answer.
	rec = corsRequest(t, router, http.MethodOptions, "/web/no-such-route", allowedSite, "", true)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != allowedSite {
		t.Errorf("preflight to an unknown path = %d with %v, want 204 allowed", rec.Code, corsHeaders(rec))
	}

	// A header the list doesn't name fails the preflight, so the browser doesn't send the request.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, limitedRoute, nil)
	req.Header.Set("Origin", allowedSite)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "x-not-allowed")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if got := corsHeaders(rec); len(got) != 0 {
		t.Errorf("preflight asking for x-not-allowed has %v, want no CORS headers", got)
	}
}

// The real request's reply, an error included, tells the browser the allowed site may read it, and
// which headers besides the basic ones; a reply varies by Origin, so a cache keeps one per site.
func TestCORSReplies(t *testing.T) {
	t.Parallel()
	router, _, issuer := testAPIFor(t, Limits{PerIP: 1, PerUser: 1, Limiter: valkeyLimiter(t)}, []string{allowedSite})
	token := issuer.Token(testSubject)
	exposed := "Location, X-Request-Id, X-Ratelimit-Limit, X-Ratelimit-Remaining, X-Ratelimit-Reset, Retry-After, Idempotent-Replayed"

	tests := []struct {
		name, token string
		wantStatus  int
	}{
		{"signed in", token, http.StatusOK},
		{"over the per-user limit", token, http.StatusTooManyRequests},
		{"not signed in", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		rec := corsRequest(t, router, http.MethodGet, limitedRoute, allowedSite, tt.token, false)
		if rec.Code != tt.wantStatus {
			t.Fatalf("%s: GET %s = %d, want %d", tt.name, limitedRoute, rec.Code, tt.wantStatus)
		}
		want := map[string]string{"Access-Control-Allow-Origin": allowedSite, "Access-Control-Expose-Headers": exposed}
		if diff := cmp.Diff(want, corsHeaders(rec)); diff != "" {
			t.Errorf("%s: reply headers (-want +got):\n%s", tt.name, diff)
		}
		if got := rec.Header().Values("Vary"); !cmp.Equal(got, []string{"Origin"}) {
			t.Errorf("%s: Vary = %q, want [Origin]", tt.name, got)
		}
	}

	rec := corsRequest(t, router, http.MethodGet, "/healthz", "https://evil.example.com", "", false)
	if got := corsHeaders(rec); len(got) != 0 {
		t.Errorf("reply to another site has %v, want no CORS headers", got)
	}
	// No Origin (a mobile app, a server): no CORS headers, but the reply still varies by Origin, so
	// a cache doesn't hand it to a page.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	noOrigin := httptest.NewRecorder()
	router.ServeHTTP(noOrigin, req)
	if got := corsHeaders(noOrigin); len(got) != 0 || !cmp.Equal(noOrigin.Header().Values("Vary"), []string{"Origin"}) {
		t.Errorf("reply without Origin has %v and Vary %q, want none and [Origin]", got, noOrigin.Header().Values("Vary"))
	}
	// A browser sends the site in lower case, but matching ignores case all the same.
	rec = corsRequest(t, router, http.MethodGet, "/healthz", "https://APP.example.com", "", false)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://APP.example.com" {
		t.Errorf("reply to https://APP.example.com: Access-Control-Allow-Origin = %q, want it echoed", got)
	}

	// Creating a row: the page reads where it is from Location.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, limitedRoute, strings.NewReader(`{"title":"from a page"}`))
	req.Header.Set("Origin", allowedSite)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+issuer.Token("another-subject"))
	created := httptest.NewRecorder()
	router.ServeHTTP(created, req)
	if created.Code != http.StatusCreated || created.Header().Get("Location") == "" ||
		!strings.Contains(created.Header().Get("Access-Control-Expose-Headers"), "Location") {
		t.Errorf("create from %s = %d, Location %q, exposed %q; want 201 with Location exposed",
			allowedSite, created.Code, created.Header().Get("Location"), created.Header().Get("Access-Control-Expose-Headers"))
	}
}

// With no sites allowed nothing changes: no CORS headers, and OPTIONS is a method the routes don't
// answer.
func TestCORSOff(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{})
	rec := corsRequest(t, router, http.MethodOptions, limitedRoute, allowedSite, "", true)
	if rec.Code != http.StatusMethodNotAllowed || len(corsHeaders(rec)) != 0 {
		t.Errorf("preflight with CORS off = %d with %v, want 405 and no CORS headers", rec.Code, corsHeaders(rec))
	}
}

// A preflight is logged like any request, with no route: it's answered before one is picked.
func TestCORSPreflightIsLogged(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), nil, []string{allowedSite})
	router.Get("/web/tasks", func(http.ResponseWriter, *http.Request) {})
	corsRequest(t, router, http.MethodOptions, "/web/tasks", allowedSite, "", true)

	lines := logLines(t, &logs)
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	httpFields, _ := lines[0]["http"].(map[string]any)
	request, _ := httpFields["request"].(map[string]any)
	response, _ := httpFields["response"].(map[string]any)
	if request["method"] != http.MethodOptions || response["status_code"] != 204.0 || httpFields["route"] != nil {
		t.Errorf("preflight's line http = %v, want OPTIONS, 204 and no route", httpFields)
	}
}
