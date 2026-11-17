package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	ratelimitvalkey "github.com/mdayat/go-template/internal/ratelimit/valkey"
	"github.com/mdayat/go-template/internal/testredis"
)

// valkeyLimiter returns the Limiter cmd/api uses, on the test Valkey, under keys of t's own that
// are deleted when t ends. Routers given the same one share their counts, like copies of the API.
func valkeyLimiter(t *testing.T) *ratelimitvalkey.Limiter {
	t.Helper()
	client := testredis.New(t)
	prefix := "test_" + strings.ToLower(rand.Text()) + ":"
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		keys, err := client.Keys(ctx, "rate:"+prefix+"*").Result()
		if err != nil {
			t.Errorf("list rate-limit keys: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Errorf("delete rate-limit keys: %v", err)
			}
		}
	})
	return ratelimitvalkey.New(client, prefix, time.Second, zerolog.Nop())
}

// request is one request to send: from remoteAddr (the connection), with an X-Forwarded-For header
// if xff isn't empty and a sign-in token if token isn't.
type request struct {
	remoteAddr, xff, token string
}

func (rq request) send(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	return rq.sendTo(t, router, "/tasks")
}

func (rq request) sendTo(t *testing.T, router http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.RemoteAddr = rq.remoteAddr
	if rq.xff != "" {
		req.Header.Set("X-Forwarded-For", rq.xff)
	}
	if rq.token != "" {
		req.Header.Set("Authorization", "Bearer "+rq.token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// sendN sends rq n times and returns the status of each reply.
func sendN(t *testing.T, router http.Handler, rq request, n int) []int {
	t.Helper()
	statuses := make([]int, n)
	for i := range statuses {
		statuses[i] = rq.send(t, router).Code
	}
	return statuses
}

// statuses returns n copies of status, then the rest, for comparing with sendN's.
func statuses(n, status int, rest ...int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = status
	}
	return append(s, rest...)
}

// The per-IP limit counts requests without a valid sign-in: the one past it gets a 429 problem reply
// saying when to retry, instead of the 401. Another IP has its own count.
func TestRateLimitPerIP(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{PerIP: 3, Limiter: valkeyLimiter(t)})
	client := request{remoteAddr: "192.0.2.1:1234"}

	if diff := cmp.Diff(statuses(3, http.StatusUnauthorized), sendN(t, router, client, 3)); diff != "" {
		t.Errorf("first 3 requests (-want +got):\n%s", diff)
	}
	rec := client.send(t, router)

	if got := readProblem(t, rec); got.Status != http.StatusTooManyRequests ||
		got.Detail != "too many requests; wait the seconds in Retry-After and try again" {
		t.Errorf("4th request = %+v, want a 429 problem reply", got)
	}
	for header, want := range map[string]string{
		"Retry-After": "20", "X-RateLimit-Limit": "3", "X-RateLimit-Remaining": "0",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("4th request %s = %q, want %q", header, got, want)
		}
	}
	// 3 a minute refill one every 20 s: the count is full again in about 60 s.
	if reset, err := strconv.ParseInt(rec.Header().Get("X-RateLimit-Reset"), 10, 64); err != nil ||
		reset < time.Now().Unix()+55 || reset > time.Now().Unix()+61 {
		t.Errorf("4th request X-RateLimit-Reset = %q, want about 60 s from now", rec.Header().Get("X-RateLimit-Reset"))
	}
	if got := (request{remoteAddr: "192.0.2.2:1234"}).send(t, router).Code; got != http.StatusUnauthorized {
		t.Errorf("another IP's request = %d, want 401: IPs are counted apart", got)
	}
}

// Signed-in requests don't count per IP: users sharing one IP (a mobile carrier's, an office's) don't
// use up each other's limit, and the IP's signed-out requests still have theirs.
func TestRateLimitPerIPSkipsSignedIn(t *testing.T) {
	t.Parallel()
	router, _, issuer := testAPIWith(t, Limits{PerIP: 2, Limiter: valkeyLimiter(t)})
	alice := request{remoteAddr: "192.0.2.1:1234", token: issuer.Token("alice")}

	signedIn := sendN(t, router, alice, 4)
	signedOut := sendN(t, router, request{remoteAddr: "192.0.2.1:1234"}, 3)

	if diff := cmp.Diff(statuses(4, http.StatusOK), signedIn); diff != "" {
		t.Errorf("signed-in requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(statuses(2, http.StatusUnauthorized, http.StatusTooManyRequests), signedOut); diff != "" {
		t.Errorf("signed-out requests after them (-want +got):\n%s", diff)
	}
}

// A 429 for too many refused sign-ins replaces the 401: it doesn't ask for credentials.
func TestRateLimitPerIPReplacesTheChallenge(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{PerIP: 1, Limiter: valkeyLimiter(t)})
	client := request{remoteAddr: "192.0.2.1:1234", token: "not-a-token"}

	first, second := client.send(t, router), client.send(t, router)

	if first.Code != http.StatusUnauthorized || first.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("first = %d, WWW-Authenticate %q; want 401, Bearer", first.Code, first.Header().Get("WWW-Authenticate"))
	}
	if second.Code != http.StatusTooManyRequests || second.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("second = %d, WWW-Authenticate %q; want 429 without it", second.Code, second.Header().Get("WWW-Authenticate"))
	}
}

