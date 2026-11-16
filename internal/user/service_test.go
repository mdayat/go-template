package user

import (
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func testService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	return NewService(db.New(pool)), pool
}

// usersInDB returns every user's subject and admin flag, by subject.
func usersInDB(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT auth_subject, is_admin FROM users")
	if err != nil {
		t.Fatalf("read users: %v", err)
	}
	type userRow struct {
		Subject string
		Admin   bool
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[userRow])
	if err != nil {
		t.Fatalf("read users: %v", err)
	}
	got := map[string]bool{}
	for _, u := range list {
		got[u.Subject] = u.Admin
	}
	return got
}

// The first sign-in records the user; later ones find the same one.
func TestSignIn(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)

	first, err := s.SignIn(t.Context(), "alice")
	if err != nil {
		t.Fatalf("first SignIn() error = %v", err)
	}
	again, err := s.SignIn(t.Context(), "alice")
	if err != nil {
		t.Fatalf("second SignIn() error = %v", err)
	}
	bob, err := s.SignIn(t.Context(), "bob")
	if err != nil {
		t.Fatalf("SignIn(bob) error = %v", err)
	}

	if first != again || first.IsAdmin {
		t.Errorf("SignIn(alice) = %+v then %+v, want the same user, not an admin", first, again)
	}
	if bob.ID == first.ID {
		t.Errorf("SignIn(bob).ID = SignIn(alice).ID = %s, want two users", bob.ID)
	}
	if diff := cmp.Diff(map[string]bool{"alice": false, "bob": false}, usersInDB(t, pool)); diff != "" {
		t.Errorf("users in database mismatch (-want +got):\n%s", diff)
	}
}

// Someone's first two requests can arrive at once: they get one user, not two rows or an error.
func TestSignInAtOnce(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)

	const requests = 20
	got := make([]User, requests)
	errs := make([]error, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() { got[i], errs[i] = s.SignIn(t.Context(), "alice") })
	}
	wg.Wait()

	for i := range requests {
		if errs[i] != nil || got[i] != got[0] {
			t.Errorf("request %d: SignIn() = %+v, %v; want %+v", i, got[i], errs[i], got[0])
		}
	}
	if diff := cmp.Diff(map[string]bool{"alice": false}, usersInDB(t, pool)); diff != "" {
		t.Errorf("users in database mismatch (-want +got):\n%s", diff)
	}
}

// The admin flag comes from our row, set by hand, never from the provider.
func TestSignInReadsTheAdminFlag(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)
	if _, err := pool.Exec(t.Context(), "INSERT INTO users (auth_subject, is_admin) VALUES ('root', true)"); err != nil {
		t.Fatal(err)
	}

	got, err := s.SignIn(t.Context(), "root")

	if err != nil || !got.IsAdmin {
		t.Errorf("SignIn(root) = %+v, %v; want an admin", got, err)
	}
}

// A subject the users table can't hold is refused by the table's own rules (and Postgres, for NUL):
// an error, never a row.
func TestSignInRefusesSubjectsTheTableCantHold(t *testing.T) {
	t.Parallel()
	s, pool := testService(t)

	for _, subject := range []string{"", strings.Repeat("é", 256), "a\x00b"} {
		if got, err := s.SignIn(t.Context(), subject); err == nil {
			t.Errorf("SignIn(%q) = %+v, want an error", subject, got)
		}
	}
	if longest, err := s.SignIn(t.Context(), strings.Repeat("é", 255)); err != nil {
		t.Errorf("SignIn(255 characters) = %+v, %v; want a user", longest, err)
	}
	if got := usersInDB(t, pool); len(got) != 1 {
		t.Errorf("users in database = %d, want only the longest valid one", len(got))
	}
}
