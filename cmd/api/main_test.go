package main

import (
	"testing"
	"time"

	"github.com/mdayat/go-template/internal/api"
	"github.com/mdayat/go-template/internal/telemetry"
)

// Too short and running requests are cut off before their own time limit; too long and Docker
// kills the process (10 s after SIGTERM by default) before it closes what's left and sends its last
// metrics.
func TestShutdownTimeout(t *testing.T) {
	t.Parallel()
	if shutdownTimeout < api.RequestTimeout || shutdownTimeout+telemetry.FlushTimeout >= 10*time.Second {
		t.Errorf("shutdownTimeout = %s, want at least api.RequestTimeout (%s), and under 10s with telemetry.FlushTimeout (%s)",
			shutdownTimeout, api.RequestTimeout, telemetry.FlushTimeout)
	}
}
