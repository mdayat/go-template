// Package valkey is idempotency.Store on Valkey (Redis): every copy of the API sees the same keys,
// and Valkey deletes each when its time is up, so nothing needs cleaning.
package valkey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mdayat/go-template/internal/idempotency"
	"github.com/mdayat/go-template/internal/json"
)

// A key holds "r<token>|<fingerprint>" while its request runs (the whole string is the token
// Finish and Release compare), then "d<record JSON>" once its reply is kept.
const (
	running = "r"
	done    = "d"
)

// record is a kept reply and the fingerprint of the request it answers.
type record struct {
	Fingerprint string            `json:"fingerprint"`
	Reply       idempotency.Reply `json:"reply"`
}

// Store keeps keys in Valkey.
type Store struct {
	client  *redis.Client
	prefix  string
	timeout time.Duration
	finish  *redis.Script // replaces a hold with the kept reply, only if the hold is still the caller's
	release *redis.Script // deletes a hold, only if it's still the caller's
}

var _ idempotency.Store = (*Store)(nil)

// New returns a Store on client, its keys starting prefix + "idem:". A call that takes longer than
// timeout fails: give client no retries (startup.RedisFailFast), or one call can retry past it.
func New(client *redis.Client, prefix string, timeout time.Duration) *Store {
	return &Store{
		client:  client,
		prefix:  prefix + "idem:",
		timeout: timeout,
		finish: redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
  return 1
end
return 0`),
		release: redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`),
	}
}

// Begin claims key with SET NX GET: one call, so two requests with the same key can't both claim
// it.
func (s *Store) Begin(ctx context.Context, key, fingerprint string, hold time.Duration) (idempotency.Claim, error) {
	token := running + rand.Text() + "|" + fingerprint
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	old, err := s.client.SetArgs(ctx, s.key(key), token, redis.SetArgs{Mode: "NX", Get: true, TTL: hold}).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return idempotency.Claim{Token: token}, nil
	case err != nil:
		// The SET may have been applied with only its answer lost (a timeout): free what it may have
		// claimed, or the key would answer "still running" for the whole hold with nothing behind it.
		_ = s.Release(context.WithoutCancel(ctx), key, token)
		return idempotency.Claim{}, fmt.Errorf("claim an idempotency key: %w", err)
	case strings.HasPrefix(old, running):
		_, oldFingerprint, _ := strings.Cut(old, "|")
		if oldFingerprint != fingerprint {
			return idempotency.Claim{Mismatch: true}, nil
		}
		return idempotency.Claim{Running: true}, nil
	case strings.HasPrefix(old, done):
		var rec record
		if err := json.Unmarshal([]byte(old[len(done):]), &rec); err != nil {
			return idempotency.Claim{}, fmt.Errorf("read a kept reply: %w", err)
		}
		if rec.Fingerprint != fingerprint {
			return idempotency.Claim{Mismatch: true}, nil
		}
		return idempotency.Claim{Reply: &rec.Reply}, nil
	default:
		return idempotency.Claim{}, errors.New("an idempotency key holds something unknown")
	}
}

// ErrHoldLost is Finish's error when the hold ended before the reply was kept: a retry may have run
// the request again meanwhile.
var ErrHoldLost = errors.New("the idempotency key's hold ended before its reply was kept")

// Finish keeps reply under key for keep.
func (s *Store) Finish(ctx context.Context, key, token string, reply idempotency.Reply, keep time.Duration) error {
	_, fingerprint, _ := strings.Cut(token, "|")
	value, err := json.Marshal(record{Fingerprint: fingerprint, Reply: reply})
	if err != nil {
		return fmt.Errorf("encode a reply to keep: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	kept, err := s.finish.Run(ctx, s.client, []string{s.key(key)}, token, done+string(value), keep.Milliseconds()).Int()
	switch {
	case err != nil:
		return fmt.Errorf("keep a reply: %w", err)
	case kept == 0:
		return ErrHoldLost
	}
	return nil
}

// Release frees key.
func (s *Store) Release(ctx context.Context, key, token string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.release.Run(ctx, s.client, []string{s.key(key)}, token).Err(); err != nil {
		return fmt.Errorf("free an idempotency key: %w", err)
	}
	return nil
}

// key is where key is stored: hashed, as a client's key can be any text.
func (s *Store) key(key string) string {
	sum := sha256.Sum256([]byte(key))
	return s.prefix + hex.EncodeToString(sum[:])
}
