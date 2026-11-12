# Instructions for AI coding agents

A Go HTTP service on Postgres: chi router, pgx + sqlc, Atlas migrations, zerolog. The full rules,
with reasons, are in `docs/standards/`; read the page for the area you're changing. The tasks
feature is the working example of every rule: copy its shape.

## Before you finish

Run `make fmt lint test check-generated`. All four must pass; don't silence a linter with
`//nolint` unless the line is truly a false positive, and then name the linter and the reason.

## Where things go

| Change | Where | Example |
|---|---|---|
| business rules, mapping database errors | `internal/<feature>/` service | `internal/task/service.go` |
| routes and handlers | `internal/api/<feature>.go`, mounted in `router.go` | `internal/api/tasks.go` |
| endpoint tests | `internal/api/<feature>_test.go` | `internal/api/tasks_test.go` |
| schema | `db/schema.sql`, then `make migrate-diff name=<what>` | `db/migrations/` |
| queries | `db/queries/<feature>.sql`, then `make generate` | `db/queries/tasks.sql` |
| settings | `internal/config/config.go` and `.example.env` | `DATABASE_URL` |

Never edit `internal/db/` (generated) or a migration that's already merged (write a new one).

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
  `h.decoder.decode`; write replies only with `writeJSON`.
- `validate` tags check shape; the service checks meaning.
- Lists: an object with a list field, cursor paging, never `null` for an empty list.

**Logging** ([logging.md](docs/standards/logging.md))
- Don't add log lines for errors: the request's summary line already carries them.
- Never log bodies, headers, query strings, secrets or personal data.
- Never call `zerolog.Ctx(ctx).UpdateContext` where `ctx` may lack a request logger.

**Database** ([database.md](docs/standards/database.md))
- Migrations must work with the app version still running: a `NOT NULL` column needs a default;
  rename or drop a column over several deploys; big-table indexes `CONCURRENTLY` with
  `-- atlas:txmode none`.
- Rules that must hold under concurrent requests are schema constraints, with names.
- Retry a transaction only on 40001 and 40P01, and never do outside calls inside it.

**Tests** ([testing.md](docs/standards/testing.md))
- Endpoint tests through `NewRouter` on `testdb.New(t)`; check the reply **and** the rows.
- Don't mock our own code. Seed rows with SQL, not through the endpoint under test.
- `t.Parallel()`, table tests, `cmp.Diff(want, got)`. Don't assert on library error text.
- Break the code a test guards and confirm the test fails (the broken code must compile).

**Style** ([code-style.md](docs/standards/code-style.md))
- Return early; no named results; `ctx` first; no package-level mutable state, no `init`.
- Accept interfaces where you use them, return concrete types.
- Comments say why, never what or when; no history in comments.
- No `util`/`common`/`helpers` packages.
