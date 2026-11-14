package valkey

import (
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/queue"
)

// Scheduler queues jobs on a timetable.
type Scheduler struct {
	scheduler *asynq.Scheduler
}

// NewScheduler returns a scheduler that queues each schedule's job on its timetable. Every worker
// process runs one, so each job is queued with its schedule's Unique: when several workers queue it
// at the same moment, only the first gets through. asynq's own messages go to logger.
func NewScheduler(client *redis.Client, logger zerolog.Logger, schedules []queue.Schedule) (*Scheduler, error) {
	tasks := make([]*asynq.Task, len(schedules))
	for i, s := range schedules {
		// asynq keeps it in whole seconds and refuses one under a second only when the job fires.
		if s.Unique < time.Second || s.Unique%time.Second != 0 {
			return nil, fmt.Errorf("schedule %s: unique %s is not a whole number of seconds", s.Job.Type, s.Unique)
		}
		task, err := newTask(s.Job)
		if err != nil {
			return nil, fmt.Errorf("schedule %s: %w", s.Job.Type, err)
		}
		tasks[i] = task
	}

	scheduler := asynq.NewScheduler(ownConnection{options: client.Options()}, &asynq.SchedulerOpts{
		Logger: asynqLogger{logger: logger},
		PostEnqueueFunc: func(info *asynq.TaskInfo, err error) {
			switch {
			case err == nil:
				logger.Info().Str("job_type", info.Type).Str("job_id", info.ID).Msg("scheduled job queued")
			case errors.Is(err, asynq.ErrDuplicateTask):
				logger.Debug().Err(err).Msg("scheduled job already queued by another worker")
			default:
				logger.Error().Err(err).Msg("queue scheduled job")
			}
		},
	})

	for i, s := range schedules {
		if _, err := scheduler.Register(s.Spec, tasks[i], asynq.Unique(s.Unique)); err != nil {
			return nil, fmt.Errorf("schedule %s: %w", s.Job.Type, err)
		}
	}
	return &Scheduler{scheduler: scheduler}, nil
}

// Start starts queueing jobs, without blocking.
func (s *Scheduler) Start() error {
	return s.scheduler.Start()
}

// Shutdown stops queueing jobs.
func (s *Scheduler) Shutdown() {
	s.scheduler.Shutdown()
}

// ownConnection gives the scheduler a connection of its own, with client's settings. On shutdown the
// scheduler closes its connection; on a shared one (asynq.NewSchedulerFromRedisClient) that fails
// and logs an error every time.
type ownConnection struct {
	options *redis.Options
}

func (c ownConnection) MakeRedisClient() any {
	// A copy: NewClient fills in defaults on the options it gets, and these are client's own.
	options := *c.options
	return redis.NewClient(&options)
}
