package softdelete

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func TestCheckPurgeAge(t *testing.T) {
	t.Parallel()
	outOfRange := &apperr.Error{Kind: apperr.KindInvalid, Detail: "the age is out of range", Fields: []apperr.FieldError{
		{Field: "older_than_days", Rule: "range", Message: "must be from 1 to 3650"},
	}}

	tests := []struct {
		days int
		want *apperr.Error
	}{
		{0, outOfRange},
		{-1, outOfRange},
		{MaxPurgeAgeDays + 1, outOfRange},
		{MaxPurgeAgeDays, nil},
		{1, nil},
	}
	for _, tt := range tests {
		var got *apperr.Error
		if err := CheckPurgeAge(tt.days); err != nil {
			e, ok := errors.AsType[*apperr.Error](err)
			if !ok {
				t.Fatalf("CheckPurgeAge(%d) = %v, want an apperr.Error", tt.days, err)
			}
			got = e
		}
		if diff := cmp.Diff(tt.want, got); diff != "" {
			t.Errorf("CheckPurgeAge(%d) mismatch (-want +got):\n%s", tt.days, diff)
		}
	}
}

// vetTables matches the table list of the sqlc vet rule named skips-deleted-rows, wherever it sits
// among the rules: ["a", "b"].exists(...) in its rule.
var vetTables = regexp.MustCompile(`(?s)name: skips-deleted-rows\b.*?\[([^\]]*)\]\.exists\(`)

// A table with deleted_at that the sqlc vet rule doesn't list gets no check that its queries skip
// deleted rows.
func TestEverySoftDeletedTableIsCheckedByLint(t *testing.T) {
	t.Parallel()
	config, err := os.ReadFile(filepath.Join("..", "..", "sqlc.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := vetTables.FindSubmatch(config)
	if m == nil {
		t.Fatal(`sqlc.yaml has no skips-deleted-rows rule with a ["table", ...].exists( list`)
	}
	var listed []string
	for name := range strings.SplitSeq(string(m[1]), ",") {
		if name = strings.Trim(strings.TrimSpace(name), `"'`); name != "" {
			listed = append(listed, name)
		}
	}
	slices.Sort(listed)

	if diff := cmp.Diff(testdb.SoftDeletedTables(t, testdb.New(t)), listed, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("tables with deleted_at (-) vs the soft-delete rule's list in sqlc.yaml (+):\n%s", diff)
	}
}
