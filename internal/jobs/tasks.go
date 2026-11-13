package jobs

import (
	"fmt"
	"time"

	"github.com/hibiken/asynq"

	"github.com/mdayat/go-template/internal/json"
)

// TypePurgeDoneTasks deletes tasks marked done long ago; see task.Service.PurgeDone.
const TypePurgeDoneTasks = "tasks:purge_done"

// PurgeDoneTasks is the payload of a TypePurgeDoneTasks job.
type PurgeDoneTasks struct {
	OlderThanDays int `json:"older_than_days"`
}

// NewPurgeDoneTasks returns a job that deletes tasks marked done more than olderThanDays days ago.
func NewPurgeDoneTasks(olderThanDays int) (*asynq.Task, error) {
	payload, err := json.Marshal(PurgeDoneTasks{OlderThanDays: olderThanDays})
	if err != nil {
		return nil, fmt.Errorf("encode %s payload: %w", TypePurgeDoneTasks, err)
	}
	return asynq.NewTask(TypePurgeDoneTasks, payload,
		asynq.Queue(Queue),
		// asynq's default is 30 minutes; one delete takes seconds.
		asynq.Timeout(5*time.Minute),
		asynq.MaxRetry(5),
	), nil
}
