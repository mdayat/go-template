// Command oneoff runs one-off data commands (fixes, backfills) by hand: oneoff <command> [flags],
// a dry run that saves nothing unless --apply is given. With no command it lists them. See
// docs/standards/commands.md.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/oneoff"
	"github.com/mdayat/go-template/internal/startup"
)

// buildVersion is set at build time with -ldflags "-X main.buildVersion=...", for builds that can't
// see .git (the Docker build).
//
//nolint:gochecknoglobals // -X can only set a package-level variable
var buildVersion string

func main() {
	err := run()
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case errors.Is(err, oneoff.ErrUsage):
		os.Exit(2)
	case errors.Is(err, oneoff.ErrFailed):
		// The run's summary line has the error.
		os.Exit(1)
	default:
		log.Error().Err(err).Msg("oneoff stopped")
		os.Exit(1)
	}
}

// run parses the command line, connects to the database and runs the command, until it ends or
// SIGINT/SIGTERM stops it after undoing the batch it was in.
func run() error {
	// Before anything can fail, so every line, a config error's included, has readable times.
	zerolog.TimeFieldFormat = time.RFC3339Nano
	// First, so a wrong command or -h needs no settings or database.
	inv, err := oneoff.Parse(os.Args[1:], oneoff.Commands(), os.Stderr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Release the signal handler once the first arrives, so a second Ctrl-C kills a stuck run.
	context.AfterFunc(ctx, stop)

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

	// No metrics or traces: a run is read from its summary line and report.
	pool, err := startup.ConnectDB(ctx, cfg.databaseURL, cfg.dbLimits, metricnoop.NewMeterProvider(), tracenoop.NewTracerProvider())
	if err != nil {
		return err
	}
	defer pool.Close()

	// Which code ran, on every line of the run.
	logger := log.Logger.With().Str("version", startup.Version(buildVersion)).Logger()
	return inv.Run(ctx, pool, logger, os.Stdout)
}
