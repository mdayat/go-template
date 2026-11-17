package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
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

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/task"
)

// taskClients are the clients serving tasks, each under its own prefix. They behave the same today,
// so every task test runs against each (a subtest per client); when one client's routes change, its
// expectations change and the other's don't.
var taskClients = []struct{ name, tasks string }{
	{"mobile", "/mobile/v1/tasks"},
	{"web", "/web/tasks"},
}

// taskReply is a task as both clients reply with it today; the mobile reply's exact form is pinned
// by TestMobileTaskReplyBytes, whatever happens to this type.
type taskReply struct {
	ID        uuid.UUID  `json:"id"`
	Title     string     `json:"title"`
	Done      bool       `json:"done"`
	OwnerID   *uuid.UUID `json:"owner_id"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type taskListReply struct {
	Tasks      []taskReply `json:"tasks"`
	NextCursor string      `json:"next_cursor"`
}

// seedTask inserts a task owned by the signed-in test user straight into the database, made
// createdAfter seedTime.
func seedTask(t *testing.T, pool *pgxpool.Pool, title string, createdAfter time.Duration) db.Task {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`INSERT INTO tasks (title, created_at, updated_at, owner_id) VALUES ($1, $2, $2, $3)
		 RETURNING id, title, done, created_at, updated_at, deleted_at, owner_id`,
		title, seedTime.Add(createdAfter), testUserID(t, pool))
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return row
}

// markDeleted deletes task straight in the database and returns it as stored.
func markDeleted(t *testing.T, pool *pgxpool.Pool, task db.Task) db.Task {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		"UPDATE tasks SET deleted_at = now() WHERE id = $1 RETURNING id, title, done, created_at, updated_at, deleted_at, owner_id",
		task.ID)
	if err != nil {
		t.Fatalf("mark task deleted: %v", err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("mark task deleted: %v", err)
	}
	return row
}

// tasksInDB returns every task in the database, deleted ones too, by title, then oldest first.
func tasksInDB(t *testing.T, pool *pgxpool.Pool) []db.Task {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT id, title, done, created_at, updated_at, deleted_at, owner_id FROM tasks ORDER BY title, created_at")
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	return tasks
}

func asReply(row db.Task) taskReply {
	return taskReply{
		ID: row.ID, Title: row.Title, Done: row.Done, OwnerID: row.OwnerID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

// The mobile apps' task reply has exactly these keys: old installs read them for years, so a key
// can be added (here too) but never renamed or removed.
func TestMobileTaskReplyKeys(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	seedTask(t, pool, "write docs", 0)
	seedTask(t, pool, "another", time.Minute)

	var list map[string]json.RawMessage
	decodeReply(t, send(t, router, http.MethodGet, "/mobile/v1/tasks?limit=1", ""), &list)
	var tasks []map[string]json.RawMessage
	if err := json.Unmarshal(list["tasks"], &tasks); err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %s, want one task", list["tasks"])
	}

	if got, want := slices.Sorted(maps.Keys(list)), []string{"next_cursor", "tasks"}; !slices.Equal(got, want) {
		t.Errorf("list reply keys = %q, want %q", got, want)
	}
	if got, want := slices.Sorted(maps.Keys(tasks[0])), []string{"created_at", "done", "id", "owner_id", "title", "updated_at"}; !slices.Equal(got, want) {
		t.Errorf("task reply keys = %q, want %q", got, want)
	}
}

// The mobile apps' task reply, byte for byte: old installs parse it for years, so a key's name,
// type, presence when null (owner_id) or time format can't change; this fails first.
func TestMobileTaskReplyBytes(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	testUser(t, pool, true) // a task without an owner is an admin's to see
	const id = "0b7e1f5e-8a2c-4c7b-9d1e-2f3a4b5c6d7e"
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO tasks (id, title, created_at, updated_at) VALUES ($1, 'write docs', $2, $2)",
		id, time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)); err != nil {
		t.Fatal(err)
	}

	rec := send(t, router, http.MethodGet, "/mobile/v1/tasks/"+id, "")

	want := `{"id":"` + id + `","title":"write docs","done":false,"owner_id":null,` +
		`"created_at":"2026-01-02T03:04:05.123456Z","updated_at":"2026-01-02T03:04:05.123456Z"}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != want {
		t.Errorf("GET /mobile/v1/tasks/{id} = %d\n%s\nwant 200\n%s", rec.Code, got, want)
	}
}

