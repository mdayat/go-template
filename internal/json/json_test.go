package json_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mdayat/go-template/internal/json"
)

type item struct {
	Title string `json:"title"`
}

type target struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Items []item `json:"items"`
}

func TestMarshalAndUnmarshal(t *testing.T) {
	t.Parallel()
	want := target{Name: "a", Count: 2, Items: []item{{Title: "b"}}}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if got := string(data); got != `{"name":"a","count":2,"items":[{"title":"b"}]}` {
		t.Errorf("Marshal() = %s", got)
	}
	var got target
	// Unmarshal ignores fields the target has no place for (job payloads from a newer version).
	if err := json.Unmarshal([]byte(`{"name":"a","count":2,"items":[{"title":"b"}],"extra":1}`), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Unmarshal() mismatch (-want +got):\n%s", diff)
	}
}

func TestDecodeStrict(t *testing.T) {
	t.Parallel()
	var got target
	if err := json.DecodeStrict([]byte(` {"name":"a","count":2} `), &got); err != nil {
		t.Fatalf("DecodeStrict() error = %v", err)
	}
	if diff := cmp.Diff(target{Name: "a", Count: 2}, got); diff != "" {
		t.Errorf("DecodeStrict() mismatch (-want +got):\n%s", diff)
	}
}

// Each problem comes back as this package's own error, whichever library is underneath: callers
// (the API's body reading) build their replies from these.
func TestDecodeStrictReportsProblems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want error
	}{
		{"syntax error", `{"name" "a"}`, &json.SyntaxError{Offset: 9}},
		{"empty", ``, json.ErrUnexpectedEnd},
		{"ends too early", `{"name":"a"`, json.ErrUnexpectedEnd},
		{"wrong type", `{"count":"two"}`, &json.TypeError{Field: "count", Type: reflect.TypeFor[int]()}},
		{"wrong type at the top", `"text"`, &json.TypeError{Field: "", Type: reflect.TypeFor[target]()}},
		{"wrong type inside a list", `{"items":[{"title":5}]}`, &json.TypeError{Field: "items[0].title", Type: reflect.TypeFor[string]()}},
		{"unknown field", `{"name":"a","colour":"red"}`, &json.UnknownFieldError{Field: "colour"}},
		{"unknown field with a quote", `{"a\"b":1}`, &json.UnknownFieldError{Field: `a"b`}},
		{"two values", `{"name":"a"}{"name":"b"}`, json.ErrTrailingData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got target
			// errors.Is for the sentinels, DeepEqual for the error types' fields.
			if err := json.DecodeStrict([]byte(tt.data), &got); !errors.Is(err, tt.want) && !reflect.DeepEqual(err, tt.want) {
				t.Errorf("DecodeStrict(%s) error = %#v, want %#v", tt.data, err, tt.want)
			}
		})
	}
}

func TestDecodeStrictRefusesANonPointer(t *testing.T) {
	t.Parallel()
	var nilTarget *target
	for _, v := range []any{target{}, nilTarget} {
		if err := json.DecodeStrict([]byte(`{}`), v); !errors.Is(err, json.ErrInvalidTarget) {
			t.Errorf("DecodeStrict(%T) error = %v, want ErrInvalidTarget", v, err)
		}
	}
}

// A parser that recurses per nesting level can run out of stack on deep data, and a stack overflow
// kills the whole process: recovery can't catch it. Any library behind this package must refuse
// such data with an error instead.
func TestDecodeStrictRefusesDeepNesting(t *testing.T) {
	t.Parallel()
	depth := 1 << 19
	data := `{"name":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`

	var got target
	if err := json.DecodeStrict([]byte(data), &got); err == nil {
		t.Error("DecodeStrict() error = nil, want an error")
	}
}
