package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
)

func TestHealthzAnswersGetAndHead(t *testing.T) {
	t.Parallel()
	router := newBaseRouter(zerolog.Nop())

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, healthPath, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200", method, healthPath, rec.Code)
		}
	}
}

// Go's HTTP server reports its own problems through http.Server.ErrorLog; they must arrive as JSON
// error lines like everything else, not as plain text.
func TestServerErrorLogWritesJSONErrorLines(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer

	ServerErrorLog(zerolog.New(&logs)).Printf("http: superfluous response.WriteHeader call\n")

	var got map[string]any
	if err := json.Unmarshal(logs.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", logs.String(), err)
	}
	want := map[string]any{
		"level":   "error",
		"source":  "net/http",
		"message": "http: superfluous response.WriteHeader call",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("log line mismatch (-want +got):\n%s", diff)
	}
}
