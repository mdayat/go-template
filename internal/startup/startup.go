// Package startup holds what every command does before its real work: set up the logger and connect
// to what it needs.
package startup

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/mdayat/go-template/internal/config"
)

const connectTimeout = 5 * time.Second

// SetupLogger sets the global logger's level and, in development, readable output.
func SetupLogger(app config.App) {
	zerolog.SetGlobalLevel(app.LogLevel)
	if app.Env == config.EnvDevelopment {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}
	// zerolog.Ctx on a context without a logger returns this one; the default drops every line.
	zerolog.DefaultContextLogger = &log.Logger
	// remove:worker
	// go-redis's own messages go to the same log.
	redis.SetLogger(redisLogger{logger: log.Logger})
	// remove:end
}

// Version is buildVersion if set, else the git commit the binary was built from, with "-dirty" if
// it had uncommitted changes. `go run` and builds outside a git checkout don't record a commit.
func Version(buildVersion string) string {
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

// ConnectDB returns a pool on url once a connection works, so a wrong URL fails at startup.
func ConnectDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		// pgx's error names the host and port it tried (never the password).
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}
