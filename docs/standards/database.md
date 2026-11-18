# Database

Postgres 17, reached through [pgx](https://github.com/jackc/pgx). SQL is written by hand and
turned into typed Go by [sqlc](https://sqlc.dev); migrations are written by
[Atlas](https://atlasgo.io) (Community edition) from the schema.

## Where things live

| File | What |
|---|---|
| `db/schema.sql` | the schema as it should be now; the one you edit |
| `db/migrations/` | generated SQL steps from the old schema to the new, plus `atlas.sum` |
| `db/queries/*.sql` | every query, one file per table or feature |
| `internal/db/` | generated Go: never edit it, run `make generate` |
| `internal/postgres/` | what services use instead of importing pgx: recognising errors (`IsNotFound`, `IsUniqueViolation`) |

## Changing the schema

1. Edit `db/schema.sql` and the queries in `db/queries/`.
2. `make migrate-diff name=add_due_date` writes the migration. Read it before committing: it's
   what runs on production.
3. `make generate` updates `internal/db`; `make migrate-apply` applies the migration locally.
4. `make check-generated` (CI runs it) fails if any of these is out of date.

Never edit a migration that's been merged: it may already have run somewhere. `atlas.sum` makes
Atlas refuse an edited file. Fix a mistake with a new migration. There are no "down" migrations;
undoing a change is a new migration too.

## Migrations that are safe while the old app runs

During a deploy, the old app keeps serving requests against the new schema for a while. So each
migration must work with the code before it **and** after it. Atlas Community doesn't check this
(its migration linter is paid), so review does:

- A new `NOT NULL` column needs a `DEFAULT`, or the old app's inserts fail.
- Don't rename or drop a column the running app uses. Do it in steps over separate deploys: add the
  new column, deploy code that writes both and reads the new one, copy the old data, then drop the
  old column.
- An index on a big table: `CREATE INDEX CONCURRENTLY`, so writes don't wait for it. It can't run
  inside a transaction, so its migration file starts with `-- atlas:txmode none` and holds only that
  statement.
- A new foreign key or `CHECK` on a big table: add it `NOT VALID`, then `VALIDATE CONSTRAINT` in a
  later migration, so the table isn't locked while every row is checked.
- A new column the new code fills (an owner, say) is empty on every row the old app writes during
  the deploy, and only the old app knew what belonged there. If those rows matter, first deploy
  code that writes the value into a column added earlier, then the code that relies on it.

The template's own migrations build indexes and add keys the plain way: its table is small. Don't
copy that to a big table; follow the steps above.

## Queries

- One SQL statement per sqlc query; name it `VerbNoun` (`GetTask`, `ListTasksAfter`).
- `sqlc.yaml` maps `uuid` to `uuid.UUID` and `timestamptz` to `time.Time` (pointers when the
  column is nullable), and nullable arguments to pointers, so services see plain Go types. Other
  types (`date`, `numeric`, `interval`) come out as pgx's `pgtype` types, which
  services can't import (lint: depguard): add an override for the type in `sqlc.yaml`, as for
  `timestamptz`, or a conversion in `internal/postgres`.
- Every list query has a `LIMIT` and an index for its `ORDER BY`, so a page stays fast however many
  rows there are.
- Postgres `text` can't hold the NUL character (JSON `"\u0000"`), and an insert with one fails as
  an unexpected error. Reject it in the service with the field's other rules (`cleanTitle`).
- Rows owned by a user (`owner_id`): a query on one row by ID takes the owner and filters
  `(sqlc.narg('owner_id')::uuid IS NULL OR owner_id = sqlc.narg('owner_id'))`, NULL meaning an
  admin's (everyone's). A list doesn't: give the owner's list and the admin's list a query each,
  each with its own index, or Postgres may settle on one plan for both and stop using the owner
  index (`ListOwnerTasks`, `ListTasks`). Unique rules include the owner, so one user's values
  neither block another's nor show through a duplicate error.
- Business rules that must hold even when two requests race (unique titles, positive balances)
  are constraints in the schema, not a "check, then write" in Go. Name every constraint: the service
  matches it by name ([errors.md](errors.md#turning-driver-errors-into-kinds)).
- After a loop over `rows.Next()`, check `rows.Err()` (lint: rowserrcheck); sqlc's generated code
  does.

## Soft delete

Deleting a row only marks it: `deleted_at` is set to the time of the delete, and NULL means the
row is live. The row keeps its history and links, and a delete can be undone. The tasks example
does this: `task.Service.Delete` marks the task, `Restore` clears the mark.

A deleted row is still in the table, so every rule below exists to keep it out of sight:

- **Every query skips deleted rows** (`AND deleted_at IS NULL`), including updates and deletes.
  `make lint` runs `sqlc vet`, which refuses a query on a soft-deleted table with no `deleted_at`
  condition (the rule is in `sqlc.yaml`: add each new soft-deleted table to its list). A query
  that must see deleted rows too ends its name in `IncludingDeleted`, so its callers see it: an
  "only once" check (a refunded payment still counts), a lookup by an outside service's ID (a
  repeated callback must find the row).
- **A join needs the condition on every soft-deleted table in it**, in its `JOIN ... ON` or
  `WHERE`: a live row can point at a deleted parent. `sqlc vet` sees one condition and is satisfied,
  so review checks the rest.
- **Unique rules cover live rows only**: a partial unique index, `CREATE UNIQUE INDEX
  tasks_title_key ON tasks (owner_id, title) NULLS NOT DISTINCT WHERE deleted_at IS NULL`, not a
  `UNIQUE` constraint, or a deleted row's title can never be used again. Name the index as the
  constraint would be named: the service matches the duplicate by that name. Indexes for lists get
  the same `WHERE`. An `INSERT ... ON CONFLICT (owner_id, title)` must repeat it (`ON CONFLICT
  (owner_id, title) WHERE deleted_at IS NULL`), or Postgres finds no index to match and fails.
- **Restoring can break a unique rule** (a live row took the value meanwhile): it's a `Conflict`,
  and the row stays deleted. A restore is a change, so it sets `updated_at`.
- **`ON DELETE CASCADE` doesn't fire**: nothing is deleted. A child that must go with its parent is
  marked in the same transaction, or its queries join the parent and skip deleted parents.
- **Deleted rows are removed for good after a while** (30 days is common; for tasks,
  `task.Service.PurgeDeleted` does it, and something must call it on a timetable, or deleted rows
  stay forever): tables keep growing otherwise, and personal data shouldn't
  stay once a user asked to delete it (privacy laws such as the GDPR give about a month). Records
  the law says to keep (payments, invoices) aren't removed on that timetable.
- **Adding `deleted_at` to a table in use:** while the deploy runs, the old app still shows rows
  the new one marks deleted, for those minutes. On a big table, build the partial unique index
  `CONCURRENTLY` under a new name in a migration of its own
  ([above](#migrations-that-are-safe-while-the-old-app-runs)), then drop the old constraint in a
  later one, so values stay checked and writes don't wait. Until then a duplicate may be reported
  under either name: make the service match both until the old one is dropped.

Soft delete keeps only a row's last state. To know who changed what and when, keep a separate
history (audit) table.

## Connections

Each program keeps a pool of database connections (`startup.ConnectDB`) and opens at most
`DB_MAX_CONNS`; a query waits for a free one. Each program sets its own default in `settings.go`:

<!-- remove:api -->
- **API**: 10.
<!-- remove:end -->
<!-- remove:worker -->
- **worker**: the number of jobs it runs at once (`WORKER_QUEUES`, [worker.md](worker.md#queues)),
  as each job uses one connection at a time; it refuses to start with fewer.
<!-- remove:end -->
- **oneoff**: 2.

Postgres has its own limit on connections (`max_connections`, often 100; less on small managed
databases). Every instance (one running copy of a program: 3 API containers are 3 instances) has a
pool, so add up `DB_MAX_CONNS` times the instances, for every program, and keep the total below that
limit, with room for migrations and your own `psql`. During a deploy old and new instances run side
by side, so count those too. Postgres refuses connections past its limit, and requests then fail.

Set it only through `DB_MAX_CONNS`: a program refuses to start if `DATABASE_URL` sets
`pool_max_conns`. A pool waiting for connections shows in the `pgxpool` metrics
([metrics.md](metrics.md)).

## Time limits

When a request or job runs out of time, its context ends and pgx asks Postgres to stop the query.
That request can't be sent when the program is killed outright, or arrive when the network drops,
and the query then runs on, holding its locks. So every connection `startup.ConnectDB` opens also
has Postgres's own limits, set by two settings:

| Setting | Postgres's name | What it stops |
|---|---|---|
| `DB_STATEMENT_TIMEOUT` | `statement_timeout` | one statement running longer; it fails with `57014`, an unexpected error to the caller |
| `DB_IDLE_IN_TRANSACTION_TIMEOUT` | `idle_in_transaction_session_timeout` | a transaction open with nothing running for longer; Postgres ends the connection, which rolls it back |

Each program sets its own defaults in `settings.go`, to fit its work:

<!-- remove:api -->
- **API**: 10 s and 10 s. Above the 8 s a request may take, so a slow request gets the API's own
  reply ([http-api.md](http-api.md#server-settings)), and its query is cancelled with it.
<!-- remove:end -->
<!-- remove:worker -->
- **worker**: 5 min, at least the longest job's time limit, so no job's statement is cut before
  its job is; and 1 min.
<!-- remove:end -->
- **oneoff**: 1 min and 1 min ([why](commands.md)).

Every program reads the same two names, so each needs its own environment: one file shared by all
would give every program one value (a short limit meant for one would cut another's long
statements).

A value is a duration (`500ms`, `10s`, `5m`) in whole milliseconds, from 1 ms to 596 hours: 0
is no limit in Postgres. Set them only through these settings: a program refuses to start if
`DATABASE_URL` or `PGOPTIONS` sets either, as one of the two values would be ignored.

The values a program sends when it connects beat any set on the database or its user (`ALTER
ROLE ... SET statement_timeout`): set them through these settings, not there. Behind a pooler
(PgBouncer, RDS Proxy, Supavisor), check it passes them on. PgBouncer refuses them at connect time
unless listed in its `ignore_startup_parameters`, and then drops them: the settings have no effect,
so set the limits on the database user instead (`ALTER ROLE api_user SET statement_timeout =
'10s'`), a user per program, so each keeps its own values, and another for migrations, which
mustn't get them.

One transaction that needs longer raises the limit for itself alone, as its first statement:
`SET LOCAL statement_timeout = '60s'` (or `idle_in_transaction_session_timeout`). It ends with the
transaction; a plain `SET` would stay on the pooled connection for whoever uses it next.

Migrations and your own `psql` sessions don't go through `ConnectDB`, so they have no limit. A
migration may rightly run long (`CREATE INDEX CONCURRENTLY`). In a `psql` session on a shared
database, run `SET statement_timeout = '30s';` first: closing the terminal doesn't stop a query
already running.

## Transactions and retries

A service that needs several statements to succeed or fail together runs them in one
transaction, with sqlc's queries bound to it:

```go
// inTx runs fn in a transaction and retries it, at most 3 times in all, only when Postgres says
// trying again can succeed: 40001 (serialization failure) and 40P01 (deadlock).
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(q *db.Queries) error) error {
	for attempt := 1; ; attempt++ {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return fn(db.New(tx))
		})
		var pgErr *pgconn.PgError
		retry := errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
		if !retry || attempt == 3 {
			return err
		}
	}
}
```

- Retry **only** these two codes. Any other error (a broken constraint, a lost connection, a bug)
  fails the same way again, or worse, half-repeats work.
- `fn` may run more than once, so it does nothing outside the database: no emails, no calls to
  other services, no counters in memory. Do those after the transaction commits.
- Postgres's default isolation (read committed) rarely raises 40001; use
  `pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, ...)` when a transaction
  reads data and decides based on it, and then the retry matters.

The template ships no such helper because its example needs only one statement per request; add it
to `internal/postgres` with the first service that needs a transaction. Services don't import pgx
(lint: depguard), so that package also gives them the pool's type, `type Pool = pgxpool.Pool`.
