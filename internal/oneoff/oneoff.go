// Package oneoff runs one-off commands: data fixes and backfills run by hand, through cmd/oneoff.
// A run is a dry run unless --apply is given: the command does all its work, in transactions that
// are then undone, so its report is what --apply would do. See docs/standards/commands.md.
package oneoff

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/db"
)

// Command is one command of cmd/oneoff.
type Command struct {
	Name    string // kebab-case, as typed: trim-task-titles
	Summary string // one line, for the list of commands
	// New registers the command's own flags on fs (besides --apply) and returns its work, which
	// reads them once they're parsed.
	New func(fs *flag.FlagSet) Work
}

// Work is a command's work. It writes only through r.Batch, so that a dry run saves nothing.
type Work func(ctx context.Context, r *Run) error

// Run is one run of a command.
type Run struct {
	// ID is new for each run, on every line it logs, to find them all.
	ID string
	// Apply says whether writes are saved. A command checks it before anything a rollback can't
	// undo (an email, a call to another service): in a dry run it only reports what it would do.
	Apply bool
	// Log carries the run's ID, the command's name and Apply on every line.
	Log zerolog.Logger

	pool   *pgxpool.Pool
	out    io.Writer
	counts []count
}

type count struct {
	name string
	n    int
}

// Batch runs fn in a transaction, with queries bound to it: saved if r.Apply, else undone, as when
// fn fails. Build services from q, never from the pool: a write through the pool isn't undone.
// Keep a batch short (a few hundred rows): it holds its locks until it ends, and Postgres ends a
// transaction left idle too long (DB_IDLE_IN_TRANSACTION_TIMEOUT).
func (r *Run) Batch(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin a batch: %w", err)
	}
	// Undoes the batch on every path but a commit, after which it does nothing.
	defer func() {
		endCtx, cancel := endContext(ctx)
		defer cancel()
		_ = tx.Rollback(endCtx)
	}()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	if !r.Apply {
		// fn went on after a failed statement or a lost connection: --apply's commit would fail, so
		// the dry run does too.
		if tx.Conn().IsClosed() || tx.Conn().PgConn().TxStatus() == 'E' {
			return errors.New("a statement in the batch failed, so --apply would save nothing")
		}
		return nil
	}
	endCtx, cancel := endContext(ctx)
	defer cancel()
	if err := tx.Commit(endCtx); err != nil {
		// The connection may have broken after Postgres saved the batch, before it said so.
		return fmt.Errorf("%w: %w", errCommitUnknown, err)
	}
	return nil
}

// errCommitUnknown is a commit with no answer: the batch may or may not have been saved.
var errCommitUnknown = errors.New("committing a batch failed, so it may or may not have been saved")

// endContext is for ending a batch: not cancelled with ctx, so a Ctrl-C during the commit can't
// leave it unknown whether the batch was saved (the next batch's Begin sees the Ctrl-C), and a
// rollback still goes through after one. Its own limit stops a dead connection hanging the run.
func endContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// Count adds n to the count called name, for the report and the summary line. Counts are listed in
// the order they're first added.
func (r *Run) Count(name string, n int) {
	for i := range r.counts {
		if r.counts[i].name == name {
			r.counts[i].n += n
			return
		}
	}
	r.counts = append(r.counts, count{name: name, n: n})
}

// Reportf writes a line of the report the person running the command reads, besides the log. Never
// personal data (titles, emails, names): IDs and counts.
func (r *Run) Reportf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.out, format+"\n", args...)
}

// ErrUsage is returned by Parse for a wrong command line, already written to its out.
var ErrUsage = errors.New("usage")

// ErrFailed is returned by Invocation.Run when the command failed or was interrupted. Its summary
// line has been logged already, with the error.
var ErrFailed = errors.New("the command failed")

// Invocation is a command and its parsed flags, ready to run.
type Invocation struct {
	command Command
	work    Work
	apply   bool
}