// What the mobile apps may send keeps being accepted, whatever the web routes need: the longest
// title and the largest page allowed when they shipped, and nulls for fields they leave alone. A
// tighter rule (in a request type or a service) breaks installs that send them. Replies stay JSON.
func TestMobileAcceptsWhatItDid(t *testing.T) {
	t.Parallel()
	router, pool := testAPI(t)
	seeded := seedTask(t, pool, "write docs", 0)

	for _, tt := range []struct{ method, target, body string }{
		{http.MethodPost, "/mobile/v1/tasks", fmt.Sprintf(`{"title":%q}`, strings.Repeat("é", 200))},
		{http.MethodGet, "/mobile/v1/tasks?limit=100", ""},
		{http.MethodPatch, "/mobile/v1/tasks/" + seeded.ID.String(), `{"title":null,"done":null}`},
	} {
		rec := send(t, router, tt.method, tt.target, tt.body)

		if rec.Code >= http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %s = %d %q %s, want success, application/json", tt.method, tt.target,
				rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
	}
}

// The tasks routes are behind the sign-in check (its cases are in auth_test.go).
func TestTasksNeedSignIn(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, _ := testAPI(t)

			for _, route := range []struct{ method, target string }{
				{http.MethodGet, tasks},
				{http.MethodPost, tasks},
				{http.MethodPost, tasks + "/" + uuid.NewString() + "/restore"},
			} {
				req := httptest.NewRequestWithContext(t.Context(), route.method, route.target, nil)
				req.Header.Set("Authorization", "Bearer not-a-token")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s %s without a valid token = %d, want 401", route.method, route.target, rec.Code)
				}
			}
		})
	}
}

// Another user's task is a 404, as if it didn't exist; an admin gets it, with its owner.
func TestTasksOfOthers(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			var bob uuid.UUID
			if err := pool.QueryRow(t.Context(), "INSERT INTO users (auth_subject) VALUES ('bob') RETURNING id").Scan(&bob); err != nil {
				t.Fatal(err)
			}
			var id uuid.UUID
			if err := pool.QueryRow(t.Context(), "INSERT INTO tasks (title, owner_id) VALUES ('bob''s', $1) RETURNING id", bob).Scan(&id); err != nil {
				t.Fatal(err)
			}

			testUser(t, pool, false)
			rec := send(t, router, http.MethodGet, tasks+"/"+id.String(), "")
			want := problemReply{Status: http.StatusNotFound, Detail: "task not found"}
			if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
				t.Errorf("GET /tasks/{bob's} as a user reply mismatch (-want +got):\n%s", diff)
			}

			testUser(t, pool, true)
			rec = send(t, router, http.MethodGet, tasks+"/"+id.String(), "")
			var got taskReply
			decodeReply(t, rec, &got)
			if rec.Code != http.StatusOK || got.ID != id || got.OwnerID == nil || *got.OwnerID != bob {
				t.Errorf("GET /tasks/{bob's} as an admin = %d %+v, want 200 with bob as owner", rec.Code, got)
			}
		})
	}
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
		// Read from the request and written in the reply through internal/json, whatever library it uses.
		{"non-ASCII title", "tulis dokumen ✍️ café", "tulis dokumen ✍️ café"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range taskClients {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					tasks := c.tasks
					router, pool := testAPI(t)

					rec := send(t, router, http.MethodPost, tasks, fmt.Sprintf(`{"title":%q}`, tt.title))

					if rec.Code != http.StatusCreated {
						t.Fatalf("POST %s status = %d, want 201; body %s", tasks, rec.Code, rec.Body)
					}
					var got taskReply
					decodeReply(t, rec, &got)
					// Under the client's own prefix.
					if loc := rec.Header().Get("Location"); loc != tasks+"/"+got.ID.String() {
						t.Errorf("Location = %q, want %s/%s", loc, tasks, got.ID)
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
		})
	}
}

