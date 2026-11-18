// Command migratecheck fails when a new migration could break the app that keeps running while it's
// applied: during a deploy the old code serves requests against the new schema for a while (see
// "Migrations that are safe while the old app runs" in docs/standards/database.md). Run it as make
// migrate-check base=<commit>: a migration is new when that commit doesn't have it. It also fails
// when a migration that commit has was edited, deleted or renamed, as it may already have run
// somewhere.
//
// It reads SQL by its keywords, not as Postgres does: it catches the common mistakes, and review
// catches the rest. A statement meant as written passes with a line right above it:
//
//	-- allow-risky: <why it's safe here>
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const migrationsDir = "db/migrations"

// allowMarker starts the comment that lets the statement below it through.
const allowMarker = "allow-risky:"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: migratecheck <commit to compare with>")
		os.Exit(2)
	}
	ok, err := run(os.Stdout, ".", os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-check:", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
}

// run checks the migrations under root that commit base lacks, and those it has that were edited
// or deleted, writing each problem to w. It reports whether there were none.
func run(w io.Writer, root, base string) (bool, error) {
	switch {
	case base == "":
		return false, errors.New("no commit to compare with: pass base=<commit>")
	case base == strings.Repeat("0", 40):
		// GitHub's "commit before the push" on a repository's first push.
		fmt.Fprintln(w, "migrate-check: no earlier commit to compare with; nothing checked")
		return true, nil
	}
	if _, err := git(root, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return false, fmt.Errorf("no commit %q to compare with (not fetched? pass base=<commit>; base=HEAD checks the migrations not committed yet): %w", base, err)
	}
	listed, err := git(root, "ls-tree", "-z", "--name-only", base, migrationsDir+"/")
	if err != nil {
		return false, err
	}
	old := strings.Split(listed, "\x00")
	// Compared with the files on disk, so a change not yet committed counts too.
	changed, err := git(root, "diff", "-z", "--relative", "--no-renames", "--name-status", "--diff-filter=MD", base, "--", migrationsDir+"/*.sql")
	if err != nil {
		return false, err
	}
	paths, err := filepath.Glob(filepath.Join(root, migrationsDir, "*.sql"))
	if err != nil {
		return false, err
	}

	sources := map[string]string{}
	var fresh, kept []string
	for _, path := range paths {
		name := migrationsDir + "/" + filepath.Base(path)
		src, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		sources[name] = string(src)
		if slices.Contains(old, name) {
			kept = append(kept, name)
		} else {
			fresh = append(fresh, name)
		}
	}

	problems := 0
	fields := strings.Split(changed, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		switch status, path := fields[i], fields[i+1]; {
		case status == "M":
			problems++
			fmt.Fprintf(w, "%s: edited after it was merged: it may already have run somewhere, and won't run again there. Put the change in a new migration (make migrate-diff)\n", path)
		case len(kept) > 0:
			// Deleting every one is starting the history afresh, in a new project; deleting or
			// renaming some leaves databases where they ran out of step with the files.
			problems++
			fmt.Fprintf(w, "%s: deleted or renamed after it was merged: Atlas then finds the databases it ran on out of step with the files. Restore it, and undo its change with a new migration\n", path)
		}
	}

	// A table is new if a new migration creates it and no earlier one does: anything goes on it,
	// as no running code uses it yet.
	existing := map[string]bool{}
	for _, name := range kept {
		for table := range createdTables(parse(sources[name])) {
			existing[table] = true
		}
	}
	created := map[string]bool{}
	for _, name := range fresh {
		for table := range createdTables(parse(sources[name])) {
			created[table] = !existing[table]
		}
	}
	for _, name := range fresh {
		for _, p := range checkFile(sources[name], created) {
			if p.allowedBecause != "" {
				// One line lets a whole statement through, every action of an ALTER TABLE: shown, so
				// review sees all it let through.
				fmt.Fprintf(w, "%s:%d: allowed (%s): %s\n", name, p.line, p.allowedBecause, p.rule.why())
				continue
			}
			problems++
			fmt.Fprintf(w, "%s:%d: %s\n", name, p.line, p.rule.why())
		}
	}
	if problems > 0 {
		fmt.Fprintf(w, "\n%d problems in migrations new since %s. Make each safe as docs/standards/database.md says, or, if one is safe here (a small table, say), put a line right above it: -- %s <why>; then make migrate-hash\n",
			problems, base, allowMarker)
		return false, nil
	}
	fmt.Fprintf(w, "migrate-check: %d new migrations since %s, none risky\n", len(fresh), base)
	return true, nil
}

