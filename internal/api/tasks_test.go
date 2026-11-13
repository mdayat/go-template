package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/task"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/testqueue"
)

// seedTask inserts a task straight into the database, made createdAfter seedTime.
func seedTask(t *testing.T, pool *pgxpool.Pool, title string, createdAfter time.Duration) db.Task {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`INSERT INTO tasks (title, created_at, updated_at) VALUES ($1, $2, $2)
		 RETURNING id, title, done, created_at, updated_at`, title, seedTime.Add(createdAfter))
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return row
}

// tasksInDB returns every task in the database, by title.
func tasksInDB(t *testing.T, pool *pgxpool.Pool) []db.Task {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT id, title, done, created_at, updated_at FROM tasks ORDER BY title")
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	return tasks
}

func asReply(row db.Task) taskResponse {
	return taskResponse{ID: row.ID, Title: row.Title, Done: row.Done, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func TestCreateTask(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		title     string
		wantTitle string
	}{
		{"plain title", "write docs", "write docs"},
		{"spaces around are trimmed", "  write docs \n", "write docs"},
		{"longest title counts characters, not bytes", strings.Repeat("é", task.MaxTitleLength), strings.Repeat("é", task.MaxTitleLength)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, pool := testAPI(t)

			rec := send(t, router, http.MethodPost, "/tasks", fmt.Sprintf(`{"title":%q}`, tt.title))

			if rec.Code != http.StatusCreated {
				t.Fatalf("POST /tasks status = %d, want 201; body %s", rec.Code, rec.Body)
			}
			var got taskResponse
			decodeReply(t, rec, &got)
			if loc := rec.Header().Get("Location"); loc != "/tasks/"+got.ID.String() {
				t.Errorf("Location = %q, want /tasks/%s", loc, got.ID)
			}
			if got.CreatedAt.Location() != time.UTC || got.UpdatedAt.Location() != time.UTC {
				t.Errorf("times = %s, %s, want UTC", got.CreatedAt, got.UpdatedAt)
			}
			if got.Title != tt.wantTitle || got.Done || !got.UpdatedAt.Equal(got.CreatedAt) {
				t.Errorf("reply = %+v, want title %q, not done, updated_at = created_at", got, tt.wantTitle)
			}
			inDB := tasksInDB(t, pool)
			if len(inDB) != 1 {
				t.Fatalf("tasks in database = %d, want 1", len(inDB))
			}
			if diff := cmp.Diff(asReply(inDB[0]), got); diff != "" {
				t.Errorf("reply differs from the database (-db +reply):\n%s", diff)
			}
		})
	}
}

func TestCreateTaskRejects(t *testing.T) {
	t.Parallel()
	requiredTitle := []fieldProblem{{Field: "title", Rule: "required", Message: "is required"}}
	noNUL := []fieldProblem{{Field: "title", Rule: "no_nul", Message: "must not contain the NUL character"}}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantDetail string
		wantErrors []fieldProblem
	}{
		{"no title", `{}`, 422, "request body is invalid", requiredTitle},
		{"blank title", `{"title":"   "}`, 422, "the title is invalid", requiredTitle},
		{
			"title too long", fmt.Sprintf(`{"title":%q}`, strings.Repeat("é", task.MaxTitleLength+1)), 422, "the title is invalid",
			[]fieldProblem{{Field: "title", Rule: "max", Message: "must be at most 200 characters long"}},
		},
		{"title with a NUL character", `{"title":"a\u0000b"}`, 422, "the title is invalid", noNUL},
		{"title taken", `{"title":"taken"}`, 409, "a task with this title already exists", nil},
		{"title taken once trimmed", `{"title":" taken "}`, 409, "a task with this title already exists", nil},
		{"form body, not JSON", `title=x`, 400, "request body must be a JSON object", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, pool := testAPI(t)
			existing := seedTask(t, pool, "taken", 0)

			rec := send(t, router, http.MethodPost, "/tasks", tt.body)

			want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
			if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
				t.Errorf("POST /tasks reply mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]db.Task{existing}, tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database after POST /tasks mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// The unique constraint, not a check-then-insert, decides between requests racing for one title.
func TestCreateTaskSameTitleAtOnce(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)

	const requests = 10
	codes := make(chan int, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			codes <- send(t, router, http.MethodPost, "/tasks", `{"title":"race"}`).Code
		})
	}
	wg.Wait()
	close(codes)

	count := map[int]int{}
	for code := range codes {
		count[code]++
	}
	if diff := cmp.Diff(map[int]int{http.StatusCreated: 1, http.StatusConflict: requests - 1}, count); diff != "" {
		t.Errorf("status counts mismatch (-want +got):\n%s", diff)
	}
	if n := len(tasksInDB(t, pool)); n != 1 {
		t.Errorf("tasks in database = %d, want 1", n)
	}
}

