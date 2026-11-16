package valkey

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/testredis"
)

// testPrefix returns a key prefix of t's own and deletes its keys when t ends: tests share the test
// database.
func testPrefix(t *testing.T, client *redis.Client) string {
	t.Helper()
	prefix := "test_" + strings.ToLower(rand.Text()) + ":"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		keys, err := client.Keys(ctx, "rate:"+prefix+"*").Result()
		if err != nil {
			t.Errorf("list keys: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Errorf("delete keys: %v", err)
			}
		}
	})
	return prefix
}

// Two Limiters on one Valkey are two copies of the program: they share one count, which never lets
// in more than the limit, and they store no key in the clear.
func TestLimitersShareOneCount(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	prefix := testPrefix(t, client)
	copy1, copy2 := New(client, prefix, time.Second, zerolog.Nop()), New(client, prefix, time.Second, zerolog.Nop())

	var allowed []bool
	for _, l := range []*Limiter{copy1, copy2, copy1, copy2} {
		d, err := l.Allow(t.Context(), "ip:192.0.2.1", 3)
		if err != nil {
			t.Fatalf("Allow() error = %v", err)
		}
		allowed = append(allowed, d.Allowed)
	}

	if want := []bool{true, true, true, false}; !slices.Equal(allowed, want) {
		t.Errorf("allowed = %v, want %v", allowed, want)
	}
	keys, err := client.Keys(t.Context(), "rate:"+prefix+"*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || strings.Contains(keys[0], "192.0.2.1") {
		t.Errorf("keys = %q, want one, without the IP", keys)
	}
}

func TestDecisionWhenRefused(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	l := New(client, testPrefix(t, client), time.Second, zerolog.Nop())

	first, _ := l.Allow(t.Context(), "k", 2)
	_, _ = l.Allow(t.Context(), "k", 2)
	refused, err := l.Allow(t.Context(), "k", 2)

	if err != nil || !first.Allowed || first.Remaining != 1 || first.RetryAfter != 0 {
		t.Errorf("first = %+v, %v; want allowed, 1 remaining, no retry wait", first, err)
	}
	// 2 a minute refill one every 30 s.
	if refused.Allowed || refused.RetryAfter <= 25*time.Second || refused.RetryAfter > 30*time.Second {
		t.Errorf("refused = %+v, want refused with about 30 s to wait", refused)
	}
}

// Valkey down: requests are counted in memory and still limited, the failure is logged once, and
// Valkey isn't tried again until skipFor has passed.
func TestValkeyDownCountsInMemory(t *testing.T) {
	t.Parallel()
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = down.Close() })
	var logs bytes.Buffer
	l := New(down, "test:", time.Second, zerolog.New(&logs))

	var allowed []bool
	for range 3 {
		d, err := l.Allow(t.Context(), "k", 2)
		if err != nil {
			t.Fatalf("Allow() error = %v, want the memory count's decision", err)
		}
		allowed = append(allowed, d.Allowed)
	}

	if want := []bool{true, true, false}; !slices.Equal(allowed, want) {
		t.Errorf("allowed = %v, want %v", allowed, want)
	}
	if lines := logMessages(t, &logs); len(lines) != 1 || !strings.Contains(lines[0], "Valkey failed") {
		t.Errorf("log = %q, want one line saying Valkey failed", lines)
	}
	if try, _ := l.tryValkey(); try {
		t.Error("tryValkey() = true right after a failure, want false until skipFor passes")
	}
}

// An outage is logged once, however many tries of Valkey fail during it.
func TestOutageIsLoggedOnce(t *testing.T) {
	t.Parallel()
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = down.Close() })
	var logs bytes.Buffer
	l := New(down, "test:", time.Second, zerolog.New(&logs))
	l.skipFor = 0 // every request tries Valkey again

	for range 3 {
		if _, err := l.Allow(t.Context(), "k", 5); err != nil {
			t.Fatalf("Allow() error = %v", err)
		}
	}

	if lines := logMessages(t, &logs); len(lines) != 1 {
		t.Errorf("log = %q, want one line", lines)
	}
}

// Once skipFor has passed, one request tries Valkey; if it works, counting is shared again.
func TestValkeyBackIsUsedAgain(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	var logs bytes.Buffer
	l := New(client, testPrefix(t, client), time.Second, zerolog.New(&logs))
	l.skipFor = time.Millisecond
	l.markDown(context.DeadlineExceeded, 0) // as if a call had just failed
	time.Sleep(2 * time.Millisecond)

	if _, err := l.Allow(t.Context(), "k", 5); err != nil {
		t.Fatalf("Allow() error = %v", err)
	}

	if l.skipUntil.Load() != 0 {
		t.Error("still skipping Valkey after a call to it worked")
	}
	if lines := logMessages(t, &logs); len(lines) != 2 || !strings.Contains(lines[1], "Valkey back") {
		t.Errorf("log = %q, want the failure, then a line saying Valkey is back", lines)
	}
}

