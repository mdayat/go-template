package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Not parallel: it changes the working directory and the process's environment.
func TestGetSyntaxErrorQuotesNoValues(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/x\n",
		".env":   "TESTENV_TEST_PASSWORD=\"s3cret\nTESTENV_TEST_TOKEN=s3cret\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	t.Setenv("TESTENV_TEST_TOKEN", "")

	_, err := Get("TESTENV_TEST_TOKEN")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("Get() error = %v, want an error without the file's values", err)
	}
}
