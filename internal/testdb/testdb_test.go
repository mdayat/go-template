package testdb

import (
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

func TestLocalURLRefusesOtherHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		url    string
		wantOK bool
	}{
		{"localhost", "postgres://u:p@localhost:5432/postgres?sslmode=disable", true},
		{"IPv4 loopback", "postgres://u:p@127.0.0.1:5432/postgres", true},
		{"IPv6 loopback", "postgresql://u:p@[::1]:5432/postgres", true},
		{"remote host", "postgres://u:p@db.example.com:5432/postgres", false},
		{"host parameter overrides the URL's host", "postgres://u:p@localhost:5432/postgres?host=db.example.com", false},
		{"second host", "postgres://u:p@localhost:5432,db.example.com:5432/postgres", false},
		{"no host", "postgres:///postgres", false},
		{"key=value form", "host=localhost dbname=postgres", false},
		{"another database", "mysql://u:p@localhost:3306/app", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := localURL(tt.url)
			if gotOK := err == nil; gotOK != tt.wantOK {
				t.Errorf("localURL(%q) error = %v, want ok = %v", tt.url, err, tt.wantOK)
			}
		})
	}
}

//nolint:tparallel // "copies" must finish, cleanups included, before the checks below
func TestNewGivesEachTestItsOwnMigratedDatabase(t *testing.T) {
	t.Parallel()
	observer := New(t)

	names := make(chan string, 2)
	t.Run("copies", func(t *testing.T) {
		for _, sub := range []string{"first", "second"} {
			t.Run(sub, func(t *testing.T) {
				t.Parallel()
				pool := New(t)
				// The same title in both: it only fits if the databases are separate.
				if _, err := pool.Exec(t.Context(), "INSERT INTO tasks (title) VALUES ('same title')"); err != nil {
					t.Fatalf("insert into the migrated tasks table: %v", err)
				}
				var name string
				if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
					t.Fatal(err)
				}
				names <- name
			})
		}
	})
	close(names)

	var seen []string
	for name := range names {
		seen = append(seen, name)
		var exists bool
		err := observer.QueryRow(t.Context(),
			"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("database %s still exists after its test passed", name)
		}
	}
	if len(seen) != 2 || seen[0] == seen[1] {
		t.Errorf("databases = %v, want two different ones", seen)
	}
}

// failedT reports failure, so New's cleanup takes the failed-test path while the real test passes.
type failedT struct{ testing.TB }

func (failedT) Failed() bool { return true }

//nolint:tparallel // "failing" must finish, cleanups included, before the checks below
func TestNewKeepsAFailedTestsDatabase(t *testing.T) {
	t.Parallel()

	var name string
	t.Run("failing", func(t *testing.T) {
		pool := New(failedT{t})
		if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
			t.Fatal(err)
		}
	})

	var exists bool
	err := shared.srv.admin.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("database %s was dropped, want it kept after a failed test", name)
	}
	if _, err := shared.srv.admin.Exec(t.Context(), "DROP DATABASE "+ident(name)); err != nil {
		t.Errorf("drop kept database: %v", err)
	}
}
