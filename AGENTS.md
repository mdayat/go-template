# Instructions for AI coding agents

A Go HTTP service on Postgres with a background worker: chi router, pgx + sqlc, Atlas migrations,
zerolog, asynq jobs on Valkey (Redis). The full rules,
with reasons, are in `docs/standards/`; read the page for the area you're changing. The tasks
feature is the working example of every rule: copy its shape.

## Before you finish

Run `make fmt lint test check-generated`. All four must pass; don't silence a linter with
`//nolint` unless the line is truly a false positive, and then name the linter and the reason.
`make test` starts Postgres and Valkey in Docker; tests read `TEST_DATABASE_URL` and
`TEST_REDIS_URL` from the environment or `.env` (see `.example.env`).

## Where things go

| Change | Where | Example |
|---|---|---|
| business rules, mapping database errors | `internal/<feature>/` service | `internal/task/service.go` |
| routes and handlers | `internal/api/<feature>.go`, mounted in `router.go` | `internal/api/tasks.go` |
| wiring a new service | a field in `api.Services` and a line in `api.NewServices` (`router.go`); the app and the tests both build services there | `Tasks` |
| endpoint tests | `internal/api/<feature>_test.go` | `internal/api/tasks_test.go` |
| a background job: type name, payload, timeout, retries | `internal/jobs/<feature>.go` | `internal/jobs/tasks.go` |
| job handlers | `internal/worker/<feature>.go`, mounted in `NewMux` (`worker.go`); services wired in `worker.Services` / `worker.NewServices` | `internal/worker/tasks.go` |
| jobs on a timetable | the feature's schedule function in `internal/worker/<feature>.go`, called from `newScheduler` (`worker.go`) | `scheduleTaskJobs` |
| job tests | `internal/worker/<feature>_test.go`, through `testWorker(t)` | `internal/worker/tasks_test.go` |
| anything several features use | not in one feature's files (removing that feature would break the rest): test helpers in `internal/api/main_test.go`, other code in a shared file such as `router.go` | `send`, `readProblem`, `defaultPageSize` |
| schema | `db/schema.sql`, then `make migrate-diff name=<what>` | `db/migrations/` |
| queries | `db/queries/<feature>.sql`, then `make generate` | `db/queries/tasks.sql` |
| settings | the program's `cmd/<name>/settings.go` (read through `internal/config`) and `.example.env` | `DATABASE_URL` |

Never edit `internal/db/` (generated) or a migration that's already merged (write a new one).
Use JSON only through `internal/json`, never go-json or `encoding/json` (tests may use
`encoding/json`); lint refuses it.

## Rules

**Errors** ([errors.md](docs/standards/errors.md))
- Services return `apperr` kinds (`BadRequest`, `Invalid`, `NotFound`, `Conflict`) for expected
  failures; anything else becomes a 500. Never return an HTTP status from a service.
- Map driver errors to kinds in the service (`pgx.ErrNoRows` → `NotFound`, a named unique
  constraint → `Conflict`), never in a handler.
- Wrap with context: `fmt.Errorf("get task %s: %w", id, err)`. Never log an error you return.
- `apperr` details are shown to callers: no SQL, hosts or internal IDs in them.

**HTTP** ([http-api.md](docs/standards/http-api.md))
- Handlers are `func(w, r) error`, wrapped with `Handle`. Read JSON bodies only with
  `tr.decoder.decode`; write replies only with `writeJSON`.
- `validate` tags check shape; the service checks meaning.
- Lists: an object with a list field, cursor paging, never `null` for an empty list.

**Logging** ([logging.md](docs/standards/logging.md))
- Don't add log lines for errors: the request's summary line already carries them.
- Never log bodies, headers, query strings, secrets or personal data.
- Never call `zerolog.Ctx(ctx).UpdateContext` where `ctx` may lack a request logger.

**Jobs** ([worker.md](docs/standards/worker.md))
- Every job may run twice (retries, a deploy mid-job, duplicates): make a second run change nothing,
  and test that.
- Payloads only gain fields; handlers accept payloads without the newest ones and ignore unknown
  ones. IDs in payloads, never secrets or personal data.
- Return an `apperr` kind for a failure a retry can't fix (it won't be retried); return `nil` when
  "not found" just means there's nothing left to do. Set `asynq.Timeout` on every job.
- An endpoint queues a job (202 with `job_id`) or writes rows, never both; check the input before
  queueing.

**Database** ([database.md](docs/standards/database.md))
- Migrations must work with the app version still running: a `NOT NULL` column needs a default;
  rename or drop a column over several deploys; big-table indexes `CONCURRENTLY` with
  `-- atlas:txmode none`.
- Rules that must hold under concurrent requests are schema constraints, with names.
- Text going into a `text` column is checked for NUL (`\u0000`) in the service; Postgres refuses it.
- Retry a transaction only on 40001 and 40P01, and never do outside calls inside it.

**Tests** ([testing.md](docs/standards/testing.md))
- Endpoint tests use `testAPI(t)` (the real router and services on a database of the test's own);
  check the reply **and** the rows. Don't build a router per feature. Job tests use `testWorker(t)`
  the same way; a queued job is checked with `testqueue.Take`.
- Don't mock our own code. Seed rows with SQL, not through the endpoint under test.
- `t.Parallel()`, table tests, `cmp.Diff(want, got)`. Don't assert on library error text.
- Break the code a test guards and confirm the test fails (the broken code must compile).

**Style** ([code-style.md](docs/standards/code-style.md))
- Return early; no named results; `ctx` first; no package-level mutable state, no `init`.
- Accept interfaces where you use them, return concrete types.
- Comments say why, never what or when; no history in comments.
- No `util`/`common`/`helpers` packages.
