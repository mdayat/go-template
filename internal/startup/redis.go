package startup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// redisLogger sends go-redis's own messages (a failed dial, a dropped connection) to the JSON log
// instead of plain-text stderr. Warn, not error: the call that failed also returns its error, and
// that is logged where it ends.
type redisLogger struct {
	logger zerolog.Logger
}

func (l redisLogger) Printf(_ context.Context, format string, v ...any) {
	l.logger.Warn().Str("source", "go-redis").Msg(fmt.Sprintf(format, v...))
}

// ConnectRedis returns a client on url (redis:// or rediss://) once a connection works. It records
// to meters, from the app's side, each command's time and the connections' use, under pool (what
// the client is for: "queue"), kept apart from the program's other clients; and each command in a
// traced request or job as a span in tracers.
func ConnectRedis(ctx context.Context, url, pool string, meters metric.MeterProvider, tracers trace.TracerProvider) (*redis.Client, error) {
	opts, err := parseRedisURL(url)
	if err != nil {
		return nil, err
	}
	client, err := instrumented(opts, pool, meters, tracers)
	if err != nil {
		return nil, err
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to redis at %s: %w", opts.Addr, err), client.Close())
	}
	return client, nil
}

// RedisFailFast returns a client on url for a caller with a fallback when Valkey is slow or down
// (the API's rate limits): each command gives up after timeout and isn't retried, so a request
// waits about timeout for it at most, instead of go-redis's defaults of seconds per try and several
// tries. Unlike ConnectRedis it doesn't connect first: the caller copes with Valkey being down. It
// records metrics and traces as ConnectRedis does.
func RedisFailFast(
	url string, timeout time.Duration, pool string, meters metric.MeterProvider, tracers trace.TracerProvider,
) (*redis.Client, error) {
	opts, err := parseRedisURL(url)
	if err != nil {
		return nil, err
	}
	opts.DialTimeout = timeout
	opts.ReadTimeout = timeout
	opts.WriteTimeout = timeout
	// Waiting for a free connection when all are busy; default: ReadTimeout + 1 s.
	opts.PoolTimeout = timeout
	// For each, 0 means go-redis's default: 3 command retries, 5 dial tries 100 ms apart.
	opts.MaxRetries = -1
	opts.DialerRetries = 1
	// A request's own deadline (its time limit, or the client hanging up) ends the call too.
	opts.ContextTimeoutEnabled = true
	return instrumented(opts, pool, meters, tracers)
}

func instrumented(opts *redis.Options, pool string, meters metric.MeterProvider, tracers trace.TracerProvider) (*redis.Client, error) {
	client := redis.NewClient(opts)
	err := redisotel.InstrumentMetrics(client,
		redisotel.WithMeterProvider(meters),
		redisotel.WithPoolName(pool),
		// The OpenTelemetry convention's counter types; off by default only for users of older versions.
		redisotel.WithSemConvCompliantMetrics(true),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("record Valkey metrics: %w", err), client.Close())
	}
	err = redisotel.InstrumentTracing(client,
		redisotel.WithTracerProvider(tracers),
		redisotel.WithAttributes(semconv.DBClientConnectionPoolName(pool)),
		// The span is named by the command (SET); its arguments, a queued job's payload among them,
		// can hold personal data.
		redisotel.WithDBStatement(false),
		// Where in go-redis or the library calling it the command was sent from: a stack walk per
		// command, for nothing a reader needs.
		redisotel.WithCallerEnabled(false),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("trace Valkey commands: %w", err), client.Close())
	}
	return client, nil
}

func parseRedisURL(url string) (*redis.Options, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		// Not wrapped: the parse error can quote the URL, password included.
		return nil, errors.New("REDIS_URL is not a valid redis:// URL")
	}
	return opts, nil
}
