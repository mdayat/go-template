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
	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/httpclient"
	"github.com/mdayat/go-template/internal/queue/valkey"
	ratelimitvalkey "github.com/mdayat/go-template/internal/ratelimit/valkey"
	"github.com/mdayat/go-template/internal/startup"
	"github.com/mdayat/go-template/internal/telemetry"
)

// At least api.RequestTimeout, so every request still running has had its full time when its
// connection is closed; below Docker's default 10 s between SIGTERM and a hard kill (the shortest
// common wait), so the API closes what's left itself. TestShutdownTimeout checks both.
const shutdownTimeout = 8 * time.Second

// authKeysTimeout is how long one fetch of the sign-in provider's keys may take.
const authKeysTimeout = 5 * time.Second

// rateLimitTimeout is how long one Valkey call for the rate limits may take before this copy counts
// on its own instead: the longest a slow or hung Valkey holds up a request, since after a failure
// the copy stops calling it for a while.
const rateLimitTimeout = 250 * time.Millisecond

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
	if err := startup.LimitMemory(log.Logger); err != nil {
		return err
	}
	version := startup.Version(buildVersion)

	providers, err := telemetry.Start(ctx, telemetry.Program{Name: "api", Version: version, Env: cfg.app.Env}, log.Logger)
	if err != nil {
		return err
	}
	// Last, after the server and connections close, so their final numbers and spans are sent too.
	defer providers.Flush(log.Logger)

	pool, err := startup.ConnectDB(ctx, cfg.databaseURL, cfg.dbLimits, providers.Meters, providers.Tracers)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Not connected here: a Valkey that's down at startup must not stop the API, which counts in
	// memory until Valkey answers (see valkey.Limiter).
	limitsClient, err := startup.RedisFailFast(cfg.redisURL, rateLimitTimeout, "ratelimit", providers.Meters, providers.Tracers)
	if err != nil {
		return err
	}
	defer limitsClient.Close()
	cfg.limits.Limiter = ratelimitvalkey.New(limitsClient, "api:", rateLimitTimeout, log.Logger)

	// remove:worker
	redisClient, err := startup.ConnectRedis(ctx, cfg.redisURL, "queue", providers.Meters, providers.Tracers)
	if err != nil {
		return err
	}
	defer redisClient.Close()

	// remove:end
	services := api.NewServices(
		db.New(pool),
		// remove:worker
		valkey.NewEnqueuer(redisClient, providers.Tracers),
		// remove:end
	)
	// Fetches the provider's keys now and hourly until ctx ends; a provider that can't be reached
	// doesn't stop the API starting (requests needing sign-in get a 503 until the keys arrive).
	authn, err := jwks.New(ctx, cfg.auth, httpclient.New(authKeysTimeout, providers.Tracers), log.Logger)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           api.NewRouter(log.Logger, providers.Meters, providers.Tracers, services, authn, cfg.limits),
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
	log.Info().Str("addr", listener.Addr().String()).Str("version", version).Msg("api listening")

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
