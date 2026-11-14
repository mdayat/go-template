package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// lines joins its arguments into a file, one per line. Test files build their markers this way, as
// quoted strings, so remove never takes a test's input for a real block.
func lines(l ...string) string {
	return strings.Join(l, "\n") + "\n"
}

func TestStrip(t *testing.T) {
	t.Parallel()
	// Every comment syntax, both parts, the template-only and markers blocks, and lines outside
	// blocks.
	file := lines(
		"keep 1",
		"// remove:api",
		"api in Go",
		"// remove:end",
		"\t# remove:worker",
		"\tworker in YAML",
		"\t# remove:end",
		"<!-- remove:api -->",
		"api in Markdown",
		"<!-- remove:end -->",
		"# remove:template",
		"only the template",
		"# remove:end",
		"# remove:markers",
		"the remove tool",
		"# remove:end",
		"keep 2",
	)
	tests := []struct {
		what string
		want string
	}{
		{"api", lines(
			"keep 1", "\t# remove:worker", "\tworker in YAML", "\t# remove:end",
			"# remove:markers", "the remove tool", "# remove:end", "keep 2",
		)},
		{"worker", lines(
			"keep 1", "// remove:api", "api in Go", "// remove:end",
			"<!-- remove:api -->", "api in Markdown", "<!-- remove:end -->",
			"# remove:markers", "the remove tool", "# remove:end", "keep 2",
		)},
		{"markers", lines("keep 1", "api in Go", "\tworker in YAML", "api in Markdown", "keep 2")},
	}
	for _, tt := range tests {
		got, err := strip([]byte(file), tt.what)
		if err != nil {
			t.Errorf("strip(%s) error = %v", tt.what, err)
			continue
		}
		if diff := cmp.Diff(tt.want, string(got)); diff != "" {
			t.Errorf("strip(%s) mismatch (-want +got):\n%s", tt.what, diff)
		}
	}
}

// Only whole-line comments are markers: prose or code that mentions one is left alone.
func TestStripLeavesMentionsAlone(t *testing.T) {
	t.Parallel()
	file := lines(
		"Wrap a block in `remove:api` and `remove:end` lines.",
		`re := "// remove:api"`,
		"x := 1 // remove:api",
	)

	got, err := strip([]byte(file), "api")

	if err != nil || string(got) != file {
		t.Errorf("strip() = %q, %v; want the file unchanged", got, err)
	}
}

// A broken marker stops everything, so a half-removed project can't happen.
func TestStripRefusesBrokenMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
	}{
		{"block without an end", lines("// remove:api", "x")},
		{"end without a start", lines("x", "// remove:end")},
		// Balanced, so only the nesting check can catch it.
		{"block inside a block", lines("// remove:api", "// remove:worker", "x", "// remove:end")},
		{"unknown part", lines("// remove:apu", "x", "// remove:end")},
	}
	for _, tt := range tests {
		for _, what := range []string{"api", "worker", "markers"} {
			if got, err := strip([]byte(tt.file), what); err == nil {
				t.Errorf("%s: strip(%s) = %q, want an error", tt.name, what, got)
			}
		}
	}
}

// writeFiles writes each file under root, making its folders.
func writeFiles(t *testing.T, root string, files map[string]string) []string {
	t.Helper()
	var names []string
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// Broken files are all reported at once, and none is changed.
func TestStripFilesChangesNothingOnABrokenMarker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	good := lines("keep", "// remove:api", "api", "// remove:end")
	names := writeFiles(t, root, map[string]string{
		"a/good.go":    good,
		"b/broken.go":  lines("// remove:api", "no end"),
		"c/broken2.go": lines("x", "// remove:end"),
	})

	_, err := stripFiles(root, names, "api", nil)

	if err == nil || !strings.Contains(err.Error(), "broken.go") || !strings.Contains(err.Error(), "broken2.go") {
		t.Errorf("stripFiles() error = %v, want one naming broken.go and broken2.go", err)
	}
	if got := readFile(t, root, "a/good.go"); got != good {
		t.Errorf("a/good.go = %q, want it unchanged", got)
	}
}

// Binary files, files under a folder about to be deleted, and names git listed that are gone are
// left alone; edited files keep their permissions.
func TestStripFilesSkips(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	block := lines("// remove:api", "api", "// remove:end")
	names := writeFiles(t, root, map[string]string{
		"keep.go":         lines("keep") + block,
		"binary":          "\x00" + block,
		"cmd/api/main.go": lines("// remove:api", "broken, but about to be deleted"),
	})

	changed, err := stripFiles(root, append(names, "deleted.go"), "api", []string{"cmd/api"})
	if err != nil || changed != 1 {
		t.Fatalf("stripFiles() = %d, %v; want 1 file changed", changed, err)
	}
	if got := readFile(t, root, "keep.go"); got != lines("keep") {
		t.Errorf("keep.go = %q, want %q", got, lines("keep"))
	}
	if got := readFile(t, root, "binary"); got != "\x00"+block {
		t.Errorf("binary = %q, want it unchanged", got)
	}
	info, err := os.Stat(filepath.Join(root, "keep.go"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("keep.go mode = %v, want -rw-r-----", info.Mode().Perm())
	}
}

// A removed block leaves no double blank line, and none at the start or end of the file.
func TestStripLeavesNoDoubleBlankLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
		want string
	}{
		{"between blank lines", lines("a", "", "# remove:api", "x", "# remove:end", "", "b"), lines("a", "", "b")},
		{"at the start", lines("# remove:api", "x", "# remove:end", "", "b"), lines("b")},
		{"at the end", lines("a", "", "# remove:api", "x", "# remove:end"), lines("a")},
		{"no blank lines around", lines("a", "# remove:api", "x", "# remove:end", "b"), lines("a", "b")},
		{"Windows line endings", "a\r\n\r\n# remove:api\r\nx\r\n# remove:end\r\n\r\nb\r\n", "a\r\n\r\nb\r\n"},
	}
	for _, tt := range tests {
		got, err := strip([]byte(tt.file), "api")
		if err != nil {
			t.Errorf("%s: strip() error = %v", tt.name, err)
			continue
		}
		if diff := cmp.Diff(tt.want, string(got)); diff != "" {
			t.Errorf("%s: strip() mismatch (-want +got):\n%s", tt.name, diff)
		}
	}
}
