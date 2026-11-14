package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/task"
)

// taskRoutes serves /tasks. Request shape (a field is present) is checked by validate tags; the
// rules about what a valid task is live in the task service.
type taskRoutes struct {
	tasks   *task.Service
	jobs    queue.Enqueuer
	decoder *bodyDecoder
}

func (tr taskRoutes) mount(r chi.Router) {
	r.Post("/tasks", Handle(tr.create))
	r.Get("/tasks", Handle(tr.list))
	r.Get("/tasks/{id}", Handle(tr.get))
	r.Patch("/tasks/{id}", Handle(tr.update))
	r.Delete("/tasks/{id}", Handle(tr.delete))
	r.Post("/task-purges", Handle(tr.purge))
}

type createTaskRequest struct {
	Title string `json:"title" validate:"required"`
}

// updateTaskRequest fields are optional: a missing or null field is left as it is.
type updateTaskRequest struct {
	Title *string `json:"title"`
	Done  *bool   `json:"done"`
}

type purgeTasksRequest struct {
	OlderThanDays *int `json:"older_than_days" validate:"required"`
}

type taskResponse struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type taskListResponse struct {
	Tasks []taskResponse `json:"tasks"`
	// Pass as ?after= for the next page; absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

func (tr taskRoutes) create(w http.ResponseWriter, r *http.Request) error {
	var req createTaskRequest
	if err := tr.decoder.decode(w, r, &req); err != nil {
		return err
	}
	t, err := tr.tasks.Create(r.Context(), req.Title)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/tasks/"+t.ID.String())
	writeJSON(w, http.StatusCreated, "application/json", toTaskResponse(t))
	return nil
}

func (tr taskRoutes) list(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	limit := defaultPageSize
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return apperr.BadRequest(fmt.Sprintf("limit must be a whole number from 1 to %d", task.MaxPageSize))
		}
		limit = n
	}
	var after *task.Cursor
	if raw := query.Get("after"); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			return err
		}
		after = &cursor
	}

	page, err := tr.tasks.List(r.Context(), limit, after)
	if err != nil {
		return err
	}
	resp := taskListResponse{Tasks: make([]taskResponse, 0, len(page.Tasks))}
	for _, t := range page.Tasks {
		resp.Tasks = append(resp.Tasks, toTaskResponse(t))
	}
	if page.Next != nil {
		resp.NextCursor = encodeCursor(*page.Next)
	}
	writeJSON(w, http.StatusOK, "application/json", resp)
	return nil
}

func (tr taskRoutes) get(w http.ResponseWriter, r *http.Request) error {
	id, err := taskID(r)
	if err != nil {
		return err
	}
	t, err := tr.tasks.Get(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, "application/json", toTaskResponse(t))
	return nil
}

func (tr taskRoutes) update(w http.ResponseWriter, r *http.Request) error {
	id, err := taskID(r)
	if err != nil {
		return err
	}
	var req updateTaskRequest
	if err := tr.decoder.decode(w, r, &req); err != nil {
		return err
	}
	t, err := tr.tasks.Update(r.Context(), id, task.Changes{Title: req.Title, Done: req.Done})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, "application/json", toTaskResponse(t))
	return nil
}

func (tr taskRoutes) delete(w http.ResponseWriter, r *http.Request) error {
	id, err := taskID(r)
	if err != nil {
		return err
	}
	if err := tr.tasks.Delete(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// purge queues a job that deletes the tasks marked done more than older_than_days ago. It only
// queues: an endpoint that also wrote rows could save them and then fail to queue the job.
func (tr taskRoutes) purge(w http.ResponseWriter, r *http.Request) error {
	var req purgeTasksRequest
	if err := tr.decoder.decode(w, r, &req); err != nil {
		return err
	}
	// Checked here as well as in the job, so the caller hears about a bad age instead of the job
	// failing later where nobody sees it.
	if err := task.CheckPurgeAge(*req.OlderThanDays); err != nil {
		return err
	}
	job, err := jobs.NewPurgeDoneTasks(*req.OlderThanDays)
	if err != nil {
		return err
	}
	id, err := tr.jobs.Enqueue(r.Context(), job)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, "application/json", jobResponse{JobID: id})
	return nil
}

// taskID reads the {id} path segment. An ID that isn't a UUID can't name a task, so it's a 404 like
// any other unknown ID.
func taskID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.UUID{}, apperr.NotFound("task not found")
	}
	return id, nil
}

func toTaskResponse(t task.Task) taskResponse {
	return taskResponse{
		ID:    t.ID,
		Title: t.Title,
		Done:  t.Done,
		// pgx returns times in this process's local time zone; replies always use UTC.
		CreatedAt: t.CreatedAt.UTC(),
		UpdatedAt: t.UpdatedAt.UTC(),
	}
}

// encodeCursor makes the cursor for the page after c. It's opaque to clients: base64url of
// "<created_at RFC 3339>|<id>" of a page's last task.
func encodeCursor(c task.Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()))
}

func decodeCursor(s string) (task.Cursor, error) {
	invalid := apperr.BadRequest("after is not a cursor from a previous page")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return task.Cursor{}, invalid
	}
	createdAt, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return task.Cursor{}, invalid
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return task.Cursor{}, invalid
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return task.Cursor{}, invalid
	}
	return task.Cursor{CreatedAt: t, ID: u}, nil
}