// Parse finds the command args name and parses its flags. With no command, or -h, it writes the
// list of commands to out and returns flag.ErrHelp; with -h after a command, that command's flags.
func Parse(args []string, commands []Command, out io.Writer) (Invocation, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		writeCommands(out, commands)
		return Invocation{}, flag.ErrHelp
	}
	i := slices.IndexFunc(commands, func(c Command) bool { return c.Name == args[0] })
	if i < 0 {
		names := make([]string, 0, len(commands))
		for _, c := range commands {
			names = append(names, c.Name)
		}
		return Invocation{}, fmt.Errorf("no command %q; the commands are: %s", args[0], strings.Join(names, ", "))
	}
	c := commands[i]
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(out)
	apply := fs.Bool("apply", false, "save the changes; without it, a dry run that saves nothing")
	work := c.New(fs)
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Invocation{}, err
		}
		// The flag package has written the error and the command's flags.
		return Invocation{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return Invocation{}, fmt.Errorf("%s: unexpected %q: flags only (-h lists them)", c.Name, fs.Arg(0))
	}
	return Invocation{command: c, work: work, apply: *apply}, nil
}

func writeCommands(out io.Writer, commands []Command) {
	_, _ = fmt.Fprintln(out, "usage: oneoff <command> [flags] [--apply]   (a dry run without --apply)")
	_, _ = fmt.Fprintln(out, "commands:")
	for _, c := range commands {
		_, _ = fmt.Fprintf(out, "  %-24s %s\n", c.Name, c.Summary)
	}
}

// Run runs the command against pool, writing its report to out, and logs one summary line,
// "command completed": its result (done, failed, interrupted), counts and time. It returns
// ErrFailed if the command failed; a panic in it counts as failing, with its stack on the line.
func (inv Invocation) Run(ctx context.Context, pool *pgxpool.Pool, logger zerolog.Logger, out io.Writer) error {
	r := &Run{ID: uuid.NewString(), Apply: inv.apply, pool: pool, out: out}
	r.Log = logger.With().Str("run_id", r.ID).Str("command", inv.command.Name).Bool("apply", r.Apply).Logger()
	if !r.Apply {
		r.Reportf("dry run: nothing will be saved (add --apply to save)")
	}

	start := time.Now()
	var wr workRun
	// What services log through zerolog.Ctx carries the run's fields too.
	wr.run(r.Log.WithContext(ctx), inv.work, r)
	err, stack := wr.err, wr.stack

	for _, c := range r.counts {
		r.Reportf("%s: %d", c.name, c.n)
	}
	counts := zerolog.Dict()
	for _, c := range r.counts {
		counts.Int(c.name, c.n)
	}
	result, level := "done", zerolog.InfoLevel
	switch {
	case err != nil && ctx.Err() != nil:
		result, level = "interrupted", zerolog.WarnLevel
	case err != nil:
		result, level = "failed", zerolog.ErrorLevel
	}
	line := r.Log.WithLevel(level).
		Str("result", result).
		Dict("counts", counts).
		Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond))
	if err != nil {
		errFields := zerolog.Dict().Str("message", err.Error())
		if stack != "" {
			errFields.Str("stack_trace", stack)
		}
		line.Dict("error", errFields)
	}
	line.Msg("command completed")

	switch {
	case errors.Is(err, errCommitUnknown):
		r.Reportf("%s (run %s): %v; the counts leave out that batch. Run it again: it skips what's fixed", result, r.ID, err)
		return ErrFailed
	case err != nil:
		r.Reportf("%s (run %s): %v; a batch that didn't finish was undone", result, r.ID, err)
		return ErrFailed
	case r.Apply:
		r.Reportf("done (run %s): saved", r.ID)
	default:
		r.Reportf("done (run %s): dry run, nothing saved", r.ID)
	}
	return nil
}

// workRun is how a command's work ended: its error, and the stack if it panicked.
type workRun struct {
	err   error
	stack string
}

// run runs work and turns a panic into an error with its stack, so it's on the summary line.
func (wr *workRun) run(ctx context.Context, work Work, r *Run) {
	defer func() {
		if p := recover(); p != nil {
			wr.err = fmt.Errorf("panic: %v", p)
			wr.stack = string(debug.Stack())
		}
	}()
	wr.err = work(ctx, r)
}
