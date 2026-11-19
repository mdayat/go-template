# Background jobs

Work that shouldn't make a caller wait (sending email, a report, a clean-up) runs as a **job** in
the worker (`cmd/worker`), not in a request. Jobs go through a queue in Valkey (an open-source
fork of Redis), run by [asynq](https://github.com/hibiken/asynq). Code puts jobs on the queue; the
worker takes them off and runs them, retrying the ones that fail. It needs Valkey 7.2+ or Redis 7+:
scheduled jobs use `SET` with both `NX` and `GET`, which older versions refuse.

Our code sees the queue only through `internal/queue`: a job (`queue.Job`), queueing one
(`queue.Enqueuer`), a handler (`func(ctx, payload []byte) error`), the run it's in (`queue.Run`)
and a timetable (`queue.Schedule`). `internal/queue/valkey` implements them with asynq, and only
`cmd/` may import it (lint: depguard). Moving to another queue is a new implementation and a few
lines in `cmd/`, not edits across the features.

The example: `deleted_rows:purge` removes for good, from every soft-deleted table, the rows
deleted more than 30 days ago ([database.md](database.md#soft-delete)). The worker's scheduler
queues it every day at midnight UTC.
<!-- remove:api -->
`POST /admin/deleted-row-purges` queues it on demand.
<!-- remove:end -->

## Where things go

| What | Where |
|---|---|
| a job's type name, payload struct and options (timeout, retries) | `internal/jobs/<feature>.go`, built as a `queue.Job` |
| the handler, `func(ctx, payload []byte) error` | `internal/worker/<feature>.go`, mounted in `NewMux` (`internal/worker/worker.go`) |
| the rules it applies | the feature's service (`internal/task`), the same one every entry point calls |
| wiring a new service | a field in `worker.Services` and a line in `worker.NewServices` |
| a job that runs on a timetable | a `queue.Schedule` from the feature's schedule function (`purgeSchedules`), listed in `Schedules` |
| tests | `internal/worker/<feature>_test.go` |

Everything that queues or runs a job builds and reads it only through `internal/jobs`, so they
can't disagree on a type name or a field.

## Queues

Every job goes to `jobs.Queue` ("default") unless it needs a queue of its own. Each queue has its
own number of jobs a worker instance (one running copy of the worker) runs at once, so jobs that
fill one queue's slots can't hold up another's. A job gets its own queue when it mustn't wait
behind others (urgent work, like a sign-in email) or would hold others up (slow work, like calling
an AI model): add a constant like `jobs.Queue` and a line in `jobs.Queues()` with its number, and
set the job's `Queue` to it. A job sent to a queue `jobs.Queues()` doesn't list is never run, so
add every job's builder to `TestQueues` (`internal/jobs/jobs_test.go`), which checks its queue.

`WORKER_QUEUES` overrides the numbers per instance, `default=7,slow=3` (once a `slow` queue is
added; a name `jobs.Queues()` lacks stops the worker at start), and an instance takes only the
queues it lists. So slow jobs can run on instances of their own (`WORKER_QUEUES=slow=3`) while
others take the rest. A queue no instance takes never runs its jobs: those queued to run now show
as a growing wait ([metrics.md](metrics.md#what-to-alert-on)), but those waiting for a retry or
a later time stay `retrying` or scheduled, as only an instance taking the queue moves them on.
The worker opens a database connection for each job it runs at once, and 2 for the outbox
([below](#queueing-through-the-outbox), [database.md](database.md#connections)).

Removing a queue takes two deploys. Jobs already in it stay there, and once it's gone from
`jobs.Queues()` no instance takes them. First send its jobs to another queue, keeping the old one
listed; once its `job.queue.jobs` shows none waiting, retrying or running, delete its line.

## Every job can run twice

asynq runs a job **at least once**, not exactly once. A job runs again when:

- it was still running when the worker stopped (a deploy): after 8 s asynq puts it back in the
  queue;
- it failed: it's retried, up to `MaxRetries` times, with growing delays (seconds at first, then
  minutes, then hours);
- something queued it twice (a user clicked twice, two workers' schedulers fired).

So write every job so that a second run changes nothing more than the first: delete or update by
a condition (`WHERE deleted_at < ...`), not "add one"; check whether the email was already sent
before sending it. Test it: run the job twice and check the rows after each run
(`TestPurgeDeletedRows`).

## Payloads are a contract between versions

During a deploy, old and new code run side by side: a job queued by new code may be run by an
old worker, and the other way round. So:

- a payload only **gains** fields; never rename or remove one, or change its meaning;
- a handler accepts a payload without the newest fields (the zero value means "not sent"), and
  ignores fields it doesn't know (`TestPurgeDeletedRowsAcceptsUnknownFields`);
- payloads hold IDs, not copies of rows: the row may have changed by the time the job runs;
- a job's type name is part of the contract too: jobs already queued carry the old name, and a
  worker without a handler for it fails them. To rename one, handle both names until no job
  with the old one is left, then drop it.

Never put secrets or personal data in a payload: it sits in Valkey in plain text and shows in
asynq's tools.

## Errors and retries

A handler returns errors like any service caller ([errors.md](errors.md)), and the worker decides
what happens:

| The handler returns | What happens |
|---|---|
| `nil` | done |
| an `apperr` kind (bad payload, `NotFound`, `Invalid`) | failed for good, no retries: it would fail the same way every time (`logJobs` marks it `queue.ErrNoRetry`) |
| any other error (database down, a timeout, another service's 503) | retried, up to the job's `MaxRetries`; then failed for good |
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

Every job sets its `Timeout` in `internal/jobs`, in whole seconds, at least 1. Queueing refuses
anything else: asynq drops fractions of a second and replaces a zero limit with its 30-minute
default. The handler's context ends at the limit, so pass `ctx` to everything it calls, as in a
request. A job past its limit counts as failed: asynq retries it while attempts are left, whatever
the handler then returns (an `apperr` kind included). A limit over 5 minutes also needs the
worker's `DB_STATEMENT_TIMEOUT` default raised to match ([database.md](database.md#time-limits)),
or Postgres cuts the job's long statements first.

On SIGTERM the worker stops its scheduler, stops taking jobs, and gives running ones 8 s to finish
(`ShutdownTimeout` in `internal/worker/worker.go`, below Docker's default 10 s before a hard
kill, the shortest common wait). Jobs still running then are put back in the queue and their
contexts cancelled, which stops their queries. They run again on the next worker, from the start.

## Logging

`logJobs` (`internal/worker/logging.go`) writes one line per run:

```json
{"level":"info","job_type":"deleted_rows:purge","job_id":"88f4e9aa-7d1a-41b2-8eb3-0f22bb65802e",
 "queue":"default","attempt":1,"max_attempts":6,"duration_ms":6.3,"message":"job completed"}
```

| Level | When |
|---|---|
| info | the job succeeded |
| warn | it failed and will be retried (`will_retry: true`); one is not a problem, many are |
| error | it failed for good, or panicked |

A failed run carries `error.message` (and `error.stack_trace` for a panic) on the same line. Inside
a handler, `zerolog.Ctx(ctx)` is the job's logger: its lines carry `job_type` and `job_id`, plus
`trace.id` and `span.id` when the run's trace is kept (traces may be sampled,
[tracing.md](tracing.md)). The run is a span: in the trace of the code that queued it, if queued
straight to the queue; in a trace of its own if queued through the outbox. Either way, a job queued
during a request carries that request's `request_id`. Log a line of your own only for what's worth
finding later (`"deleted rows purged"` with each table's count), never the payload.

asynq also logs its own events (starting, shutting down, "Retry exhausted for task", a job put
back in the queue), marked `"source":"asynq"`. Alert on the `job completed` lines, not on those.

## Queueing through the outbox

Saving a row and queueing its job touch two stores, Postgres and Valkey, and either can fail
after the other succeeded: the row saved and its job lost, or the job run for a row never saved.
So jobs are queued through the database: `internal/outbox` writes a job to the `outbox` table,
and the worker's relay (`outbox.Relay`, in every worker instance) moves each to the queue about
every second, then deletes it.

- Saving rows and queueing a job together: write both in one transaction, `outbox.Add(ctx, q, job)`
  inside `postgres.InTx`. Both are saved, or neither is.
- Only queueing: `outbox.Enqueuer`, a `queue.Enqueuer` that writes each job in a transaction of its
  own.

A job runs under its outbox row's ID, so the ID returned when queueing is the one in the worker's
logs, and carries the `request_id` of the request that queued it; it starts a trace of its own.
While Valkey is down, jobs wait in the outbox and go once it's back, so queueing doesn't fail with
it. A job waits up to about a second before the queue has it: queue straight to Valkey
(`valkey.Enqueuer`) only where that second matters more than both guarantees.

The relay deletes a job only once the queue has it, and queues it under the row's ID, which the
queue refuses a second time while it holds the job. A relay that dies between the two leaves the
row, and the next try is refused as a duplicate: the job isn't queued twice. Once the queue has
dropped the finished job it can't tell any more, so a job is still written to run twice safely
([above](#every-job-can-run-twice)). Relays in several instances take different rows (`FOR UPDATE
SKIP LOCKED`). Its metrics and alert: [metrics.md](metrics.md#what-to-alert-on). A row the queue
would never take (written by other code, or before a check got stricter) is logged and dropped, so
it can't block the rows behind it.

Deploy the migration that adds the `outbox` table first, as always ([database.md](database.md)).
Code that queues through the outbox needs a worker of the same release or newer to move its jobs:
with an older one (the API deployed first, or the worker rolled back), jobs wait in the outbox,
none lost, and the alert fires until the worker catches up. The worker needs 2 database connections
besides its jobs' (`DB_MAX_CONNS` at least jobs + 2) and `DB_IDLE_IN_TRANSACTION_TIMEOUT` of at
least 20 s; it refuses to start otherwise. A relay batch's transaction sits idle while it queues
jobs: up to 10 s (it stops taking jobs at a third of its 30 s limit) plus a Valkey call in flight.
Cut shorter, Postgres ends the transaction and undoes its deletes, so jobs already queued run again.

<!-- remove:api -->
## Queueing from the API

An endpoint that queues a job replies `202 Accepted` with the job's ID, `{"job_id": "..."}`, so
the caller can find it in the worker's logs. Check the input before queueing (the same service
check the job runs, or a read that the row exists), so the caller gets a 422 or 404 now instead of
the job failing later where nobody sees it.

Handlers queue through `queue.Enqueuer` (`Services.Jobs`), which `cmd/api` sets to the outbox's,
so the API connects to the database only, never to the queue. A service that saves rows and queues
a job adds the job in its own transaction instead ([above](#queueing-through-the-outbox)).

<!-- remove:end -->
## Scheduled jobs

Each schedule function (`purgeSchedules`) returns its jobs that run on a timetable, as
`queue.Schedule`s with cron specs in UTC (`"@daily"`, `"0 3 * * *"`). Every worker process runs a
scheduler, so with three workers each scheduled job is queued three times at the same moment;
`Unique` (required) lets only the first through. asynq releases that hold once the copy succeeds,
which for a quick job can be before another worker's timer fires. So the server also runs only the
first copy of a scheduled job in its `Unique` time and drops the rest, logging each at warn
(`claimSlot` in `internal/queue/valkey`: one Valkey key per queue, job type and payload). A retry
runs only if its copy still holds the key, so a late retry can't take the next run's place.
The window starts when a copy starts running: pick `Unique` well under the time between
runs (an hour for a daily job, 30 minutes for an hourly one), or a copy that waited in the queue
blocks the next run. Two schedules of the same job and payload share one window. A scheduled job
still has to be safe to run twice: past `Unique`, if Valkey loses the key, or while a deploy mixes
old and new workers, a second copy runs.

## What any queue can do

The queue may change (SQS, say), so jobs rely only on what most queues offer:

- **At least once.** Every queue may deliver a job twice; see [above](#every-job-can-run-twice).
- **Small payloads**: under 256 KB (SQS's limit). Put IDs in them, not files or long lists.
- **Delays up to 15 minutes** (SQS's limit), once delayed jobs are needed; anything later is a
  schedule, or a row with a due time that a scheduled job picks up.
- **Time limits and `Unique` in whole seconds.**
- **Retries per job, with the queue's own delays.** `MaxRetries` is set per job; SQS sets it per
  queue (`maxReceiveCount`), so an SQS implementation would read it from the job and give up
  itself. The delay between tries is the queue's (asynq: seconds at first, then growing); a job
  can't ask for "retry at" a time (a 429's `Retry-After`). Calls to outside services from a job:
  [outside-calls.md](outside-calls.md).
- **Timetables come from our code** (`queue.Schedule`), since not every queue has a scheduler.
- **Whether a failed run is retried is the queue's rule**, not ours: asynq retries a run past its
  time limit whatever it returned, and puts back one cut off by shutdown without counting it.
  `queue.Run.WillRetry` tells the log line what the queue will do.

## Big tables

The example deletes in one statement, which is fine for thousands of rows. For millions, delete in
batches (`DELETE ... WHERE id IN (SELECT id ... LIMIT 1000)` in a loop, each its own transaction)
so no statement holds locks for long, and add an index that matches the condition
([database.md](database.md)).

## Tests

- Job handlers are tested by calling the real mux (`testWorker(t)`: the real handlers and services
  on a database of the test's own) with `mux.Process(ctx, jobType, payload)`, then checking the
  returned error **and the rows**. No Valkey needed.
- An error that must not be retried is checked with `errors.Is(err, queue.ErrNoRetry)`.
- Code that queues through the outbox is checked by reading its row in the test database's `outbox`
  table by ID; code that queues straight to Valkey against a real Valkey (`internal/testqueue`),
  the job looked up by its ID and checked, then deleted (`testqueue.Take`).
- A scheduled job is checked in the list `Schedules` returns (spec, job, `Unique`); `cmd/worker`'s
  test checks that the queue it runs on accepts every schedule.
- The log line is checked for each kind of run (`TestLogJobs`), and on a real server for the runs
  the queue decides (past the time limit, cut off by shutdown: `TestLogJobsOnTheQueue`). That the
  queue does what `queue.Run` says (retries, the scheduler's `Unique`) is checked in the
  implementation's own tests (`internal/queue/valkey`).

## Running

```sh
make db-up        # Postgres and Valkey
make run-worker   # the worker
```

Deploy the worker image (`make image` builds `<name>-worker`) with the settings
`cmd/worker/settings.go` reads. It serves no port; run as many instances as the queues need: a
wait that keeps growing ([metrics.md](metrics.md#what-to-alert-on)) means too few.
