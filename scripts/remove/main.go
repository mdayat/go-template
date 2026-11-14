// Command remove takes a part of the template out of a project: the HTTP API, the worker (with the
// whole job queue), or only the markers once a project keeps both. Run it from the repository
// root through make: make remove what=api|worker|markers
//
// A part is its folders, plus the blocks of shared files marked for it: a line "remove:<part>"
// and a line "remove:end", each a whole-line comment in the file's own syntax (//, # or <!-- -->).
// Markers for the other part stay. Blocks marked "template" are only for the template itself (CI
// checking every shape) and go with any part; "markers" takes this tool out too.
//
// Only files git sees are changed (tracked, or new and not ignored), so a local .env is left as it
// is, and git can undo everything: commit first.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// parts are what make remove can take out, each with the folders and files that are only its own.
func parts() map[string][]string {
	return map[string][]string{
		"api": {"cmd/api", "internal/api", "docs/standards/http-api.md"},
		// Without a worker nothing runs queued jobs, so the queue goes too: an endpoint would queue
		// jobs that never run.
		"worker": {
			"cmd/worker", "internal/worker", "internal/jobs", "internal/queue", "internal/testqueue",
			"internal/startup/redis.go", "internal/startup/redis_test.go", "docs/standards/worker.md",
		},
		// A project keeping both parts needs neither the markers nor this tool.
		"markers": {"scripts/remove"},
	}
}

// templateOnly marks blocks that only the template needs; any run removes them.
const templateOnly = "template"

// markerLine matches a whole-line marker comment: "// remove:api", "# remove:end",
// "<!-- remove:worker -->".
var markerLine = regexp.MustCompile(`^\s*(?://|#|<!--)\s*remove:([a-z]+)\s*(?:-->)?\s*$`)

func main() {
	if len(os.Args) != 2 {
		fail(errors.New("usage: make remove what=api|worker|markers"))
	}
	if err := run(os.Args[1]); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "remove:", err)
	os.Exit(1)
}

func run(what string) error {
	paths, known := parts()[what]
	if !known {
		return fmt.Errorf("unknown part %q: want api, worker or markers", what)
	}
	if _, err := os.Stat("go.mod"); err != nil {
		return errors.New("run it from the repository root (where go.mod is)")
	}
	files, err := gitFiles()
	if err != nil {
		return err
	}

	changed, err := stripFiles(".", files, what, paths)
	if err != nil {
		return err
	}
	removed := 0
	for _, p := range paths {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w%s", p, err, undoHint)
		}
		removed++
	}
	if changed == 0 && removed == 0 {
		// Already removed, or a run that failed in a step below: finish those steps.
		fmt.Printf("nothing left to remove for %q; checking the project is formatted, tidy and builds\n", what)
	} else {
		fmt.Printf("removed %d folders or files and edited %d files\n", removed, changed)
	}

	// Drop the imports nobody uses now (goimports, through the linter's formatter), then the modules
	// nobody imports, and prove what's left builds. In this order: tidy keeps a module that a file
	// still imports, even an import the formatter is about to drop.
	for _, args := range [][]string{
		{"go", "tool", "-modfile=tools/go.mod", "golangci-lint", "fmt", "./..."},
		{"go", "mod", "tidy"},
		{"go", "build", "./..."},
	} {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...) //nolint:gosec // G204: our own fixed commands, no shell
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w\nFix what it reports (code that still uses a removed part), then run make remove what=%s again to finish%s",
				strings.Join(args, " "), err, what, undoHint)
		}
	}
	return nil
}

const undoHint = "; or undo everything with: git restore . && git clean -fd"

// gitFiles lists the files git sees: tracked, plus new ones it doesn't ignore.
func gitFiles() ([]string, error) {
	out, err := exec.CommandContext(context.Background(), "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, fmt.Errorf("list files with git (the project must be a git repository): %w", err)
	}
	return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00"), nil
}