// git runs git in dir and returns what it prints.
func git(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: our own fixed commands, no shell
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(bytes.TrimSpace(exit.Stderr)) > 0 {
			return "", fmt.Errorf("git %s: %s", args[0], bytes.TrimSpace(exit.Stderr))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSuffix(string(out), "\x00"), nil
}

// rule is one kind of risky statement.
type rule string

const (
	ruleDrop          rule = "drop"
	ruleDropColumn    rule = "drop-column"
	ruleRename        rule = "rename"
	ruleNotNull       rule = "not-null-without-default"
	ruleSetNotNull    rule = "set-not-null"
	ruleDropDefault   rule = "drop-default"
	ruleType          rule = "type"
	ruleRewrite       rule = "rewrite"
	ruleIndex         rule = "index"
	ruleConstraint    rule = "constraint"
	ruleTxMode        rule = "txmode"
	ruleChangesRows   rule = "changes-rows"
	ruleTruncate      rule = "truncate"
	ruleLocksTable    rule = "locks-table"
	ruleRunsCode      rule = "runs-code"
	ruleAllowNoReason rule = "allow-without-reason"
)

// why says what breaks, and what to do instead.
func (r rule) why() string {
	switch r {
	case ruleDrop:
		return "drops something the old app may still use: stop using it in one deploy, then drop it in a later one"
	case ruleDropColumn:
		return "drops a column the old app may still read: stop using it in one deploy, then drop it in a later one"
	case ruleRename:
		return "renames something the old app uses by its old name: add the new one beside the old over separate deploys"
	case ruleNotNull:
		return "adds a NOT NULL column without a DEFAULT: the old app's inserts leave it out and fail"
	case ruleSetNotNull:
		return "SET NOT NULL checks every row while the table is locked, and the old app's inserts that leave the column out fail unless it has a DEFAULT"
	case ruleDropDefault:
		return "drops a default the old app's inserts may rely on"
	case ruleType:
		return "changes a column's type: Postgres usually rewrites the table while it's locked, and the old app may expect the old type"
	case ruleRewrite:
		return "adds a column with a different value on each row (serial, GENERATED, or a DEFAULT such as gen_random_uuid()): Postgres rewrites the whole table while it's locked"
	case ruleIndex:
		return "builds an index while writes to the table wait: use CREATE INDEX CONCURRENTLY (a unique constraint can then take the index: ADD CONSTRAINT ... UNIQUE USING INDEX)"
	case ruleConstraint:
		return "checks every row while the table is locked: add it with its own ADD CONSTRAINT ... NOT VALID, then VALIDATE CONSTRAINT in a later migration"
	case ruleTxMode:
		return "CONCURRENTLY can't run inside a transaction: start the file with the line -- atlas:txmode none, then an empty line (and Unix line endings)"
	case ruleChangesRows:
		return "changes rows in a migration: on a big table it holds their locks, and the deploy waits, until it ends; backfill with a one-off command instead (docs/standards/commands.md)"
	case ruleTruncate:
		return "empties a table the old app still uses"
	case ruleLocksTable:
		return "rewrites or locks the whole table while the app waits (SET EXPRESSION, SET LOGGED, LOCK, REINDEX without CONCURRENTLY, VACUUM FULL, CLUSTER)"
	case ruleRunsCode:
		return "runs code this check can't read (DO): review what it does, then allow it"
	case ruleAllowNoReason:
		return "-- " + allowMarker + " needs a reason after it"
	}
	return string(r)
}

// problem is a risky statement, at its first line.
type problem struct {
	line int
	rule rule
	// allowedBecause is the reason on the statement's allow-risky line, if it has one.
	allowedBecause string
}

// checkFile returns the risky statements in one migration's SQL. Tables in created are new in this
// change: nothing uses them yet, so anything goes on them.
func checkFile(src string, created map[string]bool) []problem {
	noTx := txModeNone(src)
	var problems []problem
	for _, s := range parse(src) {
		allowed, reason := false, ""
		for _, c := range s.comments {
			if after, ok := strings.CutPrefix(c, allowMarker); ok {
				allowed, reason = true, strings.TrimSpace(after)
			}
		}
		if allowed && reason == "" {
			problems = append(problems, problem{line: s.line, rule: ruleAllowNoReason})
			continue
		}
		var seen []rule
		for _, r := range checkStatement(s.tokens, created, noTx) {
			if !slices.Contains(seen, r) {
				seen = append(seen, r)
				problems = append(problems, problem{line: s.line, rule: r, allowedBecause: reason})
			}
		}
	}
	return problems
}

// txModeNone says whether src's header turns off Atlas's transaction: Atlas reads directives only
// from the comment lines a file starts with, ended by an empty line ("\n\n": with Windows line
// endings it finds none). Without the header, the file runs in a transaction, and CONCURRENTLY
// fails on deploy.
func txModeNone(src string) bool {
	found := false
	for line := range strings.Lines(src) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return found
		case !strings.HasPrefix(strings.TrimSpace(line), "--"):
			return false
		case strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "--")) == "atlas:txmode none":
			found = true
		}
	}
	return false
}

