package worker

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/webhook"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// testWorker returns the real job handlers and every feature's service, built by NewServices as in
// the worker, on a database of the test's own, reporting to no webhook. Every job test uses it.
func testWorker(t *testing.T) (*queue.Mux, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	return newTestMux(t, NewServices(db.New(pool), webhook.Discard{})), pool
}

// newTestMux is NewMux for tests: no logs, no metrics.
func newTestMux(t *testing.T, services Services) *queue.Mux {
	t.Helper()
	mux, err := NewMux(zerolog.Nop(), noop.NewMeterProvider(), services)
	if err != nil {
		t.Fatalf("NewMux() error = %v", err)
	}
	return mux
}

// Helpers shared by every feature's job tests live here, not in a feature's test file, so removing
// one feature (the tasks example) can't break another's tests.

const day = 24 * time.Hour
