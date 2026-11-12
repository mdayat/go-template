package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
)

// testRouter returns the shared middleware with routes covering each logging case, and the buffer
// the logs go to.
func testRouter(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs))
	router.Get("/ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	router.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	router.Get("/fail", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	router.Get("/silent", func(http.ResponseWriter, *http.Request) {})
	router.Get(healthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("down") {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	return router, &logs
}

// logLines parses each JSON log line, dropping the fields that change on every run.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", scanner.Text(), err)
		}
		if _, ok := line["duration_ms"].(float64); !ok {
			t.Errorf("duration_ms missing or not a number in %v", line)
		}
		delete(line, "duration_ms")
		delete(line, "request_id")
		lines = append(lines, line)
	}
	return lines
}

func summaryLine(level, method, path, route string, status, size int) map[string]any {
	httpFields := map[string]any{
		"request":  map[string]any{"method": method},
		"response": map[string]any{"status_code": float64(status), "body": map[string]any{"bytes": float64(size)}},
	}
	if route != "" {
		httpFields["route"] = route
	}
	return map[string]any{
		"level":      level,
		"http":       httpFields,
		"url":        map[string]any{"path": path},
		"user_agent": map[string]any{"original": "test-agent"},
		"client":     map[string]any{"ip": "192.0.2.1", "port": float64(1234)},
		"message":    "request completed",
	}
}

func TestLogRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     string
		clientGone bool
		want       []map[string]any
	}{
		{
			name:   "success is logged at info with the route",
			target: "/ok",
			want:   []map[string]any{summaryLine("info", "GET", "/ok", "/ok", 200, 5)},
		},
		{
			name:   "route pattern groups requests, path keeps the ID",
			target: "/users/42",
			want:   []map[string]any{summaryLine("info", "GET", "/users/42", "/users/{id}", 204, 0)},
		},
		{
			name:   "5xx is logged at error",
			target: "/fail",
			want:   []map[string]any{summaryLine("error", "GET", "/fail", "/fail", 500, 0)},
		},
		{
			name:   "unknown path is a 4xx at info with no route",
			target: "/nope",
			want:   []map[string]any{summaryLine("info", "GET", "/nope", "", 404, 19)},
		},
		{
			name:   "handler that writes nothing is logged as the 200 net/http sends",
			target: "/silent",
			want:   []map[string]any{summaryLine("info", "GET", "/silent", "/silent", 200, 0)},
		},
		{
			name:       "client that hung up before any reply is logged as 499, not 200",
			target:     "/silent",
			clientGone: true,
			want:       []map[string]any{summaryLine("info", "GET", "/silent", "/silent", 499, 0)},
		},
		{
			name:   "successful health check is not logged",
			target: healthPath,
			want:   nil,
		},
		{
			name:   "failing health check is logged",
			target: healthPath + "?down",
			want:   []map[string]any{summaryLine("error", "GET", healthPath, healthPath, 503, 0)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, logs := testRouter(t)

			ctx := t.Context()
			if tt.clientGone {
				// net/http cancels the request's context when the client disconnects.
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, tt.target, nil)
			req.Header.Set("User-Agent", "test-agent")
			router.ServeHTTP(httptest.NewRecorder(), req)

			if diff := cmp.Diff(tt.want, logLines(t, logs)); diff != "" {
				t.Errorf("log lines mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLogRequestsRequestID(t *testing.T) {
	t.Parallel()
	router, logs := testRouter(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ok", nil)
	req.Header.Set("X-Request-Id", "forged-by-client")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	gotHeader := rec.Header().Get("X-Request-Id")
	if gotHeader == "" || gotHeader == "forged-by-client" {
		t.Fatalf("X-Request-Id reply header = %q, want a fresh server-made ID", gotHeader)
	}
	var line struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("parse log line: %v", err)
	}
	if line.RequestID != gotHeader {
		t.Errorf("request_id in log = %q, want the reply header %q", line.RequestID, gotHeader)
	}
}

func TestLogRequestsKeepsSecretsOut(t *testing.T) {
	t.Parallel()
	router, logs := testRouter(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ok?token=query-secret", strings.NewReader(`{"password":"body-secret"}`))
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("Cookie", "session=cookie-secret")
	router.ServeHTTP(httptest.NewRecorder(), req)

	for _, secret := range []string{"query-secret", "body-secret", "header-secret", "cookie-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log contains %q:\n%s", secret, logs.String())
		}
	}
}

// A panic must not produce a summary line claiming success; net/http recovers it and reports it
// through ServerErrorLog as one JSON line.
func TestPanicGoesToServerErrorLogOnly(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	router := newBaseRouter(logger)
	router.Get("/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	server := httptest.NewUnstartedServer(router)
	server.Config.ErrorLog = ServerErrorLog(logger)
	server.Start()
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/panic", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	res, err := server.Client().Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		t.Fatalf("GET /panic = %d, want the connection closed", res.StatusCode)
	}
	server.Close() // waits for the handler goroutine, so its log line is written

	var got []map[string]any
	for raw := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		got = append(got, line)
	}
	if len(got) != 1 {
		t.Fatalf("got %d log lines, want 1 (no summary line):\n%s", len(got), logs.String())
	}
	message, _ := got[0]["message"].(string)
	if got[0]["level"] != "error" || got[0]["source"] != "net/http" || !strings.Contains(message, "panic serving") {
		t.Errorf("server error line = %v, want an error from net/http about the panic", got[0])
	}
}

// The logging middleware wraps the ResponseWriter; handlers must still reach per-request deadlines
// and flushing through http.ResponseController (the wrapper has to expose Unwrap).
func TestResponseControllerWorksThroughLogging(t *testing.T) {
	t.Parallel()
	router := newBaseRouter(zerolog.Nop())
	errs := make(chan error, 2)
	router.Get("/stream", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		errs <- rc.SetWriteDeadline(time.Now().Add(time.Minute))
		errs <- rc.Flush()
	})

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	_ = res.Body.Close()

	for _, step := range []string{"SetWriteDeadline", "Flush"} {
		if err := <-errs; err != nil {
			t.Errorf("%s through the logging middleware: %v", step, err)
		}
	}
}
