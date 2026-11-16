package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/softdelete"
)

// purgeRoutes serves /deleted-row-purges: removing deleted rows for good, now rather than at the
// worker's daily purge.
type purgeRoutes struct {
	jobs    queue.Enqueuer
	decoder *bodyDecoder
}

func (pr purgeRoutes) mount(r chi.Router) {
	r.Post("/deleted-row-purges", Handle(pr.purge))
}

type purgeRequest struct {
	OlderThanDays *int `json:"older_than_days" validate:"required"`
}

// purge, for admins, queues a job that removes for good, from every soft-deleted table, the rows
// deleted more than older_than_days ago. It only queues: an endpoint that also wrote rows could
// save them and then fail to queue the job.
func (pr purgeRoutes) purge(w http.ResponseWriter, r *http.Request) error {
	// It removes every user's deleted rows for good: not one user's call to make.
	if !currentUser(r).IsAdmin {
		return apperr.Forbidden("only admins may purge deleted rows")
	}
	var req purgeRequest
	if err := pr.decoder.decode(w, r, &req); err != nil {
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
	id, err := pr.jobs.Enqueue(r.Context(), job)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, "application/json", jobResponse{JobID: id})
	return nil
}