func TestCreateTaskRejects(t *testing.T) {
	t.Parallel()
	requiredTitle := []handler.FieldProblem{{Field: "title", Rule: "required", Message: "is required"}}
	noNUL := []handler.FieldProblem{{Field: "title", Rule: "no_nul", Message: "must not contain the NUL character"}}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantDetail string
		wantErrors []handler.FieldProblem
	}{
		{"no title", `{}`, 422, "request body is invalid", requiredTitle},
		{"blank title", `{"title":"   "}`, 422, "the title is invalid", requiredTitle},
		{
			"title too long", fmt.Sprintf(`{"title":%q}`, strings.Repeat("é", task.MaxTitleLength+1)), 422, "the title is invalid",
			[]handler.FieldProblem{{Field: "title", Rule: "max", Message: "must be at most 200 characters long"}},
		},
		{"title with a NUL character", `{"title":"a\u0000b"}`, 422, "the title is invalid", noNUL},
		{"title taken", `{"title":"taken"}`, 409, "a task with this title already exists", nil},
		{"form body, not JSON", `title=x`, 400, "request body must be a JSON object", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range taskClients {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					tasks := c.tasks
					router, pool := testAPI(t)
					existing := seedTask(t, pool, "taken", 0)

					rec := send(t, router, http.MethodPost, tasks, tt.body)

					want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
					if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
						t.Errorf("POST %s reply mismatch (-want +got):\n%s", tasks, diff)
					}
					if diff := cmp.Diff([]db.Task{existing}, tasksInDB(t, pool)); diff != "" {
						t.Errorf("tasks in database after POST mismatch (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

// The unique constraint, not a check-then-insert, decides between requests racing for one title.
func TestCreateTaskSameTitleAtOnce(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)

			const requests = 10
			codes := make(chan int, requests)
			var wg sync.WaitGroup
			for range requests {
				wg.Go(func() {
					codes <- send(t, router, http.MethodPost, tasks, `{"title":"race"}`).Code
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
		})
	}
}

func TestGetTask(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			want := seedTask(t, pool, "write docs", 0)
			seedTask(t, pool, "another", time.Minute)

			rec := send(t, router, http.MethodGet, tasks+"/"+want.ID.String(), "")

			if rec.Code != http.StatusOK {
				t.Fatalf("GET /tasks/{id} status = %d, want 200; body %s", rec.Code, rec.Body)
			}
			var got taskReply
			decodeReply(t, rec, &got)
			if diff := cmp.Diff(asReply(want), got); diff != "" {
				t.Errorf("GET /tasks/{id} reply mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetTaskNotFound(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			seedTask(t, pool, "write docs", 0)

			for _, id := range []string{uuid.NewString(), "42"} {
				rec := send(t, router, http.MethodGet, tasks+"/"+id, "")
				want := problemReply{Status: http.StatusNotFound, Detail: "task not found"}
				if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
					t.Errorf("GET /tasks/%s reply mismatch (-want +got):\n%s", id, diff)
				}
			}
		})
	}
}

func TestListTasksIsEmptyWithoutTasks(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, _ := testAPI(t)

			rec := send(t, router, http.MethodGet, tasks, "")

			if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"tasks":[]}` {
				t.Errorf("GET %s = %d %s, want 200 {\"tasks\":[]}", tasks, rec.Code, rec.Body)
			}
		})
	}
}

func TestListTasksDefaultPageSize(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			for i := range handler.DefaultPageSize + 1 {
				seedTask(t, pool, fmt.Sprintf("task %02d", i), time.Duration(i)*time.Minute)
			}

			var got taskListReply
			decodeReply(t, send(t, router, http.MethodGet, tasks, ""), &got)

			if len(got.Tasks) != handler.DefaultPageSize || got.NextCursor == "" {
				t.Errorf("GET %s = %d tasks, next cursor %q; want %d and a cursor", tasks, len(got.Tasks), got.NextCursor, handler.DefaultPageSize)
			}
		})
	}
}

// Paging walks every task exactly once, newest first, even when tasks share a created_at.
func TestListTasksPagesThroughEveryTask(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			oldest := seedTask(t, pool, "oldest", 0)
			tiedA := seedTask(t, pool, "tied a", time.Minute)
			tiedB := seedTask(t, pool, "tied b", time.Minute)
			tiedC := seedTask(t, pool, "tied c", time.Minute)
			newest := seedTask(t, pool, "newest", 2*time.Minute)

			// Ties are ordered by id, highest first.
			tied := []db.Task{tiedA, tiedB, tiedC}
			slices.SortFunc(tied, func(a, b db.Task) int { return strings.Compare(b.ID.String(), a.ID.String()) })
			want := []taskReply{asReply(newest), asReply(tied[0]), asReply(tied[1]), asReply(tied[2]), asReply(oldest)}

			var got []taskReply
			var pageSizes []int
			target := tasks + "?limit=2"
			for range 10 { // more than enough pages; stops a loop that never ends
				rec := send(t, router, http.MethodGet, target, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s: status = %d; body %s", target, rec.Code, rec.Body)
				}
				var page taskListReply
				decodeReply(t, rec, &page)
				got = append(got, page.Tasks...)
				pageSizes = append(pageSizes, len(page.Tasks))
				if page.NextCursor == "" {
					break
				}
				target = tasks + "?limit=2&after=" + url.QueryEscape(page.NextCursor)
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("tasks across pages mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]int{2, 2, 1}, pageSizes); diff != "" {
				t.Errorf("page sizes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestListTasksRejects(t *testing.T) {
	t.Parallel()
	limitRange := []handler.FieldProblem{{Field: "limit", Rule: "range", Message: "must be from 1 to 100"}}
	notACursor := "after is not a cursor from a previous page"

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantDetail string
		wantErrors []handler.FieldProblem
	}{
		{"limit not a number", "limit=ten", 400, "limit must be a whole number from 1 to 100", nil},
		{"limit zero", "limit=0", 422, "page size is out of range", limitRange},
		{"cursor not base64", "after=%25%25", 400, notACursor, nil},
		{"cursor without a separator", "after=" + cursorOf("2026-01-01T00:00:00Z"), 400, notACursor, nil},
		{"cursor with a bad time", "after=" + cursorOf("yesterday|"+uuid.NewString()), 400, notACursor, nil},
		{"cursor with a bad id", "after=" + cursorOf("2026-01-01T00:00:00Z|42"), 400, notACursor, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range taskClients {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					tasks := c.tasks
					router, _ := testAPI(t)
					rec := send(t, router, http.MethodGet, tasks+"?"+tt.query, "")
					want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
					if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
						t.Errorf("GET %s?%s reply mismatch (-want +got):\n%s", tasks, tt.query, diff)
					}
				})
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
			for _, c := range taskClients {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					tasks := c.tasks
					router, pool := testAPI(t)
					before := seedTask(t, pool, "write docs", 0)
					other := seedTask(t, pool, "other", time.Minute)

					rec := send(t, router, http.MethodPatch, tasks+"/"+before.ID.String(), tt.body)

					if rec.Code != http.StatusOK {
						t.Fatalf("PATCH /tasks/{id} status = %d, want 200; body %s", rec.Code, rec.Body)
					}
					var got taskReply
					decodeReply(t, rec, &got)
					if got.Title != tt.wantTitle || got.Done != tt.wantDone || !got.CreatedAt.Equal(before.CreatedAt) {
						t.Errorf("reply = %+v, want title %q, done %v, created_at %s", got, tt.wantTitle, tt.wantDone, before.CreatedAt)
					}
					if unchanged := got.UpdatedAt.Equal(before.UpdatedAt); unchanged != tt.wantUnchanged {
						t.Errorf("updated_at = %s (was %s), want unchanged = %v", got.UpdatedAt, before.UpdatedAt, tt.wantUnchanged)
					}
					after := db.Task{
						ID: got.ID, Title: got.Title, Done: got.Done, CreatedAt: got.CreatedAt, UpdatedAt: got.UpdatedAt, OwnerID: got.OwnerID,
					}
					if diff := cmp.Diff(sortedByTitle(after, other), tasksInDB(t, pool)); diff != "" {
						t.Errorf("tasks in database after PATCH mismatch (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

func TestUpdateTaskRejects(t *testing.T) {
	t.Parallel()
	requiredTitle := []handler.FieldProblem{{Field: "title", Rule: "required", Message: "is required"}}

	tests := []struct {
		name       string
		id         string // "" means the seeded task
		body       string
		wantStatus int
		wantDetail string
		wantErrors []handler.FieldProblem
	}{
		{"unknown task", uuid.NewString(), `{"done":true}`, 404, "task not found", nil},
		{"unknown task, no fields", uuid.NewString(), `{}`, 404, "task not found", nil},
		{"id not a UUID", "42", `{"done":true}`, 404, "task not found", nil},
		{"blank title", "", `{"title":" "}`, 422, "the title is invalid", requiredTitle},
		{
			"title too long", "", fmt.Sprintf(`{"title":%q}`, strings.Repeat("a", task.MaxTitleLength+1)), 422,
			"the title is invalid",
			[]handler.FieldProblem{{Field: "title", Rule: "max", Message: "must be at most 200 characters long"}},
		},
		{
			"title with a NUL character", "", `{"title":"a\u0000b"}`, 422, "the title is invalid",
			[]handler.FieldProblem{{Field: "title", Rule: "no_nul", Message: "must not contain the NUL character"}},
		},
		{"title of another task", "", `{"title":"other"}`, 409, "a task with this title already exists", nil},
		{"unknown field", "", `{"colour":"red"}`, 400, `unknown field "colour"`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range taskClients {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					tasks := c.tasks
					router, pool := testAPI(t)
					seeded := seedTask(t, pool, "write docs", 0)
					other := seedTask(t, pool, "other", time.Minute)
					id := tt.id
					if id == "" {
						id = seeded.ID.String()
					}

					rec := send(t, router, http.MethodPatch, tasks+"/"+id, tt.body)

					want := problemReply{Status: tt.wantStatus, Detail: tt.wantDetail, Errors: tt.wantErrors}
					if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
						t.Errorf("PATCH /tasks/%s reply mismatch (-want +got):\n%s", id, diff)
					}
					if diff := cmp.Diff(sortedByTitle(seeded, other), tasksInDB(t, pool)); diff != "" {
						t.Errorf("tasks in database after PATCH mismatch (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

func TestDeleteTask(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			doomed := seedTask(t, pool, "write docs", 0)
			kept := seedTask(t, pool, "other", time.Minute)

			rec := send(t, router, http.MethodDelete, tasks+"/"+doomed.ID.String(), "")

			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Errorf("DELETE /tasks/{id} = %d %q, want 204 with no body", rec.Code, rec.Body)
			}
			// The row stays, marked deleted; the task is gone for the API.
			got := tasksInDB(t, pool)
			if len(got) != 2 || got[1].ID != doomed.ID || got[1].DeletedAt == nil {
				t.Fatalf("tasks in database after DELETE = %+v, want %q kept and marked deleted", got, doomed.Title)
			}
			doomed.DeletedAt = got[1].DeletedAt
			if diff := cmp.Diff(sortedByTitle(doomed, kept), got); diff != "" {
				t.Errorf("tasks in database after DELETE mismatch (-want +got):\n%s", diff)
			}
			rec = send(t, router, http.MethodGet, tasks+"/"+doomed.ID.String(), "")
			if want := (problemReply{Status: http.StatusNotFound, Detail: "task not found"}); !cmp.Equal(want, readProblem(t, rec)) {
				t.Errorf("GET /tasks/{id} after DELETE = %d %q, want 404", rec.Code, rec.Body)
			}
		})
	}
}

func TestRestoreTask(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			task := seedTask(t, pool, "write docs", 0)
			markDeleted(t, pool, task)

			rec := send(t, router, http.MethodPost, tasks+"/"+task.ID.String()+"/restore", "")

			if rec.Code != http.StatusOK {
				t.Fatalf("POST /tasks/{id}/restore status = %d, want 200; body %s", rec.Code, rec.Body)
			}
			var got taskReply
			decodeReply(t, rec, &got)
			// Restoring is a change: only updated_at moves.
			rows := tasksInDB(t, pool)
			if len(rows) != 1 || !rows[0].UpdatedAt.After(task.UpdatedAt) {
				t.Fatalf("tasks in database after restore = %+v, want the task with a later updated_at", rows)
			}
			task.UpdatedAt = rows[0].UpdatedAt
			if diff := cmp.Diff(asReply(task), got); diff != "" {
				t.Errorf("POST /tasks/{id}/restore reply mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]db.Task{task}, rows); diff != "" {
				t.Errorf("tasks in database after restore mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRestoreTaskRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		unknown bool // restore a task that doesn't exist
		want    problemReply
	}{
		{"unknown task", true, problemReply{Status: http.StatusNotFound, Detail: "task not found"}},
		{"title taken by a live task since", false, problemReply{Status: http.StatusConflict, Detail: "a task with this title already exists"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range taskClients {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					tasks := c.tasks
					router, pool := testAPI(t)
					deleted := markDeleted(t, pool, seedTask(t, pool, "write docs", 0))
					live := seedTask(t, pool, "write docs", time.Minute)
					id := deleted.ID
					if tt.unknown {
						id = uuid.New()
					}

					rec := send(t, router, http.MethodPost, tasks+"/"+id.String()+"/restore", "")

					if diff := cmp.Diff(tt.want, readProblem(t, rec)); diff != "" {
						t.Errorf("POST /tasks/%s/restore reply mismatch (-want +got):\n%s", id, diff)
					}
					if diff := cmp.Diff([]db.Task{deleted, live}, tasksInDB(t, pool)); diff != "" {
						t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

func TestDeleteTaskNotFound(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			kept := seedTask(t, pool, "write docs", 0)

			for _, id := range []string{uuid.NewString(), "42"} {
				rec := send(t, router, http.MethodDelete, tasks+"/"+id, "")
				want := problemReply{Status: http.StatusNotFound, Detail: "task not found"}
				if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
					t.Errorf("DELETE /tasks/%s reply mismatch (-want +got):\n%s", id, diff)
				}
			}
			if diff := cmp.Diff([]db.Task{kept}, tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database after DELETE mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A database failure is a 500 that tells the caller nothing about the database.
func TestTasksDatabaseDownIs500(t *testing.T) {
	t.Parallel()
	for _, c := range taskClients {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := c.tasks
			router, pool := testAPI(t)
			pool.Close()

			rec := send(t, router, http.MethodGet, tasks, "")

			want := problemReply{Status: http.StatusInternalServerError, Detail: handler.InternalErrorDetail}
			if diff := cmp.Diff(want, readProblem(t, rec)); diff != "" {
				t.Errorf("GET %s reply mismatch (-want +got):\n%s", tasks, diff)
			}
		})
	}
}

func sortedByTitle(a, b db.Task) []db.Task {
	if b.Title < a.Title {
		return []db.Task{b, a}
	}
	return []db.Task{a, b}
}
