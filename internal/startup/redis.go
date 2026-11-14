package startup

import (
	"context"
	"errors"
	"fmt"

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
