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
  tasks_title_key ON tasks (title) WHERE deleted_at IS NULL`, not a `UNIQUE` constraint, or a
  deleted row's title can never be used again. Name the index as the constraint would be named:
  the service matches the duplicate by that name. Indexes for lists get the same `WHERE`. An
  `INSERT ... ON CONFLICT (title)` must repeat it (`ON CONFLICT (title) WHERE deleted_at IS NULL`),
  or Postgres finds no index to match and fails.
- **Restoring can break a unique rule** (a live row took the value meanwhile): it's a `Conflict`,
  and the row stays deleted. A restore is a change, so it sets `updated_at`.
- **`ON DELETE CASCADE` doesn't fire**: nothing is deleted. A child that must go with its parent is
  marked in the same transaction, or its queries join the parent and skip deleted parents.
- **Deleted rows are removed for good after a while** (30 days is common; for tasks,
  `task.Service.PurgeDeleted` does it): tables keep growing otherwise, and personal data shouldn't
  stay once a user asked to delete it (privacy laws such as the GDPR give about a month). Records
  the law says to keep (payments, invoices) aren't removed on that timetable.
- **Adding `deleted_at` to a table in use:** while the deploy runs, the old app still shows rows
  the new one marks deleted, for those minutes. On a big table, build the partial unique index
  `CONCURRENTLY` under a new name in a migration of its own
  ([above](#migrations-that-are-safe-while-the-old-app-runs)), then drop the old constraint in a
  later one, so values stay checked and writes don't wait. Until then a duplicate may be reported
  under either name: the service matches both.

Soft delete keeps only a row's last state. To know who changed what and when, keep a separate
history (audit) table.

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
