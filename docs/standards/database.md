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

## Queries

- One SQL statement per sqlc query; name it `VerbNoun` (`GetTask`, `ListTasksAfter`).
- `sqlc.yaml` maps `uuid` to `uuid.UUID`, `timestamptz` to `time.Time`, and nullable arguments
  to pointers, so services see plain Go types.
- Every list query has a `LIMIT` and an index for its `ORDER BY` ([http-api.md](http-api.md#ids-and-paging)).
- Business rules that must hold even when two requests race (unique titles, positive balances)
  are constraints in the schema, not a "check, then write" in Go. Name every constraint: the service
  matches it by name ([errors.md](errors.md#turning-driver-errors-into-kinds)).
- After a loop over `rows.Next()`, check `rows.Err()` (lint: rowserrcheck); sqlc's generated code
  does.

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
with the first service that needs a transaction.
