package valkey

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/redis/go-redis/v9"

	"github.com/mdayat/go-template/internal/idempotency"
	"github.com/mdayat/go-template/internal/testredis"
)

// newStore returns a Store on the test Valkey, and a key no other test uses.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	return New(testredis.New(t), "test:", time.Second), "key-" + rand.Text()
}

// A key runs one request: while it runs, the same request is told so and a different one is
// refused; once its reply is kept, the same request gets the reply and a different one is refused.
func TestStoreKeepsOneReplyPerKey(t *testing.T) {
	t.Parallel()
	s, key := newStore(t)
	ctx := t.Context()

	claim, err := s.Begin(ctx, key, "fp-a", time.Minute)
	if err != nil || claim.Token == "" {
		t.Fatalf("first Begin = %+v, %v; want a token", claim, err)
	}
	for fp, want := range map[string]idempotency.Claim{"fp-a": {Running: true}, "fp-b": {Mismatch: true}} {
		got, err := s.Begin(ctx, key, fp, time.Minute)
		if err != nil || !cmp.Equal(got, want) {
			t.Errorf("Begin(%s) while running = %+v, %v; want %+v", fp, got, err, want)
		}
	}

	reply := idempotency.Reply{Status: 201, Header: map[string]string{"Location": "/web/tasks/1"}, Body: []byte(`{"id":"1"}`)}
	if err := s.Finish(ctx, key, claim.Token, reply, time.Minute); err != nil {
		t.Fatalf("Finish() = %v", err)
	}
	for fp, want := range map[string]idempotency.Claim{"fp-a": {Reply: &reply}, "fp-b": {Mismatch: true}} {
		got, err := s.Begin(ctx, key, fp, time.Minute)
		if err != nil || !cmp.Equal(got, want) {
			t.Errorf("Begin(%s) once kept = %+v, %v; want %+v", fp, got, err, want)
		}
	}
}

// A released key, or one whose hold ran out (the program crashed), runs again; the request that
// lost its hold can't then keep its reply, or free the key from the request that took it over.
func TestStoreFreesAKey(t *testing.T) {
	t.Parallel()
	s, key := newStore(t)
	ctx := t.Context()

	first, _ := s.Begin(ctx, key, "fp", time.Minute)
	if err := s.Release(ctx, key, first.Token); err != nil {
		t.Fatal(err)
	}
	second, err := s.Begin(ctx, key, "fp", 50*time.Millisecond)
	if err != nil || second.Token == "" {
		t.Fatalf("Begin after Release = %+v, %v; want a token", second, err)
	}

	time.Sleep(200 * time.Millisecond)
	third, err := s.Begin(ctx, key, "fp", time.Minute)
	if err != nil || third.Token == "" {
		t.Fatalf("Begin after the hold ran out = %+v, %v; want a token", third, err)
	}
	if err := s.Finish(ctx, key, second.Token, idempotency.Reply{Status: 200}, time.Minute); !errors.Is(err, ErrHoldLost) {
		t.Errorf("Finish with a lost hold = %v, want ErrHoldLost", err)
	}
	if err := s.Release(ctx, key, second.Token); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Begin(ctx, key, "fp", time.Minute); !got.Running {
		t.Errorf("Begin after a lost hold's Release = %+v, want the new hold still running", got)
	}
}

// A kept reply lasts keep, then the key is new again.
func TestStoreForgetsAfterKeep(t *testing.T) {
	t.Parallel()
	s, key := newStore(t)
	ctx := t.Context()
	claim, _ := s.Begin(ctx, key, "fp", time.Minute)
	if err := s.Finish(ctx, key, claim.Token, idempotency.Reply{Status: 200}, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got, err := s.Begin(ctx, key, "fp", time.Minute); err != nil || got.Token == "" {
		t.Errorf("Begin after keep = %+v, %v; want a token", got, err)
	}
}

// With Valkey down every call fails, rather than claiming a key it can't hold.
func TestStoreValkeyDown(t *testing.T) {
	t.Parallel()
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1}) // nothing listens
	t.Cleanup(func() { _ = down.Close() })
	s := New(down, "test:", time.Second)
	if _, err := s.Begin(t.Context(), "k", "fp", time.Minute); err == nil {
		t.Error("Begin with Valkey down = nil error, want one")
	}
}
