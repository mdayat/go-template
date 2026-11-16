// Package task holds the business rules for tasks. It reads and writes them through the generated
// queries in internal/db and reports expected failures as apperr kinds, so any entry point (HTTP
// API, worker, command) gets the same rules and the same errors.
package task

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/postgres"
	"github.com/mdayat/go-template/internal/softdelete"
)

const (
	// MaxTitleLength matches the tasks_title_length check in db/schema.sql.
	MaxTitleLength = 200
	// MaxPageSize is the most tasks one List call returns.
	MaxPageSize = 100

	// The unique index behind a duplicate title, from db/schema.sql. Postgres reports a unique
	// index's name as the violated constraint's.
	titleUniqueConstraint = "tasks_title_key"
)

// Task is a to-do item.
type Task struct {
	ID        uuid.UUID
	Title     string
	Done      bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Cursor marks the last task of a page; the next page starts after it.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Page is one page of tasks, newest first. Next is nil on the last page.
type Page struct {
	Tasks []Task
	Next  *Cursor
}

// Changes are the fields to update; a nil field is left as it is.
type Changes struct {
	Title *string
	Done  *bool
}

// Service creates, reads, updates, deletes and restores tasks.
type Service struct {
	queries *db.Queries
}

// NewService returns a Service that runs its queries through queries.
func NewService(queries *db.Queries) *Service {
	return &Service{queries: queries}
}

// Create adds a task. The title is trimmed and must be 1 to MaxTitleLength characters and unique.
func (s *Service) Create(ctx context.Context, title string) (Task, error) {
	title, err := cleanTitle(title)
	if err != nil {
		return Task{}, err
	}
	row, err := s.queries.CreateTask(ctx, title)
	if err != nil {
		return Task{}, fmt.Errorf("create task: %w", conflictOr(err))
	}
	return fromRow(row), nil
}

// Get returns the task with id.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Task, error) {
	row, err := s.queries.GetTask(ctx, id)
	if err != nil {
		return Task{}, fmt.Errorf("get task %s: %w", id, notFoundOr(err))
	}
	return fromRow(row), nil
}

// List returns up to limit tasks (1 to MaxPageSize), newest first, starting after the cursor, or
// from the newest when after is nil.
func (s *Service) List(ctx context.Context, limit int, after *Cursor) (Page, error) {
	if limit < 1 || limit > MaxPageSize {
		return Page{}, apperr.Invalid("page size is out of range", apperr.FieldError{
			Field: "limit", Rule: "range", Message: fmt.Sprintf("must be from 1 to %d", MaxPageSize),
		})
	}

	// One extra row says whether another page follows.
	fetch := int32(limit) + 1
	var (
		rows []db.Task
		err  error
	)
	if after == nil {
		rows, err = s.queries.ListTasks(ctx, fetch)
	} else {
		rows, err = s.queries.ListTasksAfter(ctx, db.ListTasksAfterParams{
			CreatedAt: after.CreatedAt, ID: after.ID, RowLimit: fetch,
		})
	}
	if err != nil {
		return Page{}, fmt.Errorf("list tasks: %w", err)
	}

	page := Page{Tasks: make([]Task, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		page.Tasks = append(page.Tasks, fromRow(row))
	}
	if len(rows) > limit {
		last := page.Tasks[limit-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

// Update applies the changes to the task with id. A new title follows the same rules as in Create.
func (s *Service) Update(ctx context.Context, id uuid.UUID, changes Changes) (Task, error) {
	if changes.Title == nil && changes.Done == nil {
		// Nothing to change: don't touch updated_at.
		return s.Get(ctx, id)
	}
	if changes.Title != nil {
		title, err := cleanTitle(*changes.Title)
		if err != nil {
			return Task{}, err
		}
		changes.Title = &title
	}

	row, err := s.queries.UpdateTask(ctx, db.UpdateTaskParams{ID: id, Title: changes.Title, Done: changes.Done})
	if err != nil {
		return Task{}, fmt.Errorf("update task %s: %w", id, conflictOr(notFoundOr(err)))
	}
	return fromRow(row), nil
}

// Delete hides the task with id: every other method then treats it as not found, and Restore
// brings it back. Deleting it again is NotFound.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	deleted, err := s.queries.DeleteTask(ctx, id)
	if err != nil {
		return fmt.Errorf("delete task %s: %w", id, err)
	}
	if deleted == 0 {
		return fmt.Errorf("delete task %s: %w", id, apperr.NotFound("task not found"))
	}
	return nil
}

// Restore brings back the deleted task with id, as changed now. A task that isn't deleted is
// returned unchanged, so a repeated restore is safe. It's a Conflict when a live task has taken the
// title since.
func (s *Service) Restore(ctx context.Context, id uuid.UUID) (Task, error) {
	row, err := s.queries.RestoreTask(ctx, id)
	if postgres.IsNotFound(err) {
		// Not deleted: live (an earlier restore), or no such task.
		return s.Get(ctx, id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("restore task %s: %w", id, conflictOr(err))
	}
	return fromRow(row), nil
}

// PurgeDeleted removes for good the tasks deleted more than olderThanDays days ago (an age
// softdelete.CheckPurgeAge accepts), and returns how many it removed. Live tasks are never removed.
// Running it again removes nothing more, so a repeated run is safe.
func (s *Service) PurgeDeleted(ctx context.Context, olderThanDays int) (int64, error) {
	if err := softdelete.CheckPurgeAge(olderThanDays); err != nil {
		return 0, err
	}
	removed, err := s.queries.PurgeDeletedTasks(ctx, int64(olderThanDays))
	if err != nil {
		return 0, fmt.Errorf("purge tasks deleted over %d days ago: %w", olderThanDays, err)
	}
	return removed, nil
}

func cleanTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	switch {
	case title == "":
		return "", apperr.Invalid("the title is invalid",
			apperr.FieldError{Field: "title", Rule: "required", Message: "is required"})
	case utf8.RuneCountInString(title) > MaxTitleLength:
		return "", apperr.Invalid("the title is invalid", apperr.FieldError{
			Field: "title", Rule: "max", Message: fmt.Sprintf("must be at most %d characters long", MaxTitleLength),
		})
	case strings.ContainsRune(title, 0):
		// Postgres text can't hold NUL (JSON "\u0000"): the insert would fail as a 500.
		return "", apperr.Invalid("the title is invalid",
			apperr.FieldError{Field: "title", Rule: "no_nul", Message: "must not contain the NUL character"})
	}
	return title, nil
}

func notFoundOr(err error) error {
	if postgres.IsNotFound(err) {
		return apperr.NotFound("task not found")
	}
	return err
}

func conflictOr(err error) error {
	if postgres.IsUniqueViolation(err, titleUniqueConstraint) {
		return apperr.Conflict("a task with this title already exists")
	}
	return err
}

func fromRow(row db.Task) Task {
	return Task{
		ID:        row.ID,
		Title:     row.Title,
		Done:      row.Done,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
