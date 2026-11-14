// Package postgres recognises the database errors services act on. Services use it instead of
// importing the driver (pgx), so moving to another driver or a new major version changes this
// package and the generated queries in internal/db, not every service.
package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

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
