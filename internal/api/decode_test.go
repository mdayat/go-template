package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"
)

type testTask struct {
	Name     string     `json:"name" validate:"required,max=10"`
	Priority int        `json:"priority" validate:"gte=1,lte=5"`
	Email    string     `json:"email" validate:"omitempty,email"`
	Status   string     `json:"status" validate:"omitempty,oneof=todo done"`
	Tags     []string   `json:"tags" validate:"max=2"`
	Items    []testItem `json:"items" validate:"dive"`
	// One field per JSON type, for the "wrong type" messages.
	Done   bool              `json:"done"`
	Weight float64           `json:"weight"`
	Meta   map[string]string `json:"meta"`
}

type testItem struct {
	Title string `json:"title" validate:"required"`
}

// testLimits has one limit per kind of value, to check how each limit is worded.
type testLimits struct {
	Title string   `json:"title" validate:"min=3"`
	Tags  []string `json:"tags" validate:"len=2"`
	Score int      `json:"score" validate:"gt=0"`
	Site  string   `json:"site" validate:"omitempty,url"`
	Ref   string   `json:"ref" validate:"omitempty,uuid"`
	Small int      `json:"small" validate:"lt=10"`
	Code  string   `json:"code" validate:"omitempty,alpha"` // a rule ruleMessage has no wording for
}

// decodeRouter echoes a decoded testTask, so a test sees exactly what decode produced.
func decodeRouter(t *testing.T) http.Handler {
	t.Helper()
	router := newBaseRouter(zerolog.Nop())
	decoder := newBodyDecoder()
	router.Post("/tasks", Handle(func(w http.ResponseWriter, r *http.Request) error {
		var task testTask
		if err := decoder.decode(w, r, &task); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, "application/json", task)
		return nil
	}))
	router.Post("/limits", Handle(func(w http.ResponseWriter, r *http.Request) error {
		var limits testLimits
		return decoder.decode(w, r, &limits)
	}))
	router.Post("/not-a-pointer", Handle(func(w http.ResponseWriter, r *http.Request) error {
		var task testTask
		return decoder.decode(w, r, task)
	}))
	return router
}

func postJSON(t *testing.T, router http.Handler, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestDecodeAcceptsValidBody(t *testing.T) {
	t.Parallel()
	body := `{"name":"write docs","priority":3,"email":"a@b.co","status":"todo","tags":["x"],"items":[{"title":"draft"}]}`

	for _, contentType := range []string{"application/json", "application/json; charset=utf-8"} {
		rec := postJSON(t, decodeRouter(t), "/tasks", contentType, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("Content-Type %q: status = %d, want 200; body %s", contentType, rec.Code, rec.Body)
		}
		var got testTask
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("reply is not JSON: %v", err)
		}
		want := testTask{
			Name: "write docs", Priority: 3, Email: "a@b.co", Status: "todo",
			Tags: []string{"x"}, Items: []testItem{{Title: "draft"}},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Content-Type %q: decoded task mismatch (-want +got):\n%s", contentType, diff)
		}
	}
}

func TestDecodeRejectsBadBodies(t *testing.T) {
	t.Parallel()
	valid := `{"name":"a","priority":1}`

	tests := []struct {
		name        string
		path        string
		contentType string
		body        string
		wantStatus  int
		wantDetail  string
	}{
		{"no Content-Type", "/tasks", "", valid, 415, "Content-Type must be application/json"},
		{"form Content-Type", "/tasks", "application/x-www-form-urlencoded", valid, 415, "Content-Type must be application/json"},
		{"empty body", "/tasks", "application/json", "", 400, "request body is empty"},
		{"whitespace only", "/tasks", "application/json", " \n ", 400, "request body is empty"},
		{"array instead of object", "/tasks", "application/json", `[]`, 400, "request body must be a JSON object"},
		{"string instead of object", "/tasks", "application/json", `"text"`, 400, "request body must be a JSON object"},
		{"null passes no fields", "/tasks", "application/json", ` null`, 400, "request body must be a JSON object"},
		{"syntax error", "/tasks", "application/json", `{"name" "a"}`, 400, "request body is not valid JSON (at byte 9)"},
		{"cut-off body", "/tasks", "application/json", `{"name":"a"`, 400, "request body is not valid JSON (it ends too early)"},
		{"wrong type", "/tasks", "application/json", `{"name":"a","priority":"high"}`, 400, `field "priority" must be a whole number`},
		{"wrong type: boolean", "/tasks", "application/json", `{"name":"a","priority":1,"done":"yes"}`, 400, `field "done" must be true or false`},
		{"wrong type: number", "/tasks", "application/json", `{"name":"a","priority":1,"weight":"heavy"}`, 400, `field "weight" must be a number`},
		{"wrong type: array", "/tasks", "application/json", `{"name":"a","priority":1,"tags":"x"}`, 400, `field "tags" must be an array`},
		{"wrong type: object", "/tasks", "application/json", `{"name":"a","priority":1,"meta":[1]}`, 400, `field "meta" must be an object`},
		{"wrong type inside a list", "/tasks", "application/json", `{"name":"a","priority":1,"items":[{"title":5}]}`, 400, `field "items[0].title" must be a string`},
		{"unknown field", "/tasks", "application/json", `{"name":"a","priority":1,"colour":"red"}`, 400, `unknown field "colour"`},
		{"two JSON values", "/tasks", "application/json", valid + valid, 400, "request body must hold a single JSON value"},
		{"too large", "/tasks", "application/json", `{"name":"` + strings.Repeat("a", maxBodyBytes) + `"}`, 413, "request body must be at most 1 MB"},
		{"our bug: not a pointer", "/not-a-pointer", "application/json", valid, 500, "internal server error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := postJSON(t, decodeRouter(t), tt.path, tt.contentType, tt.body)

			var got problem
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("reply is not JSON: %q: %v", rec.Body.String(), err)
			}
			if rec.Code != tt.wantStatus || got.Status != tt.wantStatus || got.Detail != tt.wantDetail {
				t.Errorf("reply = %d %q, want %d %q", rec.Code, got.Detail, tt.wantStatus, tt.wantDetail)
			}
		})
	}
}

