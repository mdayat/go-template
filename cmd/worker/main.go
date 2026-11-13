// Command worker runs background jobs from the queue, and queues the jobs that run on a timetable.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/startup"
	"github.com/mdayat/go-template/internal/worker"
)

// buildVersion is set at build time with -ldflags "-X main.buildVersion=...", for builds that can't
// see .git (the Docker build).
//
//nolint:gochecknoglobals // -X can only set a package-level variable
var buildVersion string

func main() {
	if err := run(); err != nil {
		log.Error().Err(err).Msg("worker stopped")
		os.Exit(1)
	}
}

// run starts the job server and the scheduler and blocks until SIGINT/SIGTERM. Every startup
// failure is returned, so main logs it once.
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

	scheduler, err := worker.NewScheduler(queue, log.Logger)
	if err != nil {
		return err
	}
	server := worker.NewServer(queue, log.Logger)
	if err := server.Start(worker.NewMux(log.Logger, worker.NewServices(db.New(pool)))); err != nil {
		return fmt.Errorf("start job server: %w", err)
	}
	if err := scheduler.Start(); err != nil {
		server.Shutdown()
		return fmt.Errorf("start scheduler: %w", err)
	}
	log.Info().Str("version", startup.Version(buildVersion)).Msg("worker started")

	<-ctx.Done()
	// Release the signal handler so a second Ctrl-C kills a stuck shutdown right away.
	stop()
	log.Info().Msg("shutting down")
	scheduler.Shutdown()
	// Waits up to its shutdown timeout for running jobs, and puts the unfinished ones back in the
	// queue. Before the deferred closes: running jobs need the database until they end.
	server.Shutdown()
	log.Info().Msg("worker stopped cleanly")
	return nil
}
