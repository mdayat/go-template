package jobs

import (
	"strings"
	"testing"

	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/startup"
)

// Every queue is one the worker will start with, and every job goes to one of them: a job sent to a
// queue missing from Queues is never run, and never measured. Add each new job's builder to
// builders.
func TestQueues(t *testing.T) {
	t.Parallel()
	listed := map[string]bool{}
	total := 0
	for _, q := range Queues() {
		switch {
		case strings.TrimSpace(q.Queue) == "":
			t.Errorf("Queues() has a blank name: %+v", q)
		case listed[q.Queue]:
			t.Errorf("Queues() lists %s twice", q.Queue)
		case q.Concurrency < 1 || q.Concurrency > 1000:
			t.Errorf("Queues(): %s runs %d jobs at once, want 1 to 1000", q.Queue, q.Concurrency)
		}
		listed[q.Queue] = true
		total += q.Concurrency
	}
	// Each job running at once needs a database connection, and a pool has at most this many.
	if total > startup.MaxDBConns {
		t.Errorf("Queues() run %d jobs at once in all, want at most %d", total, startup.MaxDBConns)
	}

	builders := map[string]func() (queue.Job, error){
		TypePurgeDeletedRows: func() (queue.Job, error) { return NewPurgeDeletedRows(30) },
	}
	for name, build := range builders {
		job, err := build()
		if err != nil {
			t.Fatalf("build %s: %v", name, err)
		}
		if !listed[job.Queue] {
			t.Errorf("job %s goes to queue %q, which Queues() doesn't list: no worker would run it", name, job.Queue)
		}
	}
}
