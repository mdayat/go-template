package valkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// scheduledHeader marks a job a scheduler queued, with its schedule's Unique in seconds.
const scheduledHeader = "x-schedule-unique-seconds"

// scheduledWindow is the Unique a scheduled job carries in headers; false for any other job.
func scheduledWindow(headers map[string]string) (time.Duration, bool) {
	s, ok := headers[scheduledHeader]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// slot is the key a scheduled job's copies share: its queue, type and payload, as asynq's own
// Unique has it, so two schedules of one type with different payloads don't block each other.
type slot struct {
	queue, jobType string
	payload        []byte
}

func (s slot) key() string {
	sum := sha256.Sum256(s.payload)
	return "schedule:" + s.queue + ":" + s.jobType + ":" + hex.EncodeToString(sum[:8])
}

// claimSlot reports whether the copy with id may run. A first attempt claims the slot for window
// unless another copy holds it (the same id again, a copy put back by a shutdown, is let through).
// A retry never claims: it runs only if the slot is empty or still its own, so a retry long after
// its window can't take the slot of the schedule's next run. Jobs queued by hand don't go through
// it, so they always run.
func claimSlot(ctx context.Context, client redis.UniversalClient, s slot, id string, window time.Duration, retry bool) (bool, error) {
	var owner string
	var err error
	if retry {
		owner, err = client.Get(ctx, s.key()).Result()
	} else {
		owner, err = client.SetArgs(ctx, s.key(), id, redis.SetArgs{Mode: "NX", Get: true, TTL: window}).Result()
	}
	switch {
	case errors.Is(err, redis.Nil):
		return true, nil // set by this call, or (a retry) held by no one
	case err != nil:
		return false, fmt.Errorf("claim the run of scheduled job %s: %w", s.jobType, err)
	}
	return owner == id, nil
}
