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
	"github.com/mdayat/go-template/internal/testdb"
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

// seedTask inserts a task straight into the database, created and last changed at changed.
func seedTask(t *testing.T, pool *pgxpool.Pool, title string, done bool, changed time.Time) Task {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`INSERT INTO tasks (title, done, created_at, updated_at) VALUES ($1, $2, $3, $3)
		 RETURNING id, title, done, created_at, updated_at`, title, done, changed)
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
		"UPDATE tasks SET updated_at = $2 WHERE id = $1 RETURNING id, title, done, created_at, updated_at", task.ID, changed)
	if err != nil {
		t.Fatalf("touch task: %v", err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.Task])
	if err != nil {
		t.Fatalf("touch task: %v", err)
	}
	return fromRow(row)
}

// tasksInDB returns every task in the database, by title.
func tasksInDB(t *testing.T, pool *pgxpool.Pool) []Task {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT id, title, done, created_at, updated_at FROM tasks ORDER BY title")
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

			got, err := s.Create(t.Context(), tt.title)
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

			_, err := s.Create(t.Context(), tt.title)

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

	got, err := s.Get(t.Context(), want.ID)
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

	_, err := s.Get(t.Context(), uuid.New())

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
				page, err := s.List(t.Context(), limit, after)
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
	s, _ := testService(t)

	page, err := s.List(t.Context(), 10, nil)

	if err != nil || len(page.Tasks) != 0 || page.Tasks == nil || page.Next != nil {
		t.Errorf("List() = %+v, %v; want an empty, non-nil list, no cursor, no error", page, err)
	}
}

func TestListRejects(t *testing.T) {
	t.Parallel()
	s, _ := testService(t)
	want := &apperr.Error{Kind: apperr.KindInvalid, Detail: "page size is out of range", Fields: []apperr.FieldError{
		{Field: "limit", Rule: "range", Message: "must be from 1 to 100"},
	}}

	for _, limit := range []int{0, -1, MaxPageSize + 1} {
		_, err := s.List(t.Context(), limit, nil)
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

			got, err := s.Update(t.Context(), before.ID, tt.changes)
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

			_, err := s.Update(t.Context(), id, tt.changes)

			if diff := cmp.Diff(tt.want, appErrOf(err)); diff != "" {
				t.Errorf("Update() error = %v, mismatch (-want +got):\n%s", err, diff)
			}
			if diff := cmp.Diff(byTitle(seeded, other), tasksInDB(t, pool)); diff != "" {
				t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	doomed := seedTask(t, pool, "write docs", false, seedTime)
	kept := seedTask(t, pool, "other", false, seedTime)

	if err := s.Delete(t.Context(), doomed.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if diff := cmp.Diff([]Task{kept}, tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
	}

	err := s.Delete(t.Context(), doomed.ID)
	if diff := cmp.Diff(errNotFound, appErrOf(err)); diff != "" {
		t.Errorf("second Delete() error = %v, mismatch (-want +got):\n%s", err, diff)
	}
}

// Deletes only tasks that are done and last changed (not created) before the age, and a second run
// deletes nothing more.
func TestPurgeDone(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	now := time.Now()
	seedTask(t, pool, "done 31 days ago", true, now.Add(-31*day))
	keep := byTitle(
		seedTask(t, pool, "done 29 days ago", true, now.Add(-29*day)),
		seedTask(t, pool, "open 40 days", false, now.Add(-40*day)),
		touch(t, pool, seedTask(t, pool, "created 40 days ago, done 29 days ago", true, now.Add(-40*day)), now.Add(-29*day)),
	)

	for run, want := range []int64{1, 0} {
		deleted, err := s.PurgeDone(t.Context(), 30)
		if err != nil {
			t.Fatalf("run %d: PurgeDone(30) error = %v", run+1, err)
		}
		if deleted != want {
			t.Errorf("run %d: PurgeDone(30) = %d, want %d", run+1, deleted, want)
		}
		if diff := cmp.Diff(keep, tasksInDB(t, pool)); diff != "" {
			t.Errorf("run %d: tasks in database mismatch (-want +got):\n%s", run+1, diff)
		}
	}
}

func TestPurgeDoneChecksTheAge(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	seeded := seedTask(t, pool, "done long ago", true, seedTime)
	outOfRange := &apperr.Error{Kind: apperr.KindInvalid, Detail: "the age is out of range", Fields: []apperr.FieldError{
		{Field: "older_than_days", Rule: "range", Message: fmt.Sprintf("must be from 1 to %d", MaxPurgeAgeDays)},
	}}

	tests := []struct {
		days int
		want *apperr.Error
	}{
		{0, outOfRange},
		{-1, outOfRange},
		{MaxPurgeAgeDays + 1, outOfRange},
		{MaxPurgeAgeDays, nil},
		{1, nil},
	}
	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, appErrOf(CheckPurgeAge(tt.days))); diff != "" {
			t.Errorf("CheckPurgeAge(%d) mismatch (-want +got):\n%s", tt.days, diff)
		}
		if tt.want == nil {
			continue
		}
		_, err := s.PurgeDone(t.Context(), tt.days)
		if diff := cmp.Diff(tt.want, appErrOf(err)); diff != "" {
			t.Errorf("PurgeDone(%d) error = %v, mismatch (-want +got):\n%s", tt.days, err, diff)
		}
	}
	if diff := cmp.Diff([]Task{seeded}, tasksInDB(t, pool)); diff != "" {
		t.Errorf("tasks in database mismatch (-want +got):\n%s", diff)
	}
}
