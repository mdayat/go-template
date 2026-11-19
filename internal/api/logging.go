package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/mdayat/go-template/internal/api/handler"
)

// maxLoggedText is the most of the path and User-Agent a line keeps. Both are the client's to
// choose (up to the 64 KB header limit), and a 404 needs no sign-in and has no rate limit: kept
// whole, any client could write lines of tens of KB at will.
const maxLoggedText = 512

// statusClientClosed is logged when the client hung up before any reply was written. It's nginx's
// "client closed request"; no reply carries it.
const statusClientClosed = 499

// logRequests writes one summary line per request: error level for 5xx replies and panics, info for
// the rest. Successful health checks and readiness checks aren't logged. The query string, headers
// and bodies are never logged: they can hold tokens and personal data. A handler's returned error
// (see Handle) or panic is attached to this line, so each request is logged exactly once.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		var rec recovery
		rec.serve(next, ww, r)

		status := ww.Status()
		if status == 0 {
			// The handler wrote nothing, so net/http sends 200 — unless the client already hung up,
			// and then nothing reaches it.
			status = http.StatusOK
			if r.Context().Err() != nil {
				status = statusClientClosed
			}
		}
		// Probes come every few seconds; a line each would bury the rest. /readyz logs its own
		// changes (Readiness.serve), and a panic in it is still logged.
		if (r.URL.Path == healthPath && status < http.StatusBadRequest) || (r.URL.Path == readyPath && !rec.panicked) {
			return
		}

		level := zerolog.InfoLevel
		if status >= http.StatusInternalServerError || rec.panicked {
			level = zerolog.ErrorLevel
		}

		httpFields := zerolog.Dict().
			Dict("request", zerolog.Dict().Str("method", r.Method)).
			Dict("response", zerolog.Dict().
				Int("status_code", status).
				Dict("body", zerolog.Dict().Int("bytes", ww.BytesWritten())))
		// The route pattern (/web/tasks/{id}) groups requests by endpoint; a path no route matches has
		// its client's prefix (/web/*), or none outside every prefix.
		if route := chi.RouteContext(r.Context()).RoutePattern(); route != "" {
			httpFields.Str("route", route)
		}

		hlog.FromRequest(r).WithLevel(level).
			Dict("http", httpFields).
			Dict("url", zerolog.Dict().Str("path", truncate(r.URL.Path))).
			Dict("user_agent", zerolog.Dict().Str("original", truncate(r.UserAgent()))).
			Dict("client", clientFields(r)).
			Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond)).
			Msg("request completed")

		if rec.midReply {
			// Part of the reply is already out. Ending normally would let the client take it as
			// complete; this makes net/http cut the connection, without logging it again.
			panic(http.ErrAbortHandler)
		}
	})
}

// recovery is what serve saw: whether the handler panicked, and whether the reply had already
// started when it did.
type recovery struct {
	panicked bool
	midReply bool
}

// serve runs next and recovers a panic: it attaches the panic and stack to the summary line and
// replies 500, or, if the reply had already started, sets midReply so the caller cuts the
// connection after logging. http.ErrAbortHandler, a deliberate abort, passes through untouched.
func (rc *recovery) serve(next http.Handler, ww middleware.WrapResponseWriter, r *http.Request) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			panic(p)
		}

		rc.panicked = true
		hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Dict("error", zerolog.Dict().
				Str("message", fmt.Sprintf("panic: %v", p)).
				Str("stack_trace", string(debug.Stack())))
		})
		if ww.Status() != 0 {
			rc.midReply = true
			return
		}
		handler.WriteProblem(ww, r, handler.NewProblem(http.StatusInternalServerError, handler.InternalErrorDetail))
	}()

	next.ServeHTTP(ww, r)
}

// clientFields names who sent r, so logs can be grouped by caller: the client's IP as clientIP found
// it (the same one the rate limits count), with the connection's port when the connection is the
// client's own rather than a proxy's. When the client couldn't be told, the connection's address.
func clientFields(r *http.Request) *zerolog.Event {
	ip := middleware.GetClientIPAddr(r.Context())
	if !ip.IsValid() {
		return zerolog.Dict().Str("address", r.RemoteAddr)
	}
	fields := zerolog.Dict().Str("ip", ip.String())
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err == nil && peer.Addr().Unmap() == ip {
		fields.Int("port", int(peer.Port()))
	}
	return fields
}

// ServerErrorLog returns a logger for http.Server.ErrorLog, so the server's own errors (a panic in
// middleware that runs before logRequests, a broken connection) reach the JSON log instead of
// plain-text stderr.
func ServerErrorLog(logger zerolog.Logger) *log.Logger {
	return log.New(serverErrorWriter{logger: logger}, "", 0)
}

type serverErrorWriter struct {
	logger zerolog.Logger
}

func (w serverErrorWriter) Write(p []byte) (int, error) {
	w.logger.Error().Str("source", "net/http").Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}

// truncate keeps at most maxLoggedText bytes of s, ending before a letter's start, and marks a cut
// with "...". A letter is at most 4 bytes, so it steps back at most 3: in text that isn't UTF-8
// (bytes with no letter start), it doesn't walk back to nothing.
func truncate(s string) string {
	if len(s) <= maxLoggedText {
		return s
	}
	cut := maxLoggedText
	for cut > maxLoggedText-(utf8.UTFMax-1) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
