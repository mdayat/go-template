package oneoff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// summary is the run's "command completed" line.
type summary struct {
	RunID   string         `json:"run_id"`
	Command string         `json:"command"`
	Apply   bool           `json:"apply"`
	Level   string         `json:"level"`
	Result  string         `json:"result"`
	Counts  map[string]int `json:"counts"`
	Error   struct {
		Message    string `json:"message"`
		StackTrace string `json:"stack_trace"`
	} `json:"error"`
}

// runCommand parses args for the one command c and runs it on pool, returning the summary line,
// the report and Run's error.
func runCommand(ctx context.Context, t *testing.T, pool *pgxpool.Pool, c Command, args ...string) (summary, string, error) {
	t.Helper()
	inv, err := Parse(append([]string{c.Name}, args...), []Command{c}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	var logs, report bytes.Buffer
	err = inv.Run(ctx, pool, zerolog.New(&logs), &report)

	var lines []summary
	for line := range strings.Lines(logs.String()) {
		var s summary
		if jerr := json.Unmarshal([]byte(line), &s); jerr != nil {
			t.Fatalf("log line %q: %v", line, jerr)
		}
		lines = append(lines, s)
	}
	if len(lines) != 1 {
		t.Fatalf("log = %q, want one summary line", logs.String())
	}
	return lines[0], report.String(), err
}

// command returns a command named test whose work is work.
func command(work Work) Command {
	return Command{Name: "test", Summary: "a test", New: func(*flag.FlagSet) Work { return work }}
}

// addUser is a batch that adds a user signed in as subject. Users, not an example feature's table:
// they stay when the example is replaced.
func addUser(ctx context.Context, subject string) func(q *db.Queries) error {
	return func(q *db.Queries) error {
		_, err := q.CreateUser(ctx, subject)
		return err
	}
}

func countUsers(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParse(t *testing.T) {
	t.Parallel()
	var batch *int
	c := Command{Name: "fix-things", Summary: "fixes things", New: func(fs *flag.FlagSet) Work {
		batch = fs.Int("batch", 500, "rows per batch")
		return func(context.Context, *Run) error { return nil }
	}}
	commands := []Command{c}

	inv, err := Parse([]string{"fix-things", "-batch", "7", "--apply"}, commands, &bytes.Buffer{})
	if err != nil || !inv.apply || *batch != 7 {
		t.Errorf("Parse(fix-things -batch 7 --apply) = apply %v, batch %d, error %v; want true, 7, nil", inv.apply, *batch, err)
	}
	inv, err = Parse([]string{"fix-things"}, commands, &bytes.Buffer{})
	if err != nil || inv.apply {
		t.Errorf("Parse(fix-things) = apply %v, error %v; want a dry run", inv.apply, err)
	}

	for _, args := range [][]string{nil, {"-h"}, {"--help"}} {
		var out bytes.Buffer
		_, err := Parse(args, commands, &out)
		if !errors.Is(err, flag.ErrHelp) || !strings.Contains(out.String(), "fix-things") {
			t.Errorf("Parse(%q) = %v, wrote %q; want flag.ErrHelp and the list of commands", args, err, out.String())
		}
	}
	for _, args := range [][]string{{"fix-thing"}, {"fix-things", "now"}, {"fix-things", "-batch", "x"}} {
		if _, err := Parse(args, commands, &bytes.Buffer{}); err == nil || errors.Is(err, flag.ErrHelp) {
			t.Errorf("Parse(%q) error = %v, want one", args, err)
		}
	}
	if _, err := Parse([]string{"fix-thing"}, commands, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "fix-things") {
		t.Errorf("Parse(unknown) error = %v, want it to list the commands", err)
	}
	// The flag package writes a bad flag's error itself: ErrUsage tells main not to again.
	if _, err := Parse([]string{"fix-things", "-nope"}, commands, &bytes.Buffer{}); !errors.Is(err, ErrUsage) {
		t.Errorf("Parse(a bad flag) error = %v, want ErrUsage", err)
	}
}

func TestADryRunSavesNothing(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	work := command(func(ctx context.Context, r *Run) error {
		for _, title := range []string{"one", "two"} {
			if err := r.Batch(ctx, addUser(ctx, title)); err != nil {
				return err
			}
			r.Count("added", 1)
		}
		return nil
	})

	line, report, err := runCommand(t.Context(), t, pool, work)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if n := countUsers(t, pool); n != 0 {
		t.Errorf("users after a dry run = %d, want 0", n)
	}
	if line.Apply || line.Result != "done" || line.Level != "info" || line.Counts["added"] != 2 || line.Command != "test" || line.RunID == "" {
		t.Errorf("summary line = %+v, want a done dry run of test with 2 added and a run ID", line)
	}
	for _, want := range []string{"dry run", "added: 2", "nothing saved", line.RunID} {
		if !strings.Contains(report, want) {
			t.Errorf("report = %q, want it to hold %q", report, want)
		}
	}
}

func TestApplySaves(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	work := command(func(ctx context.Context, r *Run) error {
		return r.Batch(ctx, addUser(ctx, r.ID)) // run twice below: a subject per run
	})

	line, report, err := runCommand(t.Context(), t, pool, work, "--apply")

	if err != nil || countUsers(t, pool) != 1 {
		t.Fatalf("Run(--apply) error = %v, users = %d; want nil, 1", err, countUsers(t, pool))
	}
	if !line.Apply || line.Result != "done" || strings.Contains(report, "dry run") {
		t.Errorf("summary line = %+v, report %q; want a done run that saved", line, report)
	}
	again, _, _ := runCommand(t.Context(), t, pool, work, "--apply")
	if again.RunID == line.RunID {
		t.Errorf("two runs share the ID %s, want one each", line.RunID)
	}
}

// A failure undoes the batch it happened in, but not the ones saved before it.
func TestAFailedBatchIsUndone(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	work := command(func(ctx context.Context, r *Run) error {
		if err := r.Batch(ctx, addUser(ctx, "saved")); err != nil {
			return err
		}
		return r.Batch(ctx, func(q *db.Queries) error {
			if err := addUser(ctx, "undone")(q); err != nil {
				return err
			}
			return errors.New("the second batch broke")
		})
	})

	line, report, err := runCommand(t.Context(), t, pool, work, "--apply")

	if !errors.Is(err, ErrFailed) {
		t.Fatalf("Run() error = %v, want ErrFailed", err)
	}
	if n := countUsers(t, pool); n != 1 {
		t.Errorf("users = %d, want 1: the first batch's", n)
	}
	if line.Result != "failed" || line.Level != "error" || line.Error.Message != "the second batch broke" {
		t.Errorf("summary line = %+v, want a failed run with the error", line)
	}
	if !strings.Contains(report, "the second batch broke") {
		t.Errorf("report = %q, want the error", report)
	}
}

func TestAPanicFails(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	work := command(func(context.Context, *Run) error { panic("boom") })

	line, _, err := runCommand(t.Context(), t, pool, work)

	if !errors.Is(err, ErrFailed) || line.Result != "failed" || line.Error.Message != "panic: boom" || line.Error.StackTrace == "" {
		t.Errorf("Run() = %v, summary line %+v; want ErrFailed, failed, the panic and its stack", err, line)
	}
}

// Ctrl-C (SIGINT) or SIGTERM ends the context: the batch it was in is undone, those before it are kept.
func TestAnInterruptedRunKeepsFinishedBatches(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	work := command(func(ctx context.Context, r *Run) error {
		if err := r.Batch(ctx, addUser(ctx, "saved")); err != nil {
			return err
		}
		r.Count("added", 1)
		return r.Batch(ctx, func(q *db.Queries) error {
			if err := addUser(ctx, "undone")(q); err != nil {
				return err
			}
			cancel()
			return addUser(ctx, "never")(q)
		})
	})

	line, _, err := runCommand(ctx, t, pool, work, "--apply")

	if !errors.Is(err, ErrFailed) {
		t.Fatalf("Run() error = %v, want ErrFailed", err)
	}
	if n := countUsers(t, pool); n != 1 {
		t.Errorf("users = %d, want 1: the finished batch's", n)
	}
	if line.Result != "interrupted" || line.Level != "warn" || line.Counts["added"] != 1 {
		t.Errorf("summary line = %+v, want an interrupted run at warn with 1 added", line)
	}
}

// A batch whose work swallowed a failed statement: --apply's commit would fail, so the dry run
// fails too rather than report a success --apply can't repeat.
func TestADryRunFailsABatchWithAFailedStatement(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	work := command(func(ctx context.Context, r *Run) error {
		return r.Batch(ctx, func(q *db.Queries) error {
			if err := addUser(ctx, "kept")(q); err != nil {
				return err
			}
			_ = addUser(ctx, "")(q) // breaks the subject's length rule; the error is ignored
			return nil
		})
	})

	line, _, err := runCommand(t.Context(), t, pool, work)

	if !errors.Is(err, ErrFailed) || line.Result != "failed" {
		t.Errorf("Run() = %v, result %s; want the dry run failed", err, line.Result)
	}
}

// What a command's services log through zerolog.Ctx carries the run's fields, as its own lines do.
func TestLinesInARunCarryItsID(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	c := command(func(ctx context.Context, r *Run) error {
		zerolog.Ctx(ctx).Info().Msg("from a service")
		r.Log.Info().Msg("from the command")
		return nil
	})
	inv, err := Parse([]string{c.Name}, []Command{c}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer

	if err := inv.Run(t.Context(), pool, zerolog.New(&logs), &bytes.Buffer{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	ids := map[string]bool{}
	for line := range strings.Lines(logs.String()) {
		var s summary
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		ids[s.RunID] = true
	}
	if len(ids) != 1 || ids[""] || strings.Count(logs.String(), "\n") != 3 {
		t.Errorf("log = %q, want three lines with one run ID", logs.String())
	}
}

// A Ctrl-C that lands as a batch's work ends doesn't stop its commit: the batch is saved and
// counted, and the run stops before the next.
func TestACtrlCDuringACommitStillCommits(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	work := command(func(ctx context.Context, r *Run) error {
		for _, subject := range []string{"saved", "never"} {
			err := r.Batch(ctx, func(q *db.Queries) error {
				if err := addUser(ctx, subject)(q); err != nil {
					return err
				}
				cancel()
				return nil
			})
			if err != nil {
				return err
			}
			r.Count("added", 1)
		}
		return nil
	})

	line, _, err := runCommand(ctx, t, pool, work, "--apply")

	if !errors.Is(err, ErrFailed) || line.Result != "interrupted" {
		t.Fatalf("Run() = %v, result %s; want ErrFailed, interrupted", err, line.Result)
	}
	if n := countUsers(t, pool); n != 1 || line.Counts["added"] != 1 {
		t.Errorf("users = %d, counted %d; want the first batch saved and counted", n, line.Counts["added"])
	}
}
