package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/json"
	"github.com/mdayat/go-template/internal/task"
)

// purgeAfterDays is how long a done task is kept before the daily purge deletes it.
const purgeAfterDays = 30

// scheduleTaskJobs adds the tasks feature's jobs that run on a timetable.
func scheduleTaskJobs(scheduler *asynq.Scheduler) error {
	purge, err := jobs.NewPurgeDoneTasks(purgeAfterDays)
	if err != nil {
		return err
	}
	// Midnight UTC.
	if _, err := scheduler.Register("@daily", purge, asynq.Unique(time.Hour)); err != nil {
		return fmt.Errorf("schedule %s: %w", purge.Type(), err)
	}
	return nil
}

// taskJobs runs the tasks feature's jobs. The rules live in the task service, as for the API.
type taskJobs struct {
	tasks *task.Service
}

func (tj taskJobs) mount(mux *asynq.ServeMux) {
	mux.HandleFunc(jobs.TypePurgeDoneTasks, tj.purgeDone)
}

func (tj taskJobs) purgeDone(ctx context.Context, t *asynq.Task) error {
	var payload jobs.PurgeDoneTasks
	// Unknown fields are allowed: a newer API may queue a payload with fields this version lacks.
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("decode payload: %w: %w", err, apperr.BadRequest("the payload is not valid JSON"))
	}
	deleted, err := tj.tasks.PurgeDone(ctx, payload.OlderThanDays)
	if err != nil {
		return err
	}
	zerolog.Ctx(ctx).Info().Int64("deleted", deleted).Msg("done tasks purged")
	return nil
}
