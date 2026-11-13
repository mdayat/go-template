package api

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

// HandlerFunc is an HTTP handler that returns its failure instead of writing it.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Handle adapts h to an http.HandlerFunc. A returned error becomes an RFC 9457 reply and is attached
// to the request's summary log line; nothing else logs it.
func Handle(h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeError(w, r, err)
		}
	}
}

// problem is an RFC 9457 "problem details" reply body. request_id and errors are extension members.
type problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Errors    []fieldProblem `json:"errors,omitempty"`
}

type fieldProblem struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

const internalErrorDetail = "internal server error"

// statusError is a failure that only exists in HTTP (an oversized body, a wrong Content-Type), so it
// isn't an apperr kind. Its detail is safe to show the caller.
type statusError struct {
	status int
	detail string
}

func (e *statusError) Error() string {
	return e.detail
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
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
	// The request's own time limit ran out (limitRequestTime): our failure, and one a client may
	// retry, unlike a plain 500.
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		p = newProblem(http.StatusServiceUnavailable, "the request took too long")
	}
	writeProblem(w, r, p)
}

// problemFor maps an error to its reply. Only a *statusError's or *apperr.Error's detail reaches the
// caller; any other error is a 500 with a fixed message, since its text may hold internal details.
func problemFor(err error) problem {
	if statusErr, ok := errors.AsType[*statusError](err); ok {
		return newProblem(statusErr.status, statusErr.detail)
	}
	appErr, ok := errors.AsType[*apperr.Error](err)
	if !ok {
		return newProblem(http.StatusInternalServerError, internalErrorDetail)
	}

	switch appErr.Kind {
	case apperr.KindBadRequest:
		return newProblem(http.StatusBadRequest, appErr.Detail)
	case apperr.KindInvalid:
		p := newProblem(http.StatusUnprocessableEntity, appErr.Detail)
		for _, f := range appErr.Fields {
			p.Errors = append(p.Errors, fieldProblem(f))
		}
		return p
	case apperr.KindNotFound:
		return newProblem(http.StatusNotFound, appErr.Detail)
	case apperr.KindConflict:
		return newProblem(http.StatusConflict, appErr.Detail)
	}
	// After the switch, not as its default: with no default case the linter (exhaustive) fails until
	// a new kind gets its own case. Only a zero or made-up Kind gets here.
	return newProblem(http.StatusInternalServerError, internalErrorDetail)
}

func newProblem(status int, detail string) problem {
	// "about:blank" means the status code says it all (RFC 9457 section 4.2.1).
	return problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail}
}

func writeProblem(w http.ResponseWriter, r *http.Request, p problem) {
	// Drop headers describing a reply body. A handler may set them and then fail; left on the problem
	// reply they'd misdescribe it (a gzip Content-Encoding on plain JSON breaks the client, a
	// success's Cache-Control lets a proxy cache the error). Middleware headers like X-Request-Id stay.
	for _, h := range [...]string{
		"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Content-Length",
		"Content-Range", "ETag", "Expires", "Last-Modified",
	} {
		w.Header().Del(h)
	}
	if id, ok := hlog.IDFromRequest(r); ok {
		p.RequestID = id.String()
	}
	writeJSON(w, p.Status, "application/problem+json", p)
}

// writeJSON writes v as the reply. All JSON replies go through here, so each gets the same headers.
func writeJSON(w http.ResponseWriter, status int, contentType string, v any) {
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

// recordError attaches err to the request's summary log line (written by logRequests). A request
// without its own logger (a route outside newBaseRouter) has no summary line, and zerolog.Ctx then
// returns the shared fallback logger: changing that would stamp this error on every later line. So
// the error gets a line of its own instead.
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

// responseStarted reports whether a status has already been sent. w is logRequests' wrapper on every
// routed request; without it, assume nothing was sent.
func responseStarted(w http.ResponseWriter) bool {
	ww, ok := w.(middleware.WrapResponseWriter)
	return ok && ww.Status() != 0
}

// notFound replies 404 for paths no route matches.
func notFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, newProblem(http.StatusNotFound, "no route matches this path"))
}

// methodNotAllowed replies 405 with the Allow header chi's own 405 reply would set; chi adds that
// header only in its built-in handler, so it's rebuilt here by asking the router about each method.
func methodNotAllowed(router chi.Routes) http.HandlerFunc {
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
			// GET routes answer HEAD too (middleware.GetHead in newBaseRouter).
			getAnswersHead := method == http.MethodHead && slices.Contains(allowed, http.MethodGet)
			if getAnswersHead || router.Match(chi.NewRouteContext(), method, path) {
				allowed = append(allowed, method)
			}
		}
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeProblem(w, r, newProblem(http.StatusMethodNotAllowed, "method not allowed on this path"))
	}
}
