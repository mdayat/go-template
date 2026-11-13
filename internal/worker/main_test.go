package worker

import (
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/testqueue"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// testWorker returns the real job handlers and every feature's service, built by NewServices as in
// the worker, on a database of the test's own. Every job test uses it.
func testWorker(t *testing.T) (*asynq.ServeMux, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	return NewMux(zerolog.Nop(), NewServices(db.New(pool))), pool
}

// Helpers shared by every feature's job tests live here, not in a feature's test file, so removing
// one feature (the tasks example) can't break another's tests.

const day = 24 * time.Hour

// scheduledEntry is one job on the scheduler's timetable, in the form asynq publishes it.
type scheduledEntry struct {
	Spec    string
	Type    string
	Payload string
	Opts    []string
}

// scheduledEntries starts the real scheduler and returns its entries for jobs of jobType, each
// distinct entry once: every test's scheduler, and any other test run's on the same Valkey,
// publishes the same entries to one shared list.
func scheduledEntries(t *testing.T, jobType string) []scheduledEntry {
	t.Helper()
	client := testqueue.New(t)
	scheduler, err := newScheduler(client, zerolog.Nop(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("newScheduler() error = %v", err)
	}
	if err := scheduler.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(scheduler.Shutdown)

	// The scheduler publishes its entries on each heartbeat, from a goroutine of its own.
	inspector := asynq.NewInspectorFromRedisClient(client)
	var got []scheduledEntry
	for deadline := time.Now().Add(5 * time.Second); len(got) == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		entries, err := inspector.SchedulerEntries()
		if err != nil {
			t.Fatalf("list scheduler entries: %v", err)
		}
		for _, e := range entries {
			if e.Task.Type() != jobType {
				continue
			}
			entry := scheduledEntry{Spec: e.Spec, Type: jobType, Payload: string(e.Task.Payload())}
			for _, o := range e.Opts {
				entry.Opts = append(entry.Opts, o.String())
			}
			if !slices.ContainsFunc(got, func(g scheduledEntry) bool { return cmp.Equal(g, entry) }) {
				got = append(got, entry)
			}
		}
	}
	return got
}
