package valkey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/propagation"

	"github.com/mdayat/go-template/internal/queue"
)

// ServerOptions configures a Server.
type ServerOptions struct {
	// Queues are the queues the server takes jobs from, at least one, each with how many of its
	// jobs run at once.
	Queues []queue.Limit
	// ShutdownTimeout is how long running jobs get to finish after Shutdown, at least a second;
	// asynq then puts the ones still running back in their queue and cancels their contexts.
	ShutdownTimeout time.Duration
	// Logger gets asynq's own messages (starting, shutting down, a lost connection).
	Logger zerolog.Logger
}

// Server takes jobs off the queues and runs them through a queue.Mux.
type Server struct {
	// One asynq server per queue: asynq's limit is per server, shared by all of its queues, and
	// its queue priorities only change which queue is picked first.
	servers []*asynq.Server
	client  redis.UniversalClient
	mux     *queue.Mux
}

// NewServer returns a server that runs the jobs in opts.Queues through mux. It doesn't close client.
// It refuses options asynq would quietly replace: no queues (asynq reads "default"), a blank queue
// name, a limit under 1 (asynq runs as many jobs as there are CPUs), or no shutdown time (asynq
// waits 8 s); and a queue listed twice.
func NewServer(client redis.UniversalClient, mux *queue.Mux, opts ServerOptions) (*Server, error) {
	if len(opts.Queues) == 0 {
		return nil, errors.New("a server needs at least one queue")
	}
	if opts.ShutdownTimeout < time.Second {
		return nil, fmt.Errorf("shutdown timeout %s is under a second", opts.ShutdownTimeout)
	}
	s := &Server{client: client, mux: mux}
	seen := make(map[string]bool, len(opts.Queues))
	for _, q := range opts.Queues {
		switch {
		case strings.TrimSpace(q.Queue) == "":
			return nil, errors.New("a server's queue name is blank")
		case seen[q.Queue]:
			return nil, fmt.Errorf("queue %s is listed twice", q.Queue)
		case q.Concurrency < 1:
			return nil, fmt.Errorf("queue %s: %d jobs at once, want at least 1", q.Queue, q.Concurrency)
		}
		seen[q.Queue] = true
		s.servers = append(s.servers, asynq.NewServerFromRedisClient(client, asynq.Config{
			Queues:          map[string]int{q.Queue: 1},
			Concurrency:     q.Concurrency,
			ShutdownTimeout: opts.ShutdownTimeout,
			Logger:          asynqLogger{logger: opts.Logger},
		}))
	}
	return s, nil
}

// Start starts taking jobs from every queue, without blocking. If one queue can't start, the ones
// started are stopped again.
func (s *Server) Start() error {
	for i, server := range s.servers {
		if err := server.Start(asynq.HandlerFunc(s.process)); err != nil {
			for _, started := range s.servers[:i] {
				started.Shutdown()
			}
			return err
		}
	}
	return nil
}

// Shutdown stops taking jobs and waits up to ShutdownTimeout for running ones: all queues at once,
// so the wait isn't that times the number of queues.
func (s *Server) Shutdown() {
	var wg sync.WaitGroup
	for _, server := range s.servers {
		wg.Go(server.Shutdown)
	}
	wg.Wait()
}

// process runs one asynq task through the mux, with the run's details in its context, and the
// trace it was queued in, if any (see newTask), for the run's span to join.
func (s *Server) process(ctx context.Context, t *asynq.Task) error {
	ctx = traceHeaders.Extract(ctx, propagation.MapCarrier(t.Headers()))
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
		RequestID:   t.Headers()[requestIDHeader],
		WillRetry:   func(err error) bool { return willRetry(ctx, err, retried, maxRetry) },
	}

	if window, ok := scheduledWindow(t.Headers()); ok {
		logger := zerolog.Ctx(ctx).With().Str("job_type", t.Type()).Str("job_id", id).Logger()
		first, err := claimSlot(ctx, s.client, slot{queue: queueName, jobType: t.Type(), payload: t.Payload()}, id, window, retried > 0)
		if err != nil {
			// Before the mux, whose middleware logs every run: this one's line is here. asynq
			// retries it.
			logger.Error().Err(err).Msg("scheduled job not run")
			return err
		}
		if !first {
			// Another worker's scheduler queued this run too, and its copy came first. A retry (or a
			// run from the archive, which keeps its retry count) dropped here is worth a warning:
			// someone may be waiting for it.
			level := zerolog.InfoLevel
			if retried > 0 {
				level = zerolog.WarnLevel
			}
			logger.WithLevel(level).Msg("scheduled job already ran in this time, so this copy didn't")
			return nil
		}
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
