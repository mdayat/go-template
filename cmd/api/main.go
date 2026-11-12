// Command api runs the HTTP API server.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/mdayat/go-template/internal/api"
	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/db"
)

const (
	dbConnectTimeout = 5 * time.Second
	// Long enough for in-flight requests to finish (WriteTimeout caps each one).
	shutdownTimeout = 15 * time.Second
)

// buildVersion is set at build time with -ldflags "-X main.buildVersion=...", for builds that can't
// see .git (the Docker build).
//
//nolint:gochecknoglobals // -X can only set a package-level variable
var buildVersion string

func main() {
	if err := run(); err != nil {
		log.Error().Err(err).Msg("api stopped")
		os.Exit(1)
	}
}

// run starts the server and blocks until SIGINT/SIGTERM or a server failure. Every startup and
// shutdown failure is returned, so main logs it once.
func run() error {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogger(cfg)

	pool, err := connectDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	server := &http.Server{
		Handler:           api.NewRouter(log.Logger, api.NewServices(db.New(pool))),
		ErrorLog:          api.ServerErrorLog(log.Logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Bind before logging, so "listening" is only logged once the port is ours.
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	log.Info().Str("addr", listener.Addr().String()).Str("version", version()).Msg("api listening")

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}
	// Release the signal handler so a second Ctrl-C kills a stuck shutdown right away.
	stop()

	log.Info().Msg("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// The server stops first so no request is cut off mid-query; the deferred pool.Close runs after.
	if err := server.Shutdown(shutdownCtx); err != nil {
		// Requests still running: closing their connections cancels their contexts, which stops
		// their queries and returns their connections. Otherwise the deferred pool.Close waits for
		// them forever and the process never exits.
		return errors.Join(fmt.Errorf("shut down http server: %w", err), server.Close())
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}

	log.Info().Msg("api stopped cleanly")
	return nil
}

func setupLogger(cfg config.Config) {
	zerolog.SetGlobalLevel(cfg.LogLevel)
	if cfg.AppEnv == config.EnvDevelopment {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}
	// zerolog.Ctx on a context without a logger returns this one; the default drops every line.
	zerolog.DefaultContextLogger = &log.Logger
}

// version is buildVersion if set, else the git commit the binary was built from, with "-dirty" if
// it had uncommitted changes. `go run` and builds outside a git checkout don't record a commit.
func version() string {
	if buildVersion != "" {
		return buildVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "unknown"
	}
	if modified == "true" {
		revision += "-dirty"
	}
	return revision
}

func connectDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, dbConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		// pgx's error names the host and port it tried (never the password).
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}
