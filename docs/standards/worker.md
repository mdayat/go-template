# Background jobs

Work that shouldn't make a caller wait (sending email, a report, a clean-up) runs as a **job** in
the worker (`cmd/worker`), not in a request. Jobs go through a queue in Valkey (an open-source
fork of Redis; any Redis works), run by [asynq](https://github.com/hibiken/asynq). The API puts
jobs on the queue; the worker takes them off and runs them, retrying the ones that fail.

The example: `tasks:purge_done` deletes tasks marked done long ago. The worker's scheduler queues
it every day at midnight UTC, and `POST /task-purges` queues it on demand.

## Where things go

| What | Where |
|---|---|
| a job's type name, payload struct and options (timeout, retries) | `internal/jobs/<feature>.go` |
| the handler | `internal/worker/<feature>.go`, mounted in `NewMux` (`internal/worker/worker.go`) |
| the rules it applies | the feature's service (`internal/task`), the same one the API calls |
| wiring a new service | a field in `worker.Services` and a line in `worker.NewServices` |
| a job that runs on a timetable | `scheduler.Register` in the feature's schedule function (`scheduleTaskJobs`), called from `newScheduler` |
| tests | `internal/worker/<feature>_test.go` |

The API and the worker build and read jobs only through `internal/jobs`, so they can't disagree on
a type name or a field.

## Every job can run twice

asynq runs a job **at least once**, not exactly once. A job runs again when:

- it was still running when the worker stopped (a deploy): after 8 s asynq puts it back in the
  queue;
- it failed: it's retried, up to `MaxRetry` times, with growing delays (seconds at first, then
  minutes, then hours);
