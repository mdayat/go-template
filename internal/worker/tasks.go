package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/httpclient"
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
	tasks        *task.Service
	purgeReports EventSender
}

// tasksPurgedEvent is what the purge reports. It says what's true after the purge, not how many
// rows it deleted: if the report fails, the job runs again, deletes nothing, and a count would
// say 0.
type tasksPurgedEvent struct {
	Event         string `json:"event"` // "tasks.purged"
	OlderThanDays int    `json:"older_than_days"`
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

	// Last, after the work it reports: a failure retries the whole job, which is safe to repeat.
	// The job's ID is the same on every try, so the receiver can drop a repeated report.
	run, _ := queue.RunFrom(ctx)
	event, err := json.Marshal(tasksPurgedEvent{Event: "tasks.purged", OlderThanDays: payload.OlderThanDays})
	if err != nil {
		return fmt.Errorf("encode purge report: %w", err)
	}
	if err := tj.purgeReports.Send(ctx, run.ID, event); err != nil {
		if !httpclient.Retryable(err) {
			return fmt.Errorf("report purge: %w: %w", err, queue.ErrNoRetry)
		}
		return fmt.Errorf("report purge: %w", err)
	}
	return nil
}
