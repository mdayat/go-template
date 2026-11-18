package valkey

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/testqueue"
	"github.com/mdayat/go-template/internal/testredis"
)

func TestClaimSlot(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	sl := slot{queue: testqueue.NewQueue(t, client), jobType: "test:scheduled", payload: []byte(`{"n":1}`)}
	claim := func(id string, retry bool) bool {
		t.Helper()
		first, err := claimSlot(t.Context(), client, sl, id, time.Second, retry)
		if err != nil {
			t.Fatalf("claimSlot(%s) error = %v", id, err)
		}
		return first
	}

	if !claim("a", false) {
		t.Error("the first copy wasn't let through")
	}
	if claim("b", false) {
		t.Error("a second copy in the same time was let through")
	}
	if !claim("a", false) || !claim("a", true) {
		t.Error("the first copy, put back by a shutdown or retried, was refused")
	}
	if claim("b", true) {
		t.Error("a retry of a copy that never held the slot was let through")
	}
	other := slot{queue: sl.queue, jobType: sl.jobType, payload: []byte(`{"n":2}`)}
	if first, err := claimSlot(t.Context(), client, other, "c", time.Second, false); err != nil || !first {
		t.Errorf("the same job with another payload = %v, %v; want let through: another schedule", first, err)
	}

	time.Sleep(1100 * time.Millisecond)
	// A late retry runs, but doesn't take the slot: the next run still gets it.
	if !claim("a", true) {
		t.Error("a retry after the time had passed was refused")
	}
	if !claim("d", false) {
		t.Error("the next run was refused after a late retry")
	}
}

func TestScheduledWindow(t *testing.T) {
	t.Parallel()
	for headers, want := range map[*map[string]string]time.Duration{
		{scheduledHeader: "3600"}: time.Hour,
		{scheduledHeader: "0"}:    0,
		{scheduledHeader: "x"}:    0,
		{}:                        0,
	} {
		got, ok := scheduledWindow(*headers)
		if got != want || ok != (want > 0) {
			t.Errorf("scheduledWindow(%v) = %v, %v; want %v", *headers, got, ok, want)
		}
	}
}

// Two workers' schedulers can each queue a run (asynq's Unique lets go once one succeeds): only
// the first copy runs. Another schedule of the same job type (another payload), and copies queued
// by hand, all run.
func TestServerRunsOneScheduledCopy(t *testing.T) {
	t.Parallel()
	client := testredis.New(t)
	queueName := testqueue.NewQueue(t, client)
	var runs atomic.Int32
	mux := queue.NewMux()
	mux.Handle("test:scheduled", func(_ context.Context, payload []byte) error {
		if string(payload) == "other" {
			runs.Add(100) // another schedule of the same job type
		} else {
			runs.Add(1)
		}
		return nil
	})
	mux.Handle("test:by-hand", func(context.Context, []byte) error { runs.Add(10); return nil })
	server, err := NewServer(client, mux, ServerOptions{Queues: []queue.Limit{{Queue: queueName, Concurrency: 10}}, ShutdownTimeout: time.Second, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Shutdown)

	enqueuer := asynq.NewClientFromRedisClient(client)
	for _, job := range []struct {
		typ, payload string
		headers      map[string]string
	}{
		{"test:scheduled", "", map[string]string{scheduledHeader: "3600"}},
		{"test:scheduled", "", map[string]string{scheduledHeader: "3600"}},
		{"test:scheduled", "other", map[string]string{scheduledHeader: "3600"}},
		{"test:by-hand", "", nil},
		{"test:by-hand", "", nil},
	} {
		task, err := newTaskWithHeaders(t.Context(), queue.Job{Type: job.typ, Queue: queueName, Payload: []byte(job.payload), Timeout: time.Minute}, job.headers)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enqueuer.EnqueueContext(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for runs.Load() < 121 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // a second scheduled run, if any, has its time to show
	if got := runs.Load(); got != 121 {
		t.Errorf("runs = %d, want 121: one scheduled copy (1), the other schedule (100), both copies by hand (20)", got)
	}
}
