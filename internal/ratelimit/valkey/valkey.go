// Package valkey is ratelimit.Limiter on Valkey (Redis): every copy of a program counts in the same
// place, so a limit holds however many copies run. Counting is go-redis/redis_rate's: one atomic
// script per request (GCRA: a limit a minute allows a burst of that many, refilling evenly), timed
// by Valkey's clock, so copies never let in more than the limit together.
package valkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/ratelimit"
)

// skipFor is how long the Limiter counts in memory after a Valkey call fails before trying Valkey
// again, so a down or hung Valkey costs one request a wait every 5 s instead of every request.
const skipFor = 5 * time.Second

// Limiter counts in Valkey and, while Valkey fails, in this process's memory (ratelimit.Memory): the
// limit then holds per copy, so N copies let in up to N times it. Not refused instead: a Valkey
// outage would then take the whole program down.
type Limiter struct {
	rate    *redis_rate.Limiter
	prefix  string
	timeout time.Duration
	memory  *ratelimit.Memory
	logger  zerolog.Logger
	// skipUntil is when to try Valkey again, in Unix nanoseconds; 0 while Valkey works.
	skipUntil atomic.Int64
	skipFor   time.Duration // skipFor; tests set another
}

var _ ratelimit.Limiter = (*Limiter)(nil)

// New returns a Limiter on client, its keys starting "rate:" + prefix (redis_rate adds "rate:").
// A Valkey call that takes longer than timeout counts as failed: that's the longest a slow Valkey
// holds up a request. Give client no retries (startup.RedisFailFast), or one call can retry past it.
func New(client *redis.Client, prefix string, timeout time.Duration, logger zerolog.Logger) *Limiter {
	return &Limiter{
		rate:    redis_rate.NewLimiter(client),
		prefix:  prefix,
		timeout: timeout,
		memory:  ratelimit.NewMemory(),
		logger:  logger,
		skipFor: skipFor,
	}
}

// Allow counts one request for key. Keys are stored hashed, not in the clear. It returns an error
// only when ctx ends during the call.
func (l *Limiter) Allow(ctx context.Context, key string, perMinute int) (ratelimit.Decision, error) {
	try, probe := l.tryValkey()
	if !try {
		return l.memory.Allow(ctx, key, perMinute)
	}
	callCtx, cancel := context.WithTimeout(ctx, l.timeout)
	res, err := l.rate.Allow(callCtx, l.prefix+hashed(key), redis_rate.PerMinute(perMinute))
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ratelimit.Decision{}, ctx.Err() // the request ended, not Valkey's failure
		}
		l.markDown(err, probe)
		return l.memory.Allow(ctx, key, perMinute)
	}
	if probe != 0 {
		l.markUp(probe)
	}
	return decision(res, perMinute), nil
}

// tryValkey reports whether to call Valkey: yes while it works. After a failure, once skipFor has
// passed, one request probes it, and gets the skipUntil it set (0 when not a probe), so a hung
// Valkey holds up one request at a time.
func (l *Limiter) tryValkey() (bool, int64) {
	until := l.skipUntil.Load()
	if until == 0 {
		return true, 0
	}
	now := time.Now().UnixNano()
	next := now + int64(l.skipFor)
	if now >= until && l.skipUntil.CompareAndSwap(until, next) {
		return true, next
	}
	return false, 0
}

// markDown starts counting in memory, logging it once per outage. A failed probe has already moved
// the next try on.
func (l *Limiter) markDown(err error, probe int64) {
	if probe != 0 {
		return
	}
	if l.skipUntil.CompareAndSwap(0, time.Now().Add(l.skipFor).UnixNano()) {
		l.logger.Error().Err(err).Str("key_prefix", l.prefix).
			Msg("rate limit: Valkey failed; this copy counts on its own until it's back")
	}
}

// markUp goes back to Valkey after a probe worked. Only a probe may: a call that started before an
// outage was noticed and then worked says nothing about Valkey now, and letting it switch back
// would flap between Valkey and memory while Valkey fails only some calls.
func (l *Limiter) markUp(probe int64) {
	if l.skipUntil.CompareAndSwap(probe, 0) {
		l.logger.Info().Str("key_prefix", l.prefix).Msg("rate limit: Valkey back; counting is shared again")
	}
}

// decision turns redis_rate's result into a Decision. Remaining is worked out from ResetAfter:
// redis_rate's own truncates a float in Valkey's script and can be one low (598 of 600 left after
// one request).
func decision(res *redis_rate.Result, perMinute int) ratelimit.Decision {
	d := ratelimit.Decision{
		Allowed:    res.Allowed > 0,
		RetryAfter: max(res.RetryAfter, 0), // -1 when allowed
		ResetAfter: res.ResetAfter,
	}
	if d.Allowed {
		// Each request a minute's limit allows takes up 60 s / perMinute until it refills; the
		// rounding absorbs the float's last digits.
		used := math.Ceil(res.ResetAfter.Seconds()*float64(perMinute)/60 - 1e-6)
		d.Remaining = max(0, perMinute-int(used))
	}
	return d
}

// hashed returns a fixed-length stand-in for key.
func hashed(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}
