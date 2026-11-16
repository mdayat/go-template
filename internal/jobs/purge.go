package jobs

import (
	"fmt"
	"time"

	"github.com/mdayat/go-template/internal/json"
	"github.com/mdayat/go-template/internal/queue"
)

// TypePurgeDeletedRows removes for good, from every soft-deleted table, the rows deleted long ago.
const TypePurgeDeletedRows = "deleted_rows:purge"

// PurgeDeletedRows is the payload of a TypePurgeDeletedRows job.
type PurgeDeletedRows struct {
	OlderThanDays int `json:"older_than_days"`
}

// NewPurgeDeletedRows returns a job that removes for good the rows deleted more than olderThanDays
// days ago.
func NewPurgeDeletedRows(olderThanDays int) (queue.Job, error) {
	payload, err := json.Marshal(PurgeDeletedRows{OlderThanDays: olderThanDays})
	if err != nil {
		return queue.Job{}, fmt.Errorf("encode %s payload: %w", TypePurgeDeletedRows, err)
	}
	return queue.Job{
		Type:    TypePurgeDeletedRows,
		Queue:   Queue,
		Payload: payload,
		// One delete per table, each taking seconds.
		Timeout:    5 * time.Minute,
		MaxRetries: 5,
	}, nil
}
