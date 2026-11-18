package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCheckFile(t *testing.T) {
	t.Parallel()
	// users is new in the change under test; tasks is in use.
	created := map[string]bool{"users": true}
	tests := []struct {
		name string
		sql  string
		want []rule
	}{
		// Atlas's output for safe changes, as it writes them.
		{"new table and its index", `-- Create "users" table
CREATE TABLE "users" ("id" uuid NOT NULL, "name" text NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "users_name_key" UNIQUE ("name"));
-- Create index "users_name_idx" to table: "users"
CREATE INDEX "users_name_idx" ON "users" ("name");
ALTER TABLE "users" ADD COLUMN "age" integer NOT NULL, DROP COLUMN "name", ADD CONSTRAINT "c" CHECK (age > 0);`, nil},
		{"nullable column", `ALTER TABLE "tasks" ADD COLUMN "deleted_at" timestamptz NULL;`, nil},
		{"NOT NULL column with a default", `ALTER TABLE "tasks" ADD COLUMN "done" boolean NOT NULL DEFAULT false;`, nil},
		{"default now()", `ALTER TABLE tasks ADD COLUMN seen timestamptz NOT NULL DEFAULT now();`, nil},
		{"index CONCURRENTLY", "-- atlas:txmode none\n\nCREATE INDEX CONCURRENTLY \"i\" ON \"tasks\" (\"title\");", nil},
		{"txmode among other header lines", "-- Build the index without blocking writes.\n--atlas:txmode none\n\n-- Create index\nCREATE INDEX CONCURRENTLY i ON tasks (title);", nil},
		{"reindex CONCURRENTLY", "-- atlas:txmode none\n\nREINDEX INDEX CONCURRENTLY tasks_title_key;", nil},
		{"index dropped CONCURRENTLY", "-- atlas:txmode none\n\nDROP INDEX CONCURRENTLY tasks_title_key;", nil},
		{"rows locked, not changed, in a CTE", `WITH t AS (SELECT id FROM tasks FOR NO KEY UPDATE) SELECT count(*) FROM t;`, nil},
		{"upsert in a CTE", `WITH v AS (SELECT 'x' AS title) INSERT INTO tasks (title) SELECT title FROM v ON CONFLICT DO UPDATE SET done = false;`, nil},
		{"rows inserted through a CTE", `WITH t AS (SELECT 1) INSERT INTO tasks (title) SELECT 'x' FROM t;`, nil},
		{"constraints NOT VALID, then validated", `ALTER TABLE "tasks" ADD CONSTRAINT "f" FOREIGN KEY ("owner_id") REFERENCES "users" ("id") NOT VALID, ADD CONSTRAINT "c" CHECK (char_length(title) < 9) NOT VALID;
ALTER TABLE "tasks" VALIDATE CONSTRAINT "f";`, nil},
		{"unique constraint on an index built beforehand", `ALTER TABLE tasks ADD CONSTRAINT k UNIQUE USING INDEX i, ADD PRIMARY KEY USING INDEX j;`, nil},
		{"dropping indexes and constraints", `DROP INDEX "tasks_title_key"; ALTER TABLE "tasks" DROP CONSTRAINT "tasks_title_key";`, nil},
		{"new enum value", `ALTER TYPE mood ADD VALUE 'meh';`, nil},
		{"inserting rows", `INSERT INTO tasks (title) VALUES ('DROP TABLE tasks');`, nil},
		{"keywords in comments, text and function bodies", `-- DROP TABLE tasks;
/* outer /* nested */ DROP TABLE tasks; */
ALTER TABLE tasks ALTER COLUMN title SET DEFAULT 'it''s; RENAME, DROP COLUMN done';
ALTER TABLE tasks ALTER COLUMN title SET DEFAULT E'it\'s; RENAME, DROP COLUMN done';
ALTER TABLE tasks ALTER COLUMN title SET DEFAULT $body$ it's; RENAME, DROP COLUMN done $body$;
ALTER TABLE tasks ALTER COLUMN title SET DEFAULT $$ RENAME, DROP COLUMN done $$;
CREATE FUNCTION g() RETURNS void AS $$ UPDATE tasks SET done = true; $$ LANGUAGE sql;`, nil},
		{"a column named after a keyword", `ALTER TABLE tasks ADD COLUMN "unique" text NULL, ADD COLUMN "check" text NULL;`, nil},

		{"schema-qualified new table", `CREATE TABLE public.users (id int); CREATE INDEX i ON public."users" (id);`, nil},
		{"data in a new table", `UPDATE users SET name = 'x'; DELETE FROM users; TRUNCATE users;`, nil},

		{"drop table", `DROP TABLE "tasks";`, []rule{ruleDrop}},
		{"drop a new table and one in use", `DROP TABLE IF EXISTS users, tasks CASCADE;`, []rule{ruleDrop}},
		{"drop view", `DROP MATERIALIZED VIEW task_counts;`, []rule{ruleDrop}},
		{"drop function", `DROP FUNCTION f();`, []rule{ruleDrop}},
		{"truncate", `TRUNCATE TABLE tasks;`, []rule{ruleTruncate}},
		{"drop column", `ALTER TABLE "tasks" DROP COLUMN "done";`, []rule{ruleDropColumn}},
		{"drop column without COLUMN", `ALTER TABLE ONLY public.tasks DROP done;`, []rule{ruleDropColumn}},
		{"rename column", `ALTER TABLE "tasks" RENAME COLUMN "title" TO "name";`, []rule{ruleRename}},
		{"rename table", `ALTER TABLE tasks RENAME TO todos;`, []rule{ruleRename}},
		{"rename index", `ALTER INDEX tasks_title_key RENAME TO tasks_name_key;`, []rule{ruleRename}},
		{"rename enum value", `ALTER TYPE mood RENAME VALUE 'sad' TO 'blue';`, []rule{ruleRename}},
		{"NOT NULL without a default", `ALTER TABLE "tasks" ADD COLUMN "owner_id" uuid NOT NULL;`, []rule{ruleNotNull}},
		{"NOT NULL among other actions", "ALTER TABLE \"tasks\" ADD COLUMN \"a\" text NULL,\n  ADD COLUMN IF NOT EXISTS \"b\" text NOT NULL REFERENCES users (id);", []rule{ruleNotNull, ruleConstraint}},
		{"set NOT NULL", `ALTER TABLE tasks ALTER COLUMN title SET NOT NULL;`, []rule{ruleSetNotNull}},
		{"drop default", `ALTER TABLE tasks ALTER done DROP DEFAULT;`, []rule{ruleDropDefault}},
		{"type change", `ALTER TABLE "tasks" ALTER COLUMN "title" TYPE varchar(100);`, []rule{ruleType}},
		{"type change, long form", `ALTER TABLE tasks ALTER COLUMN title SET DATA TYPE text;`, []rule{ruleType}},
		{"volatile default", `ALTER TABLE tasks ADD COLUMN code uuid NOT NULL DEFAULT gen_random_uuid();`, []rule{ruleRewrite}},
		{"volatile default inside an expression", `ALTER TABLE tasks ADD COLUMN n int DEFAULT (floor(random() * 10));`, []rule{ruleRewrite}},
		{"serial", `ALTER TABLE tasks ADD COLUMN n bigserial;`, []rule{ruleRewrite}},
		{"identity", `ALTER TABLE tasks ADD COLUMN n int GENERATED ALWAYS AS IDENTITY;`, []rule{ruleRewrite}},
		{"stored generated", `ALTER TABLE tasks ADD COLUMN t text NOT NULL GENERATED ALWAYS AS (lower(title)) STORED;`, []rule{ruleRewrite}},
		{"plain index", `-- Create index "i" to table: "tasks"
CREATE INDEX "i" ON "tasks" ("created_at" DESC, "id" DESC) WHERE (deleted_at IS NULL);`, []rule{ruleIndex}},
		{"plain unique index", `CREATE UNIQUE INDEX IF NOT EXISTS i ON ONLY tasks USING btree (title) NULLS NOT DISTINCT;`, []rule{ruleIndex}},
		{"unique constraint", `ALTER TABLE tasks ADD CONSTRAINT k UNIQUE (title);`, []rule{ruleIndex}},
		{"primary key", `ALTER TABLE tasks ADD PRIMARY KEY (id);`, []rule{ruleIndex}},
		{"unique column", `ALTER TABLE tasks ADD COLUMN code text UNIQUE;`, []rule{ruleIndex}},
		{"foreign key", "ALTER TABLE \"tasks\" ADD COLUMN \"owner_id\" uuid NULL, ADD\nCONSTRAINT \"f\" FOREIGN KEY (\"owner_id\") REFERENCES \"users\" (\"id\") ON UPDATE NO ACTION ON DELETE NO ACTION;", []rule{ruleConstraint}},
		{"check", `ALTER TABLE tasks ADD CHECK (char_length(title) < 9);`, []rule{ruleConstraint}},
		{"CONCURRENTLY inside a transaction", `CREATE INDEX CONCURRENTLY i ON tasks (title);`, []rule{ruleTxMode}},
		{"txmode without the empty line after it", "-- atlas:txmode none\nCREATE INDEX CONCURRENTLY i ON tasks (title);", []rule{ruleTxMode}},
		{"txmode with Windows line endings", "-- atlas:txmode none\r\n\r\nCREATE INDEX CONCURRENTLY i ON tasks (title);\r\n", []rule{ruleTxMode}},
		{"txmode before a line of spaces", "-- atlas:txmode none\n  \nCREATE INDEX CONCURRENTLY i ON tasks (title);", []rule{ruleTxMode}},
		{"index dropped CONCURRENTLY inside a transaction", `DROP INDEX CONCURRENTLY tasks_title_key;`, []rule{ruleTxMode}},
		{"reindex CONCURRENTLY inside a transaction", `REINDEX INDEX CONCURRENTLY tasks_title_key;`, []rule{ruleTxMode}},
		{"txmode below the first statement", "SELECT 1;\n-- atlas:txmode none\nCREATE INDEX CONCURRENTLY i ON tasks (title);", []rule{ruleTxMode}},
		{"update", `UPDATE tasks SET done = false WHERE done IS NULL;`, []rule{ruleChangesRows}},
		{"delete", `DELETE FROM ONLY "public"."tasks";`, []rule{ruleChangesRows}},
		{"update in a CTE", `WITH done AS (UPDATE tasks SET done = true RETURNING id) SELECT count(*) FROM done;`, []rule{ruleChangesRows}},
		{"merge", `MERGE INTO tasks USING x ON tasks.id = x.id WHEN MATCHED THEN DELETE;`, []rule{ruleChangesRows}},
		{"code block", `DO $$ BEGIN ALTER TABLE tasks DROP COLUMN done; END $$;`, []rule{ruleRunsCode}},
		{"lock", `LOCK TABLE tasks IN ACCESS EXCLUSIVE MODE;`, []rule{ruleLocksTable}},
		{"reindex", `REINDEX TABLE tasks;`, []rule{ruleLocksTable}},
		{"vacuum full", `VACUUM (FULL) tasks;`, []rule{ruleLocksTable}},
		{"cluster", `CLUSTER tasks USING tasks_pkey;`, []rule{ruleLocksTable}},
		{"set expression", `ALTER TABLE tasks ALTER COLUMN t SET EXPRESSION AS (upper(title));`, []rule{ruleLocksTable}},
		{"set logged", `ALTER TABLE tasks SET UNLOGGED;`, []rule{ruleLocksTable}},
		{"foreign key on the column", `ALTER TABLE tasks ADD COLUMN owner uuid REFERENCES users (id);`, []rule{ruleConstraint}},
		{"NOT NULL inside a CHECK, not the column's", `ALTER TABLE tasks ADD COLUMN note text CHECK (note IS NOT NULL OR done);`, []rule{ruleConstraint}},
		{"check on the column", `ALTER TABLE tasks ADD COLUMN n int DEFAULT 1 CHECK (n > 0);`, []rule{ruleConstraint}},
		{"random bytes default", `ALTER TABLE tasks ADD COLUMN salt bytea DEFAULT gen_random_bytes(16);`, []rule{ruleRewrite}},
		{"no semicolon at the end", `DROP TABLE tasks`, []rule{ruleDrop}},
		{"after text with an escaped quote", `COMMENT ON TABLE tasks IS E'\''; DROP TABLE tasks;`, []rule{ruleDrop}},
		{"after a function body with a quote", `SELECT $$it's$$; DROP TABLE tasks;`, []rule{ruleDrop}},
		{"two rules in one statement", `ALTER TABLE tasks DROP COLUMN a, ALTER COLUMN b TYPE text, DROP COLUMN c;`, []rule{ruleDropColumn, ruleType}},

		{"allowed", `-- Modify "tasks" table
-- allow-risky: tasks has 50 rows, read by nothing yet
ALTER TABLE "tasks" DROP COLUMN "done", ADD COLUMN "n" int NOT NULL;
DROP TABLE tasks;`, []rule{ruleDrop}},
		{"allowed without a reason", "-- allow-risky:\nDROP TABLE tasks;", []rule{ruleAllowNoReason}},
		{"allow line after the statement on its line", "DROP TABLE a; -- allow-risky: no\nDROP TABLE b;", []rule{ruleDrop, ruleDrop}},
		{"allow line above an earlier statement", "-- allow-risky: small\nSELECT 1;\nDROP TABLE tasks;", []rule{ruleDrop}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []rule
			for _, p := range checkFile(tt.sql, created) {
				if p.allowedBecause == "" {
					got = append(got, p.rule)
				}
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("checkFile() rules (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCheckFileLines(t *testing.T) {
	t.Parallel()
	sql := "-- Create \"t\"\nCREATE TABLE t (id int);\n\n-- Modify \"tasks\" table\nALTER TABLE \"tasks\"\n  DROP COLUMN \"done\";\nSELECT 1; DROP TABLE tasks;\n"
	got := checkFile(sql, map[string]bool{})
	want := []problem{{line: 5, rule: ruleDropColumn}, {line: 7, rule: ruleDrop}}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(problem{})); diff != "" {
		t.Errorf("checkFile() (-want +got):\n%s", diff)
	}
}

func TestEveryRuleSaysWhy(t *testing.T) {
	t.Parallel()
	for _, r := range []rule{
		ruleDrop, ruleDropColumn, ruleRename, ruleNotNull, ruleSetNotNull, ruleDropDefault, ruleType,
		ruleRewrite, ruleIndex, ruleConstraint, ruleTxMode, ruleChangesRows, ruleTruncate, ruleLocksTable, ruleRunsCode,
		ruleAllowNoReason,
	} {
		if r.why() == string(r) {
			t.Errorf("rule %s has no reason", r)
		}
	}
}

// TestRun checks which files are compared, in a repository of its own: the migrations of the
// commit compared with pass whatever they hold, new ones are checked, edited ones fail.
func TestRun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	writeMigration(t, root, "1_old.sql", "DROP TABLE legacy;\n")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "old")
	base := strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))

	ok, out := runIn(t, root, base)
	if !ok || !strings.Contains(out, "0 new migrations") {
		t.Errorf("no new migrations: run() = %v, %q; want a pass", ok, out)
	}

	// New ones: an index on a table created in another new file passes.
	writeMigration(t, root, "2_create.sql", "CREATE TABLE notes (id int);\n")
	writeMigration(t, root, "3_index.sql", "CREATE INDEX i ON notes (id);\n")
	ok, out = runIn(t, root, base)
	if !ok || !strings.Contains(out, "2 new migrations") {
		t.Errorf("safe new migrations: run() = %v, %q; want a pass", ok, out)
	}

	// Untracked, then committed: new either way.
	writeMigration(t, root, "4_risky.sql", "-- Modify\nALTER TABLE legacy DROP COLUMN a;\n")
	for _, commit := range []bool{false, true} {
		if commit {
			gitIn(t, root, "add", ".")
			gitIn(t, root, "commit", "-q", "-m", "new")
		}
		ok, out = runIn(t, root, base)
		if ok || !strings.Contains(out, "db/migrations/4_risky.sql:2: drops a column") {
			t.Errorf("risky migration (committed %v): run() = %v, %q; want it named", commit, ok, out)
		}
	}
	// Compared with the latest commit, it's old: passes.
	if ok, out = runIn(t, root, "HEAD"); !ok {
		t.Errorf("compared with HEAD: run() = %v, %q; want a pass", ok, out)
	}

	writeMigration(t, root, "1_old.sql", "DROP TABLE legacy;\n-- a fix\n")
	ok, out = runIn(t, root, "HEAD")
	if ok || !strings.Contains(out, "db/migrations/1_old.sql: edited after it was merged") {
		t.Errorf("edited migration: run() = %v, %q; want it named", ok, out)
	}
	gitIn(t, root, "checkout", "-q", "--", ".")

	// A table an earlier migration created isn't new, even created again.
	writeMigration(t, root, "5_again.sql", "DROP TABLE notes;\nCREATE TABLE notes (id int);\nCREATE TABLE IF NOT EXISTS logs (id int);\nCREATE INDEX j ON logs (id);\n")
	ok, out = runIn(t, root, "HEAD")
	if ok || !strings.Contains(out, "5_again.sql:1: drops") || !strings.Contains(out, "5_again.sql:4: builds an index") {
		t.Errorf("table created again: run() = %v, %q; want the drop and the index named", ok, out)
	}

	// An allowed statement passes, and is shown with all it let through.
	writeMigration(t, root, "5_again.sql", "-- allow-risky: notes is empty\nALTER TABLE notes DROP COLUMN a, ADD COLUMN b int NOT NULL;\n")
	ok, out = runIn(t, root, "HEAD")
	if !ok || !strings.Contains(out, "5_again.sql:2: allowed (notes is empty): drops a column") ||
		!strings.Contains(out, "5_again.sql:2: allowed (notes is empty): adds a NOT NULL column") {
		t.Errorf("allowed statement: run() = %v, %q; want a pass naming both", ok, out)
	}
	if err := os.Remove(filepath.Join(root, migrationsDir, "5_again.sql")); err != nil {
		t.Fatal(err)
	}

	// Deleting or renaming one fails; deleting them all (a new project's fresh start) doesn't.
	if err := os.Rename(filepath.Join(root, migrationsDir, "2_create.sql"), filepath.Join(root, migrationsDir, "2_renamed.sql")); err != nil {
		t.Fatal(err)
	}
	ok, out = runIn(t, root, "HEAD")
	if ok || !strings.Contains(out, "db/migrations/2_create.sql: deleted or renamed") {
		t.Errorf("renamed migration: run() = %v, %q; want it named", ok, out)
	}
	if err := os.RemoveAll(filepath.Join(root, migrationsDir)); err != nil {
		t.Fatal(err)
	}
	writeMigration(t, root, "9_init.sql", "CREATE TABLE things (id int);\nCREATE INDEX k ON things (id);\n")
	if ok, out = runIn(t, root, "HEAD"); !ok {
		t.Errorf("history started afresh: run() = %v, %q; want a pass", ok, out)
	}
}

func TestRunWithoutBase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	writeMigration(t, root, "1.sql", "DROP TABLE tasks;\n")
	// A repository's first push: nothing to compare with.
	if ok, out := runIn(t, root, strings.Repeat("0", 40)); !ok || !strings.Contains(out, "nothing checked") {
		t.Errorf("all-zero commit: run() = %v, %q; want a pass that says so", ok, out)
	}
	var buf bytes.Buffer
	if _, err := run(&buf, root, "origin/main"); err == nil || !strings.Contains(err.Error(), `no commit "origin/main"`) {
		t.Errorf("missing commit: run() error = %v; want it named", err)
	}
	if _, err := run(&buf, root, ""); err == nil {
		t.Error("empty commit: run() error = nil; want one, not a silent pass")
	}
}

func runIn(t *testing.T, root, base string) (bool, string) {
	t.Helper()
	var out bytes.Buffer
	ok, err := run(&out, root, base)
	if err != nil {
		t.Fatalf("run(%s) error = %v", base, err)
	}
	return ok, out.String()
}

func writeMigration(t *testing.T, root, name, sql string) {
	t.Helper()
	dir := filepath.Join(root, migrationsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gitIn runs git in dir as a fixed author, so it works without a git identity set up.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
