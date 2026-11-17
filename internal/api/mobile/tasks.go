package mobile

import (
	"net/http"
	"path"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/task"
)

// Tasks serves /tasks. Request shape (a field is present) is checked by validate tags; the rules
// about what a valid task is live in the task service.
type Tasks struct {
	Tasks   *task.Service
	Decoder *handler.Decoder
}

// Mount adds the routes to r.
func (tr Tasks) Mount(r chi.Router) {
	r.Post("/tasks", handler.Handle(tr.create))
	r.Get("/tasks", handler.Handle(tr.list))
	r.Get("/tasks/{id}", handler.Handle(tr.get))
	r.Patch("/tasks/{id}", handler.Handle(tr.update))
	r.Delete("/tasks/{id}", handler.Handle(tr.delete))
	r.Post("/tasks/{id}/restore", handler.Handle(tr.restore))
}

type createTaskRequest struct {
	Title string `json:"title" validate:"required"`
}

// updateTaskRequest fields are optional: a missing or null field is left as it is.
type updateTaskRequest struct {
	Title *string `json:"title"`
	Done  *bool   `json:"done"`
}

type taskResponse struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
	Done  bool      `json:"done"`
	// null for a task from before tasks had owners (only admins see those).
	OwnerID   *uuid.UUID `json:"owner_id"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type taskListResponse struct {
	Tasks []taskResponse `json:"tasks"`
	// Pass as ?after= for the next page; absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

func (tr Tasks) create(w http.ResponseWriter, r *http.Request) error {
	var req createTaskRequest
	if err := tr.Decoder.Decode(w, r, &req); err != nil {
		return err
	}
	t, err := tr.Tasks.Create(r.Context(), handler.CurrentUser(r), req.Title)
	if err != nil {
		return err
	}
	// Under this client's own prefix: the collection's path plus the ID.
	w.Header().Set("Location", path.Join(r.URL.Path, t.ID.String()))
	handler.WriteJSON(w, http.StatusCreated, "application/json", toTaskResponse(t))
	return nil
}

func (tr Tasks) list(w http.ResponseWriter, r *http.Request) error {
	limit, err := handler.PageLimit(r, task.MaxPageSize)
	if err != nil {
		return err
	}
	cursor, err := handler.DecodeCursor(r)
	if err != nil {
		return err
	}
	var after *task.Cursor
	if cursor != nil {
		after = &task.Cursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	}

	page, err := tr.Tasks.List(r.Context(), handler.CurrentUser(r), limit, after)
	if err != nil {
		return err
	}
	resp := taskListResponse{Tasks: make([]taskResponse, 0, len(page.Tasks))}
	for _, t := range page.Tasks {
		resp.Tasks = append(resp.Tasks, toTaskResponse(t))
	}
	if page.Next != nil {
		resp.NextCursor = handler.EncodeCursor(handler.Cursor{CreatedAt: page.Next.CreatedAt, ID: page.Next.ID})
	}
	handler.WriteJSON(w, http.StatusOK, "application/json", resp)
	return nil
}

func (tr Tasks) get(w http.ResponseWriter, r *http.Request) error {
	id, err := handler.PathUUID(r, "id", "task")
	if err != nil {
		return err
	}
	t, err := tr.Tasks.Get(r.Context(), handler.CurrentUser(r), id)
	if err != nil {
		return err
	}
	handler.WriteJSON(w, http.StatusOK, "application/json", toTaskResponse(t))
	return nil
}

func (tr Tasks) update(w http.ResponseWriter, r *http.Request) error {
	id, err := handler.PathUUID(r, "id", "task")
	if err != nil {
		return err
	}
	var req updateTaskRequest
	if err := tr.Decoder.Decode(w, r, &req); err != nil {
		return err
	}
	t, err := tr.Tasks.Update(r.Context(), handler.CurrentUser(r), id, task.Changes{Title: req.Title, Done: req.Done})
	if err != nil {
		return err
	}
	handler.WriteJSON(w, http.StatusOK, "application/json", toTaskResponse(t))
	return nil
}

func (tr Tasks) delete(w http.ResponseWriter, r *http.Request) error {
	id, err := handler.PathUUID(r, "id", "task")
	if err != nil {
		return err
	}
	if err := tr.Tasks.Delete(r.Context(), handler.CurrentUser(r), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// restore undoes a delete. It takes no body; restoring a task that isn't deleted returns it as it
// is, so a client can safely send it again.
func (tr Tasks) restore(w http.ResponseWriter, r *http.Request) error {
	id, err := handler.PathUUID(r, "id", "task")
	if err != nil {
		return err
	}
	t, err := tr.Tasks.Restore(r.Context(), handler.CurrentUser(r), id)
	if err != nil {
		return err
	}
	handler.WriteJSON(w, http.StatusOK, "application/json", toTaskResponse(t))
	return nil
}

func toTaskResponse(t task.Task) taskResponse {
	return taskResponse{
		ID:      t.ID,
		Title:   t.Title,
		Done:    t.Done,
		OwnerID: t.OwnerID,
		// pgx returns times in this process's local time zone; replies always use UTC.
		CreatedAt: t.CreatedAt.UTC(),
		UpdatedAt: t.UpdatedAt.UTC(),
	}
}
