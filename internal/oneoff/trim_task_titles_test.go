package oneoff

import (
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mdayat/go-template/internal/testdb"
)

func titles(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT title FROM tasks")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}

// The rules are TrimTitles' (internal/task); this checks the command pages through every task,
// saves only with --apply, and counts.
func TestTrimTaskTitles(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	for _, title := range []string{" a", "b ", "c", " c", "   ", "  "} {
		if _, err := pool.Exec(t.Context(), "INSERT INTO tasks (title) VALUES ($1)", title); err != nil {
			t.Fatal(err)
		}
	}
	untouched := titles(t, pool)
	wantCounts := map[string]int{"checked": 6, "trimmed": 2, "left_title_taken": 1, "left_blank": 2, "left_changed": 0}

	// A batch of 2 makes three batches.
	line, _, err := runCommand(t.Context(), t, pool, trimTaskTitles(), "-batch", "2")
	if err != nil {
		t.Fatalf("dry run error = %v", err)
	}
	if !slices.Equal(titles(t, pool), untouched) {
		t.Errorf("titles after a dry run = %q, want them untouched", titles(t, pool))
	}
	if !maps.Equal(line.Counts, wantCounts) {
		t.Errorf("dry run counts = %v, want %v", line.Counts, wantCounts)
	}

	line, _, err = runCommand(t.Context(), t, pool, trimTaskTitles(), "-batch", "2", "--apply")
	if err != nil {
		t.Fatalf("--apply error = %v", err)
	}
	if want := []string{"  ", "   ", " c", "a", "b", "c"}; !slices.Equal(titles(t, pool), want) {
		t.Errorf("titles after --apply = %q, want %q", titles(t, pool), want)
	}
	if !maps.Equal(line.Counts, wantCounts) {
		t.Errorf("--apply counts = %v, want the dry run's %v", line.Counts, wantCounts)
	}

	line, _, _ = runCommand(t.Context(), t, pool, trimTaskTitles(), "--apply")
	if line.Counts["trimmed"] != 0 {
		t.Errorf("a second run trimmed %d, want 0", line.Counts["trimmed"])
	}

	if line, _, err := runCommand(t.Context(), t, pool, trimTaskTitles(), "-batch", "0"); err == nil || line.Result != "failed" {
		t.Errorf("-batch 0 = %v, %s; want it refused", err, line.Result)
	}
}
