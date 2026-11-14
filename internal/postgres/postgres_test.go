package postgres

import (
	"errors"
	"fmt"
	"testing"

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
