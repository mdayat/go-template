# Where code goes

Go's own guide ([Organizing a Go module](https://go.dev/doc/modules/layout)) asks only two things
of a server: each program in `cmd/<name>`, and the code behind them in `internal/`, which no other
module can import. Inside `internal/`, these rules are ours:

- **A package per feature, named for what it gives**: `internal/task` gives `task.Service` and
  `task.Task`. Never a package by kind (`models`, `utils`, `common`): it grows into everything, and
  every feature then depends on all of it. A package of what one kind of entry point shares (what
  every HTTP handler needs, say) is the exception, kept to just that.
- **Rules live in the feature's package**, and every entry point calls them: what a valid task is,
  who may see it. An entry point (an HTTP handler, a job handler) only reads its input, calls the
  service and writes the result, so a rule is written, and tested, once.
- **Entry points depend on features, never the reverse.** A service doesn't import chi, net/http
  or the job queue: the same service serves every entry point. Go refuses import loops, so the
  direction can't flip by accident.
- **Each entry point has a file per feature**, in the entry point's package, not in the feature's:
  removing a whole entry point (`make remove`) then takes its files and leaves the features whole.
- **A library sits behind our own package** when it may be replaced (`internal/json` for JSON,
  `internal/postgres` for database errors); lint refuses the library anywhere else.
- **Shared code isn't put in one feature's files**: removing that feature would break the rest. Test
  helpers go in a package's `main_test.go`; other shared code in a package of its own
  (`internal/testdb`, which every database test uses).

## A feature, file by file

The tasks feature is the example to copy. A new feature, `invoice`:

| What | Where |
|---|---|
| table and its changes | `db/schema.sql`, then `make migrate-diff name=<what>` |
| queries | `db/queries/invoices.sql`, then `make generate` (`internal/db`, never edited) |
| rules, the service | `internal/invoice/service.go`, tests beside it on a real database |
| wiring the service | a field in each entry point's `Services` and a line in its `NewServices` |

<!-- remove:api -->
In the HTTP API:

| What | Where |
|---|---|
| mobile routes | `internal/api/mobile/invoices.go`, mounted in `NewRouter` (`internal/api/router.go`) |
| web routes | `internal/api/web/invoices.go`, mounted the same way |
| admin routes, if any | `internal/api/admin/invoices.go`, mounted under `/admin` |
| endpoint tests | `internal/api/invoices_test.go`, through the real router (`testAPI`), a subtest per client |

<!-- remove:end -->
<!-- remove:worker -->
In the worker:

| What | Where |
|---|---|
| background jobs: type, payload, limits | `internal/jobs/invoice.go` |
| job handlers and timetables | `internal/worker/invoice.go`, mounted in `NewMux`, listed in `Schedules` |
| job tests | `internal/worker/invoice_test.go`, through the real queue |

<!-- remove:end -->
A data fix or backfill for the feature, run once by hand, is a command of `cmd/oneoff`:
`internal/oneoff/<name>.go` with its test, listed in `Commands()` ([commands.md](commands.md)).

A feature only one entry point uses has only its rows.
<!-- remove:api -->
A route only one app needs is only in that app's package.

## The HTTP API

`internal/api` holds what every request goes through (sign-in, rate limits, logging, telemetry,
the time limit) and `NewRouter`, which mounts each client's routes under its own prefix:
`internal/api/mobile` at `/mobile/v1`, `internal/api/web` at `/web`, `internal/api/admin` at
`/admin` ([http-api.md](http-api.md#clients-paths-and-versions) says why, and what each may
change). What their handlers share is `internal/api/handler`: it can't be in `internal/api` itself,
which imports the client packages to mount them. Its tests are in `internal/api`
(`decode_test.go`, `problem_test.go`), through a router with the shared middleware.

A client's package keeps a file per feature while that's enough. One that outgrows it (five or
more files for one feature) becomes a package of its own under the client
(`internal/api/web/invoice`), mounted the same way.
<!-- remove:end -->
