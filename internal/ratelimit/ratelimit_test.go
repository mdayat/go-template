package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

func TestMemoryRefusesPastTheLimitPerKey(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	now := time.Date(2026, 1, 1, 10, 0, 15, 0, time.UTC)
	m.now = func() time.Time { return now }

	for i := range 3 {
		d, _ := m.Allow(t.Context(), "a", 3)
		if !d.Allowed || d.Remaining != 2-i {
			t.Fatalf("request %d = %+v, want allowed with %d remaining", i+1, d, 2-i)
		}
	}
	d, _ := m.Allow(t.Context(), "a", 3)
	if d.Allowed || d.RetryAfter != 45*time.Second {
		t.Errorf("4th request = %+v, want refused, retry after the minute's 45 s left", d)
	}
	if d, _ := m.Allow(t.Context(), "b", 3); !d.Allowed {
		t.Errorf("another key's request = %+v, want allowed: keys are counted apart", d)
	}
}

// The counts start again each minute, so the map holds only the current minute's keys.
func TestMemoryStartsAgainEachMinute(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	now := time.Date(2026, 1, 1, 10, 0, 59, 0, time.UTC)
	m.now = func() time.Time { return now }
	_, _ = m.Allow(t.Context(), "a", 1)
	_, _ = m.Allow(t.Context(), "old", 1)

	now = now.Add(2 * time.Second)
	d, _ := m.Allow(t.Context(), "a", 1)

	if !d.Allowed {
		t.Errorf("request in the next minute = %+v, want allowed", d)
	}
	if _, kept := m.counts["old"]; kept {
		t.Error("last minute's keys are still held")
	}
}

// Past maxKeys in a minute, new keys are let through uncounted, so memory stays bounded; keys
// already counted are still limited.
func TestMemoryHoldsAtMostMaxKeys(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	for i := range maxKeys {
		_, _ = m.Allow(t.Context(), strconv.Itoa(i), 1)
	}

	for range 3 {
		if d, _ := m.Allow(t.Context(), "new", 1); !d.Allowed {
			t.Fatalf("new key past maxKeys = %+v, want allowed", d)
		}
	}
	if len(m.counts) != maxKeys {
		t.Errorf("held %d keys, want %d", len(m.counts), maxKeys)
	}
	if d, _ := m.Allow(t.Context(), "0", 1); d.Allowed {
		t.Errorf("a counted key past its limit = %+v, want refused", d)
	}
}
