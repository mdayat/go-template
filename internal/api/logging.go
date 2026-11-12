package api

import (
	stdlog "log"
	"net"
	"net/http"
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

// logRequests writes one summary line per request after the handler returns: error level for 5xx
// replies, info for the rest. Successful health checks aren't logged. The query string, headers and
// bodies are never logged: they can hold tokens and personal data.
//
// It logs only when the handler returns normally, not from a defer: after a panic no status has been
// written yet, so the line would claim a 200. The server's error log records the panic instead.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

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
		if status >= http.StatusInternalServerError {
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
	})
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

// ServerErrorLog returns a logger for http.Server.ErrorLog, so the server's own errors (a handler
// panic it recovered, a broken connection) reach the JSON log instead of plain-text stderr.
func ServerErrorLog(logger zerolog.Logger) *stdlog.Logger {
	return stdlog.New(serverErrorWriter{logger: logger}, "", 0)
}

type serverErrorWriter struct {
	logger zerolog.Logger
}

func (w serverErrorWriter) Write(p []byte) (int, error) {
	w.logger.Error().Str("source", "net/http").Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}
