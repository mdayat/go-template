// Package startup holds what every command does before its real work: set up the logger and connect
// to what it needs.
package startup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
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

// MaxDBConns is the most DB_MAX_CONNS may be: Postgres's own limit on connections is usually in the
// hundreds, and a pool larger than this is a typo.
const MaxDBConns = 1000

// CheckMaxConns reports whether n is a DB_MAX_CONNS ConnectDB takes, for a program's settings to
// report with the others before connecting.
func CheckMaxConns(n int) error {
	if n < 1 || n > MaxDBConns {
		return fmt.Errorf("DB_MAX_CONNS must be from 1 to %d, got %d", MaxDBConns, n)
	}
	return nil
}

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

// DBLimits are how many connections a pool opens, and Postgres's own time limits on each. When a
// context ends, pgx already asks Postgres to stop the query; the time limits stop what that can't
// reach: a query or transaction left running when the program is killed, or when the cancel request
// is lost on the network.
type DBLimits struct {
	// MaxConns is the most connections the pool opens at once (pgx's pool_max_conns, whose own
	// default is the number of CPUs, at least 4); a query waits for a free one.
	MaxConns int
	// Statement is the longest one statement may run (statement_timeout); Postgres then cancels it.
	Statement time.Duration
	// IdleInTransaction is the longest a transaction may stay open with nothing running
	// (idle_in_transaction_session_timeout), holding its locks; Postgres then ends the connection,
	// which rolls the transaction back.
	IdleInTransaction time.Duration
}

// ConnectDB returns a pool on url once a connection works, so a wrong URL fails at startup. The pool
// and every connection have limits, and url must not set them too. It records to meters, from the app's side:
// each query's time (by its sqlc name), and the pool's use (connections in use and idle, waits for
// a free one), which the database itself can't see. Each query in a traced request or job is a span
// in tracers ("query GetTask"), with its SQL but not its values (a failed query's error message can
// quote one).
func ConnectDB(ctx context.Context, url string, limits DBLimits, meters metric.MeterProvider, tracers trace.TracerProvider) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx's parse error leaves the password out.
		return nil, fmt.Errorf("read DATABASE_URL: %w", err)
	}
	params := cfg.ConnConfig.RuntimeParams
	// Postgres matches a setting's name ignoring case, and in options takes --statement-timeout=...
	// as -c statement_timeout=...
	options := strings.ReplaceAll(strings.ToLower(params["options"]), "-", "_")
	for _, l := range []struct {
		name, setting string
		value         time.Duration
	}{
		{"statement_timeout", "DB_STATEMENT_TIMEOUT", limits.Statement},
		{"idle_in_transaction_session_timeout", "DB_IDLE_IN_TRANSACTION_TIMEOUT", limits.IdleInTransaction},
	} {
		// 0 would be no limit; Postgres refuses more than its largest whole number of milliseconds.
		if l.value < time.Millisecond || l.value.Milliseconds() > math.MaxInt32 {
			return nil, fmt.Errorf("%s must be from 1ms to 596h, got %v", l.setting, l.value)
		}
		// One value in two places means one is silently ignored. The error doesn't quote the URL: it
		// holds a password.
		set := strings.Contains(options, l.name)
		for k := range params {
			set = set || strings.EqualFold(k, l.name)
		}
		if set {
			return nil, fmt.Errorf("DATABASE_URL (or PGOPTIONS, or a service file) sets %s: set %s instead", l.name, l.setting)
		}
		// Postgres reads a number without a unit as milliseconds.
		params[l.name] = strconv.FormatInt(l.value.Milliseconds(), 10)
	}
	if err := CheckMaxConns(limits.MaxConns); err != nil {
		return nil, err
	}
	// pgx has already taken it out of the parsed settings, so look in the URL itself.
	if strings.Contains(strings.ToLower(url), "pool_max_conns") {
		return nil, errors.New("DATABASE_URL sets pool_max_conns: set DB_MAX_CONNS instead")
	}
	cfg.MaxConns = int32(limits.MaxConns) //nolint:gosec // CheckMaxConns above keeps it within 1 to 1000
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

// remove:api

// DBReady returns the API's readiness check (/readyz): the database answers a ping. A ping first
// takes a free connection, so with every connection busy (slow queries, a burst) it would fail on
// every instance at once and the load balancer would drop them all, turning busy into down: a pool
// that's full when the time runs out counts as ready.
func DBReady(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		err := pool.Ping(ctx)
		if stat := pool.Stat(); err != nil && ctx.Err() != nil && stat.AcquiredConns() >= stat.MaxConns() {
			return nil
		}
		return err
	}
}

// remove:end

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
