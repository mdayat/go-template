package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/softdelete"
)

// Purges serves /deleted-row-purges: removing deleted rows for good, now rather than at the
// worker's daily purge.
type Purges struct {
	Jobs    queue.Enqueuer
	Decoder *handler.Decoder
}

// Mount adds the routes to r.
func (pr Purges) Mount(r chi.Router) {
	r.Post("/deleted-row-purges", handler.Handle(pr.purge))
}

type purgeRequest struct {
	OlderThanDays *int `json:"older_than_days" validate:"required"`
}

// jobResponse is the reply of an endpoint that queues a job (202 Accepted): the job's ID, for
// finding it in the worker's logs.
type jobResponse struct {
	JobID string `json:"job_id"`
}

// purge queues a job that removes for good, from every soft-deleted table, the rows deleted more
// than older_than_days ago. It only queues: an endpoint that also wrote rows could save them and
// then fail to queue the job.
func (pr Purges) purge(w http.ResponseWriter, r *http.Request) error {
	var req purgeRequest
	if err := pr.Decoder.Decode(w, r, &req); err != nil {
		return err
	}
	// Checked here as well as in the job, so the caller hears about a bad age instead of the job
	// failing later where nobody sees it.
	if err := softdelete.CheckPurgeAge(*req.OlderThanDays); err != nil {
		return err
	}
	job, err := jobs.NewPurgeDeletedRows(*req.OlderThanDays)
	if err != nil {
		return err
	}
	id, err := pr.Jobs.Enqueue(r.Context(), job)
	if err != nil {
		return err
	}
	handler.WriteJSON(w, http.StatusAccepted, "application/json", jobResponse{JobID: id})
	return nil
}
