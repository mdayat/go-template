package startup

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
)

// go-redis reports its own problems through its logger; they must arrive as JSON lines like
// everything else, not as plain text.
func TestRedisLoggerWritesJSONLines(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer

	redisLogger{logger: zerolog.New(&logs)}.Printf(t.Context(), "redis: failed to dial after %d attempts", 5)

	var got map[string]any
	if err := json.Unmarshal(logs.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", logs.String(), err)
	}
	want := map[string]any{
		"level":   "warn",
		"source":  "go-redis",
		"message": "redis: failed to dial after 5 attempts",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("log line mismatch (-want +got):\n%s", diff)
	}
}

// A Valkey that accepts the connection but never answers: the fail-fast client's call gives up
// after its timeout instead of go-redis's default of seconds per try and three retries.
func TestRedisFailFastGivesUpAtItsTimeout(t *testing.T) {
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

	took, err := pingFailFast(t, listener.Addr().String())

	if err == nil {
		t.Fatal("Ping() error = nil, want a timeout")
	}
	if took > time.Second {
		t.Errorf("Ping() took %v, want about 100ms", took)
	}
}

// A request's own deadline ends the call, even one shorter than the client's timeout.
func TestRedisFailFastEndsWithTheRequest(t *testing.T) {
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
	client, err := RedisFailFast("redis://"+listener.Addr().String()+"/0", 5*time.Second)
	if err != nil {
		t.Fatalf("RedisFailFast() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = client.Ping(ctx).Err()

	if err == nil {
		t.Fatal("Ping() error = nil, want the request's deadline")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Ping() took %v, want about 50ms", took)
	}
}

// Nothing listening: a refused connection fails at once, without go-redis's dial retries.
func TestRedisFailFastDialsOnce(t *testing.T) {
	t.Parallel()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close() // the port is now refused

	took, err := pingFailFast(t, addr)

	if err == nil {
		t.Fatal("Ping() error = nil, want a refused connection")
	}
	if took > 250*time.Millisecond {
		t.Errorf("Ping() took %v, want one dial, at once", took)
	}
}

// pingFailFast makes a fail-fast client on addr (100 ms) and times one Ping. Making the client
// must not need Valkey.
func pingFailFast(t *testing.T, addr string) (time.Duration, error) {
	t.Helper()
	client, err := RedisFailFast("redis://"+addr+"/0", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("RedisFailFast() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	start := time.Now()
	err = client.Ping(t.Context()).Err()
	return time.Since(start), err
}
