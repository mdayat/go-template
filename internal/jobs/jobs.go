// Package jobs defines the background jobs: each one's type name, payload and queue options. The
// API and the scheduler build jobs here and the worker reads them here, so the two sides can't
// disagree on a name or a field.
//
// A payload is read by whichever worker version is running when the job is picked up, which during
// a deploy may be older or newer than the code that queued it. So a payload only gains fields, and
// a handler accepts a payload without the newest ones.
package jobs

import "github.com/mdayat/go-template/internal/queue"

// Queue is the queue a job goes to unless it needs its own.
const Queue = "default"

// Queues are every queue and how many of its jobs a worker instance runs at once, unless
// WORKER_QUEUES says otherwise. A job that mustn't wait behind others (urgent work), or would hold
// up others (slow work, like calling an AI model), gets its own queue: a constant like Queue, and a
// line here.
func Queues() []queue.Limit {
	return []queue.Limit{
		{Queue: Queue, Concurrency: 10},
	}
}
