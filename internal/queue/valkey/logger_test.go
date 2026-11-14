package valkey

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
)

// asynq's own messages must arrive as JSON lines like everything else, not as plain text.
func TestAsynqLoggerWritesJSONLines(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer

	asynqLogger{logger: zerolog.New(&logs)}.Warn("Quitting worker. task id=", "abc")

	var got map[string]any
	if err := json.Unmarshal(logs.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", logs.String(), err)
	}
	want := map[string]any{"level": "warn", "source": "asynq", "message": "Quitting worker. task id=abc"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("log line mismatch (-want +got):\n%s", diff)
	}
}
