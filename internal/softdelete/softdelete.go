// Package softdelete holds what every soft-deleted table shares: the rule for how long a deleted
// row stays before a purge removes it for good. Each table's queries and purge live with its
// feature; see docs/standards/database.md#soft-delete.
package softdelete

import (
	"fmt"

	"github.com/mdayat/go-template/internal/apperr"
)

// MaxPurgeAgeDays is the oldest age a purge accepts, about ten years.
const MaxPurgeAgeDays = 3650

// CheckPurgeAge reports whether olderThanDays is an age a purge accepts: at least a day, so a purge
// never removes a row deleted moments ago that a user may still restore. Code that queues a purge
// checks it first, so the caller hears about a bad age instead of the job failing later.
func CheckPurgeAge(olderThanDays int) error {
	if olderThanDays < 1 || olderThanDays > MaxPurgeAgeDays {
		return apperr.Invalid("the age is out of range", apperr.FieldError{
			Field: "older_than_days", Rule: "range", Message: fmt.Sprintf("must be from 1 to %d", MaxPurgeAgeDays),
		})
	}
	return nil
}
