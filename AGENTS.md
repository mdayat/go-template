# Instructions for AI coding agents

A Go service on Postgres: pgx + sqlc, Atlas migrations, zerolog. The full rules, with reasons, are
in `docs/standards/`; read the page for the area you're changing. The tasks feature is the working
example of every rule: copy its shape.
<!-- remove:api -->
The HTTP API (`cmd/api`, `internal/api`) uses the chi router.
<!-- remove:end -->
<!-- remove:worker -->
The worker (`cmd/worker`, `internal/worker`) runs jobs queued with asynq on Valkey (Redis).
<!-- remove:end -->

## Before you finish

Run `make fmt lint test check-generated`. All four must pass; don't silence a linter with
`//nolint` unless the line is truly a false positive, and then name the linter and the reason.
`make test` starts the compose services in Docker; tests read `TEST_DATABASE_URL` from the
environment or `.env` (see `.example.env`).
<!-- remove:worker -->
Tests that queue jobs also read `TEST_REDIS_URL`.
<!-- remove:end -->

## Where things go

| Change | Where | Example |
|---|---|---|
| business rules, mapping database errors | `internal/<feature>/` service | `internal/task/service.go` |
| service tests: every rule, on a real database | `internal/<feature>/service_test.go` | `internal/task/service_test.go` |
| schema | `db/schema.sql`, then `make migrate-diff name=<what>` | `db/migrations/` |
| queries | `db/queries/<feature>.sql`, then `make generate` | `db/queries/tasks.sql` |
| settings | the program's `cmd/<name>/settings.go` (read through `internal/config`), its `settings_test.go`, and `.example.env` | `DATABASE_URL` |
| anything several features use | not in one feature's files (removing that feature would break the rest): test helpers in a package's `main_test.go`, other code in a shared file | `internal/testdb` (one package every test uses) |

<!-- remove:api -->
In the HTTP API:

| Change | Where | Example |
|---|---|---|
| routes and handlers | `internal/api/<feature>.go`, mounted in `router.go` | `internal/api/tasks.go` |
| wiring a new service | a field in `api.Services` and a line in `api.NewServices` (`router.go`); the app and the tests both build services there | `Tasks` |
| endpoint tests | `internal/api/<feature>_test.go` | `internal/api/tasks_test.go` |
| helpers several features use | test helpers in `internal/api/main_test.go`, other code in `router.go` | `send`, `readProblem`, `defaultPageSize` |

<!-- remove:end -->
<!-- remove:worker -->
In the worker:

| Change | Where | Example |
|---|---|---|
| a background job: type name, payload, timeout, retries | `internal/jobs/<feature>.go` | `internal/jobs/tasks.go` |
| job handlers, `func(ctx, payload []byte) error` | `internal/worker/<feature>.go`, mounted in `NewMux` (`worker.go`); services wired in `worker.Services` / `worker.NewServices` | `internal/worker/tasks.go` |
| jobs on a timetable | the feature's schedule function in `internal/worker/<feature>.go`, returning `queue.Schedule`s, listed in `Schedules` (`worker.go`) | `taskSchedules` |
| job tests | `internal/worker/<feature>_test.go`, through `testWorker(t)` | `internal/worker/tasks_test.go` |

<!-- remove:end -->
Never edit `internal/db/` (generated) or a migration that's already merged (write a new one).
Use JSON only through `internal/json`, never go-json or `encoding/json` (tests may use
`encoding/json`); lint refuses it. Import pgx only in `internal/postgres`, `internal/startup`
and `internal/testdb` (tests may use it to seed rows); lint refuses it elsewhere. A generated
`pgtype` type gets an override in `sqlc.yaml` instead.
<!-- remove:worker -->
Jobs are queued, handled and scheduled through `internal/queue`, never asynq or go-redis; lint
refuses them outside `internal/queue/valkey`, `internal/startup` and `internal/testqueue`, and
`internal/queue/valkey` itself outside `cmd/`.
<!-- remove:end -->

## Rules

**Errors** ([errors.md](docs/standards/errors.md))
- Services return `apperr` kinds (`BadRequest`, `Invalid`, `NotFound`, `Conflict`) for expected
  failures; anything else is unexpected. Never decide a response (an HTTP status, a retry) in a
  service.
