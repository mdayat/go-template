package jobs

import (
	"fmt"
	"time"

	"github.com/mdayat/go-template/internal/json"
	"github.com/mdayat/go-template/internal/queue"
)

// TypePurgeDoneTasks deletes tasks marked done long ago; see task.Service.PurgeDone.
const TypePurgeDoneTasks = "tasks:purge_done"

// PurgeDoneTasks is the payload of a TypePurgeDoneTasks job.
type PurgeDoneTasks struct {
	OlderThanDays int `json:"older_than_days"`
}

// NewPurgeDoneTasks returns a job that deletes tasks marked done more than olderThanDays days ago.
func NewPurgeDoneTasks(olderThanDays int) (queue.Job, error) {
	payload, err := json.Marshal(PurgeDoneTasks{OlderThanDays: olderThanDays})
	if err != nil {
		return queue.Job{}, fmt.Errorf("encode %s payload: %w", TypePurgeDoneTasks, err)
	}
	return queue.Job{
		Type:    TypePurgeDoneTasks,
		Queue:   Queue,
		Payload: payload,
		// One delete takes seconds.
		Timeout:    5 * time.Minute,
		MaxRetries: 5,
	}, nil
}
