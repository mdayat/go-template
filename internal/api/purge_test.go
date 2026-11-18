package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/queue/valkey"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/testqueue"
	"github.com/mdayat/go-template/internal/testredis"
)

// The endpoint queues the job with the age the caller asked for; the worker removes the rows. It
// uses only the shared helpers in main_test.go: it outlives any one feature's tests.
func TestPurgeDeletedRowsQueuesAJob(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	testUser(t, pool, true)

	rec := send(t, router, http.MethodPost, "/admin/deleted-row-purges", `{"older_than_days":45}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /admin/deleted-row-purges status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	var reply struct {
		JobID string `json:"job_id"`
	}
	decodeReply(t, rec, &reply)
	job := testqueue.Take(t, testredis.New(t), jobs.Queue, reply.JobID)
	if job.Type != jobs.TypePurgeDeletedRows || job.Timeout != 5*time.Minute || job.MaxRetry != 5 {
		t.Errorf("queued job = type %q, timeout %s, max retry %d; want %q, 5m0s, 5",
			job.Type, job.Timeout, job.MaxRetry, jobs.TypePurgeDeletedRows)
	}
	var payload jobs.PurgeDeletedRows
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("queued payload is not JSON: %q: %v", job.Payload, err)
	}
	if diff := cmp.Diff(jobs.PurgeDeletedRows{OlderThanDays: 45}, payload); diff != "" {
		t.Errorf("queued payload mismatch (-want +got):\n%s", diff)
	}
}

func TestPurgeDeletedRowsRejects(t *testing.T) {
	t.Parallel()
	outOfRange := []handler.FieldProblem{{Field: "older_than_days", Rule: "range", Message: "must be from 1 to 3650"}}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantDetail string
		wantErrors []handler.FieldProblem
	}{
		{
			"no age", `{}`, 422, "request body is invalid",
			[]handler.FieldProblem{{Field: "older_than_days", Rule: "required", Message: "is required"}},
		},
		{"zero days", `{"older_than_days":0}`, 422, "the age is out of range", outOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, pool := testAPI(t)
			testUser(t, pool, true)

			rec := send(t, router, http.MethodPost, "/admin/deleted-row-purges", tt.body)

			want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
			if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
				t.Errorf("POST /admin/deleted-row-purges reply mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A queue that's down is a 500 that tells the caller nothing about the queue.
func TestPurgeDeletedRowsQueueDownIs500(t *testing.T) {
	t.Parallel()
	// Nothing listens on port 1.
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1})
	t.Cleanup(func() { _ = down.Close() })
	authn, issuer := testAuthenticator(t)
	pool := testdb.New(t)
	testUser(t, pool, true)
	router := signedIn(NewRouter(zerolog.Nop(), noop.NewMeterProvider(), tracenoop.NewTracerProvider(), NewServices(db.New(pool), valkey.NewEnqueuer(down, tracenoop.NewTracerProvider())), NewReadiness(pool.Ping, zerolog.Nop()), authn, Limits{}, nil),
		issuer.Token(testSubject))

	rec := send(t, router, http.MethodPost, "/admin/deleted-row-purges", `{"older_than_days":30}`)

	want := problemReply{Status: http.StatusInternalServerError, Detail: handler.InternalErrorDetail}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("POST /admin/deleted-row-purges reply mismatch (-want +got):\n%s", diff)
	}
}

// Purging removes every user's deleted rows for good, so only an admin may (the /admin group's
// check, admin.RequireAdmin); anyone else gets a 403.
func TestPurgeDeletedRowsIsForAdmins(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	testUser(t, pool, false)

	rec := send(t, router, http.MethodPost, "/admin/deleted-row-purges", `{"older_than_days":30}`)

	want := problemReply{Status: http.StatusForbidden, Detail: "only admins may use this"}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("POST /admin/deleted-row-purges reply mismatch (-want +got):\n%s", diff)
	}
}