- Map driver errors to kinds in the service, through `internal/postgres` (`IsNotFound` →
  `NotFound`, `IsUniqueViolation` on a named constraint → `Conflict`), never in a handler.
- Wrap with context: `fmt.Errorf("get task %s: %w", id, err)`. Never log an error you return.
- `apperr` details are shown to callers: no SQL, hosts or internal IDs in them.

<!-- remove:api -->
**HTTP** ([http-api.md](docs/standards/http-api.md))
- Handlers are `func(w, r) error`, wrapped with `Handle`. Read JSON bodies only with
  `tr.decoder.decode`; write replies only with `writeJSON`.
- `validate` tags check shape; the service checks meaning.
- Lists: an object with a list field, cursor paging, never `null` for an empty list.

<!-- remove:end -->
**Logging** ([logging.md](docs/standards/logging.md))
- Don't add log lines for errors: the summary line of the work that failed already carries them.
- Never log bodies, headers, query strings, secrets or personal data.
- Never call `zerolog.Ctx(ctx).UpdateContext` where `ctx` may lack a logger of its own.

<!-- remove:worker -->
**Jobs** ([worker.md](docs/standards/worker.md))
- Every job may run twice (retries, a deploy mid-job, duplicates): make a second run change nothing,
  and test that.
- Payloads only gain fields; handlers accept payloads without the newest ones and ignore unknown
  ones. IDs in payloads, never secrets or personal data.
- Return an `apperr` kind for a failure a retry can't fix (it won't be retried); return `nil` when
  "not found" just means there's nothing left to do. Set `Timeout` on every job's `queue.Job`.
- Check the input before queueing, so the caller hears about it now. An endpoint that queues a
  job replies 202 with `job_id` and writes no rows (an outbox when it must do both).

<!-- remove:end -->
**Outside services** ([outside-calls.md](docs/standards/outside-calls.md))
- Behind a small interface defined where it's used, passed in from `cmd/`; its implementation in
  its own package (`internal/webhook`). HTTP clients come from `httpclient.New(timeout)`, never
  `http.Get`/`http.DefaultClient` or a client you build yourself.
- While a caller waits (a request), don't retry.
<!-- remove:worker -->
- In a job, return the error; mark it `queue.ErrNoRetry` when `httpclient.Retryable` says a retry
  can't help. Send the job's ID as an idempotency key.
<!-- remove:end -->
- The call comes last, after work that's safe to repeat, and sends what's true after a repeat.
  Never put the URL, bodies or tokens in errors or logs.
- Tests fake the service with `httptest.Server` and check what we sent.

**Database** ([database.md](docs/standards/database.md))
- Migrations must work with the app version still running: a `NOT NULL` column needs a default;
  rename or drop a column over several deploys; big-table indexes `CONCURRENTLY` with
  `-- atlas:txmode none`.
- Rules that must hold under concurrent requests are schema constraints, with names.
- Text going into a `text` column is checked for NUL (`\u0000`) in the service; Postgres refuses it.
- Retry a transaction only on 40001 and 40P01, and never do outside calls inside it.

**Tests** ([testing.md](docs/standards/testing.md))
- Test every rule on the service (`internal/<feature>/service_test.go`); tests through an entry
  point need one case per distinct outcome, not every input that gives it.
<!-- remove:api -->
- Endpoint tests use `testAPI(t)` (the real router and services on a database of the test's own);
  check the reply **and** the rows. Don't build a router per feature.
<!-- remove:end -->
<!-- remove:worker -->
- Job tests use `testWorker(t)` (the real handlers and services on a database of the test's own);
  check the error **and** the rows. A queued job is checked with `testqueue.Take`.
<!-- remove:end -->
- Don't mock our own code. Seed rows with SQL, not through the code under test.
- `t.Parallel()`, table tests, `cmp.Diff(want, got)`. Don't assert on library error text.
- Break the code a test guards and confirm the test fails (the broken code must compile).

**Style** ([code-style.md](docs/standards/code-style.md))
- Return early; no named results; `ctx` first; no package-level mutable state, no `init`.
- Accept interfaces where you use them, return concrete types.
- Comments say why, never what or when; no history in comments.
- No `util`/`common`/`helpers` packages.
