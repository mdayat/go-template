# One-off commands

A data fix or backfill run once by hand (trim titles saved untrimmed, fill a new column for old
rows) is a command of `cmd/oneoff`, never a script or SQL pasted into a shell: it gets the same
rules, a dry run, a log you can find, and tests.

```sh
go run ./cmd/oneoff                                  # lists the commands
go run ./cmd/oneoff trim-task-titles                 # a dry run: does the work, saves nothing
go run ./cmd/oneoff trim-task-titles --apply         # saves
docker run --rm -e DATABASE_URL=... <name>-oneoff trim-task-titles --apply   # on a server
```

`make image-oneoff` builds the image (`make image` builds it with the rest). It reads
`DATABASE_URL`, `APP_ENV`, `LOG_LEVEL`, `DB_MAX_CONNS` (2 by default: a run works one batch at a
time), the memory settings every program reads (`AUTOMEMLIMIT`), and the
[time limits](database.md#time-limits), 1 minute each by default: a batch is short, so a statement
or an idle transaction that long is stuck, holding locks while the apps run.

## What every run gets

`internal/oneoff` runs the command (`Invocation.Run`):

- **A dry run unless `--apply`.** The command does all its work, in transactions that are then
  undone, so its report is what `--apply` would do: the same code, the same counts. The exception:
  when one row's fix depends on an earlier batch's, the later batch sees the rows as they were,
  since the earlier one was undone (`--apply` may then count a clash the dry run didn't). A batch
  whose work went on after a failed statement fails in a dry run too, since `--apply` couldn't save
  it.
- **A run ID** (`run_id`) on every line the run logs, through `r.Log` or, in the services it
  calls, `zerolog.Ctx(ctx)`; with `command`, `apply` and the build's `version`.
- **One summary line**, `command completed`: `result` (`done`, `failed` or `interrupted`), the
  `counts` and `duration_ms`, and the `error` (with its stack after a panic). Info when done, warn
  when interrupted, error when it failed; the program exits 1 unless done (2 for a wrong flag).
- **A report** on standard output for the person running it: whether it was a dry run, the counts,
  the run ID. `r.Reportf` adds lines to it.
- **Ctrl-C or SIGTERM** stops it: the batch it was in is undone (or saved, if it was already
  committing), those before it stay saved, and the summary says `interrupted`. A second Ctrl-C
  kills it at once. A commit that gets no answer (a broken connection) may or may not have saved
  its batch: the report says so, and running it again finishes the job.

## Rules

- **Write only in `r.Batch`**, through the queries it passes in, and build services from them
  (`task.NewService(q)`). A write through the pool isn't undone in a dry run.
- **Small batches**, a few hundred rows each (a flag, `-batch`, with a default): a batch holds its
  locks until it ends.
- **Page in a fixed order** (by ID: "the next 500 after the last ID seen"), never "the rows still
  to fix": a dry run fixes nothing, so those would be the same rows every time, forever.
- **Rules live in the service**, as for every entry point: the command only pages and counts
  (`task.Service.TrimTitles`).
- **Safe to run again**: a second run changes nothing more, so a run cut short is run again from
  the start and skips what's already fixed. Pick rows by what's wrong with them, not by a list
  made beforehand.
- **The apps keep running** meanwhile, so **write only from what you read**: each update checks
  the row still has the value the batch read (`AND title = sqlc.arg('old_title')`), so a user's
  edit since then isn't overwritten; count those left. An app's write can also clash with a
  batch's (two writes of one unique value at once): the run then fails, that batch is undone, and
  running it again goes on.
- **Nothing a rollback can't undo in a dry run**: before an email, a webhook or a call to another
  service, check `r.Apply`; in a dry run only count what would be sent.
- **Count with `r.Count` once a batch has ended**, so a failed batch isn't counted.
- **Never personal data in the report or the log** (titles, emails, names): IDs and counts.
- **Try it with the dry run first**, on the real database, and read the counts before `--apply`.

## Adding a command

1. A file in `internal/oneoff`, `<name>.go`, returning a `Command`: a kebab-case `Name`, a one-line
   `Summary`, and `New`, which registers the command's own flags and returns its work.
   `trim_task_titles.go` is the example to copy.
2. A line in `Commands()` (`commands.go`).
3. The rule it applies in the feature's service, with its service tests.
4. A test in `internal/oneoff/<name>_test.go`: a dry run changes nothing, `--apply` does, and the
   counts (`trim_task_titles_test.go`).

Delete a command once it has run everywhere it needed to: the commit that removes it keeps it in
the history.
