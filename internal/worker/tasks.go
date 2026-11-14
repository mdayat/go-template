package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/json"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/task"
)

// purgeAfterDays is how long a done task is kept before the daily purge deletes it.
const purgeAfterDays = 30

// taskSchedules returns the tasks feature's jobs that run on a timetable.
func taskSchedules() ([]queue.Schedule, error) {
	purge, err := jobs.NewPurgeDoneTasks(purgeAfterDays)
	if err != nil {
		return nil, err
	}
	return []queue.Schedule{
		// Midnight UTC.
		{Spec: "@daily", Job: purge, Unique: time.Hour},
	}, nil
}

// taskJobs runs the tasks feature's jobs. The rules live in the task service, as for the API.
type taskJobs struct {
	tasks *task.Service
}

func (tj taskJobs) mount(mux *queue.Mux) {
	mux.Handle(jobs.TypePurgeDoneTasks, tj.purgeDone)
}

func (tj taskJobs) purgeDone(ctx context.Context, raw []byte) error {
	var payload jobs.PurgeDoneTasks
	// Unknown fields are allowed: a newer API may queue a payload with fields this version lacks.
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode payload: %w: %w", err, apperr.BadRequest("the payload is not valid JSON"))
	}
	deleted, err := tj.tasks.PurgeDone(ctx, payload.OlderThanDays)
	if err != nil {
		return err
	}
	zerolog.Ctx(ctx).Info().Int64("deleted", deleted).Msg("done tasks purged")
	return nil
}
