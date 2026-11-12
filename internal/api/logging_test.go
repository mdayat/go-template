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
	// Replaces the base router's health check (chi keeps the last route registered for a path), so
	// a test can make it fail with ?down.
	router.Get(healthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("down") {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	return router, &logs
}

// anyNumber stands in for a duration_ms that is a number, whatever its value.
const anyNumber = "<a number>"

// logLines parses each JSON log line. request_id, new on every run, is dropped; a numeric
// duration_ms becomes anyNumber, so a missing or non-numeric one still fails a comparison.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", scanner.Text(), err)
		}
		if _, ok := line["duration_ms"].(float64); ok {
			line["duration_ms"] = anyNumber
		}
		delete(line, "request_id")
		lines = append(lines, line)
	}
	return lines
}

// lineSpec is the part of a summary line that differs between test cases.
type lineSpec struct {
	level  string
	route  string
	status int
	size   int
}

// replySize as a lineSpec size means "the size of the reply the client got".
const replySize = -1

func summaryLine(level, path, route string, status, size int) map[string]any {
	httpFields := map[string]any{
		"request":  map[string]any{"method": http.MethodGet},
		"response": map[string]any{"status_code": float64(status), "body": map[string]any{"bytes": float64(size)}},
	}
	if route != "" {
		httpFields["route"] = route
	}
	return map[string]any{
		"level":       level,
		"http":        httpFields,
		"url":         map[string]any{"path": path},
		"user_agent":  map[string]any{"original": "test-agent"},
		"client":      map[string]any{"ip": "192.0.2.1", "port": float64(1234)},
		"duration_ms": anyNumber,
		"message":     "request completed",
	}
}

func TestLogRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     string
		clientGone bool
		want       []lineSpec
	}{
		{
			name:   "success is logged at info with the route",
			target: "/ok",
			want:   []lineSpec{{"info", "/ok", 200, 5}},
		},
		{
			name:   "route pattern groups requests, path keeps the ID",
			target: "/users/42",
			want:   []lineSpec{{"info", "/users/{id}", 204, 0}},
		},
		{
			name:   "5xx is logged at error",
			target: "/fail",
			want:   []lineSpec{{"error", "/fail", 500, 0}},
		},
		{
			name:   "unknown path is a 4xx at info with no route",
			target: "/nope",
			want:   []lineSpec{{"info", "", 404, replySize}},
		},
		{
			name:   "handler that writes nothing is logged as the 200 net/http sends",
			target: "/silent",
			want:   []lineSpec{{"info", "/silent", 200, 0}},
		},
		{
			name:       "client that hung up before any reply is logged as 499, not 200",
			target:     "/silent",
			clientGone: true,
			want:       []lineSpec{{"info", "/silent", 499, 0}},
		},
		{
			name:   "successful health check is not logged",
			target: healthPath,
			want:   nil,
		},
		{
			name:   "failing health check is logged",
			target: healthPath + "?down",
			want:   []lineSpec{{"error", healthPath, 503, 0}},
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
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			var want []map[string]any
			for _, s := range tt.want {
				size := s.size
				if size == replySize {
					size = rec.Body.Len()
				}
				want = append(want, summaryLine(s.level, req.URL.Path, s.route, s.status, size))
			}
			if diff := cmp.Diff(want, logLines(t, logs)); diff != "" {
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

// served is what a client and the log saw for one request.
type served struct {
	status      int
	contentType string
	body        []byte
	err         error // sending the request or reading the reply failed
	lines       []map[string]any
}

// serveOnce sends one GET through a real server whose ErrorLog shares the router's log, then waits
// for the handler to finish.
func serveOnce(t *testing.T, path string, handler http.HandlerFunc) served {
	t.Helper()
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	router := newBaseRouter(logger)
	router.Get(path, handler)

	server := httptest.NewUnstartedServer(router)
	server.Config.ErrorLog = ServerErrorLog(logger)
	server.Start()
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	var got served
	res, err := server.Client().Do(req)
	if err == nil {
		got.status, got.contentType = res.StatusCode, res.Header.Get("Content-Type")
		got.body, err = io.ReadAll(res.Body)
		_ = res.Body.Close()
	}
	got.err = err
	server.Close() // waits for the handler, so its log lines are written

	for raw := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		got.lines = append(got.lines, line)
	}
	return got
}

// lineSummary is the level, status and error fields of a summary line.
type lineSummary struct {
	level   string
	status  float64
	message string
	stack   string
}

func summarize(line map[string]any) lineSummary {
	var s lineSummary
	s.level, _ = line["level"].(string)
	httpFields, _ := line["http"].(map[string]any)
	response, _ := httpFields["response"].(map[string]any)
	s.status, _ = response["status_code"].(float64)
	errFields, _ := line["error"].(map[string]any)
	s.message, _ = errFields["message"].(string)
	s.stack, _ = errFields["stack_trace"].(string)
	return s
}

func TestPanicBeforeReplyBecomes500(t *testing.T) {
	t.Parallel()
	got := serveOnce(t, "/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	if got.err != nil {
		t.Fatalf("GET /panic: %v", got.err)
	}

	if got.status != http.StatusInternalServerError || got.contentType != "application/problem+json" {
		t.Errorf("reply = %d %q, want 500 application/problem+json", got.status, got.contentType)
	}
	if strings.Contains(string(got.body), "boom") {
		t.Errorf("reply leaks the panic value: %s", got.body)
	}
	if len(got.lines) != 1 {
		t.Fatalf("got %d log lines, want 1 summary line:\n%v", len(got.lines), got.lines)
	}
	s := summarize(got.lines[0])
	if s.level != "error" || s.status != 500 || s.message != "panic: boom" || !strings.Contains(s.stack, "logging_test.go") {
		t.Errorf("summary line = %+v, want error level, 500, \"panic: boom\", stack naming logging_test.go", s)
	}
}

func TestPanicMidReplyCutsTheConnection(t *testing.T) {
	t.Parallel()
	got := serveOnce(t, "/panic-mid", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		_ = http.NewResponseController(w).Flush()
		panic("mid")
	})

	// Without the cut, the client would read "partial" as a complete 200 reply.
	if got.err == nil {
		t.Errorf("client read %q without error, want the connection cut", got.body)
	}
	if len(got.lines) != 1 {
		t.Fatalf("got %d log lines, want 1 summary line (net/http must not log the abort):\n%v", len(got.lines), got.lines)
	}
	if s := summarize(got.lines[0]); s.level != "error" || s.status != 200 || s.message != "panic: mid" {
		t.Errorf("summary line = %+v, want error level, 200, \"panic: mid\"", s)
	}
}

func TestAbortHandlerPassesThrough(t *testing.T) {
	t.Parallel()
	got := serveOnce(t, "/abort", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})

	if got.err == nil {
		t.Errorf("client got a %d reply, want the connection cut", got.status)
	}
	if len(got.lines) != 0 {
		t.Errorf("got log lines for a deliberate abort, want none:\n%v", got.lines)
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