// createdTables returns the tables stmts create.
func createdTables(stmts []statement) map[string]bool {
	tables := map[string]bool{}
	for _, s := range stmts {
		ts := s.tokens
		if !startsWith(ts, "CREATE") {
			continue
		}
		ts = ts[1:]
		for len(ts) > 0 && ts[0].is("TEMP", "TEMPORARY", "UNLOGGED", "GLOBAL", "LOCAL") {
			ts = ts[1:]
		}
		// Not CREATE TABLE IF NOT EXISTS: the table may be one in use.
		if startsWith(ts, "TABLE") && !startsWith(ts[1:], "IF") {
			table, _ := name(ts[1:])
			tables[table] = true
		}
	}
	return tables
}

// checkStatement returns the rules one statement breaks.
func checkStatement(ts []token, created map[string]bool, noTx bool) []rule {
	switch {
	case hasWord(ts, "CONCURRENTLY") && !noTx:
		return []rule{ruleTxMode}
	case startsWith(ts, "CREATE"):
		return checkCreate(ts[1:], created)
	case startsWith(ts, "DROP"):
		return checkDrop(ts[1:], created)
	case startsWith(ts, "TRUNCATE"):
		if allCreated(names(skip(skip(ts[1:], "TABLE"), "ONLY")), created) {
			return nil
		}
		return []rule{ruleTruncate}
	case startsWith(ts, "MERGE"), startsWith(ts, "WITH") && changesRows(ts):
		return []rule{ruleChangesRows}
	case startsWith(ts, "DO"):
		return []rule{ruleRunsCode}
	case startsWith(ts, "LOCK"), startsWith(ts, "CLUSTER"), startsWith(ts, "VACUUM") && hasWord(ts, "FULL"),
		startsWith(ts, "REINDEX") && !hasWord(ts, "CONCURRENTLY"):
		return []rule{ruleLocksTable}
	case startsWith(ts, "UPDATE"), startsWith(ts, "DELETE", "FROM"):
		if table, _ := name(skip(skip(ts[1:], "FROM"), "ONLY")); created[table] {
			return nil
		}
		return []rule{ruleChangesRows}
	case startsWith(ts, "ALTER", "TABLE"):
		return checkAlterTable(ts[2:], created)
	case startsWith(ts, "ALTER") && hasWord(flat(ts), "RENAME"):
		return []rule{ruleRename}
	}
	return nil
}

// checkCreate checks CREATE ... after its CREATE: an index on a table in use must be built
// CONCURRENTLY.
func checkCreate(ts []token, created map[string]bool) []rule {
	ts = skip(ts, "UNIQUE")
	if !startsWith(ts, "INDEX") || startsWith(ts[1:], "CONCURRENTLY") {
		return nil
	}
	flatTs := flat(ts)
	on := slices.IndexFunc(flatTs, func(t token) bool { return t.is("ON") })
	if on < 0 {
		return nil
	}
	if table, _ := name(skip(flatTs[on+1:], "ONLY")); created[table] {
		return nil
	}
	return []rule{ruleIndex}
}

// checkDrop checks DROP ... after its DROP. Indexes and constraints aren't checked: Atlas drops and
// builds them again for every change to one, so review weighs them.
func checkDrop(ts []token, created map[string]bool) []rule {
	switch {
	case startsWith(ts, "TABLE"):
		if allCreated(names(skip(ts[1:], "IF", "EXISTS")), created) {
			return nil
		}
		return []rule{ruleDrop}
	case startsWith(ts, "MATERIALIZED", "VIEW"), len(ts) > 0 && ts[0].is("VIEW", "FUNCTION", "PROCEDURE", "TYPE", "DOMAIN", "SCHEMA", "SEQUENCE"):
		return []rule{ruleDrop}
	}
	return nil
}

