package main

import (
	"testing"
	"time"

	"github.com/mdayat/go-template/internal/telemetry"
	"github.com/mdayat/go-template/internal/worker"
)

// Running jobs' wait and the last metrics' flush must end before Docker's hard kill (10 s after
// SIGTERM by default), or the flush is cut off.
func TestShutdownFitsDockersWait(t *testing.T) {
	t.Parallel()
	if worker.ShutdownTimeout+telemetry.FlushTimeout >= 10*time.Second {
		t.Errorf("worker.ShutdownTimeout (%s) + telemetry.FlushTimeout (%s) = %s, want under 10s",
			worker.ShutdownTimeout, telemetry.FlushTimeout, worker.ShutdownTimeout+telemetry.FlushTimeout)
	}
}
