// Package startup holds what every command (cmd/api, cmd/worker) does before its real work: set up
// the logger and connect to Postgres and Valkey.
package startup

import (
	"context"
	"errors"
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

// SetupLogger sets the global logger's level and, in development, readable output, and sends
// go-redis's own messages to it.
func SetupLogger(app config.App) {
	zerolog.SetGlobalLevel(app.LogLevel)
	if app.Env == config.EnvDevelopment {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}
	// zerolog.Ctx on a context without a logger returns this one; the default drops every line.
	zerolog.DefaultContextLogger = &log.Logger
	redis.SetLogger(redisLogger{logger: log.Logger})
}

// redisLogger sends go-redis's own messages (a failed dial, a dropped connection) to the JSON log
// instead of plain-text stderr. Warn, not error: the call that failed also returns its error, and
// that is logged where it ends.
type redisLogger struct {
	logger zerolog.Logger
}

func (l redisLogger) Printf(_ context.Context, format string, v ...any) {
	l.logger.Warn().Str("source", "go-redis").Msg(fmt.Sprintf(format, v...))
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

// ConnectRedis returns a client on url (redis:// or rediss://) once a connection works.
func ConnectRedis(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		// Not wrapped: the parse error can quote the URL, password included.
		return nil, errors.New("REDIS_URL is not a valid redis:// URL")
	}
	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to redis at %s: %w", opts.Addr, err), client.Close())
	}
	return client, nil
}