// checkAlterTable checks ALTER TABLE ... after its ALTER TABLE, each action of it.
func checkAlterTable(ts []token, created map[string]bool) []rule {
	table, rest := name(skip(skip(ts, "IF", "EXISTS"), "ONLY"))
	if created[table] {
		return nil
	}
	if hasWord(flat(rest), "RENAME") {
		return []rule{ruleRename}
	}
	var rules []rule
	for _, action := range splitTop(rest) {
		switch {
		case startsWith(action, "DROP"):
			if !startsWith(action[1:], "CONSTRAINT") {
				rules = append(rules, ruleDropColumn)
			}
		case startsWith(action, "ADD"):
			rules = append(rules, checkAdd(action[1:])...)
		case startsWith(action, "ALTER"):
			rules = append(rules, checkAlterColumn(skip(action[1:], "COLUMN"))...)
		case startsWith(action, "SET", "LOGGED"), startsWith(action, "SET", "UNLOGGED"):
			rules = append(rules, ruleLocksTable)
		}
	}
	return rules
}

// checkAdd checks one ADD action after its ADD: a constraint or a column.
func checkAdd(ts []token) []rule {
	if startsWith(ts, "CONSTRAINT") {
		_, ts = name(ts[1:])
	}
	switch {
	case startsWith(ts, "UNIQUE", "USING"), startsWith(ts, "PRIMARY", "KEY", "USING"):
		return nil // takes an index built beforehand
	case len(ts) > 0 && ts[0].is("UNIQUE", "PRIMARY", "EXCLUDE"):
		return []rule{ruleIndex}
	case len(ts) > 0 && ts[0].is("FOREIGN", "CHECK"):
		if hasSeq(ts, "NOT", "VALID") {
			return nil
		}
		return []rule{ruleConstraint}
	}

	_, column := name(skip(skip(ts, "COLUMN"), "IF", "NOT", "EXISTS"))
	top := flat(column)
	var rules []rule
	serial := len(column) > 0 && column[0].is("SERIAL", "BIGSERIAL", "SMALLSERIAL", "SERIAL2", "SERIAL4", "SERIAL8")
	fills := serial || hasWord(top, "GENERATED")
	if def := slices.IndexFunc(top, func(t token) bool { return t.is("DEFAULT") }); def >= 0 {
		// The whole default, its parentheses included: a function call in it is what matters.
		start := slices.IndexFunc(column, func(t token) bool { return t.is("DEFAULT") })
		if slices.ContainsFunc(column[start:], func(t token) bool {
			return t.is("GEN_RANDOM_UUID", "GEN_RANDOM_BYTES", "UUID_GENERATE_V1", "UUID_GENERATE_V1MC",
				"UUID_GENERATE_V4", "UUIDV4", "UUIDV7", "RANDOM", "RANDOM_NORMAL", "CLOCK_TIMESTAMP",
				"TIMEOFDAY", "NEXTVAL")
		}) {
			fills = true
		}
	} else if hasSeq(top, "NOT", "NULL") && !fills {
		rules = append(rules, ruleNotNull)
	}
	if fills {
		rules = append(rules, ruleRewrite)
	}
	if hasWord(top, "UNIQUE") || hasWord(top, "PRIMARY") {
		rules = append(rules, ruleIndex)
	}
	if hasWord(top, "REFERENCES") || hasWord(top, "CHECK") {
		// Written on the column, it can't be NOT VALID: add the column, then the constraint.
		rules = append(rules, ruleConstraint)
	}
	return rules
}

// checkAlterColumn checks one ALTER [COLUMN] action after ALTER COLUMN.
func checkAlterColumn(ts []token) []rule {
	if startsWith(ts, "CONSTRAINT") {
		return nil
	}
	_, ts = name(ts)
	switch {
	case startsWith(ts, "TYPE"), startsWith(ts, "SET", "DATA", "TYPE"):
		return []rule{ruleType}
	case startsWith(ts, "SET", "EXPRESSION"):
		return []rule{ruleLocksTable}
	case startsWith(ts, "SET", "NOT", "NULL"):
		return []rule{ruleSetNotNull}
	case startsWith(ts, "DROP", "DEFAULT"):
		return []rule{ruleDropDefault}
	}
	return nil
}

// changesRows says whether a WITH statement updates or deletes rows: an UPDATE that isn't a lock
// (FOR UPDATE, FOR NO KEY UPDATE) or an insert's ON CONFLICT DO UPDATE, which a plain INSERT may do
// too.
func changesRows(ts []token) bool {
	for i, t := range ts {
		if t.is("DELETE") || (t.is("UPDATE") && (i == 0 || !ts[i-1].is("FOR", "KEY", "DO"))) {
			return true
		}
	}
	return false
}

// allCreated says whether every table in tables is in created (and there's at least one).
func allCreated(tables []string, created map[string]bool) bool {
	for _, t := range tables {
		if !created[t] {
			return false
		}
	}
	return len(tables) > 0
}