// The per-user limit counts each signed-in user apart, wherever they send from; requests refused at
// sign-in don't count.
func TestRateLimitPerUser(t *testing.T) {
	t.Parallel()
	router, _, issuer := testAPIWith(t, Limits{PerUser: 2, Limiter: valkeyLimiter(t)})
	alice := request{remoteAddr: "192.0.2.1:1234", token: issuer.Token("alice")}
	bob := request{remoteAddr: "192.0.2.1:1234", token: issuer.Token("bob")}

	sendN(t, router, request{remoteAddr: "192.0.2.1:1234"}, 3) // not signed in
	got := append(sendN(t, router, alice, 2), (request{remoteAddr: "198.51.100.9:1", token: alice.token}).send(t, router).Code)

	if diff := cmp.Diff(statuses(2, http.StatusOK, http.StatusTooManyRequests), got); diff != "" {
		t.Errorf("alice's requests, the last from another IP (-want +got):\n%s", diff)
	}
	if got := bob.send(t, router).Code; got != http.StatusOK {
		t.Errorf("bob's request = %d, want 200: users are counted apart", got)
	}
}

// Every copy of the API counts in the same place, so the limit holds however many copies run.
func TestRateLimitIsSharedByCopies(t *testing.T) {
	t.Parallel()
	limiter := valkeyLimiter(t)
	copy1, _, _ := testAPIWith(t, Limits{PerIP: 2, Limiter: limiter})
	copy2, _, _ := testAPIWith(t, Limits{PerIP: 2, Limiter: limiter})
	client := request{remoteAddr: "192.0.2.1:1234"}

	got := []int{client.send(t, copy1).Code, client.send(t, copy2).Code, client.send(t, copy1).Code}

	if diff := cmp.Diff(statuses(2, http.StatusUnauthorized, http.StatusTooManyRequests), got); diff != "" {
		t.Errorf("requests to two copies (-want +got):\n%s", diff)
	}
}

// An IPv6 client is counted by its /64, which it usually holds whole: changing address inside it
// doesn't get a fresh count. Another /64 has its own.
func TestRateLimitCountsIPv6ByItsSlash64(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{PerIP: 2, Limiter: valkeyLimiter(t)})

	got := []int{
		request{remoteAddr: "[2001:db8:1:2::1]:1234"}.send(t, router).Code,
		request{remoteAddr: "[2001:db8:1:2::2]:1234"}.send(t, router).Code,
		request{remoteAddr: "[2001:db8:1:2:ffff::3]:1234"}.send(t, router).Code,
		request{remoteAddr: "[2001:db8:1:3::1]:1234"}.send(t, router).Code,
	}

	want := []int{401, 401, 429, 401}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// Without trusted proxies X-Forwarded-For is ignored: a client can't get a fresh count by sending a
// new one each time.
func TestRateLimitIgnoresForwardedForWithoutTrustedProxies(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{PerIP: 2, Limiter: valkeyLimiter(t)})

	var got []int
	for _, xff := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		got = append(got, request{remoteAddr: "192.0.2.1:1234", xff: xff}.send(t, router).Code)
	}

	if diff := cmp.Diff(statuses(2, http.StatusUnauthorized, http.StatusTooManyRequests), got); diff != "" {
		t.Errorf("requests with forged X-Forwarded-For (-want +got):\n%s", diff)
	}
}

// Behind a trusted proxy each client is counted by the address the proxy added; what the client put
// in X-Forwarded-For itself is skipped. A request that doesn't come from the proxy is counted by
// its connection, whatever it sends.
func TestRateLimitBehindTrustedProxy(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{
		TrustedProxies: []string{"10.0.0.0/8"}, PerIP: 2, Limiter: valkeyLimiter(t),
	})
	viaProxy := func(xff string) request { return request{remoteAddr: "10.0.0.5:4000", xff: xff} }

	got := []int{
		viaProxy("198.51.100.1").send(t, router).Code,
		// The client forged the first entry; the proxy appended the real one.
		viaProxy("203.0.113.66, 198.51.100.1").send(t, router).Code,
		viaProxy("203.0.113.77, 198.51.100.1").send(t, router).Code,
		viaProxy("198.51.100.2").send(t, router).Code,
		// Straight to the API, claiming to be a client with requests left.
		request{remoteAddr: "192.0.2.1:1234", xff: "198.51.100.2"}.send(t, router).Code,
		request{remoteAddr: "192.0.2.1:1234", xff: "198.51.100.2"}.send(t, router).Code,
		request{remoteAddr: "192.0.2.1:1234", xff: "198.51.100.2"}.send(t, router).Code,
	}

	want := []int{401, 401, 429, 401, 401, 401, 429}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// Requests from inside a trusted range without X-Forwarded-For (the proxy's own, another service's)