func TestDecodeReportsRuleBreaksByJSONName(t *testing.T) {
	t.Parallel()
	body := `{"priority":9,"email":"nope","status":"later","tags":["a","b","c"],"items":[{"title":""}]}`

	rec := postJSON(t, decodeRouter(t), "/tasks", "application/json", body)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	var got problem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	want := []fieldProblem{
		{Field: "name", Rule: "required", Message: "is required"},
		{Field: "priority", Rule: "lte", Message: "must be at most 5"},
		{Field: "email", Rule: "email", Message: "must be an email address"},
		{Field: "status", Rule: "oneof", Message: "must be one of: todo, done"},
		{Field: "tags", Rule: "max", Message: "must be at most 2 items"},
		{Field: "items[0].title", Rule: "required", Message: "is required"},
	}
	if got.Detail != "request body is invalid" {
		t.Errorf("detail = %q, want %q", got.Detail, "request body is invalid")
	}
	if diff := cmp.Diff(want, got.Errors); diff != "" {
		t.Errorf("field errors mismatch (-want +got):\n%s", diff)
	}
}

func TestRuleMessageWordsLimitsByKind(t *testing.T) {
	t.Parallel()
	rec := postJSON(t, decodeRouter(t), "/limits", "application/json", `{"title":"ab","tags":["x"],"score":0,"site":"not a url","ref":"x","small":10,"code":"a1"}`)

	var reply problem
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("reply is not JSON: %q: %v", rec.Body.String(), err)
	}
	got := map[string]string{}
	for _, f := range reply.Errors {
		got[f.Field] = f.Message
	}
	want := map[string]string{
		"title": "must be at least 3 characters long",
		"tags":  "must be exactly 2 items",
		"score": "must be more than 0",
		"site":  "must be a URL",
		"ref":   "must be a UUID",
		"small": "must be less than 10",
		"code":  "is invalid",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("messages mismatch (-want +got):\n%s", diff)
	}
}

// A JSON parser that recurses per nesting level can run out of stack on a deep body, and a stack
// overflow kills the whole process: recovery can't catch it. go-json caps the depth; this guards
// against a library swap or upgrade that doesn't.
func TestDecodeSurvivesDeepNesting(t *testing.T) {
	t.Parallel()
	depth := maxBodyBytes/2 - 20
	body := `{"name":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`

	rec := postJSON(t, decodeRouter(t), "/tasks", "application/json", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %.200s", rec.Code, rec.Body)
	}
}

// errAfter is a request body that breaks partway, like a client that announced more than it sent.
type errAfter struct{ data *strings.Reader }

func (e errAfter) Read(p []byte) (int, error) {
	if e.data.Len() == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	return e.data.Read(p)
}

func TestDecodeBodyThatBreaksMidRead(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	router := newBaseRouter(zerolog.New(&logs))
	decoder := newBodyDecoder()
	router.Post("/tasks", Handle(func(w http.ResponseWriter, r *http.Request) error {
		var task testTask
		return decoder.decode(w, r, &task)
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/tasks", errAfter{strings.NewReader(`{"name":`)})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var got problem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply is not JSON: %q: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusBadRequest || got.Detail != "request body can't be read" {
		t.Errorf("reply = %d %q, want 400 \"request body can't be read\"", rec.Code, got.Detail)
	}
	// The client sees the fixed detail; the log keeps the cause.
	if !strings.Contains(logs.String(), "unexpected EOF") {
		t.Errorf("log lacks the read error's cause:\n%s", logs.String())
	}
}
