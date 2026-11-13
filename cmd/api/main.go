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
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/mdayat/go-template/internal/api"
	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/startup"
)

// Long enough for in-flight requests to finish (WriteTimeout caps each one).
const shutdownTimeout = 15 * time.Second

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
	// Before anything can fail, so every line, a config error's included, has readable times.
	zerolog.TimeFieldFormat = time.RFC3339Nano
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Add a secret manager's source after env, so the environment can still override it.
	env, err := config.Env()
	if err != nil {
		return err
	}
	cfg, err := loadSettings(env)
	if err != nil {
		return err
	}
	startup.SetupLogger(cfg.app)

	pool, err := startup.ConnectDB(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	queue, err := startup.ConnectRedis(ctx, cfg.redisURL)
	if err != nil {
		return err
	}
	defer queue.Close()

	server := &http.Server{
		Handler:           api.NewRouter(log.Logger, api.NewServices(db.New(pool), queue)),
		ErrorLog:          api.ServerErrorLog(log.Logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Bind before logging, so "listening" is only logged once the port is ours.
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.httpAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.httpAddr, err)
	}
	log.Info().Str("addr", listener.Addr().String()).Str("version", startup.Version(buildVersion)).Msg("api listening")

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

	// The server stops first so no request is cut off mid-query; the deferred closes run after.
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