// are counted by their connection: each such caller has its own count.
func TestRateLimitTrustedCallersWithoutForwardedFor(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{
		TrustedProxies: []string{"10.0.0.0/8"}, PerIP: 1, Limiter: valkeyLimiter(t),
	})

	got := []int{
		request{remoteAddr: "10.0.0.5:4000"}.send(t, router).Code,
		request{remoteAddr: "10.1.2.3:4000"}.send(t, router).Code,
		request{remoteAddr: "10.0.0.5:4001"}.send(t, router).Code,
	}

	want := []int{401, 401, 429}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// A limit set without a Limiter is a bug in the wiring: it fails when the router is built, not on a
// request.
func TestRateLimitWithoutLimiterPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("NewRouter() with a limit and no Limiter didn't panic")
		}
	}()
	NewRouter(zerolog.Nop(), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), Services{}, nil, Limits{PerIP: 1})
}

// Load balancers check the health route often, from few IPs: it's never limited.
func TestRateLimitSkipsHealthCheck(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{PerIP: 1, Limiter: valkeyLimiter(t)})
	client := request{remoteAddr: "192.0.2.1:1234"}

	for i := range 3 {
		if got := client.sendTo(t, router, healthPath).Code; got != http.StatusOK {
			t.Fatalf("health check %d = %d, want 200", i+1, got)
		}
	}
}

// A limit of 0 is off: nothing is counted, so nothing needs a counter, and replies carry no
// X-RateLimit headers.
func TestRateLimitZeroIsOff(t *testing.T) {
	t.Parallel()
	router, _, _ := testAPIWith(t, Limits{})
	client := request{remoteAddr: "192.0.2.1:1234"}

	if diff := cmp.Diff(statuses(5, http.StatusUnauthorized), sendN(t, router, client, 5)); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if got := client.send(t, router).Header().Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("X-RateLimit-Limit = %q, want none", got)
	}
}

// X-Forwarded-For from a sender that isn't a trusted proxy (one missing from TRUSTED_PROXIES, or a
// client forging it) is logged once per copy: behind such a proxy every client shares one limit.
func TestUntrustedForwardedForIsLoggedOnce(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), []string{"10.0.0.0/8"})
	router.Get("/ok", func(http.ResponseWriter, *http.Request) {})

	warnings := func() int {
		n := 0
		for _, line := range logLines(t, &logs) {
			if line["level"] == "warn" {
				n++
			}
		}
		return n
	}

	request{remoteAddr: "10.0.0.5:4000", xff: "203.0.113.1"}.sendTo(t, router, "/ok") // via the proxy
	request{remoteAddr: "192.0.2.1:1234"}.sendTo(t, router, "/ok")                    // no header
	if got := warnings(); got != 0 {
		t.Fatalf("got %d warnings from the trusted proxy and a plain request, want 0", got)
	}
	request{remoteAddr: "192.0.2.1:1234", xff: "203.0.113.1"}.sendTo(t, router, "/ok")
	request{remoteAddr: "192.0.2.2:1234", xff: "203.0.113.2"}.sendTo(t, router, "/ok")
	if got := warnings(); got != 1 {
		t.Errorf("got %d warnings from two untrusted senders, want 1", got)
	}
}

// The request log names the client the limits count: behind a trusted proxy, the address it added,
// without a port (the connection's is the proxy's); otherwise the connection's address and port.
func TestLogNamesTheClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		trustedProxies []string
		rq             request
		want           map[string]any
	}{
		{
			"no proxy", nil,
			request{remoteAddr: "192.0.2.1:1234", xff: "203.0.113.1"},
			map[string]any{"ip": "192.0.2.1", "port": float64(1234)},
		},
		{
			"via a trusted proxy",
			[]string{"10.0.0.0/8"},
			request{remoteAddr: "10.0.0.5:4000", xff: "203.0.113.1"},
			map[string]any{"ip": "203.0.113.1"},
		},
		{
			"straight past a trusted proxy",
			[]string{"10.0.0.0/8"},
			request{remoteAddr: "192.0.2.1:1234", xff: "203.0.113.1"},
			map[string]any{"ip": "192.0.2.1", "port": float64(1234)},
		},
		{
			"IPv4 on an IPv6 connection", nil,
			request{remoteAddr: "[::ffff:192.0.2.1]:1234"},
			map[string]any{"ip": "192.0.2.1", "port": float64(1234)},
		},
		{
			"unreadable X-Forwarded-For",
			[]string{"10.0.0.0/8"},
			request{remoteAddr: "10.0.0.5:4000", xff: "not-an-ip"},
			map[string]any{"address": "10.0.0.5:4000"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			router := newBaseRouter(zerolog.New(&logs), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), tt.trustedProxies)
			router.Get("/ok", func(http.ResponseWriter, *http.Request) {})

			tt.rq.sendTo(t, router, "/ok")

			var summary map[string]any
			for _, line := range logLines(t, &logs) {
				if line["message"] == "request completed" {
					summary = line
				}
			}
			if diff := cmp.Diff(tt.want, summary["client"]); diff != "" {
				t.Errorf("client fields mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
