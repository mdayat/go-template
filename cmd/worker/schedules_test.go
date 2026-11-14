package main

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/queue/valkey"
	"github.com/mdayat/go-template/internal/testqueue"
	"github.com/mdayat/go-template/internal/worker"
)

// Every feature's schedule is one the queue this worker runs on accepts (a cron spec it can parse,
// a Unique it keeps), so a typo fails here, not when the worker starts.
func TestTheQueueAcceptsTheWorkersSchedules(t *testing.T) {
	t.Parallel()
	schedules, err := worker.Schedules()
	if err != nil {
		t.Fatalf("worker.Schedules() error = %v", err)
	}

	// Not started: registering is what checks the schedules.
	if _, err := valkey.NewScheduler(testqueue.New(t), zerolog.Nop(), schedules); err != nil {
		t.Errorf("valkey.NewScheduler(worker.Schedules()) error = %v", err)
	}
}