// While Valkey is skipped, only one request at a time tries it again.
func TestOnlyOneRequestTriesValkeyAgain(t *testing.T) {
	t.Parallel()
	l := New(redis.NewClient(&redis.Options{Addr: "localhost:1"}), "test:", time.Second, zerolog.Nop())
	l.skipUntil.Store(time.Now().Add(-time.Second).UnixNano()) // skipFor has passed

	first, _ := l.tryValkey()
	second, _ := l.tryValkey()

	if !first || second {
		t.Errorf("tryValkey() = %v, then %v; want true once, then false", first, second)
	}
}

// A request whose context ends during the call gets the context's error: its end isn't Valkey's
// failure, so Valkey isn't skipped for it.
func TestEndedRequestIsNotAValkeyFailure(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	l := New(client, testPrefix(t, client), time.Second, zerolog.Nop())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := l.Allow(ctx, "k", 5)

	if err == nil {
		t.Error("Allow() error = nil, want the context's error")
	}
	if l.skipUntil.Load() != 0 {
		t.Error("Valkey is skipped after a request ended, want it still used")
	}
}

func logMessages(t *testing.T, logs *bytes.Buffer) []string {
	t.Helper()
	var messages []string
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		var line struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line is not JSON: %q", scanner.Text())
		}
		messages = append(messages, line.Message)
	}
	return messages
}

// Remaining is exact: redis_rate's own would say 598 of 600 after one request.
func TestRemainingIsExact(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	l := New(client, testPrefix(t, client), time.Second, zerolog.Nop())

	for _, limit := range []int{600, 7, 1000, 3} {
		d, err := l.Allow(t.Context(), "k"+strconv.Itoa(limit), limit)
		if err != nil || d.Remaining != limit-1 {
			t.Errorf("first of %d a minute = %+v, %v; want %d remaining", limit, d, err, limit-1)
		}
	}
}

// A hung Valkey holds a request up for the Limiter's timeout, not the client's, then counts in
// memory: many requests at once all finish about then.
func TestHungValkeyHoldsUpForTheTimeoutOnly(t *testing.T) {
	t.Parallel()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	// Long timeouts on the client, as on one not made fail-fast; the request's deadline follows ctx.
	hung := redis.NewClient(&redis.Options{
		Addr: listener.Addr().String(), MaxRetries: -1, ReadTimeout: 5 * time.Second, ContextTimeoutEnabled: true,
	})
	t.Cleanup(func() { _ = hung.Close() })
	l := New(hung, "test:", 100*time.Millisecond, zerolog.Nop())

	start := time.Now()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := l.Allow(t.Context(), "k", 1000); err != nil {
				t.Errorf("Allow() error = %v", err)
			}
		})
	}
	wg.Wait()

	if took := time.Since(start); took > time.Second {
		t.Errorf("50 requests took %v, want about 100ms", took)
	}
}

// failSome makes every third Valkey command fail, as a Valkey that fails only some calls.
type failSome struct{ n atomic.Int64 }

func (f *failSome) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *failSome) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if f.n.Add(1)%3 == 0 {
			cmd.SetErr(errors.New("injected failure"))
			return cmd.Err()
		}
		return next(ctx, cmd)
	}
}

func (f *failSome) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// While Valkey fails only some calls, the Limiter doesn't flap between Valkey and memory: a call that
// works says nothing about Valkey once a failure was seen, so the outage is logged once and only a
// probe ends it.
func TestSomeFailuresDontFlap(t *testing.T) {
	t.Parallel()
	opts := *testredis.New(t).Options()
	flaky := redis.NewClient(&opts)
	flaky.AddHook(&failSome{})
	t.Cleanup(func() { _ = flaky.Close() })
	var logs bytes.Buffer
	l := New(flaky, testPrefix(t, testredis.New(t)), time.Second, zerolog.New(&syncWriter{w: &logs}))

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 10 {
				if _, err := l.Allow(t.Context(), "k", 100_000); err != nil {
					t.Errorf("Allow() error = %v", err)
				}
			}
		})
	}
	wg.Wait()

	if lines := logMessages(t, &logs); len(lines) != 1 {
		t.Errorf("got %d log lines, want 1: the outage, then quiet until a probe", len(lines))
	}
}

// syncWriter lets goroutines log to one buffer.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
