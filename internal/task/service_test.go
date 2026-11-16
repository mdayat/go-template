package task

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/softdelete"
	"github.com/mdayat/go-template/internal/testdb"
	"github.com/mdayat/go-template/internal/user"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// seedTime is when seeded tasks were made: in the past, so a change made by a test moves UpdatedAt.
var seedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func testService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	return NewService(db.New(pool)), pool
}

// seedUser inserts a user straight into the database, or returns the one with subject.
func seedUser(t *testing.T, pool *pgxpool.Pool, subject string, admin bool) user.User {
	t.Helper()
	var u user.User
	err := pool.QueryRow(t.Context(),
		`INSERT INTO users (auth_subject, is_admin) VALUES ($1, $2)
		 ON CONFLICT (auth_subject) DO UPDATE SET is_admin = EXCLUDED.is_admin
		 RETURNING id, is_admin`, subject, admin).Scan(&u.ID, &u.IsAdmin)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}

// me is the signed-in user most tests act as, and who owns what seedTask inserts.
func me(t *testing.T, pool *pgxpool.Pool) user.User {
	t.Helper()
	return seedUser(t, pool, "me", false)
}

// seedTask inserts a task owned by me straight into the database, created and last changed at
// changed.
func seedTask(t *testing.T, pool *pgxpool.Pool, title string, done bool, changed time.Time) Task {
	t.Helper()
	return seedTaskOf(t, pool, me(t, pool).ID, title, done, changed)
}

// seedTaskOf is seedTask for another owner (nil for a task from before tasks had owners).
func seedTaskOf(t *testing.T, pool *pgxpool.Pool, owner any, title string, done bool, changed time.Time) Task {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`INSERT INTO tasks (title, done, created_at, updated_at, owner_id) VALUES ($1, $2, $3, $3, $4)
		 RETURNING id, title, done, created_at, updated_at, deleted_at, owner_id`, title, done, changed, owner)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return fromRow(row)
}

// touch sets task's UpdatedAt to changed and returns the task as stored.
func touch(t *testing.T, pool *pgxpool.Pool, task Task, changed time.Time) Task {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		"UPDATE tasks SET updated_at = $2 WHERE id = $1 RETURNING id, title, done, created_at, updated_at, deleted_at, owner_id",
		task.ID, changed)
	if err != nil {
		t.Fatalf("touch task: %v", err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("touch task: %v", err)
	}
	return fromRow(row)
}

// markDeleted deletes task straight in the database, as Delete does, at deletedAt.
func markDeleted(t *testing.T, pool *pgxpool.Pool, task Task, deletedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), "UPDATE tasks SET deleted_at = $2 WHERE id = $1", task.ID, deletedAt); err != nil {
		t.Fatalf("mark task deleted: %v", err)
	}
}

// deletedInDB returns the titles of the deleted tasks in the database, sorted.
func deletedInDB(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT title FROM tasks WHERE deleted_at IS NOT NULL ORDER BY title")
	if err != nil {
		t.Fatalf("read deleted tasks: %v", err)
	}
	titles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read deleted tasks: %v", err)
	}
	return titles
}

// tasksInDB returns every task in the database, deleted ones too, by title.
func tasksInDB(t *testing.T, pool *pgxpool.Pool) []Task {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT id, title, done, created_at, updated_at, deleted_at, owner_id FROM tasks ORDER BY title, created_at")
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	result := make([]Task, 0, len(tasks))
	for _, row := range tasks {
		result = append(result, fromRow(row))
	}
	return result
}

// appErrOf returns the apperr.Error inside err, or nil if there is none.
func appErrOf(err error) *apperr.Error {
	if e, ok := errors.AsType[*apperr.Error](err); ok {
		return e
	}
	return nil
}

func byTitle(tasks ...Task) []Task {
	slices.SortFunc(tasks, func(a, b Task) int { return strings.Compare(a.Title, b.Title) })
	return tasks
}

