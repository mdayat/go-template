// Package postgres is what services need of the database beyond their queries: recognising the
// errors they act on, and running statements in one transaction. Services use it instead of
// importing the driver (pgx), so moving to another driver or a new major version changes this
// package and the generated queries in internal/db, not every service.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mdayat/go-template/internal/db"
)

// Pool is the database's connection pool, for a service that runs transactions (InTx) to hold
// without importing the driver.
type Pool = pgxpool.Pool

// Postgres's error code for a unique violation.
const codeUniqueViolation = "23505"

// IsNotFound reports whether err says a query for one row found none.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// IsUniqueViolation reports whether err is a unique violation of the named constraint. Matching the
// name keeps a unique rule added later from being reported as the wrong conflict.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation && pgErr.ConstraintName == constraint
}

// Postgres's error codes for a transaction that can succeed if tried again from the start.
const (
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
)

// maxTxAttempts is how many times InTx runs a transaction in all.
const maxTxAttempts = 3

// InTx runs fn in a transaction, with queries bound to it: committed if fn returns nil, rolled back
// if it returns an error or panics. It tries again, maxTxAttempts times in all, only when Postgres
// says trying again can succeed: a serialization failure or a deadlock. Any other error (a broken
// constraint, a lost connection, a bug) would fail the same way again, or half-repeat work.
//
// fn may run more than once, so it does nothing outside the database: no emails, no calls to other
// services, no counters in memory. Do those after InTx returns, or write what's to be done to a table
// (an outbox) for something else to act on once it's saved.
func InTx(ctx context.Context, pool *Pool, fn func(q *db.Queries) error) error {
	for attempt := 1; ; attempt++ {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return fn(db.New(tx))
		})
		var pgErr *pgconn.PgError
		retry := errors.As(err, &pgErr) && (pgErr.Code == codeSerializationFailure || pgErr.Code == codeDeadlockDetected)
		if !retry || attempt == maxTxAttempts {
			return err
		}
	}
}
