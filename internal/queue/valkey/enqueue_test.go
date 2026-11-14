package valkey

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/testqueue"
)

// queuedJob is the part of a queued asynq task that our Job decides.
type queuedJob struct {
	Type     string
	Payload  string
	Queue    string
	Timeout  time.Duration
	MaxRetry int
}

func TestEnqueueKeepsTheJobsOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		maxRetries int
	}{
		{"with retries", 3},
		// asynq's own default is 25 retries; 0 must stay 0.
		{"no retries", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := testqueue.New(t)
			queueName := testqueue.NewQueue(t, client)
			job := queue.Job{
				Type: "test:job", Queue: queueName, Payload: []byte(`{"n":1}`), Timeout: 90 * time.Second, MaxRetries: tt.maxRetries,
			}

			id, err := NewEnqueuer(client).Enqueue(t.Context(), job)
			if err != nil {
				t.Fatalf("Enqueue() error = %v", err)
			}

			info := testqueue.Take(t, client, queueName, id)
			got := queuedJob{
				Type: info.Type, Payload: string(info.Payload), Queue: info.Queue, Timeout: info.Timeout, MaxRetry: info.MaxRetry,
			}
			want := queuedJob{Type: "test:job", Payload: `{"n":1}`, Queue: queueName, Timeout: 90 * time.Second, MaxRetry: tt.maxRetries}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("queued job mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A job asynq would refuse only once scheduled, or would quietly change, is refused before anything
// is queued.
func TestEnqueueRefusesBadJobs(t *testing.T) {
	t.Parallel()
	client := testqueue.New(t)
	queueName := testqueue.NewQueue(t, client)
	enqueuer := NewEnqueuer(client)
	valid := queue.Job{Type: "test:job", Queue: queueName, Timeout: time.Minute, MaxRetries: 1}

	tests := []struct {
		name   string
		change func(*queue.Job)
	}{
		{"no type", func(j *queue.Job) { j.Type = "" }},
		{"blank type", func(j *queue.Job) { j.Type = " " }},
		{"no queue", func(j *queue.Job) { j.Queue = "" }},
		{"no timeout", func(j *queue.Job) { j.Timeout = 0 }},
		{"timeout under a second", func(j *queue.Job) { j.Timeout = 500 * time.Millisecond }},
		{"timeout not in whole seconds", func(j *queue.Job) { j.Timeout = 1500 * time.Millisecond }},
		{"negative retries", func(j *queue.Job) { j.MaxRetries = -1 }},
	}
	for _, tt := range tests {
		job := valid
		tt.change(&job)
		// The scheduler takes newTask's task without asynq's queueing checks.
		if _, err := newTask(job); err == nil {
			t.Errorf("%s: newTask(%+v) error = nil, want one", tt.name, job)
		}
		if id, err := enqueuer.Enqueue(t.Context(), job); err == nil {
			t.Errorf("%s: Enqueue(%+v) = %q, want an error", tt.name, job, id)
		}
	}

	keys, err := client.Keys(t.Context(), "asynq:{"+queueName+"}:*").Result()
	if err != nil {
		t.Fatalf("list queue keys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("queue keys = %q, want none: nothing should be queued", keys)
	}
}
