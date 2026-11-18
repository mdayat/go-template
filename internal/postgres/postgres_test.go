package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// Errors come from a real Postgres, on a table of the test's own, so the test doesn't depend on
// any feature's schema.
func TestRecognisesDatabaseErrors(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	ctx := t.Context()
	_, err := pool.Exec(ctx, `CREATE TABLE things (
		name text CONSTRAINT things_name_key UNIQUE,
		size int CONSTRAINT things_size_positive CHECK (size > 0)
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO things (name, size) VALUES ('taken', 1)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, duplicate := pool.Exec(ctx, "INSERT INTO things (name, size) VALUES ('taken', 1)")
	_, broken := pool.Exec(ctx, "INSERT INTO things (name, size) VALUES ('new', 0)")
	var name string
	noRows := pool.QueryRow(ctx, "SELECT name FROM things WHERE name = 'missing'").Scan(&name)

	tests := []struct {
		name         string
		err          error
		wantNotFound bool
		wantUnique   bool // a unique violation of things_name_key
	}{
		{"no row", noRows, true, false},
		{"no row, wrapped", fmt.Errorf("get thing: %w", noRows), true, false},
		{"duplicate", duplicate, false, true},
		{"duplicate, wrapped", fmt.Errorf("create thing: %w", duplicate), false, true},
		{"another constraint", broken, false, false},
		{"not a database error", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, tt := range tests {
		if got := IsNotFound(tt.err); got != tt.wantNotFound {
			t.Errorf("%s: IsNotFound(%v) = %v, want %v", tt.name, tt.err, got, tt.wantNotFound)
		}
		if got := IsUniqueViolation(tt.err, "things_name_key"); got != tt.wantUnique {
			t.Errorf("%s: IsUniqueViolation(%v, things_name_key) = %v, want %v", tt.name, tt.err, got, tt.wantUnique)
		}
	}
	if IsUniqueViolation(duplicate, "other_key") {
		t.Errorf("IsUniqueViolation(%v, other_key) = true, want false: another constraint's name", duplicate)
	}
}

// InTx saves fn's statements together or not at all, and tries again only when Postgres says trying
// again can succeed.
func TestInTx(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	ctx := t.Context()
	// Through a generated query, as services write; users is a table every project keeps.
	n := 0
	insert := func(q *db.Queries) error {
		n++
		_, err := q.CreateUser(ctx, fmt.Sprintf("intx-%d", n))
		return err
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	failed := errors.New("second step failed")
	err := InTx(ctx, pool, func(q *db.Queries) error {
		if err := insert(q); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) || count() != 0 {
		t.Errorf("a failed transaction = %v with %d rows saved, want its error and 0", err, count())
	}

	for _, tt := range []struct {
		code      string
		wantTries int
	}{
		{"40001", 3}, // serialization failure
		{"40P01", 3}, // deadlock
		{"23505", 1}, // unique violation: the same again
	} {
		tries := 0
		err := InTx(ctx, pool, func(q *db.Queries) error {
			tries++
			if err := insert(q); err != nil {
				return err
			}
			return fmt.Errorf("step: %w", &pgconn.PgError{Code: tt.code})
		})
		if err == nil || tries != tt.wantTries {
			t.Errorf("code %s: tries = %d, error %v; want %d and the error", tt.code, tries, err, tt.wantTries)
		}
	}
	if n := count(); n != 0 {
		t.Errorf("rows after failed transactions = %d, want 0", n)
	}

	tries := 0
	err = InTx(ctx, pool, func(q *db.Queries) error {
		tries++
		if err := insert(q); err != nil {
			return err
		}
		if tries == 1 {
			return &pgconn.PgError{Code: "40001"}
		}
		return nil
	})
	if err != nil || tries != 2 || count() != 1 {
		t.Errorf("a transaction that succeeds on its second try = %v after %d tries, %d rows; want nil, 2, 1 (the first undone)", err, tries, count())
	}
}

// A panic in fn rolls the transaction back, and goes on up.
func TestInTxRollsBackOnPanic(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("InTx swallowed fn's panic")
			}
		}()
		_ = InTx(t.Context(), pool, func(q *db.Queries) error {
			if _, err := q.CreateUser(t.Context(), "panicked"); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&n); err != nil || n != 0 {
		t.Errorf("users after a panic = %d (%v), want 0", n, err)
	}
}
