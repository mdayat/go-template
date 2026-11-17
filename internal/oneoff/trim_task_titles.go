package oneoff

import (
	"context"
	"flag"

	"github.com/google/uuid"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/task"
)

// trimTaskTitles trims the spaces around task titles that still have them. It's the example to
// copy: batches in ID order, the rules in the service, safe to run again.
func trimTaskTitles() Command {
	return Command{
		Name:    "trim-task-titles",
		Summary: "trim the spaces around task titles",
		New: func(fs *flag.FlagSet) Work {
			batch := fs.Int("batch", 500, "tasks per transaction")
			return func(ctx context.Context, r *Run) error {
				var after uuid.UUID
				for {
					var page task.TitleTrim
					err := r.Batch(ctx, func(q *db.Queries) error {
						var err error
						page, err = task.NewService(q).TrimTitles(ctx, after, *batch)
						return err
					})
					if err != nil {
						return err
					}
					if page.Checked == 0 {
						return nil
					}
					// Counted once the batch has ended well: a failed one changed nothing.
					r.Count("checked", page.Checked)
					r.Count("trimmed", page.Trimmed)
					r.Count("left_title_taken", page.Taken) // another of the owner's tasks has the trimmed title
					r.Count("left_blank", page.Blank)       // only spaces: a title can't be empty
					r.Count("left_changed", page.Changed)   // edited or deleted by a user meanwhile
					// The next page starts after this one even in a dry run, where nothing changed.
					after = page.Last
				}
			}
		},
	}
}