// stripFiles strips what's blocks (or every marker, for "markers") from files under dir, skipping
// those under skip (about to be deleted), and returns how many it changed. It checks every file
// before writing any and reports every broken one, so a broken marker leaves the project untouched.
// Files are reached through an os.Root, so a symlink can't lead it outside dir.
func stripFiles(dir string, files []string, what string, skip []string) (int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	edits := map[string][]byte{}
	var errs []error
	for _, path := range files {
		if path == "" || under(path, skip) {
			continue
		}
		info, err := root.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted but not yet staged
		}
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		content, err := root.ReadFile(path)
		if err != nil {
			return 0, err
		}
		if bytes.IndexByte(content, 0) >= 0 { // a binary file
			continue
		}
		stripped, err := strip(content, what)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if !bytes.Equal(stripped, content) {
			edits[path] = stripped
		}
	}
	if len(errs) > 0 {
		return 0, fmt.Errorf("nothing changed; fix these markers first:\n%w", errors.Join(errs...))
	}

	// Each file goes through a temporary one, so a crash can't leave it half written.
	for path, content := range edits {
		info, err := root.Stat(path)
		if err != nil {
			return 0, err
		}
		tmp := path + ".remove-tmp"
		if err := root.WriteFile(tmp, content, info.Mode().Perm()); err != nil {
			return 0, err
		}
		if err := root.Rename(tmp, path); err != nil {
			return 0, err
		}
	}
	return len(edits), nil
}

// under reports whether path is one of paths or inside one of them.
func under(path string, paths []string) bool {
	for _, p := range paths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// strip returns content without what's marked blocks and the template-only ones, keeping every
// other block with its markers. For "markers" it keeps the other blocks' lines and drops only their
// marker lines. A marker inside another block, an end without a start, a start without an end, or
// an unknown part is an error.
//
// A dropped block takes one of the blank lines around it with it, so it leaves no double blank
// line, and none at the start or end of the file.
func strip(content []byte, what string) ([]byte, error) {
	lines := strings.SplitAfter(string(content), "\n")
	known := parts()
	drop := func(part string) bool { return part == what || part == templateOnly }
	keepMarkers := what != "markers"

	var out []string
	open, openedAt := "", 0
	skipBlank, dropped := false, false
	for i, line := range lines {
		m := markerLine.FindStringSubmatch(line)
		if m == nil {
			switch {
			case open != "" && drop(open):
			case skipBlank && blank(line):
				skipBlank = false
			default:
				skipBlank = false
				out = append(out, line)
			}
			continue
		}
		part := m[1]
		switch {
		case part == "end" && open == "":
			return nil, fmt.Errorf("line %d: remove:end without a start", i+1)
		case part == "end":
			if drop(open) {
				// The block went: drop the blank line after it if one is already before it.
				skipBlank = len(out) == 0 || blank(out[len(out)-1])
				dropped = true
			} else if keepMarkers {
				out = append(out, line)
			}
			open = ""
		case open != "":
			return nil, fmt.Errorf("line %d: remove:%s inside the remove:%s block from line %d", i+1, part, open, openedAt)
		case known[part] == nil && part != templateOnly:
			return nil, fmt.Errorf("line %d: unknown part remove:%s", i+1, part)
		default:
			if !drop(part) && keepMarkers {
				out = append(out, line)
			}
			open, openedAt = part, i+1
		}
	}
	if open != "" {
		return nil, fmt.Errorf("line %d: remove:%s has no remove:end", openedAt, open)
	}
	if dropped {
		for len(out) > 0 && blank(out[len(out)-1]) {
			out = out[:len(out)-1]
		}
		if len(out) > 0 && !strings.HasSuffix(out[len(out)-1], "\n") {
			out[len(out)-1] += "\n"
		}
	}
	return []byte(strings.Join(out, "")), nil
}

// blank reports whether line holds nothing but white space (its line ending included).
func blank(line string) bool {
	return strings.TrimSpace(line) == ""
}