- something queued it twice (a user clicked twice, two workers' schedulers fired).

So write every job so that a second run changes nothing more than the first: delete or update by
a condition (`WHERE done AND updated_at < ...`), not "add one"; check whether the email was already
sent before sending it. Test it: run the job twice and check the rows after each run
(`TestPurgeDoneTasks`).

## Payloads are a contract between versions

During a deploy, old and new code run side by side: a job queued by the new API may be run by an
old worker, and the other way round. So:

- a payload only **gains** fields; never rename or remove one, or change its meaning;
- a handler accepts a payload without the newest fields (the zero value means "not sent"), and
  ignores fields it doesn't know (`TestPurgeDoneTasksAcceptsUnknownFields`);
- payloads hold IDs, not copies of rows: the row may have changed by the time the job runs.

Never put secrets or personal data in a payload: it sits in Valkey in plain text and shows in
asynq's tools.

## Errors and retries

A handler returns errors like any service caller ([errors.md](errors.md)), and the worker decides
what happens:

| The handler returns | What happens |
|---|---|
| `nil` | done |
| an `apperr` kind (bad payload, `NotFound`, `Invalid`) | failed for good, no retries: it would fail the same way every time |
| any other error (database down, a timeout, another service's 503) | retried, up to the job's `MaxRetry`; then failed for good |
| a panic | caught, logged with its stack, retried like an error |

A failed-for-good job is kept in Valkey ("archived") for asynq's tools to show; it doesn't run
again unless someone re-runs it.

If "not found" is normal for a job (the row was deleted after the job was queued), return `nil`:
there's nothing left to do, and that's not a failure.

```go
note, err := nj.notes.Get(ctx, payload.NoteID)
if e, ok := errors.AsType[*apperr.Error](err); ok && e.Kind == apperr.KindNotFound {
	return nil
}
```

## Time limits and shutdown

Every job sets `asynq.Timeout` in `internal/jobs` (asynq's default is 30 minutes). asynq keeps it
in whole seconds, so anything under 1 s is dropped and the default applies. The handler's context
ends at the limit, so pass `ctx` to everything it calls, as in a request. A job past its limit
counts as failed and is retried, whatever the handler then returns (an `apperr` kind included).

On SIGTERM the worker stops its scheduler, stops taking jobs, and gives running ones 8 s to finish
(`shutdownTimeout` in `internal/worker/worker.go`, below the 10 s most platforms wait before a hard
kill). Jobs still running then are put back in the queue and their contexts cancelled, which stops
their queries. They run again on the next worker, from the start.

## Logging

`logJobs` (`internal/worker/logging.go`) writes one line per run:

```json
{"level":"info","job_type":"tasks:purge_done","job_id":"88f4e9aa-7d1a-41b2-8eb3-0f22bb65802e",
 "queue":"default","attempt":1,"max_attempts":6,"duration_ms":6.3,"message":"job completed"}
```

| Level | When |
|---|---|
| info | the job succeeded |
| warn | it failed and will be retried (`will_retry: true`); one is not a problem, many are |
| error | it failed for good, or panicked |

A failed run carries `error.message` (and `error.stack_trace` for a panic) on the same line. Inside
a handler, `zerolog.Ctx(ctx)` is the job's logger: lines logged through it carry `job_type` and
`job_id`. Log on a line of your own only what's worth finding later (`"done tasks purged"` with the
count), never the payload.

asynq also logs its own events (starting, shutting down, "Retry exhausted for task", a job put
back in the queue), marked `"source":"asynq"`. Alert on the `job completed` lines, not on those.

## Queueing from the API

An endpoint that queues a job replies `202 Accepted` with the job's ID, `{"job_id": "..."}`, so
the caller can find it in the worker's logs. Check the input before queueing (the same service
check the job runs, or a read that the row exists), so the caller gets a 422 or 404 now instead of
the job failing later where nobody sees it.

**An endpoint either queues a job or writes rows, not both.** Writing a row and then queueing can
half-fail: the row is saved, the queue is down, the job is lost; the other order runs the job for
a row that was never saved. When you need both (save an order, then email it), use an **outbox**:
in the same transaction as the row, insert a row into an `outbox` table; a scheduled job reads the
outbox, queues the jobs and deletes the rows it queued.

## Scheduled jobs

Each feature's schedule function (`scheduleTaskJobs`) registers its jobs that run on a timetable,
as cron specs in UTC (`"@daily"`,
`"0 3 * * *"`). Every worker process runs a scheduler, so with three workers each scheduled job is
queued three times at the same moment; `asynq.Unique(d)` lets only the first through. Pick `d`
shorter than the time between runs (an hour for a daily job, 30 minutes for an hourly one), or it
blocks the next run too. A scheduled job still has to be safe to run twice: `Unique` stops
duplicates only until the first copy succeeds or `d` is up.

## Big tables

The example deletes in one statement, which is fine for thousands of rows. For millions, delete in
batches (`DELETE ... WHERE id IN (SELECT id ... LIMIT 1000)` in a loop, each its own transaction)
so no statement holds locks for long, and add an index that matches the condition
([database.md](database.md)).

## Tests

- Job handlers are tested by calling the real mux (`testWorker(t)`: the real handlers and services
  on a database of the test's own) with a real `asynq.Task`, then checking the returned error **and
  the rows**. No Valkey needed.
- An error that must not be retried is checked with `errors.Is(err, asynq.SkipRetry)`.
- Code that queues jobs is tested against a real Valkey (`internal/testqueue`), the job looked up by
  its ID and checked, then deleted (`testqueue.Take`).
- A scheduled job's entry (cron spec, payload, `Unique`) is checked with
  `scheduledEntries(t, jobType)` (`internal/worker/main_test.go`), which runs the real scheduler and
  returns only that job type's entries.
- The worker's own behaviour (log lines, retries) is tested by running a real asynq server on a
  queue of the test's own (`TestLogJobs`).

## Running

```sh
make db-up        # Postgres and Valkey
make run-worker   # the worker; `make run` for the API
```

Deploy the worker image (`make image` builds `<name>-worker`) next to the API, with the
settings `cmd/worker/settings.go` reads. It serves no port; run as many copies as the queue needs.
