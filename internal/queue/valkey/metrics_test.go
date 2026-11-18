package valkey

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/metrictest"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/testqueue"
	"github.com/mdayat/go-template/internal/testredis"
)

// Jobs nobody takes show as waiting, with the oldest one's wait growing; a queue nothing was ever
// queued to shows as empty, not missing.
func TestRecordQueues(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	busy, empty := testqueue.NewQueue(t, client), testqueue.NewQueue(t, client)
	recorder := metrictest.New()
	// Its own connection, to count its commands (testredis.New's is shared).
	own := redis.NewClient(client.Options())
	t.Cleanup(func() { _ = own.Close() })
	stop, err := RecordQueues(own, []string{busy, empty}, recorder.Provider, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	enqueuer := NewEnqueuer(client, tracenoop.NewTracerProvider())
	for range 2 {
		if _, err := enqueuer.Enqueue(t.Context(), queue.Job{Type: "test:job", Queue: busy, Timeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1100 * time.Millisecond)

	got := recorder.Collect(t)
	for _, tt := range []struct {
		name, queue, state string
		want               int64
	}{
		{"job.queue.jobs", busy, "waiting", 2},
		{"job.queue.jobs", busy, "running", 0},
		{"job.queue.jobs", busy, "retrying", 0},
		{"job.queue.jobs", empty, "waiting", 0},
	} {
		points := got.Find(tt.name, map[string]string{"job.queue": tt.queue, "job.state": tt.state})
		if diff := cmp.Diff([]int64{tt.want}, counts(points)); diff != "" {
			t.Errorf("%s{%s, %s} (-want +got):\n%s", tt.name, tt.queue, tt.state, diff)
		}
	}
	if wait := counts(got.Find("job.queue.oldest_wait", map[string]string{"job.queue": busy})); len(wait) != 1 || wait[0] < 1 {
		t.Errorf("job.queue.oldest_wait for a job queued over a second ago = %v s, want one value of at least 1", wait)
	}
	if wait := counts(got.Find("job.queue.oldest_wait", map[string]string{"job.queue": empty})); !cmp.Equal(wait, []int64{0}) {
		t.Errorf("job.queue.oldest_wait for an empty queue = %v, want [0]", wait)
	}

	// Stopped, the queues are no longer read, so the last send on the way out doesn't touch a client
	// that's closing.
	var reads atomic.Int32
	own.AddHook(countCommands{&reads})
	stop()
	if got := recorder.Collect(t); len(got["job.queue.jobs"]) != 0 || reads.Load() != 0 {
		t.Errorf("after stop: job.queue.jobs = %v, Valkey commands = %d; want nothing, 0", got["job.queue.jobs"], reads.Load())
	}
}

// A queue that can't be read is left out; the send of every other metric still goes through
// (Collect fails the test if it returns an error).
func TestRecordQueuesSurvivesValkeyDown(t *testing.T) {
	t.Parallel()
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1}) // nothing listens
	t.Cleanup(func() { _ = down.Close() })
	recorder := metrictest.New()
	var logs bytes.Buffer
	if _, err := RecordQueues(down, []string{"default"}, recorder.Provider, zerolog.New(&logs)); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Collect(t); len(got["job.queue.jobs"]) != 0 {
		t.Errorf("job.queue.jobs with Valkey down = %v, want nothing", got["job.queue.jobs"])
	}
	if !strings.Contains(logs.String(), "job queues not measured") {
		t.Errorf("log = %q, want the failure logged", logs.String())
	}
}

// countCommands counts the commands a client sends.
type countCommands struct{ n *atomic.Int32 }

func (c countCommands) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c countCommands) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.n.Add(1)
		return next(ctx, cmd)
	}
}

func (c countCommands) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.n.Add(int32(len(cmds)))
		return next(ctx, cmds)
	}
}

func counts(points []metrictest.Point) []int64 {
	var c []int64
	for _, p := range points {
		c = append(c, p.Count)
	}
	return c
}
