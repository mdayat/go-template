// Package worker runs background jobs: the handler for each job type, the middleware every job
// shares, and the schedule of jobs that run on a timetable.
package worker

import (
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/task"
)

// shutdownTimeout is how long running jobs get to finish after SIGTERM; asynq puts the ones still
// running back in the queue. Below the 10 s most platforms (Docker included) wait before they kill
// the process.
const shutdownTimeout = 8 * time.Second

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
func NewMux(logger zerolog.Logger, services Services) *asynq.ServeMux {
	mux := asynq.NewServeMux()
	mux.Use(logJobs(logger))
	taskJobs{tasks: services.Tasks}.mount(mux)
	return mux
}

// NewServer returns a server that runs the jobs in jobs.Queue. asynq's own messages go to logger.
func NewServer(client redis.UniversalClient, logger zerolog.Logger) *asynq.Server {
	return newServer(client, logger, jobs.Queue)
}

func newServer(client redis.UniversalClient, logger zerolog.Logger, queue string) *asynq.Server {
	return asynq.NewServerFromRedisClient(client, asynq.Config{
		Queues:          map[string]int{queue: 1},
		ShutdownTimeout: shutdownTimeout,
		Logger:          asynqLogger{logger: logger},
	})
}

// NewScheduler returns a scheduler that queues the jobs that run on a timetable, each feature's
// added by its schedule function. Every worker process runs one, so each scheduled job is queued
// with asynq.Unique: when several workers queue it at the same moment, only the first gets through.
func NewScheduler(client *redis.Client, logger zerolog.Logger) (*asynq.Scheduler, error) {
	return newScheduler(client, logger, 0)
}

// newScheduler is NewScheduler with how often the scheduler publishes its entries (0: asynq's
// default, 10 s); tests read them sooner.
func newScheduler(client *redis.Client, logger zerolog.Logger, heartbeat time.Duration) (*asynq.Scheduler, error) {
	scheduler := asynq.NewScheduler(ownConnection{options: client.Options()}, &asynq.SchedulerOpts{
		HeartbeatInterval: heartbeat,
		Logger:            asynqLogger{logger: logger},
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

	if err := scheduleTaskJobs(scheduler); err != nil {
		return nil, err
	}
	return scheduler, nil
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

// asynqLogger sends asynq's own messages (starting, shutting down, a lost Redis connection) to the
// JSON log, marked source=asynq. A job's result is logged by logJobs, not here.
type asynqLogger struct {
	logger zerolog.Logger
}

func (l asynqLogger) Debug(args ...any) { l.log(zerolog.DebugLevel, args) }
func (l asynqLogger) Info(args ...any)  { l.log(zerolog.InfoLevel, args) }
func (l asynqLogger) Warn(args ...any)  { l.log(zerolog.WarnLevel, args) }
func (l asynqLogger) Error(args ...any) { l.log(zerolog.ErrorLevel, args) }

// Fatal exits the process, as asynq's Logger interface requires.
func (l asynqLogger) Fatal(args ...any) {
	l.logger.Fatal().Str("source", "asynq").Msg(fmt.Sprint(args...))
}

func (l asynqLogger) log(level zerolog.Level, args []any) {
	l.logger.WithLevel(level).Str("source", "asynq").Msg(fmt.Sprint(args...))
}