func TestGetTask(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	want := seedTask(t, pool, "write docs", 0)
	seedTask(t, pool, "another", time.Minute)

	rec := send(t, router, http.MethodGet, "/tasks/"+want.ID.String(), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/{id} status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var got taskResponse
	decodeReply(t, rec, &got)
	if diff := cmp.Diff(asReply(want), got); diff != "" {
		t.Errorf("GET /tasks/{id} reply mismatch (-want +got):\n%s", diff)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	seedTask(t, pool, "write docs", 0)

	for _, id := range []string{uuid.NewString(), "42"} {
		rec := send(t, router, http.MethodGet, "/tasks/"+id, "")
		want := problemReply{Status: http.StatusNotFound, Detail: "task not found"}
		if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
			t.Errorf("GET /tasks/%s reply mismatch (-want +got):\n%s", id, diff)
		}
	}
}

func TestListTasksIsEmptyWithoutTasks(t *testing.T) {
	t.Parallel()
	router, _ := testAPI(t)

	rec := send(t, router, http.MethodGet, "/tasks", "")

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"tasks":[]}` {
		t.Errorf("GET /tasks = %d %s, want 200 {\"tasks\":[]}", rec.Code, rec.Body)
	}
}

func TestListTasksDefaultPageSize(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	for i := range defaultPageSize + 1 {
		seedTask(t, pool, fmt.Sprintf("task %02d", i), time.Duration(i)*time.Minute)
	}

	var got taskListResponse
	decodeReply(t, send(t, router, http.MethodGet, "/tasks", ""), &got)

	if len(got.Tasks) != defaultPageSize || got.NextCursor == "" {
		t.Errorf("GET /tasks = %d tasks, next cursor %q; want %d and a cursor", len(got.Tasks), got.NextCursor, defaultPageSize)
	}
}

// Paging walks every task exactly once, newest first, even when tasks share a created_at.
func TestListTasksPagesThroughEveryTask(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	oldest := seedTask(t, pool, "oldest", 0)
	tiedA := seedTask(t, pool, "tied a", time.Minute)
	tiedB := seedTask(t, pool, "tied b", time.Minute)
	tiedC := seedTask(t, pool, "tied c", time.Minute)
	newest := seedTask(t, pool, "newest", 2*time.Minute)

	// Ties are ordered by id, highest first.
	tied := []db.Task{tiedA, tiedB, tiedC}
	slices.SortFunc(tied, func(a, b db.Task) int { return strings.Compare(b.ID.String(), a.ID.String()) })
	want := []taskResponse{asReply(newest), asReply(tied[0]), asReply(tied[1]), asReply(tied[2]), asReply(oldest)}

	var got []taskResponse
	var pageSizes []int
	target := "/tasks?limit=2"
	for range 10 { // more than enough pages; stops a loop that never ends
		rec := send(t, router, http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d; body %s", target, rec.Code, rec.Body)
		}
		var page taskListResponse
		decodeReply(t, rec, &page)
		got = append(got, page.Tasks...)
		pageSizes = append(pageSizes, len(page.Tasks))
		if page.NextCursor == "" {
			break
		}
		target = "/tasks?limit=2&after=" + url.QueryEscape(page.NextCursor)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("tasks across pages mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int{2, 2, 1}, pageSizes); diff != "" {
		t.Errorf("page sizes mismatch (-want +got):\n%s", diff)
	}
}

func TestListTasksRejects(t *testing.T) {
	t.Parallel()
	limitRange := []fieldProblem{{Field: "limit", Rule: "range", Message: "must be from 1 to 100"}}
	notACursor := "after is not a cursor from a previous page"

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantDetail string
		wantErrors []fieldProblem
	}{
		{"limit not a number", "limit=ten", 400, "limit must be a whole number from 1 to 100", nil},
		{"limit zero", "limit=0", 422, "page size is out of range", limitRange},
		{"limit over the most", "limit=101", 422, "page size is out of range", limitRange},
		{"cursor not base64", "after=%25%25", 400, notACursor, nil},
		{"cursor without a separator", "after=" + cursorOf("2026-01-01T00:00:00Z"), 400, notACursor, nil},
		{"cursor with a bad time", "after=" + cursorOf("yesterday|"+uuid.NewString()), 400, notACursor, nil},
		{"cursor with a bad id", "after=" + cursorOf("2026-01-01T00:00:00Z|42"), 400, notACursor, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, _ := testAPI(t)
			rec := send(t, router, http.MethodGet, "/tasks?"+tt.query, "")
			want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
			if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
				t.Errorf("GET /tasks?%s reply mismatch (-want +got):\n%s", tt.query, diff)
			}
		})
	}
}

func TestUpdateTask(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		wantTitle     string
		wantDone      bool
		wantUnchanged bool // updated_at stays as it was
	}{
		{"new title, trimmed", `{"title":" renamed "}`, "renamed", false, false},
		{"mark done", `{"done":true}`, "write docs", true, false},
		{"both", `{"title":"renamed","done":true}`, "renamed", true, false},
		{"same title as before", `{"title":"write docs"}`, "write docs", false, false},
		{"no fields changes nothing", `{}`, "write docs", false, true},
		{"null fields change nothing", `{"title":null,"done":null}`, "write docs", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, pool := testAPI(t)
			before := seedTask(t, pool, "write docs", 0)
			other := seedTask(t, pool, "other", time.Minute)

			rec := send(t, router, http.MethodPatch, "/tasks/"+before.ID.String(), tt.body)

			if rec.Code != http.StatusOK {
				t.Fatalf("PATCH /tasks/{id} status = %d, want 200; body %s", rec.Code, rec.Body)
			}
			var got taskResponse
			decodeReply(t, rec, &got)
			if got.Title != tt.wantTitle || got.Done != tt.wantDone || !got.CreatedAt.Equal(before.CreatedAt) {
				t.Errorf("reply = %+v, want title %q, done %v, created_at %s", got, tt.wantTitle, tt.wantDone, before.CreatedAt)
			}
			if unchanged := got.UpdatedAt.Equal(before.UpdatedAt); unchanged != tt.wantUnchanged {
				t.Errorf("updated_at = %s (was %s), want unchanged = %v", got.UpdatedAt, before.UpdatedAt, tt.wantUnchanged)
			}
			after := db.Task{ID: got.ID, Title: got.Title, Done: got.Done, CreatedAt: got.CreatedAt, UpdatedAt: got.UpdatedAt}
			if diff := cmp.Diff(sortedByTitle(after, other), tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database after PATCH mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateTaskRejects(t *testing.T) {
	t.Parallel()
	requiredTitle := []fieldProblem{{Field: "title", Rule: "required", Message: "is required"}}

	tests := []struct {
		name       string
		id         string // "" means the seeded task
		body       string
		wantStatus int
		wantDetail string
		wantErrors []fieldProblem
	}{
		{"unknown task", uuid.NewString(), `{"done":true}`, 404, "task not found", nil},
		{"unknown task, no fields", uuid.NewString(), `{}`, 404, "task not found", nil},
		{"id not a UUID", "42", `{"done":true}`, 404, "task not found", nil},
		{"blank title", "", `{"title":" "}`, 422, "the title is invalid", requiredTitle},
		{
			"title too long", "", fmt.Sprintf(`{"title":%q}`, strings.Repeat("a", task.MaxTitleLength+1)), 422,
			"the title is invalid",
			[]fieldProblem{{Field: "title", Rule: "max", Message: "must be at most 200 characters long"}},
		},
		{
			"title with a NUL character", "", `{"title":"a\u0000b"}`, 422, "the title is invalid",
			[]fieldProblem{{Field: "title", Rule: "no_nul", Message: "must not contain the NUL character"}},
		},
		{"title of another task", "", `{"title":"other"}`, 409, "a task with this title already exists", nil},
		{"unknown field", "", `{"colour":"red"}`, 400, `unknown field "colour"`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, pool := testAPI(t)
			seeded := seedTask(t, pool, "write docs", 0)
			other := seedTask(t, pool, "other", time.Minute)
			id := tt.id
			if id == "" {
				id = seeded.ID.String()
			}

			rec := send(t, router, http.MethodPatch, "/tasks/"+id, tt.body)

			want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
			if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
				t.Errorf("PATCH /tasks/%s reply mismatch (-want +got):\n%s", id, diff)
			}
			if diff := cmp.Diff(sortedByTitle(seeded, other), tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database after PATCH mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDeleteTask(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	doomed := seedTask(t, pool, "write docs", 0)
	kept := seedTask(t, pool, "other", time.Minute)

	rec := send(t, router, http.MethodDelete, "/tasks/"+doomed.ID.String(), "")

	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("DELETE /tasks/{id} = %d %q, want 204 with no body", rec.Code, rec.Body)
	}
	if diff := cmp.Diff([]db.Task{kept}, tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database after DELETE mismatch (-want +got):\n%s", diff)
	}
}

func TestDeleteTaskNotFound(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	kept := seedTask(t, pool, "write docs", 0)

	for _, id := range []string{uuid.NewString(), "42"} {
		rec := send(t, router, http.MethodDelete, "/tasks/"+id, "")
		want := problemReply{Status: http.StatusNotFound, Detail: "task not found"}
		if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
			t.Errorf("DELETE /tasks/%s reply mismatch (-want +got):\n%s", id, diff)
		}
	}
	if diff := cmp.Diff([]db.Task{kept}, tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database after DELETE mismatch (-want +got):\n%s", diff)
	}
}

// A database failure is a 500 that tells the caller nothing about the database.
func TestTasksDatabaseDownIs500(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	pool.Close()

	rec := send(t, router, http.MethodGet, "/tasks", "")

	want := problemReply{Status: http.StatusInternalServerError, Detail: internalErrorDetail}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("GET /tasks reply mismatch (-want +got):\n%s", diff)
	}
}

func sortedByTitle(a, b db.Task) []db.Task {
	if b.Title < a.Title {
		return []db.Task{b, a}
	}
	return []db.Task{a, b}
}

// The endpoint only queues the job, with the age the caller asked for; the worker deletes the rows.
func TestPurgeTasksQueuesAJob(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	done := seedTask(t, pool, "done long ago", 0)
	if _, err := pool.Exec(t.Context(), "UPDATE tasks SET done = true WHERE id = $1", done.ID); err != nil {
		t.Fatalf("mark task done: %v", err)
	}
	before := tasksInDB(t, pool)

	rec := send(t, router, http.MethodPost, "/task-purges", `{"older_than_days":45}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /task-purges status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	var reply jobResponse
	decodeReply(t, rec, &reply)
	job := testqueue.Take(t, testqueue.New(t), jobs.Queue, reply.JobID)
	if job.Type != jobs.TypePurgeDoneTasks {
		t.Errorf("queued job type = %q, want %q", job.Type, jobs.TypePurgeDoneTasks)
	}
	var payload jobs.PurgeDoneTasks
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("queued payload is not JSON: %q: %v", job.Payload, err)
	}
	if diff := cmp.Diff(jobs.PurgeDoneTasks{OlderThanDays: 45}, payload); diff != "" {
		t.Errorf("queued payload mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database after POST /task-purges mismatch (-want +got):\n%s", diff)
	}
}

