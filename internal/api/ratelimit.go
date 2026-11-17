package api

import (
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/mdayat/go-template/internal/ratelimit"
)

// Limits says how the API tells clients apart and how many requests each may make a minute. Every
// route but the health check is limited; paths no route matches (404, 405) aren't, as they do no
// work.
type Limits struct {
	// TrustedProxies are the IP ranges (CIDR) of the proxies in front of the API: a load balancer,
	// a CDN. A request from one has its client's IP read from X-Forwarded-For, right to left,
	// skipping the trusted ones. With none, the client is whoever opened the connection, which
	// behind a proxy is the proxy itself, so every client would share one limit.
	TrustedProxies []string
	// PerIP limits each client IP's requests without a valid sign-in (none, or a refused token), so
	// a flood of bad tokens is cut off. 0 = no limit.
	PerIP int
	// PerUser limits each signed-in user, whatever IP they come from. 0 = no limit.
	PerUser int
	// Limiter counts the requests; cmd/api picks it (in Valkey, so every copy of the API shares
	// the counts). Needed when a limit isn't 0.
	Limiter ratelimit.Limiter
}

// clientIP finds the client's IP for the rate limits, the request log (read it with
// middleware.GetClientIP) and the request's span, whose client.address otelhttp took from
// X-Forwarded-For whoever sent it. Not from headers like X-Real-IP, which anyone can send: behind trusted
// proxies only, and only the part of X-Forwarded-For they added. The first request carrying
// X-Forwarded-For from a sender that isn't a trusted proxy is logged at warn: behind a proxy missing
// from TRUSTED_PROXIES, every client looks like the proxy and shares its signed-out limit.
func clientIP(trustedProxies []string, logger zerolog.Logger) func(http.Handler) http.Handler {
	find := middleware.ClientIPFromRemoteAddr
	if len(trustedProxies) > 0 {
		find = middleware.ClientIPFromXFF(trustedProxies...)
	}
	var warned atomic.Bool
	return func(next http.Handler) http.Handler {
		return find(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Forwarded-For") != "" && !warned.Load() && clientIsPeer(r) &&
				warned.CompareAndSwap(false, true) {
				logger.Warn().Msg("a request came with X-Forwarded-For from an address not in TRUSTED_PROXIES: " +
					"if the API is behind a proxy, add its range, or every client behind it shares one rate limit " +
					"(logged once per copy)")
			}
			// Always replaced: otelhttp's is whatever X-Forwarded-For starts with, any text at all.
			// When the client can't be told, the connection's address, as the request log has it.
			address := r.RemoteAddr
			if ip := middleware.GetClientIPAddr(r.Context()); ip.IsValid() {
				address = ip.String()
			} else if peer, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
				address = peer.Addr().String()
			}
			trace.SpanFromContext(r.Context()).SetAttributes(semconv.ClientAddress(address))
			next.ServeHTTP(w, r)
		}))
	}
}

// clientIsPeer reports whether the client clientIP found is whoever opened r's connection, not a
// client behind a trusted proxy.
func clientIsPeer(r *http.Request) bool {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && peer.Addr().Unmap() == middleware.GetClientIPAddr(r.Context())
}

// rateLimit refuses requests past limit a minute per key. A nil *rateLimit is no limit.
type rateLimit struct {
	name    string
	limit   int
	limiter ratelimit.Limiter
	key     func(*http.Request) string
}

func newRateLimit(name string, limit int, limiter ratelimit.Limiter, key func(*http.Request) string) *rateLimit {
	if limit == 0 {
		return nil
	}
	if limiter == nil {
		panic("api: a rate limit is set but Limits.Limiter is nil")
	}
	return &rateLimit{name: name, limit: limit, limiter: limiter, key: key}
}

// signedOutLimit counts, per client IP, the requests that come without a valid sign-in (see
// authenticate), so a flood of bad tokens is cut off. Signed-in requests aren't counted per IP:
// many users can share one (a mobile carrier's, an office's).
func signedOutLimit(limit int, limiter ratelimit.Limiter) *rateLimit {
	return newRateLimit("ip", limit, limiter, func(r *http.Request) string {
		ip := middleware.GetClientIPAddr(r.Context())
		if !ip.IsValid() {
			// A trusted proxy's own request, or another service's from inside a trusted range, with
			// no X-Forwarded-For: counted by the connection, so each such caller has its own count.
			if peer, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
				ip = peer.Addr().Unmap()
			}
		}
		ip = ip.WithZone("")
		if ip.Is6() {
			// One count per /64: a client usually holds a whole one and could change address in it
			// to get a fresh count each time.
			prefix, _ := ip.Prefix(64)
			return prefix.String()
		}
		return ip.String() // "invalid IP" for a connection that isn't IP: they share one count
	})
}

// limitPerUser refuses a signed-in user's requests past limit a minute. Only behind authenticate.
func limitPerUser(limit int, limiter ratelimit.Limiter) func(http.Handler) http.Handler {
	l := newRateLimit("user", limit, limiter, func(r *http.Request) string {
		return currentUser(r).ID.String()
	})
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l.check(w, r) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// check counts r and sets X-RateLimit-Limit, -Remaining and -Reset (when the count is full again,
// in Unix seconds). Past the limit it replies 429, saying in Retry-After how many seconds to wait,
// and returns false; the caller then writes nothing more.
func (l *rateLimit) check(w http.ResponseWriter, r *http.Request) bool {
	if l == nil {
		return true
	}
	d, err := l.limiter.Allow(r.Context(), l.name+":"+l.key(r), l.limit)
	if err != nil {
		writeError(w, r, fmt.Errorf("rate limit per %s: %w", l.name, err))
		return false
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(l.limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
	// Rounded up: a time already past would tell the client the count is full now.
	h.Set("X-RateLimit-Reset", strconv.FormatInt(int64(math.Ceil(float64(time.Now().Add(d.ResetAfter).UnixNano())/1e9)), 10))
	if d.Allowed {
		return true
	}
	h.Set("Retry-After", strconv.Itoa(seconds(d.RetryAfter)))
	writeError(w, r, fmt.Errorf("rate limit per %s reached: %w", l.name, &statusError{
		status: http.StatusTooManyRequests,
		detail: "too many requests; wait the seconds in Retry-After and try again",
	}))
	return false
}

// seconds rounds d up to whole seconds, at least 1: waiting 0 s would be refused again.
func seconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
