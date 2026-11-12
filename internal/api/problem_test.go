package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
)

// problemRouter returns the shared middleware with one route per error case, and the log buffer.
func problemRouter(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs))
	routes := map[string]HandlerFunc{
		"/bad-request": func(http.ResponseWriter, *http.Request) error {
			return apperr.BadRequest("body must be JSON")
		},
		"/invalid": func(http.ResponseWriter, *http.Request) error {
			return apperr.Invalid("the task is invalid",
				apperr.FieldError{Field: "name", Rule: "required", Message: "is required"})
		},
		"/not-found": func(http.ResponseWriter, *http.Request) error {
			return fmt.Errorf("get task 42: %w", apperr.NotFound("task not found"))
		},
		"/conflict": func(http.ResponseWriter, *http.Request) error {
			return fmt.Errorf("create task: %w", apperr.Conflict("a task with this name exists"))
		},
		"/unexpected": func(http.ResponseWriter, *http.Request) error {
			return errors.New("query tasks: connect to db password=hunter2")
		},
		"/after-reply": func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("created"))
			return errors.New("send event: broker down")
		},
		"/cancelled": func(_ http.ResponseWriter, r *http.Request) error {
			if err := r.Context().Err(); err != nil {
				return fmt.Errorf("list tasks: %w", err)
			}
			// A cancel of our own (a timeout we set, a cancelled child context) with the client still here.
			return fmt.Errorf("list tasks: %w", context.Canceled)
		},
	}
	for path, h := range routes {
		router.Get(path, Handle(h))
	}
	router.Post("/tasks", func(http.ResponseWriter, *http.Request) {})
	router.Get("/tasks", func(http.ResponseWriter, *http.Request) {})
	router.Get("/tasks/{id}", func(http.ResponseWriter, *http.Request) {})
	router.Delete("/tasks/{id}", func(http.ResponseWriter, *http.Request) {})
	return router, &logs
}

func TestErrorReplies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		target     string
		clientGone bool
		wantStatus int
		wantReply  *problem // nil: no problem reply
		wantBody   string   // the reply body when wantReply is nil
		wantLevel  string
		wantLogErr string // error.message on the summary line; "" means none
	}{
		{
			name:       "bad request",
			method:     http.MethodGet,
			target:     "/bad-request",
			wantStatus: 400,
			wantReply:  &problem{Type: "about:blank", Title: "Bad Request", Status: 400, Detail: "body must be JSON"},
			wantLevel:  "info",
			wantLogErr: "body must be JSON",
		},
		{
			name:       "invalid input lists the fields",
			method:     http.MethodGet,
			target:     "/invalid",
			wantStatus: 422,
			wantReply: &problem{
				Type: "about:blank", Title: "Unprocessable Entity", Status: 422, Detail: "the task is invalid",
				Errors: []fieldProblem{{Field: "name", Rule: "required", Message: "is required"}},
			},
			wantLevel:  "info",
			wantLogErr: "the task is invalid",
		},
		{
			name:       "not found: reply has the safe detail, log has the wrapped chain",
			method:     http.MethodGet,
			target:     "/not-found",
			wantStatus: 404,
			wantReply:  &problem{Type: "about:blank", Title: "Not Found", Status: 404, Detail: "task not found"},
			wantLevel:  "info",
			wantLogErr: "get task 42: task not found",
		},
		{
			name:       "conflict",
			method:     http.MethodGet,
			target:     "/conflict",
			wantStatus: 409,
			wantReply:  &problem{Type: "about:blank", Title: "Conflict", Status: 409, Detail: "a task with this name exists"},
			wantLevel:  "info",
			wantLogErr: "create task: a task with this name exists",
		},
		{
			name:       "unexpected error: fixed 500 reply, real error only in the log",
			method:     http.MethodGet,
			target:     "/unexpected",
			wantStatus: 500,
			wantReply:  &problem{Type: "about:blank", Title: "Internal Server Error", Status: 500, Detail: "internal server error"},
			wantLevel:  "error",
			wantLogErr: "query tasks: connect to db password=hunter2",
		},
		{
			name:       "error after the reply started keeps the sent reply",
			method:     http.MethodGet,
			target:     "/after-reply",
			wantStatus: 201,
			wantBody:   "created",
			wantLevel:  "info",
			wantLogErr: "send event: broker down",
		},
		{
			name:       "client gone: no reply, logged as 499 at info",
			method:     http.MethodGet,
			target:     "/cancelled",
			clientGone: true,
			wantStatus: 200, // the recorder's default: nothing was written
			wantLevel:  "info",
			wantLogErr: "list tasks: context canceled",
		},
		{
			name:       "our own cancel with the client still here is a 500",
			method:     http.MethodGet,
			target:     "/cancelled",
			wantStatus: 500,
			wantReply:  &problem{Type: "about:blank", Title: "Internal Server Error", Status: 500, Detail: "internal server error"},
			wantLevel:  "error",
			wantLogErr: "list tasks: context canceled",
		},
		{
			name:       "unknown path",
			method:     http.MethodGet,
			target:     "/nope",
			wantStatus: 404,
			wantReply:  &problem{Type: "about:blank", Title: "Not Found", Status: 404, Detail: "no route matches this path"},
			wantLevel:  "info",
		},
		{
			name:       "wrong method",
			method:     http.MethodDelete,
			target:     "/tasks",
			wantStatus: 405,
			wantReply:  &problem{Type: "about:blank", Title: "Method Not Allowed", Status: 405, Detail: "method not allowed on this path"},
			wantLevel:  "info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, logs := problemRouter(t)

			ctx := t.Context()
			if tt.clientGone {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, tt.method, tt.target, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantReply == nil {
				if got := rec.Body.String(); got != tt.wantBody {
					t.Errorf("body = %q, want %q", got, tt.wantBody)
				}
			} else {
				if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
					t.Errorf("Content-Type = %q, want application/problem+json", got)
				}
				var got problem
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("reply is not JSON: %q: %v", rec.Body.String(), err)
				}
				want := *tt.wantReply
				want.RequestID = rec.Header().Get("X-Request-Id")
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("reply mismatch (-want +got):\n%s", diff)
				}
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("reply leaks internal error text: %s", rec.Body.String())
			}

			lines := logLines(t, logs)
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want 1 summary line:\n%v", len(lines), lines)
			}
			if s := summarize(lines[0]); s.level != tt.wantLevel || s.message != tt.wantLogErr {
				t.Errorf("summary line level %q, error %q; want %q, %q", s.level, s.message, tt.wantLevel, tt.wantLogErr)
			}
		})
	}
}

