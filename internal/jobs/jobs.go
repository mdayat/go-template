// Package jobs defines the background jobs: each one's type name, payload and queue options. The
// API and the scheduler build jobs here and the worker reads them here, so the two sides can't
// disagree on a name or a field.
//
// A payload is read by whichever worker version is running when the job is picked up, which during
// a deploy may be older or newer than the code that queued it. So a payload only gains fields, and
// a handler accepts a payload without the newest ones.
package jobs

// Queue is the queue every job goes to and the worker reads.
const Queue = "default"
