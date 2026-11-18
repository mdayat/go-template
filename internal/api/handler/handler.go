// Package handler holds what every client's handlers share (internal/api/mobile, web, admin):
// turning a returned error into a reply, reading request bodies, writing JSON, the signed-in user,
// paging and path IDs. The mobile apps depend on all of it: a change here to a reply or to what a
// request accepts must only add (docs/standards/http-api.md).
package handler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/json"
)

// Func is an HTTP handler that returns its failure instead of writing it.
type Func func(w http.ResponseWriter, r *http.Request) error

// Handle adapts h to an http.HandlerFunc. A returned error becomes an RFC 9457 reply and is attached
// to the request's summary log line; nothing else logs it.
func Handle(h Func) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, r, err)
		}
	}
}

// Problem is an RFC 9457 "problem details" reply body. request_id and errors are extension members.
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Errors    []FieldProblem `json:"errors,omitempty"`
}

// FieldProblem is one field a request got wrong.
type FieldProblem struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// InternalErrorDetail is the detail of every 500: the error itself may hold internal details.
const InternalErrorDetail = "internal server error"

// StatusError is a failure that only exists in HTTP (an oversized body, a wrong Content-Type, a
// rate limit), so it isn't an apperr kind. Its Detail is safe to show the caller.
type StatusError struct {
	Status int
	Detail string
}

func (e *StatusError) Error() string {
	return e.Detail
}

// WriteError replies to a failed request with err's problem (see problemFor) and attaches err to
// the request's summary log line.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	recordError(r, err)

	// The client is gone (net/http cancels the context when it disconnects): nobody would read a
	// reply, and writing none makes the summary line show 499 instead of a 5xx.
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}
	if responseStarted(w) {
		// Headers are already sent; a second status can't be sent.
		return
	}
	p := problemFor(err)
	// The request's own time limit ran out (the router's limit): our failure, and one a client may
	// retry, unlike a plain 500.
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		p = NewProblem(http.StatusServiceUnavailable, "the request took too long")
	}
	WriteProblem(w, r, p)
}

// problemFor maps an error to its reply. Only a *StatusError's or *apperr.Error's detail reaches the
// caller; any other error is a 500 with a fixed message, since its text may hold internal details.
func problemFor(err error) Problem {
	if statusErr, ok := errors.AsType[*StatusError](err); ok {
		return NewProblem(statusErr.Status, statusErr.Detail)
	}
	appErr, ok := errors.AsType[*apperr.Error](err)
	if !ok {
		return NewProblem(http.StatusInternalServerError, InternalErrorDetail)
	}

	switch appErr.Kind {
	case apperr.KindBadRequest:
		return NewProblem(http.StatusBadRequest, appErr.Detail)
	case apperr.KindInvalid:
		p := NewProblem(http.StatusUnprocessableEntity, appErr.Detail)
		for _, f := range appErr.Fields {
			p.Errors = append(p.Errors, FieldProblem(f))
		}
		return p
	case apperr.KindNotFound:
		return NewProblem(http.StatusNotFound, appErr.Detail)
	case apperr.KindConflict:
		return NewProblem(http.StatusConflict, appErr.Detail)
	case apperr.KindUnauthenticated:
		return NewProblem(http.StatusUnauthorized, appErr.Detail)
	case apperr.KindForbidden:
		return NewProblem(http.StatusForbidden, appErr.Detail)
	}
	// After the switch, not as its default: with no default case the linter (exhaustive) fails until
	// a new kind gets its own case. Only a zero or made-up Kind gets here.
	return NewProblem(http.StatusInternalServerError, InternalErrorDetail)
}

// NewProblem is the reply for status with detail.
func NewProblem(status int, detail string) Problem {
	// "about:blank" means the status code says it all (RFC 9457 section 4.2.1).
	return Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail}
}

// WriteProblem writes p as the reply, with the request's ID.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	// Drop headers describing a reply body. A handler may set them and then fail; left on the problem
	// reply they'd misdescribe it (a gzip Content-Encoding on plain JSON breaks the client). Middleware
	// headers like X-Request-Id stay.
	for _, h := range [...]string{
		"Content-Disposition", "Content-Encoding", "Content-Language", "Content-Length",
		"Content-Range", "ETag", "Expires", "Last-Modified",
	} {
		w.Header().Del(h)
	}
	// An error is never cached, even where the handler allowed caching its success.
	w.Header().Set("Cache-Control", "no-store")
	if id, ok := hlog.IDFromRequest(r); ok {
		p.RequestID = id.String()
	}
	WriteJSON(w, p.Status, "application/problem+json", p)
}

// WriteJSON writes v as the reply. All JSON replies go through here, so each gets the same headers.
func WriteJSON(w http.ResponseWriter, status int, contentType string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Only our own reply types reach here, and they always marshal; treat a failure as a bug.
		panic("marshal reply: " + err.Error())
	}
	w.Header().Set("Content-Type", contentType)
	// Replies can echo client input (a field name in an error); nosniff stops a browser from
	// guessing they're HTML and running them.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// A write error means the client is gone; there's no one left to tell.
	_, _ = w.Write(body)
}

// recordError attaches err to the request's summary log line (written by the router's request
// log). A request without its own logger (a route outside the router) has no summary line, and
// zerolog.Ctx then returns the shared fallback logger: changing that would stamp this error on
// every later line. So the error gets a line of its own instead.
func recordError(r *http.Request, err error) {
	logger := zerolog.Ctx(r.Context())
	if logger == zerolog.DefaultContextLogger {
		logger.Error().Dict("error", zerolog.Dict().Str("message", err.Error())).Msg("request failed")
		return
	}
	logger.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Dict("error", zerolog.Dict().Str("message", err.Error()))
	})
}

// responseStarted reports whether a status has already been sent. w is the request log's wrapper on
// every routed request; without it, assume nothing was sent.
func responseStarted(w http.ResponseWriter) bool {
	ww, ok := w.(middleware.WrapResponseWriter)
	return ok && ww.Status() != 0
}

// NotFound replies 404 for paths no route matches.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, NewProblem(http.StatusNotFound, "no route matches this path"))
}

// MethodNotAllowed replies 405 with the Allow header chi's own 405 reply would set; chi adds that
// header only in its built-in handler, so it's rebuilt here by asking the router about each method.
func MethodNotAllowed(router chi.Routes) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The path chi routed on: the raw form when the request has one (an encoded "/" as %2F), so
		// the match below sees the same path.
		path := r.URL.RawPath
		if path == "" {
			path = r.URL.Path
		}
		var allowed []string
		for _, method := range []string{
			http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
			http.MethodPatch, http.MethodDelete, http.MethodOptions,
		} {
			// GET routes answer HEAD too (middleware.GetHead in the router).
			getAnswersHead := method == http.MethodHead && slices.Contains(allowed, http.MethodGet)
			if getAnswersHead || router.Match(chi.NewRouteContext(), method, path) {
				allowed = append(allowed, method)
			}
		}
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		WriteProblem(w, r, NewProblem(http.StatusMethodNotAllowed, "method not allowed on this path"))
	}
}
