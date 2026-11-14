package valkey

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/testqueue"
)

// Two workers' schedulers fire at the same moments; Unique lets one copy of the job through, with
// the job's own options.
func TestSchedulersQueueEachJobOnce(t *testing.T) {
	t.Parallel()
	client := testqueue.New(t)
	queueName := testqueue.NewQueue(t, client)
	schedule := queue.Schedule{
		Spec:   "@every 1s",
		Job:    queue.Job{Type: "test:scheduled", Queue: queueName, Payload: []byte(`{"n":1}`), Timeout: 90 * time.Second, MaxRetries: 2},
		Unique: time.Hour,
	}
	for range 2 {
		scheduler, err := NewScheduler(client, zerolog.Nop(), []queue.Schedule{schedule})
		if err != nil {
			t.Fatalf("NewScheduler() error = %v", err)
		}
		if err := scheduler.Start(); err != nil {
			t.Fatalf("start scheduler: %v", err)
		}
		t.Cleanup(scheduler.Shutdown)
	}

	// Both fire every second; three seconds give each at least two tries.
	time.Sleep(3 * time.Second)
	pending, err := asynq.NewInspectorFromRedisClient(client).ListPendingTasks(queueName)
	if err != nil {
		t.Fatalf("list pending jobs: %v", err)
	}

	var got []queuedJob
	for _, info := range pending {
		got = append(got, queuedJob{
			Type: info.Type, Payload: string(info.Payload), Queue: info.Queue, Timeout: info.Timeout, MaxRetry: info.MaxRetry,
		})
	}
	want := []queuedJob{{Type: "test:scheduled", Payload: `{"n":1}`, Queue: queueName, Timeout: 90 * time.Second, MaxRetry: 2}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("queued jobs mismatch (-want +got):\n%s", diff)
	}
}

// A schedule asynq would take and then fail on when the job fires is refused at startup.
func TestNewSchedulerRefusesBadSchedules(t *testing.T) {
	t.Parallel()
	client := testqueue.New(t)
	valid := queue.Schedule{
		Spec:   "@daily",
		Job:    queue.Job{Type: "test:scheduled", Queue: "default", Timeout: time.Minute},
		Unique: time.Hour,
	}

	tests := []struct {
		name   string
		change func(*queue.Schedule)
	}{
		{"no unique", func(s *queue.Schedule) { s.Unique = 0 }},
		{"unique under a second", func(s *queue.Schedule) { s.Unique = 500 * time.Millisecond }},
		{"unique not in whole seconds", func(s *queue.Schedule) { s.Unique = 1500 * time.Millisecond }},
		{"job without a queue", func(s *queue.Schedule) { s.Job.Queue = "" }},
		{"spec that isn't one", func(s *queue.Schedule) { s.Spec = "every day" }},
	}
	for _, tt := range tests {
		schedule := valid
		tt.change(&schedule)
		if _, err := NewScheduler(client, zerolog.Nop(), []queue.Schedule{schedule}); err == nil {
			t.Errorf("%s: NewScheduler(%+v) error = nil, want one", tt.name, schedule)
		}
	}
	if _, err := NewScheduler(client, zerolog.Nop(), []queue.Schedule{valid}); err != nil {
		t.Errorf("NewScheduler(%+v) error = %v, want nil", valid, err)
	}
}
