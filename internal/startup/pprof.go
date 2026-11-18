package startup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

// MaxProfileSeconds is the longest CPU profile or execution trace ServeProfiles takes.
const MaxProfileSeconds = 60

// CheckProfileAddr reports whether addr (PPROF_ADDR) may serve profiles: "" (off), or a host:port
// on this machine only (127.0.0.1, ::1, localhost). Profiles show the program's insides, its
// stacks and memory, so they're never on the network; reach them with a port forward.
func CheckProfileAddr(addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err == nil && host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); err == nil && ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("PPROF_ADDR must be a host:port on this machine, like 127.0.0.1:6060, or unset")
}

// ServeProfiles serves Go's profiles at /debug/pprof/ on addr until the returned func stops it;
// addr "" serves nothing. addr must pass CheckProfileAddr, and the port it binds must be on this
// machine (a hosts file can point "localhost" elsewhere). Not cmdline: it shows the program's
// arguments. A profile may last up to MaxProfileSeconds.
//
// Importing net/http/pprof also registers these pages on http.DefaultServeMux, so no server in the
// program may serve that one: each has its own router.
func ServeProfiles(ctx context.Context, addr string, logger zerolog.Logger) (func(), error) {
	if addr == "" {
		return func() {}, nil
	}
	if err := CheckProfileAddr(addr); err != nil {
		return nil, err
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for profiles on %s: %w", addr, err)
	}
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = listener.Close()
		return nil, fmt.Errorf("profiles: %s is bound to %s, not this machine only", addr, listener.Addr())
	}
	server := &http.Server{
		Handler:           profileHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// For the pages without ?seconds=; pprof adds a profile's seconds to it itself.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  time.Minute,
	}
	// Warn, so profiling isn't left on unnoticed.
	logger.Warn().Str("addr", listener.Addr().String()).Msg("profiling on")
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error().Err(err).Msg("profiles stopped")
		}
	}()
	return func() { _ = server.Close() }, nil
}

func profileHandler() http.Handler {
	mux := http.NewServeMux()
	// Index also serves each named profile: /debug/pprof/heap, goroutine, allocs, block, mutex.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/pprof/cmdline", http.NotFound)
	return limitRequests(mux)
}

// maxSymbolBody is the most a POST to symbol may send: pprof reads it all, and keeps its answer in
// memory, before replying.
const maxSymbolBody = 1 << 20

// limitRequests lets through GET and HEAD (and POST for symbol, which takes addresses in its body,
// up to maxSymbolBody), with seconds, if any, up to MaxProfileSeconds: pprof reads it from the
// query or a form body, on every profile (a heap or block profile with seconds= is a difference
// over that time). Whole seconds: only trace takes fractions; profile would quietly run its
// default 30 s, and the others refuse them.
func limitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet, r.Method == http.MethodHead:
		case r.Method == http.MethodPost && r.URL.Path == "/debug/pprof/symbol":
			if r.ContentLength > maxSymbolBody {
				http.Error(w, "too many addresses", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxSymbolBody)
			next.ServeHTTP(w, r)
			return
		default:
			http.Error(w, "only GET", http.StatusMethodNotAllowed)
			return
		}
		if s := r.FormValue("seconds"); s != "" {
			n, err := strconv.ParseFloat(s, 64)
			whole := n == math.Trunc(n) || r.URL.Path == "/debug/pprof/trace"
			if err != nil || !(n > 0 && n <= MaxProfileSeconds) || !whole {
				http.Error(w, fmt.Sprintf("seconds must be a whole number from 1 to %d (a trace may take fractions)", MaxProfileSeconds), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
