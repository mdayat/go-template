// Package worker runs background jobs: the handler for each job type, the middleware every job
// shares, and the schedule of jobs that run on a timetable. It sees the queue only through
// internal/queue; cmd/worker picks the implementation that runs them.
package worker

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/task"
)

// ShutdownTimeout is how long running jobs get to finish after SIGTERM; the queue then puts the
// ones still running back. Below the 10 s most platforms (Docker included) wait before they kill
// the process.
const ShutdownTimeout = 8 * time.Second

// Services holds what the job handlers call, one field per feature. A new feature adds its field
// here and builds it in NewServices.
type Services struct {
	Tasks *task.Service
}

// NewServices builds every feature's service on queries. The worker (cmd/worker) and the job tests
// both get their services here, so a feature can't be wired into one and forgotten in the other.
func NewServices(queries *db.Queries) Services {
	return Services{
		Tasks: task.NewService(queries),
	}
}

// NewMux returns the handler for every job type, each job logged to logger.
func NewMux(logger zerolog.Logger, services Services) *queue.Mux {
	mux := queue.NewMux()
	mux.Use(logJobs(logger))
	taskJobs{tasks: services.Tasks}.mount(mux)
	return mux
}

// Schedules returns every feature's jobs that run on a timetable. A new feature adds its schedule
// function here.
func Schedules() ([]queue.Schedule, error) {
	var all []queue.Schedule
	for _, schedules := range []func() ([]queue.Schedule, error){taskSchedules} {
		s, err := schedules()
		if err != nil {
			return nil, err
		}
		all = append(all, s...)
	}
	return all, nil
}
