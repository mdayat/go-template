package api

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

// statusClientClosed is logged when the client hung up before any reply was written. It's nginx's
// "client closed request"; no reply carries it.
const statusClientClosed = 499

// logRequests writes one summary line per request: error level for 5xx replies and panics, info for
// the rest. Successful health checks aren't logged. The query string, headers and bodies are never
// logged: they can hold tokens and personal data. A handler's returned error (see Handle) or panic
// is attached to this line, so each request is logged exactly once.
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
		if r.URL.Path == healthPath && status < http.StatusBadRequest {
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
		// The route pattern (/users/{id}) groups requests by endpoint; it's empty when nothing matched.
		if route := chi.RouteContext(r.Context()).RoutePattern(); route != "" {
			httpFields.Str("route", route)
		}

		hlog.FromRequest(r).WithLevel(level).
			Dict("http", httpFields).
			Dict("url", zerolog.Dict().Str("path", r.URL.Path)).
			Dict("user_agent", zerolog.Dict().Str("original", r.UserAgent())).
			Dict("client", clientFields(r.RemoteAddr)).
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
		writeProblem(ww, r, newProblem(http.StatusInternalServerError, internalErrorDetail))
	}()

	next.ServeHTTP(ww, r)
}

// clientFields splits the connection's address into ip and port, so logs can be grouped by caller.
func clientFields(remoteAddr string) *zerolog.Event {
	host, port, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return zerolog.Dict().Str("address", remoteAddr)
	}
	fields := zerolog.Dict().Str("ip", host)
	if n, err := strconv.Atoi(port); err == nil {
		fields.Int("port", n)
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
