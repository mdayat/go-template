package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/httpclient"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/webhook"
)

// seedTask inserts a task made and last changed 40 days ago, done, and deleted deletedAgo ago, or
// live when deletedAgo is 0. Only the delete's age decides a purge.
func seedTask(t *testing.T, pool *pgxpool.Pool, title string, deletedAgo time.Duration) {
	t.Helper()
	var deletedAt *time.Time
	if deletedAgo > 0 {
		at := time.Now().Add(-deletedAgo)
		deletedAt = &at
	}
	_, err := pool.Exec(t.Context(),
		`INSERT INTO tasks (title, done, created_at, updated_at, deleted_at)
		 VALUES ($1, true, now() - interval '40 days', now() - interval '40 days', $2)`,
		title, deletedAt)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// taskTitles returns the title of every task in the database, deleted ones too, sorted.
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

// purgePayload returns the payload queued for a purge.
func purgePayload(t *testing.T, olderThanDays int) []byte {
	t.Helper()
	job, err := jobs.NewPurgeDeletedRows(olderThanDays)
	if err != nil {
		t.Fatalf("NewPurgeDeletedRows(%d) error = %v", olderThanDays, err)
	}
	return job.Payload
}

// Removes only rows deleted before the age, and a second run changes nothing: asynq runs a job
// again when the worker stops mid-job, and the same purge can be queued twice.
func TestPurgeDeletedRows(t *testing.T) {
	t.Parallel()
	mux, pool := testWorker(t)
	seedTask(t, pool, "deleted 31 days ago", 31*day)
	seedTask(t, pool, "deleted 29 days ago", 29*day)
	seedTask(t, pool, "live, done 40 days ago", 0)
	want := []string{"deleted 29 days ago", "live, done 40 days ago"}

	for _, run := range []string{"first", "second"} {
		if err := mux.Process(t.Context(), jobs.TypePurgeDeletedRows, purgePayload(t, 30)); err != nil {
			t.Fatalf("%s purge error = %v", run, err)
		}
		if diff := cmp.Diff(want, taskTitles(t, pool)); diff != "" {
			t.Errorf("tasks after %s purge mismatch (-want +got):\n%s", run, diff)
		}
	}
}

// A newer version may add payload fields this worker doesn't know yet; the job still runs.
func TestPurgeDeletedRowsAcceptsUnknownFields(t *testing.T) {
	t.Parallel()
	mux, pool := testWorker(t)
	seedTask(t, pool, "deleted 31 days ago", 31*day)

	payload := []byte(`{"older_than_days":30,"added_later":true}`)
	if err := mux.Process(t.Context(), jobs.TypePurgeDeletedRows, payload); err != nil {
		t.Fatalf("purge error = %v", err)
	}
	if got := taskTitles(t, pool); len(got) != 0 {
		t.Errorf("tasks after purge = %q, want none", got)
	}
}

// A payload that can't work fails without retries, and removes nothing.
func TestPurgeDeletedRowsRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
	}{
		{"not JSON", `not json`},
		{"no age", `{}`},
		{"zero days", `{"older_than_days":0}`},
		{"age as text", `{"older_than_days":"30"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux, pool := testWorker(t)
			seedTask(t, pool, "deleted 31 days ago", 31*day)

			err := mux.Process(t.Context(), jobs.TypePurgeDeletedRows, []byte(tt.payload))

			if !errors.Is(err, queue.ErrNoRetry) {
				t.Errorf("purge error = %v, want one wrapping queue.ErrNoRetry", err)
			}
			if diff := cmp.Diff([]string{"deleted 31 days ago"}, taskTitles(t, pool)); diff != "" {
				t.Errorf("tasks after purge mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// With no soft-deleted table, no table's purge checks the age: the job must, or it reports a purge
// that never ran.
func TestPurgeDeletedRowsWithNoTablesRejectsABadAge(t *testing.T) {
	t.Parallel()
	server, received := testWebhook(t, http.StatusNoContent)
	mux := queue.NewMux()
	// As NewMux does: logJobs turns an apperr kind into a failure that isn't retried.
	mux.Use(logJobs(zerolog.Nop(), noop.Float64Histogram{}))
	purgeJobs{reports: webhook.NewHTTPSender(httpclient.New(5*time.Second), server.URL)}.mount(mux)

	err := mux.Process(t.Context(), jobs.TypePurgeDeletedRows, []byte(`{"older_than_days":0}`))

	if !errors.Is(err, queue.ErrNoRetry) {
		t.Errorf("purge error = %v, want one wrapping queue.ErrNoRetry", err)
	}
	if len(received) != 0 {
		t.Errorf("webhook got %d reports, want none", len(received))
	}
}

// The purge is on the timetable, unique so that several workers queue it once a day, not once each.
func TestSchedulesHoldTheDailyPurge(t *testing.T) {
	t.Parallel()

	schedules, err := Schedules()
	if err != nil {
		t.Fatalf("Schedules() error = %v", err)
	}

	want := queue.Schedule{
		Spec: "@daily",
		Job: queue.Job{
			Type: jobs.TypePurgeDeletedRows, Queue: jobs.Queue, Payload: []byte(`{"older_than_days":30}`),
			Timeout: 5 * time.Minute, MaxRetries: 5,
		},
		Unique: time.Hour,
	}
	var got []queue.Schedule
	for _, s := range schedules {
		if s.Job.Type == jobs.TypePurgeDeletedRows {
			got = append(got, s)
		}
	}
	if diff := cmp.Diff([]queue.Schedule{want}, got); diff != "" {
		t.Errorf("purge schedules mismatch (-want +got):\n%s", diff)
	}
}

// receivedReport is what the test webhook got in one request.
type receivedReport struct {
	Method         string
	ContentType    string
	IdempotencyKey string
	Body           string
}

// testWebhook is an outside service the purge reports to, answering each request with the next
// status in replies (the last one repeats), and recording what it got.
func testWebhook(t *testing.T, replies ...int) (*httptest.Server, chan receivedReport) {
	t.Helper()
	received := make(chan receivedReport, 10)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedReport{
			Method: r.Method, ContentType: r.Header.Get("Content-Type"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Body: string(body),
		}
		w.WriteHeader(replies[min(int(calls.Add(1))-1, len(replies)-1)])
	}))
	t.Cleanup(server.Close)
	return server, received
}

// The purge reports what's now true, with the job's ID as the idempotency key; when the webhook
// fails, the job fails to be retried, and the retry reports the same thing with the same key.
func TestPurgeDeletedRowsReportsToTheWebhook(t *testing.T) {
	t.Parallel()
	server, received := testWebhook(t, http.StatusServiceUnavailable, http.StatusNoContent)
	mux, pool := testWorkerReportingTo(t, server.URL, 5*time.Second)
	seedTask(t, pool, "deleted 31 days ago", 31*day)
	ctx := queue.WithRun(t.Context(), queue.Run{ID: "job-1", Type: jobs.TypePurgeDeletedRows, Attempt: 1, MaxAttempts: 6})

	first := mux.Process(ctx, jobs.TypePurgeDeletedRows, purgePayload(t, 30))
	second := mux.Process(ctx, jobs.TypePurgeDeletedRows, purgePayload(t, 30))

	if first == nil || errors.Is(first, queue.ErrNoRetry) {
		t.Errorf("first purge error = %v, want one that's retried: the webhook replied 503", first)
	}
	if second != nil {
		t.Errorf("second purge error = %v, want nil", second)
	}
	want := receivedReport{
		Method: http.MethodPost, ContentType: "application/json", IdempotencyKey: "job-1",
		Body: `{"event":"deleted_rows.purged","older_than_days":30}`,
	}
	for _, run := range []string{"first", "second"} {
		if diff := cmp.Diff(want, <-received); diff != "" {
			t.Errorf("%s report mismatch (-want +got):\n%s", run, diff)
		}
	}
	if got := taskTitles(t, pool); len(got) != 0 {
		t.Errorf("tasks after purge = %q, want none", got)
	}
}

// A webhook failure decides whether the job is retried: a retry can't fix a 4xx other than 408 and
// 429, so the job fails for good; anything else is retried.
func TestPurgeDeletedRowsWebhookFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		reply       int
		wantNoRetry bool
	}{
		{"bad request is not retried", http.StatusBadRequest, true},
		{"gone is not retried", http.StatusGone, true},
		{"too many requests is retried", http.StatusTooManyRequests, false},
		{"server error is retried", http.StatusInternalServerError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server, _ := testWebhook(t, tt.reply)
			mux, _ := testWorkerReportingTo(t, server.URL, 5*time.Second)

			err := mux.Process(t.Context(), jobs.TypePurgeDeletedRows, purgePayload(t, 30))

			if err == nil || errors.Is(err, queue.ErrNoRetry) != tt.wantNoRetry {
				t.Errorf("purge error = %v, want an error with ErrNoRetry = %v", err, tt.wantNoRetry)
			}
		})
	}
}

// A webhook that doesn't answer is cut off at the client's time limit, and the job is retried.
func TestPurgeDeletedRowsSlowWebhookIsCutOff(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	mux, _ := testWorkerReportingTo(t, server.URL, 200*time.Millisecond)

	// A deadline of its own, so a client that lost its limit fails the timing check below instead of
	// hanging the test.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	start := time.Now()
	err := mux.Process(ctx, jobs.TypePurgeDeletedRows, purgePayload(t, 30))

	if err == nil || errors.Is(err, queue.ErrNoRetry) {
		t.Errorf("purge error = %v, want one that's retried", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("purge took %s, want it cut off near the client's 200ms time limit", took)
	}
	if strings.Contains(fmt.Sprint(err), server.URL) {
		t.Errorf("purge error = %v, want it without the webhook's URL", err)
	}
}

// Every table with deleted_at is purged: one left out of tablePurges keeps its deleted rows
// forever.
func TestEverySoftDeletedTableIsPurged(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	var purged []string
	for _, tp := range tablePurges(NewServices(db.New(pool), webhook.Discard{})) {
		purged = append(purged, tp.table)
	}
	slices.Sort(purged)

	if diff := cmp.Diff(testdb.SoftDeletedTables(t, pool), purged, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("tables with deleted_at (-) vs tablePurges (+):\n%s", diff)
	}
}

// testWorkerReportingTo is testWorker with purges reported to the webhook at url, through a client
// with timeout.
func testWorkerReportingTo(t *testing.T, url string, timeout time.Duration) (*queue.Mux, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	reports := webhook.NewHTTPSender(httpclient.New(timeout), url)
	return newTestMux(t, NewServices(db.New(pool), reports)), pool
}
