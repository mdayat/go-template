// Package idempotency lets a client retry a request safely. The client makes up a key for each
// action and sends it with every try; the first try with a key runs and its reply is kept, and a
// later try with the same key gets that reply instead of running again. Store is what the code knows
// about where keys are kept; internal/idempotency/valkey keeps them in Valkey.
package idempotency

import (
	"context"
	"time"
)

// Reply is a kept reply: what a later try with the same key gets back.
type Reply struct {
	Status int `json:"status"`
	// Header holds the reply's headers worth repeating (Content-Type, Location).
	Header map[string]string `json:"header"`
	Body   []byte            `json:"body"`
	// TooLarge says the body was too large to keep (Body is empty): the request ran, but its reply
	// can't be sent again.
	TooLarge bool `json:"too_large,omitempty"`
}

// Claim is what Begin found for a key: exactly one of Token (the key is now this request's),
// Reply, Running or Mismatch.
type Claim struct {
	// Token holds the key for this request: pass it to Finish or Release.
	Token string
	// Reply is the kept reply of an earlier request with the key.
	Reply *Reply
	// Running says an earlier request with the key hasn't finished.
	Running bool
	// Mismatch says the key was used for a different request (another fingerprint).
	Mismatch bool
}

// Store keeps keys and their replies. Keys are the caller's to scope (a user's ID and the client's
// key) and are stored hashed.
type Store interface {
	// Begin claims key for a request whose fingerprint (a hash of what it asks) is fingerprint,
	// held for hold, or reports what holds it. A hold that ends without Finish or Release (the
	// program crashed) frees the key, so a retry runs again.
	Begin(ctx context.Context, key, fingerprint string, hold time.Duration) (Claim, error)
	// Finish keeps reply under key for keep, if token still holds it (a hold that ended was lost to
	// a later request).
	Finish(ctx context.Context, key, token string, reply Reply, keep time.Duration) error
	// Release frees key, if token still holds it, so a retry runs again.
	Release(ctx context.Context, key, token string) error
}
