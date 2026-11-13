package startup

import (
	"bytes"
	"encoding/json"
	"testing"

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
