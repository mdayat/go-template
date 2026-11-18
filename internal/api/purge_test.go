package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/jobs"
)

// The endpoint queues the job with the age the caller asked for, through the outbox, whose row's ID
// is the job's; the worker moves it to the queue and removes the rows. It uses only the shared
// helpers in main_test.go: it outlives any one feature's tests.
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
	var jobType string
	var timeout, maxRetries int
	var payload []byte
	err := pool.QueryRow(t.Context(), "SELECT job_type, timeout_seconds, max_retries, payload FROM outbox WHERE id = $1", reply.JobID).
		Scan(&jobType, &timeout, &maxRetries, &payload)
	if err != nil {
		t.Fatalf("job %s not in the outbox: %v", reply.JobID, err)
	}
	if jobType != jobs.TypePurgeDeletedRows || timeout != 300 || maxRetries != 5 {
		t.Errorf("queued job = type %q, timeout %d s, max retries %d; want %q, 300, 5", jobType, timeout, maxRetries, jobs.TypePurgeDeletedRows)
	}
	var got jobs.PurgeDeletedRows
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("queued payload is not JSON: %q: %v", payload, err)
	}
	if diff := cmp.Diff(jobs.PurgeDeletedRows{OlderThanDays: 45}, got); diff != "" {
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