var (
	errTitleRequired = &apperr.Error{Kind: apperr.KindInvalid, Detail: "the title is invalid", Fields: []apperr.FieldError{
		{Field: "title", Rule: "required", Message: "is required"},
	}}
	errTitleTooLong = &apperr.Error{Kind: apperr.KindInvalid, Detail: "the title is invalid", Fields: []apperr.FieldError{
		{Field: "title", Rule: "max", Message: "must be at most 200 characters long"},
	}}
	errTitleNUL = &apperr.Error{Kind: apperr.KindInvalid, Detail: "the title is invalid", Fields: []apperr.FieldError{
		{Field: "title", Rule: "no_nul", Message: "must not contain the NUL character"},
	}}
	errTitleTaken = &apperr.Error{Kind: apperr.KindConflict, Detail: "a task with this title already exists"}
	errNotFound   = &apperr.Error{Kind: apperr.KindNotFound, Detail: "task not found"}
)

func TestCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		title     string
		wantTitle string
	}{
		{"plain title", "write docs", "write docs"},
		{"spaces around are trimmed", "  write docs \n", "write docs"},
		{"longest title counts characters, not bytes", strings.Repeat("é", MaxTitleLength), strings.Repeat("é", MaxTitleLength)},
		{"longest title is measured after trimming", " " + strings.Repeat("a", MaxTitleLength) + " ", strings.Repeat("a", MaxTitleLength)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, pool := testService(t)

			got, err := s.Create(t.Context(), me(t, pool), tt.title)
			if err != nil {
				t.Fatalf("Create(%q) error = %v", tt.title, err)
			}

			if got.Title != tt.wantTitle || got.Done || !got.UpdatedAt.Equal(got.CreatedAt) {
				t.Errorf("Create(%q) = %+v, want title %q, not done, UpdatedAt = CreatedAt", tt.title, got, tt.wantTitle)
			}
			if diff := cmp.Diff([]Task{got}, tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		title string
		want  *apperr.Error
	}{
		{"blank title", "  \t", errTitleRequired},
		{"title too long", strings.Repeat("é", MaxTitleLength+1), errTitleTooLong},
		{"title with a NUL character", "a\x00b", errTitleNUL},
		{"title taken", "taken", errTitleTaken},
		{"title taken once trimmed", " taken ", errTitleTaken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, pool := testService(t)
			existing := seedTask(t, pool, "taken", false, seedTime)

			_, err := s.Create(t.Context(), me(t, pool), tt.title)

			if diff := cmp.Diff(tt.want, appErrOf(err)); diff != "" {
				t.Errorf("Create(%q) error = %v, mismatch (-want +got):\n%s", tt.title, err, diff)
			}
			if diff := cmp.Diff([]Task{existing}, tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGet(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	want := seedTask(t, pool, "write docs", false, seedTime)
	seedTask(t, pool, "another", false, seedTime)

	got, err := s.Get(t.Context(), me(t, pool), want.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get() mismatch (-want +got):\n%s", diff)
	}
}

func TestGetNotFound(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	seedTask(t, pool, "write docs", false, seedTime)

	_, err := s.Get(t.Context(), me(t, pool), uuid.New())

	if diff := cmp.Diff(errNotFound, appErrOf(err)); diff != "" {
		t.Errorf("Get() error = %v, mismatch (-want +got):\n%s", err, diff)
	}
}

// Paging walks every task exactly once, newest created first (not newest changed), even when tasks
// share a CreatedAt, and the last page has no cursor even when it's full.
func TestListPagesThroughEveryTask(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	// Changed last, so ordering or paging by UpdatedAt would put it first.
	oldest := touch(t, pool, seedTask(t, pool, "oldest", false, seedTime), seedTime.Add(time.Hour))
	tied := []Task{
		seedTask(t, pool, "tied a", false, seedTime.Add(time.Minute)),
		seedTask(t, pool, "tied b", false, seedTime.Add(time.Minute)),
		seedTask(t, pool, "tied c", false, seedTime.Add(time.Minute)),
	}
	newest := seedTask(t, pool, "newest", false, seedTime.Add(2*time.Minute))
	// Ties are ordered by id, highest first.
	slices.SortFunc(tied, func(a, b Task) int { return strings.Compare(b.ID.String(), a.ID.String()) })
	want := []Task{newest, tied[0], tied[1], tied[2], oldest}

	for _, limit := range []int{1, 2, 5, MaxPageSize} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			t.Parallel()
			var got []Task
			var pageSizes []int
			var after *Cursor
			for range 10 { // more than enough pages; stops a loop that never ends
				page, err := s.List(t.Context(), me(t, pool), limit, after)
				if err != nil {
					t.Fatalf("List(%d) error = %v", limit, err)
				}
				got = append(got, page.Tasks...)
				pageSizes = append(pageSizes, len(page.Tasks))
				if page.Next == nil {
					break
				}
				after = page.Next
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("List(%d): tasks across pages mismatch (-want +got):\n%s", limit, diff)
			}
			var wantSizes []int
			for left := len(want); left > 0; left -= limit {
				wantSizes = append(wantSizes, min(left, limit))
			}
			if diff := cmp.Diff(wantSizes, pageSizes); diff != "" {
				t.Errorf("List(%d): page sizes mismatch (-want +got):\n%s", limit, diff)
			}
		})
	}
}

func TestListIsEmptyWithoutTasks(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)

	page, err := s.List(t.Context(), me(t, pool), 10, nil)

	if err != nil || len(page.Tasks) != 0 || page.Tasks == nil || page.Next != nil {
		t.Errorf("List() = %+v, %v; want an empty, non-nil list, no cursor, no error", page, err)
	}
}

func TestListRejects(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	want := &apperr.Error{Kind: apperr.KindInvalid, Detail: "page size is out of range", Fields: []apperr.FieldError{
		{Field: "limit", Rule: "range", Message: "must be from 1 to 100"},
	}}

	for _, limit := range []int{0, -1, MaxPageSize + 1} {
		_, err := s.List(t.Context(), me(t, pool), limit, nil)
		if diff := cmp.Diff(want, appErrOf(err)); diff != "" {
			t.Errorf("List(%d) error = %v, mismatch (-want +got):\n%s", limit, err, diff)
		}
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	renamed, trimmed, same, done := "renamed", " renamed ", "write docs", true

	tests := []struct {
		name          string
		changes       Changes
		wantTitle     string
		wantDone      bool
		wantUnchanged bool // UpdatedAt stays as it was
	}{
		{"new title, trimmed", Changes{Title: &trimmed}, "renamed", false, false},
		{"mark done", Changes{Done: &done}, "write docs", true, false},
		{"both", Changes{Title: &renamed, Done: &done}, "renamed", true, false},
		{"same title as before", Changes{Title: &same}, "write docs", false, false},
		{"no changes leave UpdatedAt alone", Changes{}, "write docs", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, pool := testService(t)
			before := seedTask(t, pool, "write docs", false, seedTime)
			other := seedTask(t, pool, "other", false, seedTime)

			got, err := s.Update(t.Context(), me(t, pool), before.ID, tt.changes)
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}

			if got.Title != tt.wantTitle || got.Done != tt.wantDone || !got.CreatedAt.Equal(before.CreatedAt) {
				t.Errorf("Update() = %+v, want title %q, done %v, CreatedAt %s", got, tt.wantTitle, tt.wantDone, before.CreatedAt)
			}
			if unchanged := got.UpdatedAt.Equal(before.UpdatedAt); unchanged != tt.wantUnchanged {
				t.Errorf("UpdatedAt = %s (was %s), want unchanged = %v", got.UpdatedAt, before.UpdatedAt, tt.wantUnchanged)
			}
			if diff := cmp.Diff(byTitle(got, other), tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateRejects(t *testing.T) {
	t.Parallel()
	blank, tooLong, nul, taken, done := " ", strings.Repeat("a", MaxTitleLength+1), "a\x00b", "other", true

	tests := []struct {
		name    string
		unknown bool // update a task that doesn't exist
		changes Changes
		want    *apperr.Error
	}{
		{"unknown task", true, Changes{Done: &done}, errNotFound},
		{"unknown task, no changes", true, Changes{}, errNotFound},
		{"blank title", false, Changes{Title: &blank}, errTitleRequired},
		{"title too long", false, Changes{Title: &tooLong}, errTitleTooLong},
		{"title with a NUL character", false, Changes{Title: &nul}, errTitleNUL},
		{"title of another task", false, Changes{Title: &taken}, errTitleTaken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, pool := testService(t)
			seeded := seedTask(t, pool, "write docs", false, seedTime)
			other := seedTask(t, pool, "other", false, seedTime)
			id := seeded.ID
			if tt.unknown {
				id = uuid.New()
			}

			_, err := s.Update(t.Context(), me(t, pool), id, tt.changes)

			if diff := cmp.Diff(tt.want, appErrOf(err)); diff != "" {
				t.Errorf("Update() error = %v, mismatch (-want +got):\n%s", err, diff)
			}
			if diff := cmp.Diff(byTitle(seeded, other), tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A deleted task stays in the database, marked deleted, and every other method treats it as not
// found.
func TestDelete(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	doomed := seedTask(t, pool, "write docs", false, seedTime)
	kept := seedTask(t, pool, "other", false, seedTime)

	if err := s.Delete(t.Context(), me(t, pool), doomed.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if diff := cmp.Diff(byTitle(doomed, kept), tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"write docs"}, deletedInDB(t, pool)); diff != "" {
		t.Errorf("deleted tasks mismatch (-want +got):\n%s", diff)
	}

	err := s.Delete(t.Context(), me(t, pool), doomed.ID)
	if diff := cmp.Diff(errNotFound, appErrOf(err)); diff != "" {
		t.Errorf("second Delete() error = %v, mismatch (-want +got):\n%s", err, diff)
	}
	err = s.Delete(t.Context(), me(t, pool), uuid.New())
	if diff := cmp.Diff(errNotFound, appErrOf(err)); diff != "" {
		t.Errorf("Delete(unknown) error = %v, mismatch (-want +got):\n%s", err, diff)
	}
}

func TestDeletedTaskIsHidden(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	newest := seedTask(t, pool, "newest", false, seedTime.Add(2*time.Minute))
	deleted := seedTask(t, pool, "deleted", false, seedTime.Add(time.Minute))
	oldest := seedTask(t, pool, "oldest", false, seedTime)
	markDeleted(t, pool, deleted, time.Now())
	done := true

	if _, err := s.Get(t.Context(), me(t, pool), deleted.ID); appErrOf(err) == nil || appErrOf(err).Kind != apperr.KindNotFound {
		t.Errorf("Get(deleted) error = %v, want not found", err)
	}
	if _, err := s.Update(t.Context(), me(t, pool), deleted.ID, Changes{Done: &done}); appErrOf(err) == nil || appErrOf(err).Kind != apperr.KindNotFound {
		t.Errorf("Update(deleted) error = %v, want not found", err)
	}
	// One page, then one task per page: both list queries skip it.
	first, err := s.List(t.Context(), me(t, pool), 10, nil)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]Task{newest, oldest}, first.Tasks); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
	second, err := s.List(t.Context(), me(t, pool), 1, &Cursor{CreatedAt: newest.CreatedAt, ID: newest.ID})
	if err != nil {
		t.Fatalf("List(after newest) error = %v", err)
	}
	if diff := cmp.Diff([]Task{oldest}, second.Tasks); diff != "" {
		t.Errorf("List(after newest) mismatch (-want +got):\n%s", diff)
	}
	if second.Next != nil {
		t.Errorf("List(after newest).Next = %+v, want nil: only deleted tasks follow", second.Next)
	}
	if diff := cmp.Diff(byTitle(newest, deleted, oldest), tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
	}
}

// Titles are unique among live tasks only.
func TestDeletedTaskTitleCanBeReused(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	gone := seedTask(t, pool, "write docs", false, seedTime)
	markDeleted(t, pool, gone, time.Now())
	other := seedTask(t, pool, "other", false, seedTime)
	title := "write docs"

	if _, err := s.Create(t.Context(), me(t, pool), title); err != nil {
		t.Errorf("Create(deleted task's title) error = %v", err)
	}
	if err := s.Delete(t.Context(), me(t, pool), other.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	// Two deleted tasks may share a title too.
	if _, err := s.Create(t.Context(), me(t, pool), "other"); err != nil {
		t.Errorf("Create(\"other\") error = %v", err)
	}
	if diff := cmp.Diff([]string{"other", "write docs"}, deletedInDB(t, pool)); diff != "" {
		t.Errorf("deleted tasks mismatch (-want +got):\n%s", diff)
	}
}

func TestRestore(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	task := seedTask(t, pool, "write docs", true, seedTime)
	markDeleted(t, pool, task, time.Now())

	got, err := s.Restore(t.Context(), me(t, pool), task.ID)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	// Restoring is a change, so the purge counts the task's age from now.
	if !got.UpdatedAt.After(task.UpdatedAt) {
		t.Errorf("Restore().UpdatedAt = %s, want later than %s", got.UpdatedAt, task.UpdatedAt)
	}
	want := task
	want.UpdatedAt = got.UpdatedAt
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Restore() mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]Task{want}, tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
	}
	if got := deletedInDB(t, pool); len(got) != 0 {
		t.Errorf("deleted tasks = %q, want none", got)
	}

	// A second restore finds it live and returns it unchanged.
	again, err := s.Restore(t.Context(), me(t, pool), task.ID)
	if err != nil || !cmp.Equal(want, again) {
		t.Errorf("second Restore() = %+v, %v; want %+v unchanged", again, err, want)
	}
}

func TestRestoreRejects(t *testing.T) {
	t.Parallel()

	t.Run("unknown task", func(t *testing.T) {
		t.Parallel()
		s, pool := testService(t)
		_, err := s.Restore(t.Context(), me(t, pool), uuid.New())
		if diff := cmp.Diff(errNotFound, appErrOf(err)); diff != "" {
			t.Errorf("Restore() error = %v, mismatch (-want +got):\n%s", err, diff)
		}
	})
	t.Run("title taken by a live task since", func(t *testing.T) {
		t.Parallel()
		s, pool := testService(t)
		deleted := seedTask(t, pool, "write docs", false, seedTime)
		markDeleted(t, pool, deleted, time.Now())
		live := seedTask(t, pool, "write docs", false, seedTime)

		_, err := s.Restore(t.Context(), me(t, pool), deleted.ID)

		if diff := cmp.Diff(errTitleTaken, appErrOf(err)); diff != "" {
			t.Errorf("Restore() error = %v, mismatch (-want +got):\n%s", err, diff)
		}
		if diff := cmp.Diff([]string{"write docs"}, deletedInDB(t, pool)); diff != "" {
			t.Errorf("deleted tasks mismatch (-want +got):\n%s", diff)
		}
		if got, err := s.Get(t.Context(), me(t, pool), live.ID); err != nil || !cmp.Equal(live, got) {
			t.Errorf("Get(live) = %+v, %v; want the live task unchanged", got, err)
		}
	})
}

// Removes only the tasks deleted before the age, whatever their other dates; live tasks stay
// however old. A second run removes nothing more.
func TestPurgeDeleted(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	now := time.Now()
	markDeleted(t, pool, seedTask(t, pool, "deleted 31 days ago", false, now.Add(-40*day)), now.Add(-31*day))
	deletedLately := seedTask(t, pool, "done 40 days ago, deleted 29 days ago", true, now.Add(-40*day))
	markDeleted(t, pool, deletedLately, now.Add(-29*day))
	restored := seedTask(t, pool, "deleted 31 days ago, then restored", false, now.Add(-40*day))
	markDeleted(t, pool, restored, now.Add(-31*day))
	restored, err := s.Restore(t.Context(), me(t, pool), restored.ID)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	keep := byTitle(
		seedTask(t, pool, "done 40 days ago", true, now.Add(-40*day)),
		seedTask(t, pool, "open 40 days", false, now.Add(-40*day)),
		deletedLately,
		restored,
	)

	for run, want := range []int64{1, 0} {
		removed, err := s.PurgeDeleted(t.Context(), 30)
		if err != nil {
			t.Fatalf("run %d: PurgeDeleted(30) error = %v", run+1, err)
		}
		if removed != want {
			t.Errorf("run %d: PurgeDeleted(30) = %d, want %d", run+1, removed, want)
		}
		if diff := cmp.Diff(keep, tasksInDB(t, pool)); diff != "" {
			t.Errorf("run %d: tasks in database mismatch (-want +got):\n%s", run+1, diff)
		}
	}
}

// An age softdelete.CheckPurgeAge refuses removes nothing: 0 would remove a task deleted a moment
// ago.
func TestPurgeDeletedChecksTheAge(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	seeded := seedTask(t, pool, "deleted long ago", false, seedTime)
	markDeleted(t, pool, seeded, seedTime)

	for _, days := range []int{0, -1, softdelete.MaxPurgeAgeDays + 1} {
		_, err := s.PurgeDeleted(t.Context(), days)
		if e := appErrOf(err); e == nil || e.Kind != apperr.KindInvalid {
			t.Errorf("PurgeDeleted(%d) error = %v, want invalid", days, err)
		}
	}
	if diff := cmp.Diff([]string{"deleted long ago"}, deletedInDB(t, pool)); diff != "" {
		t.Errorf("deleted tasks mismatch (-want +got):\n%s", diff)
	}
}

// Someone else's task is not found for every action, the same as one that doesn't exist (a 403
// would confirm it does), and nothing about it changes.
func TestOthersTasksAreNotFound(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	bob := seedUser(t, pool, "bob", false)
	bobs := seedTaskOf(t, pool, bob.ID, "bob's task", false, seedTime)
	ownerless := seedTaskOf(t, pool, nil, "from before owners", false, seedTime)
	done := true

	for _, task := range []Task{bobs, ownerless} {
		calls := map[string]error{}
		_, calls["Get"] = s.Get(t.Context(), me(t, pool), task.ID)
		_, calls["Update"] = s.Update(t.Context(), me(t, pool), task.ID, Changes{Done: &done})
		_, calls["Update with no changes"] = s.Update(t.Context(), me(t, pool), task.ID, Changes{})
		calls["Delete"] = s.Delete(t.Context(), me(t, pool), task.ID)
		_, calls["Restore"] = s.Restore(t.Context(), me(t, pool), task.ID)
		for call, err := range calls {
			if diff := cmp.Diff(errNotFound, appErrOf(err)); diff != "" {
				t.Errorf("%s(%q) error = %v, mismatch (-want +got):\n%s", call, task.Title, err, diff)
			}
		}
	}
	// Deleted, it's still not mine to restore.
	markDeleted(t, pool, bobs, time.Now())
	if _, err := s.Restore(t.Context(), me(t, pool), bobs.ID); appErrOf(err) == nil || appErrOf(err).Kind != apperr.KindNotFound {
		t.Errorf("Restore(bob's deleted task) error = %v, want not found", err)
	}

	page, err := s.List(t.Context(), me(t, pool), MaxPageSize, nil)
	if err != nil || len(page.Tasks) != 0 {
		t.Errorf("List() = %+v, %v; want none of the others' tasks", page.Tasks, err)
	}
	if diff := cmp.Diff(byTitle(bobs, ownerless), tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"bob's task"}, deletedInDB(t, pool)); diff != "" {
		t.Errorf("deleted tasks mismatch (-want +got):\n%s", diff)
	}
}

// A new task is its creator's; each user's list pages through only their own.
func TestTasksBelongToTheirCreator(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	bob := seedUser(t, pool, "bob", false)
	seedTaskOf(t, pool, bob.ID, "bob's", false, seedTime.Add(time.Minute))
	mine := []Task{
		seedTask(t, pool, "mine 2", false, seedTime.Add(2*time.Minute)),
		seedTask(t, pool, "mine 1", false, seedTime),
	}

	created, err := s.Create(t.Context(), me(t, pool), "mine 3")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.OwnerID == nil || *created.OwnerID != me(t, pool).ID {
		t.Errorf("Create().OwnerID = %v, want me", created.OwnerID)
	}

	var got []Task
	var after *Cursor
	for range 5 {
		page, err := s.List(t.Context(), me(t, pool), 1, after)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		got = append(got, page.Tasks...)
		if after = page.Next; after == nil {
			break
		}
	}
	if diff := cmp.Diff(append([]Task{created}, mine...), got); diff != "" {
		t.Errorf("my tasks across pages mismatch (-want +got):\n%s", diff)
	}
}

// Titles are unique among one owner's tasks: another user's titles neither block nor show.
func TestTitlesAreUniquePerOwner(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	bob := seedUser(t, pool, "bob", false)
	seedTaskOf(t, pool, bob.ID, "write docs", false, seedTime)
	seedTaskOf(t, pool, nil, "write docs", false, seedTime)

	if _, err := s.Create(t.Context(), me(t, pool), "write docs"); err != nil {
		t.Fatalf("Create(a title bob has) error = %v", err)
	}
	_, err := s.Create(t.Context(), me(t, pool), "write docs")
	if diff := cmp.Diff(errTitleTaken, appErrOf(err)); diff != "" {
		t.Errorf("Create(my own title again) error = %v, mismatch (-want +got):\n%s", err, diff)
	}
}

// An admin sees and changes everyone's tasks, ownerless ones too, and lists them all.
func TestAdminsReachEveryonesTasks(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	admin := seedUser(t, pool, "root", true)
	bob := seedUser(t, pool, "bob", false)
	bobs := seedTaskOf(t, pool, bob.ID, "bob's", false, seedTime.Add(time.Minute))
	ownerless := seedTaskOf(t, pool, nil, "from before owners", false, seedTime)
	done := true

	page, err := s.List(t.Context(), admin, MaxPageSize, nil)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]Task{bobs, ownerless}, page.Tasks); diff != "" {
		t.Errorf("admin's List() mismatch (-want +got):\n%s", diff)
	}
	second, err := s.List(t.Context(), admin, 1, &Cursor{CreatedAt: bobs.CreatedAt, ID: bobs.ID})
	if err != nil || len(second.Tasks) != 1 || second.Tasks[0].ID != ownerless.ID {
		t.Errorf("admin's List(after bob's) = %+v, %v; want the ownerless task", second.Tasks, err)
	}
	if got, err := s.Get(t.Context(), admin, ownerless.ID); err != nil || got.ID != ownerless.ID {
		t.Errorf("admin's Get(ownerless) = %+v, %v; want it", got, err)
	}
	updated, err := s.Update(t.Context(), admin, bobs.ID, Changes{Done: &done})
	if err != nil || !updated.Done || updated.OwnerID == nil || *updated.OwnerID != bob.ID {
		t.Errorf("admin's Update(bob's) = %+v, %v; want it done and still bob's", updated, err)
	}
	if err := s.Delete(t.Context(), admin, bobs.ID); err != nil {
		t.Errorf("admin's Delete(bob's) error = %v", err)
	}
	if _, err := s.Restore(t.Context(), admin, bobs.ID); err != nil {
		t.Errorf("admin's Restore(bob's) error = %v", err)
	}
	if got := deletedInDB(t, pool); len(got) != 0 {
		t.Errorf("deleted tasks = %q, want none", got)
	}
}
