package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/jobs"
	"github.com/mdayat/go-template/internal/queue"
	"github.com/mdayat/go-template/internal/startup"
)

// settings are what the worker reads at startup; .example.env describes each.
type settings struct {
	app         config.App
	databaseURL string
	dbLimits    startup.DBLimits
	redisURL    string // the job queue
	// queues are the queues this instance takes jobs from, and how many of each it runs at once.
	queues []queue.Limit
	// purgeWebhookURL gets a report after each purge; "" sends none.
	purgeWebhookURL string
	pprofAddr       string // where to serve profiles; "" serves none
}

// loadSettings reads the worker's settings from sources, the first winning, and reports every
// missing or invalid one in one error.
func loadSettings(sources ...config.Source) (settings, error) {
	r := config.NewReader(sources...)
	queues, queuesErr := readQueues(r.String("WORKER_QUEUES", ""), jobs.Queues())
	running := 0
	for _, q := range queues {
		running += q.Concurrency
	}
	s := settings{
		app:         r.App(),
		databaseURL: r.Required("DATABASE_URL"),
		// Statement: at least the longest job's Timeout (internal/jobs), so no job's statement is cut
		// before its job is.
		dbLimits: startup.DBLimits{
			// A job uses one connection at a time: one for every job running at once, and the spare.
			MaxConns:          r.Count("DB_MAX_CONNS", running+spareConns),
			Statement:         r.Duration("DB_STATEMENT_TIMEOUT", 5*time.Minute),
			IdleInTransaction: r.Duration("DB_IDLE_IN_TRANSACTION_TIMEOUT", time.Minute),
		},
		redisURL: r.Required("REDIS_URL"),
		queues:   queues,

		purgeWebhookURL: r.OptionalHTTPURL("PURGE_WEBHOOK_URL"),
		pprofAddr:       r.String("PPROF_ADDR", ""),
	}
	err := errors.Join(r.Err(), queuesErr, startup.CheckProfileAddr(s.pprofAddr))
	// Only with queues read: without them the pool's default (their total) means nothing.
	if queuesErr == nil {
		err = errors.Join(err, checkMaxConns(s.dbLimits.MaxConns, running))
	}
	if s.dbLimits.IdleInTransaction < minIdleInTransaction {
		err = errors.Join(err, fmt.Errorf("DB_IDLE_IN_TRANSACTION_TIMEOUT is %s; at least %s, as the outbox's relay keeps a transaction open while it queues a batch", s.dbLimits.IdleInTransaction, minIdleInTransaction))
	}
	if err != nil {
		return settings{}, err
	}
	return s, nil
}

// maxConcurrency is the most jobs of one queue an instance may run at once: more is a typo.
const maxConcurrency = 1000

// readQueues reads WORKER_QUEUES, "default=7,slow=3": the queues this instance takes jobs from, each
// with how many of its jobs it runs at once, or all of known, at their own limits, if value is "".
// A queue left out isn't taken by this instance: run another that takes it, or its jobs never run.
func readQueues(value string, known []queue.Limit) ([]queue.Limit, error) {
	if value == "" {
		return known, nil
	}
	var queues []queue.Limit
	for entry := range strings.SplitSeq(value, ",") {
		name, count, _ := strings.Cut(strings.TrimSpace(entry), "=")
		n, err := strconv.Atoi(count)
		switch {
		case !slices.ContainsFunc(known, func(q queue.Limit) bool { return q.Queue == name }):
			names := make([]string, 0, len(known))
			for _, q := range known {
				names = append(names, q.Queue)
			}
			return nil, fmt.Errorf("WORKER_QUEUES: no queue %q; the queues are %s", name, strings.Join(names, ", "))
		case err != nil || n < 1 || n > maxConcurrency:
			return nil, fmt.Errorf("WORKER_QUEUES: %s needs how many jobs at once, 1 to %d, like %s=5", name, maxConcurrency, name)
		case slices.ContainsFunc(queues, func(q queue.Limit) bool { return q.Queue == name }):
			return nil, fmt.Errorf("WORKER_QUEUES: %s is listed twice", name)
		}
		queues = append(queues, queue.Limit{Queue: name, Concurrency: n})
	}
	return queues, nil
}

// minIdleInTransaction is the shortest DB_IDLE_IN_TRANSACTION_TIMEOUT the worker takes. A relay
// batch's transaction sits idle while it queues jobs: up to 10 s (a third of its 30 s limit) plus
// a Valkey call in flight (up to 2 s each to get a connection, connect, send and answer), so 20 s
// leaves room to spare.
// Cut shorter, the batch's deletes are lost and its jobs queued again: a finished one runs twice.
const minIdleInTransaction = 20 * time.Second

// spareConns are the worker's connections besides its jobs': the outbox's relay holds one through
// each batch, and its metrics take one each time they're sent.
const spareConns = 2

// checkMaxConns refuses a pool smaller than the jobs running at once and the spare: some would wait
// for a connection, and could run out their time limit waiting.
func checkMaxConns(maxConns, running int) error {
	switch {
	case running+spareConns > startup.MaxDBConns:
		return fmt.Errorf("the queues (jobs.Queues, or WORKER_QUEUES) run %d jobs at once; at most %d, as each needs a database connection", running, startup.MaxDBConns-spareConns)
	case maxConns < running+spareConns:
		return fmt.Errorf("DB_MAX_CONNS is %d, fewer than the %d jobs WORKER_QUEUES runs at once and %d for the outbox", maxConns, running, spareConns)
	}
	return startup.CheckMaxConns(maxConns)
}