func TestMethodNotAllowedListsAllowedMethods(t *testing.T) {
	t.Parallel()
	router, _ := problemRouter(t)

	tests := []struct {
		name, method, target, wantAllow string
	}{
		{"GET routes answer HEAD too", http.MethodDelete, "/tasks", "GET, HEAD, POST"},
		// chi routes an encoded path by its raw form; Allow must be built from the same form.
		{"encoded slash in the path", http.MethodPut, "/tasks/a%2Fb", "GET, HEAD, DELETE"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.target, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != tt.wantAllow {
			t.Errorf("%s: %s %s = %d, Allow %q; want 405, Allow %q",
				tt.name, tt.method, tt.target, rec.Code, rec.Header().Get("Allow"), tt.wantAllow)
		}
	}
}

func TestHeadIsServedByGetRoutes(t *testing.T) {
	t.Parallel()
	router, _ := problemRouter(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/tasks", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("HEAD /tasks = %d, want 200", rec.Code)
	}
}

func TestRequestsHaveATimeLimit(t *testing.T) {
	t.Parallel()
	router := newBaseRouter(zerolog.Nop())
	router.Get("/deadline", func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > requestTimeout {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/deadline", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the request's context has no deadline of at most %s", rec.Code, requestTimeout)
	}
}

// When the time limit runs out, the caller gets a 503 and the summary line shows it at error level,
// not as a client that hung up (499).
func TestRequestOverTheTimeLimitIs503(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs))
	router.With(limitRequestTime(10*time.Millisecond)).Get("/slow", Handle(func(_ http.ResponseWriter, r *http.Request) error {
		<-r.Context().Done()
		return fmt.Errorf("list tasks: %w", r.Context().Err())
	}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/slow", nil))

	var got problem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply is not JSON: %q: %v", rec.Body, err)
	}
	if rec.Code != http.StatusServiceUnavailable || got.Detail != "the request took too long" {
		t.Errorf("reply = %d %q, want 503 %q", rec.Code, got.Detail, "the request took too long")
	}
	lines := logLines(t, &logs)
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	want := lineSummary{level: "error", status: 503, message: "list tasks: context deadline exceeded"}
	if got := summarize(lines[0]); got != want {
		t.Errorf("summary line = %+v, want %+v", got, want)
	}
}

func TestErrorReplyDropsBodyHeaders(t *testing.T) {
	t.Parallel()
	router := newBaseRouter(zerolog.Nop())
	router.Get("/export", Handle(func(w http.ResponseWriter, _ *http.Request) error {
		// Set up for a gzipped, cacheable file download, then fail before writing it.
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="tasks.csv"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"v1"`)
		return apperr.NotFound("no tasks to export")
	}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/export", nil))

	for _, h := range []string{"Content-Encoding", "Content-Disposition", "Cache-Control", "ETag"} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("problem reply has %s: %q, want it dropped", h, got)
		}
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("problem reply lost X-Request-Id, a middleware header")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

// Not parallel: it swaps zerolog's package-level fallback logger. Parallel tests wait until
// sequential ones finish, so no other test runs meanwhile.
func TestHandleOutsideRouterLeavesSharedLoggerAlone(t *testing.T) {
	var out bytes.Buffer
	shared := zerolog.New(&out)
	previous := zerolog.DefaultContextLogger
	zerolog.DefaultContextLogger = &shared // what main does
	t.Cleanup(func() { zerolog.DefaultContextLogger = previous })

	// A plain mux: no logging middleware, so the request carries no logger of its own.
	mux := http.NewServeMux()
	mux.Handle("/x", Handle(func(http.ResponseWriter, *http.Request) error {
		return errors.New("db down")
	}))
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
	shared.Info().Msg("an unrelated line, later")

	var lines []map[string]any
	for raw := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want the error's own line and the later line:\n%s", len(lines), out.String())
	}
	if s := summarize(lines[0]); s.level != "error" || s.message != "db down" {
		t.Errorf("first line = %+v, want an error line with message \"db down\"", s)
	}
	if _, ok := lines[1]["error"]; ok {
		t.Errorf("a later, unrelated line carries the request's error: %v", lines[1])
	}
}
