// Package startup holds what every command does before its real work: set up the logger and connect
// to what it needs.
package startup

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

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
	// remove:valkey
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

// ConnectDB returns a pool on url once a connection works, so a wrong URL fails at startup. It
// records to meters, from the app's side: each query's time (by its sqlc name), and the pool's use
// (connections in use and idle, waits for a free one), which the database itself can't see. Each
// query in a traced request or job is a span in tracers ("query GetTask"), with its SQL but not
// its values (a failed query's error message can quote one).
func ConnectDB(ctx context.Context, url string, meters metric.MeterProvider, tracers trace.TracerProvider) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx's parse error leaves the password out.
		return nil, fmt.Errorf("read DATABASE_URL: %w", err)
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithMeterProvider(meters),
		otelpgx.WithTracerProvider(tracers),
		otelpgx.WithMetricOperationName(queryName),
		otelpgx.WithSpanNameCtxFunc(queryName),
		// "query GetTask", "prepare GetTask": pgx prepares each query on a connection the first
		// time, and both steps would otherwise be called GetTask.
		otelpgx.WithQuerySpanNamePrefix(),
	)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	err = otelpgx.RecordStats(pool,
		otelpgx.WithStatsMeterProvider(meters),
		// Instead of otelpgx's "host:port/database", which would put the database's address on
		// every pool series. The program has one pool.
		otelpgx.WithStatsAttributes(semconv.DBClientConnectionPoolName("main")),
	)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("record database pool metrics: %w", err)
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

// queryName names a query for its metrics and span: sqlc's name for it ("GetTask", from its first
// line, "-- name: GetTask :one"), else its first SQL word ("SELECT"). Never the SQL itself: metrics
// must keep to a small, fixed set of names.
func queryName(ctx context.Context, stmt string) string {
	if rest, ok := strings.CutPrefix(stmt, "-- name: "); ok {
		firstLine, _, _ := strings.Cut(rest, "\n")
		if fields := strings.Fields(firstLine); len(fields) > 0 {
			return fields[0]
		}
	}
	return otelpgx.SQLOperationName(ctx, stmt)
}
