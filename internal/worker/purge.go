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
	"github.com/mdayat/go-template/internal/softdelete"
)

// purgeAfterDays is how long a deleted row can still be restored before the daily purge removes it
// for good.
const purgeAfterDays = 30

// tablePurge removes for good one soft-deleted table's rows deleted more than olderThanDays days
// ago, and returns how many it removed.
type tablePurge struct {
	table string
	purge func(ctx context.Context, olderThanDays int) (int64, error)
}

// tablePurges lists the purge of every soft-deleted table: a new one adds its line, or its deleted
// rows are kept forever (TestEverySoftDeletedTableIsPurged fails; it checks the names, so each
// line's purge must be its own table's). A table that other rows link to comes after them:
// removed first, it would break their links (or take them along, with ON DELETE CASCADE) while
// they're still kept. Rows the law says to keep (payments, invoices) aren't purged on this
// timetable: their table isn't soft deleted, or has a purge of its own.
func tablePurges(services Services) []tablePurge {
	return []tablePurge{
		{table: "tasks", purge: services.Tasks.PurgeDeleted},
	}
}

// purgeSchedules returns the daily purge.
func purgeSchedules() ([]queue.Schedule, error) {
	purge, err := jobs.NewPurgeDeletedRows(purgeAfterDays)
	if err != nil {
		return nil, err
	}
	return []queue.Schedule{
		// Midnight UTC.
		{Spec: "@daily", Job: purge, Unique: time.Hour},
	}, nil
}

// purgeJobs runs the purge of deleted rows.
type purgeJobs struct {
	tables  []tablePurge
	reports EventSender
}

// deletedRowsPurgedEvent is what the purge reports. It says what's true after the purge, not how
// many rows it removed: if the report fails, the job runs again, removes nothing, and a count would
// say 0.
type deletedRowsPurgedEvent struct {
	Event         string `json:"event"` // "deleted_rows.purged"
	OlderThanDays int    `json:"older_than_days"`
}

func (pj purgeJobs) mount(mux *queue.Mux) {
	mux.Handle(jobs.TypePurgeDeletedRows, pj.purge)
}

func (pj purgeJobs) purge(ctx context.Context, raw []byte) error {
	var payload jobs.PurgeDeletedRows
	// Unknown fields are allowed: a newer version may queue a payload with fields this one lacks.
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode payload: %w: %w", err, apperr.BadRequest("the payload is not valid JSON"))
	}
	// Each table's purge checks the age too; checked here as well so a bad one fails the job even
	// with no soft-deleted table, instead of reporting a purge that never ran.
	if err := softdelete.CheckPurgeAge(payload.OlderThanDays); err != nil {
		return err
	}
	// A failure part way retries the whole job: each table's delete is safe to repeat.
	removed := zerolog.Dict()
	for _, t := range pj.tables {
		n, err := t.purge(ctx, payload.OlderThanDays)
		if err != nil {
			return fmt.Errorf("purge %s: %w", t.table, err)
		}
		removed.Int64(t.table, n)
	}
	zerolog.Ctx(ctx).Info().Dict("removed", removed).Msg("deleted rows purged")

	// Last, after the work it reports: a failure retries the whole job, which is safe to repeat.
	// The job's ID is the same on every try, so the receiver can drop a repeated report.
	run, _ := queue.RunFrom(ctx)
	event, err := json.Marshal(deletedRowsPurgedEvent{Event: "deleted_rows.purged", OlderThanDays: payload.OlderThanDays})
	if err != nil {
		return fmt.Errorf("encode purge report: %w", err)
	}
	if err := pj.reports.Send(ctx, run.ID, event); err != nil {
		if !httpclient.Retryable(err) {
			return fmt.Errorf("report purge: %w: %w", err, queue.ErrNoRetry)
		}
		return fmt.Errorf("report purge: %w", err)
	}
	return nil
}
