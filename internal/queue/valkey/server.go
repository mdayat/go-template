package valkey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/queue"
)

// ServerOptions configures a Server.
type ServerOptions struct {
	// Queues are the queues the server takes jobs from, all with the same priority; at least one.
	Queues []string
	// ShutdownTimeout is how long running jobs get to finish after Shutdown, at least a second;
	// asynq then puts the ones still running back in their queue and cancels their contexts.
	ShutdownTimeout time.Duration
	// Logger gets asynq's own messages (starting, shutting down, a lost connection).
	Logger zerolog.Logger
}

// Server takes jobs off the queues and runs them through a queue.Mux.
type Server struct {
	server *asynq.Server
	mux    *queue.Mux
}

// NewServer returns a server that runs the jobs in opts.Queues through mux. It doesn't close client.
// It refuses options asynq would quietly replace: no queues (asynq reads "default"), a blank queue
// name, or no shutdown time (asynq waits 8 s).
func NewServer(client redis.UniversalClient, mux *queue.Mux, opts ServerOptions) (*Server, error) {
	if len(opts.Queues) == 0 {
		return nil, errors.New("a server needs at least one queue")
	}
	if opts.ShutdownTimeout < time.Second {
		return nil, fmt.Errorf("shutdown timeout %s is under a second", opts.ShutdownTimeout)
	}
	queues := make(map[string]int, len(opts.Queues))
	for _, name := range opts.Queues {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("a server's queue name is blank")
		}
		queues[name] = 1
	}
	return &Server{
		server: asynq.NewServerFromRedisClient(client, asynq.Config{
			Queues:          queues,
			ShutdownTimeout: opts.ShutdownTimeout,
			Logger:          asynqLogger{logger: opts.Logger},
		}),
		mux: mux,
	}, nil
}

// Start starts taking jobs, without blocking.
func (s *Server) Start() error {
	return s.server.Start(asynq.HandlerFunc(s.process))
}

// Shutdown stops taking jobs and waits up to ShutdownTimeout for running ones.
func (s *Server) Shutdown() {
	s.server.Shutdown()
}

// process runs one asynq task through the mux, with the run's details in its context.
func (s *Server) process(ctx context.Context, t *asynq.Task) error {
	id, _ := asynq.GetTaskID(ctx)
	queueName, _ := asynq.GetQueueName(ctx)
	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	run := queue.Run{
		ID:          id,
		Type:        t.Type(),
		Queue:       queueName,
		Attempt:     retried + 1,
		MaxAttempts: maxRetry + 1,
		WillRetry:   func(err error) bool { return willRetry(ctx, err, retried, maxRetry) },
	}

	err := s.mux.Process(queue.WithRun(ctx, run), t.Type(), t.Payload())
	if errors.Is(err, queue.ErrNoRetry) {
		return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
	}
	return err
}

// willRetry is asynq's rule for retrying a run that ended with err, after retried earlier attempts.
func willRetry(ctx context.Context, err error, retried, maxRetry int) bool {
	// Once the run's context has ended, asynq decides without waiting for the handler: past the
	// time limit it retries by count alone; cut off by shutdown it puts the job back in the queue
	// without counting the attempt. (A lost lease also cancels the context and is counted; it's
	// rare and reported here as a retry.)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return !errors.Is(ctxErr, context.DeadlineExceeded) || retried < maxRetry
	}
	return err != nil && !errors.Is(err, queue.ErrNoRetry) && retried < maxRetry
}
