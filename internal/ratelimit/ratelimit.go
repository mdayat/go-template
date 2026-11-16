// Package ratelimit is what the code knows about counting requests against a limit: a Limiter says
// whether one more request may go now. Implementations: Memory (one process's own counts) and
// valkey.Limiter (counts every copy of the program shares).
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter counts one request for key against perMinute requests a minute. An error means the
// request couldn't be counted at all (its context ended); an implementation copes with its own
// store failing.
type Limiter interface {
	Allow(ctx context.Context, key string, perMinute int) (Decision, error)
}

// Decision is whether a request may go, and what to tell the caller about the limit.
type Decision struct {
	Allowed bool
	// Remaining is how many more requests may go right now.
	Remaining int
	// RetryAfter is how long until the next request may go, when this one may not.
	RetryAfter time.Duration
	// ResetAfter is how long until the count is back to a full limit.
	ResetAfter time.Duration
}

// Memory counts in this process only, per calendar minute: with N copies of a program, a client gets
// up to N times the limit, and up to twice it across the turn of a minute. Its counts start again
// every minute, so it holds only the keys of the current one, and at most maxKeys of them.
type Memory struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
	now    func() time.Time // time.Now; tests set another
}

// maxKeys caps the keys Memory holds in a minute (about 10 MB). Past it, a new key's requests are let
// through uncounted: clients spread over that many addresses can't be stopped per address anyway,
// and refusing them would refuse everyone new until the minute ends.
const maxKeys = 100_000

// NewMemory returns an empty Memory.
func NewMemory() *Memory {
	return &Memory{counts: map[string]int{}, now: time.Now}
}

var _ Limiter = (*Memory)(nil)

// Allow counts one request for key. It never returns an error.
func (m *Memory) Allow(_ context.Context, key string, perMinute int) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if window := now.Truncate(time.Minute); !window.Equal(m.window) {
		m.window, m.counts = window, map[string]int{}
	}
	untilReset := m.window.Add(time.Minute).Sub(now)
	count, counted := m.counts[key]
	if !counted && len(m.counts) >= maxKeys {
		return Decision{Allowed: true, Remaining: perMinute - 1, ResetAfter: untilReset}, nil
	}
	if count >= perMinute {
		return Decision{RetryAfter: untilReset, ResetAfter: untilReset}, nil
	}
	m.counts[key] = count + 1
	return Decision{Allowed: true, Remaining: perMinute - count - 1, ResetAfter: untilReset}, nil
}
