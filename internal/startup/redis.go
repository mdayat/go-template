package startup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
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

// ConnectRedis returns a client on url (redis:// or rediss://) once a connection works.
func ConnectRedis(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := parseRedisURL(url)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)

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
// tries. Unlike ConnectRedis it doesn't connect first: the caller copes with Valkey being down.
func RedisFailFast(url string, timeout time.Duration) (*redis.Client, error) {
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
	return redis.NewClient(opts), nil
}

func parseRedisURL(url string) (*redis.Options, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		// Not wrapped: the parse error can quote the URL, password included.
		return nil, errors.New("REDIS_URL is not a valid redis:// URL")
	}
	return opts, nil
}
