package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mdayat/go-template/internal/jobs"
)

// seedTask inserts a task last changed age ago.
func seedTask(t *testing.T, pool *pgxpool.Pool, title string, done bool, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(t.Context(),
		"INSERT INTO tasks (title, done, created_at, updated_at) VALUES ($1, $2, now() - $3::interval, now() - $3::interval)",
		title, done, age)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// taskTitles returns the title of every task in the database, sorted.
func taskTitles(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT title FROM tasks ORDER BY title")
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	titles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	return titles
}

func purgeJob(t *testing.T, olderThanDays int) *asynq.Task {
	t.Helper()
	job, err := jobs.NewPurgeDoneTasks(olderThanDays)
	if err != nil {
		t.Fatalf("NewPurgeDoneTasks(%d) error = %v", olderThanDays, err)
	}
	return job
}

// Deletes only tasks that are done and older than the age, and a second run changes nothing: asynq
// runs a job again when the worker stops mid-job, and the API can queue the same purge twice.
func TestPurgeDoneTasks(t *testing.T) {
	t.Parallel()
	mux, pool := testWorker(t)
	seedTask(t, pool, "done 31 days ago", true, 31*day)
	seedTask(t, pool, "done 29 days ago", true, 29*day)
	seedTask(t, pool, "open 40 days", false, 40*day)
	want := []string{"done 29 days ago", "open 40 days"}

	for _, run := range []string{"first", "second"} {
		if err := mux.ProcessTask(t.Context(), purgeJob(t, 30)); err != nil {
			t.Fatalf("%s purge error = %v", run, err)
		}
		if diff := cmp.Diff(want, taskTitles(t, pool)); diff != "" {
			t.Errorf("tasks after %s purge mismatch (-want +got):\n%s", run, diff)
		}
	}
}

// A newer API may add payload fields this worker doesn't know yet; the job still runs.
func TestPurgeDoneTasksAcceptsUnknownFields(t *testing.T) {
	t.Parallel()
	mux, pool := testWorker(t)
	seedTask(t, pool, "done 31 days ago", true, 31*day)

	job := asynq.NewTask(jobs.TypePurgeDoneTasks, []byte(`{"older_than_days":30,"added_later":true}`))
	if err := mux.ProcessTask(t.Context(), job); err != nil {
		t.Fatalf("purge error = %v", err)
	}
	if got := taskTitles(t, pool); len(got) != 0 {
		t.Errorf("tasks after purge = %q, want none", got)
	}
}

// A payload that can't work fails without retries, and deletes nothing.
func TestPurgeDoneTasksRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
	}{
		{"not JSON", `not json`},
		{"no age", `{}`},
		{"zero days", `{"older_than_days":0}`},
		{"over the limit", `{"older_than_days":3651}`},
		{"age as text", `{"older_than_days":"30"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux, pool := testWorker(t)
			seedTask(t, pool, "done 31 days ago", true, 31*day)

			err := mux.ProcessTask(t.Context(), asynq.NewTask(jobs.TypePurgeDoneTasks, []byte(tt.payload)))

			if !errors.Is(err, asynq.SkipRetry) {
				t.Errorf("purge error = %v, want one wrapping asynq.SkipRetry", err)
			}
			if diff := cmp.Diff([]string{"done 31 days ago"}, taskTitles(t, pool)); diff != "" {
				t.Errorf("tasks after purge mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// The purge is on the timetable, unique so that several workers queue it once a day, not once each.
func TestSchedulerQueuesDailyPurge(t *testing.T) {
	t.Parallel()

	want := []scheduledEntry{{
		Spec: "@daily", Type: jobs.TypePurgeDoneTasks, Payload: `{"older_than_days":30}`,
		Opts: []string{"Unique(1h0m0s)"},
	}}
	if diff := cmp.Diff(want, scheduledEntries(t, jobs.TypePurgeDoneTasks)); diff != "" {
		t.Errorf("scheduler entries mismatch (-want +got):\n%s", diff)
	}
}