func TestPurgeTasksRejects(t *testing.T) {
	t.Parallel()
	outOfRange := []fieldProblem{{Field: "older_than_days", Rule: "range", Message: "must be from 1 to 3650"}}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantDetail string
		wantErrors []fieldProblem
	}{
		{
			"no age", `{}`, 422, "request body is invalid",
			[]fieldProblem{{Field: "older_than_days", Rule: "required", Message: "is required"}},
		},
		{"zero days", `{"older_than_days":0}`, 422, "the age is out of range", outOfRange},
		{"negative days", `{"older_than_days":-1}`, 422, "the age is out of range", outOfRange},
		{"over the limit", fmt.Sprintf(`{"older_than_days":%d}`, task.MaxPurgeAgeDays+1), 422, "the age is out of range", outOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, _ := testAPI(t)

			rec := send(t, router, http.MethodPost, "/task-purges", tt.body)

			want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
			if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
				t.Errorf("POST /task-purges reply mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A queue that's down is a 500 that tells the caller nothing about the queue.
func TestPurgeTasksQueueDownIs500(t *testing.T) {
	t.Parallel()
	// Nothing listens on port 1.
	down := redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1})
	t.Cleanup(func() { _ = down.Close() })
	router := NewRouter(zerolog.Nop(), NewServices(db.New(testdb.New(t)), down))

	rec := send(t, router, http.MethodPost, "/task-purges", `{"older_than_days":30}`)

	want := problemReply{Status: http.StatusInternalServerError, Detail: internalErrorDetail}
	if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
		t.Errorf("POST /task-purges reply mismatch (-want +got):\n%s", diff)
	}
}
