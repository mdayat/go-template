// Package json reads and writes JSON. It is the only package that imports a JSON library (a lint
// rule refuses it anywhere else), so the library can be replaced here alone. Marshal and Unmarshal
// behave like encoding/json's; DecodeStrict reports problems with this package's own error types.
package json

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	gojson "github.com/goccy/go-json"
)

// Marshal returns the JSON encoding of v.
func Marshal(v any) ([]byte, error) {
	return gojson.Marshal(v)
}

// Unmarshal parses data into v (a non-nil pointer). Object fields v has no place for are ignored.
func Unmarshal(data []byte, v any) error {
	return gojson.Unmarshal(data, v)
}

// Problems DecodeStrict reports with data, so a caller can tell the sender what to fix.
var (
	// ErrUnexpectedEnd: data ends before its JSON value does.
	ErrUnexpectedEnd = errors.New("json: data ends too early")
	// ErrTrailingData: data holds more than one JSON value.
	ErrTrailingData = errors.New("json: more than one JSON value")
)

// ErrInvalidTarget means DecodeStrict was given something other than a non-nil pointer: a bug in
// the caller, not a problem with the data.
var ErrInvalidTarget = errors.New("json: decode target must be a non-nil pointer")

// SyntaxError reports data that isn't valid JSON.
type SyntaxError struct {
	Offset int64 // the byte where reading failed
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("json: invalid JSON at byte %d", e.Offset)
}

// TypeError reports a JSON value of the wrong type for its Go field.
type TypeError struct {
	Field string       // the path to the field, "items[0].title"; empty for the top-level value
	Type  reflect.Type // the Go type the value had to fit
}

func (e *TypeError) Error() string {
	return fmt.Sprintf("json: field %q can't hold a value for Go type %v", e.Field, e.Type)
}

// UnknownFieldError reports an object field the target struct has no place for.
type UnknownFieldError struct {
	Field string
}

func (e *UnknownFieldError) Error() string {
	return fmt.Sprintf("json: unknown field %q", e.Field)
}

// DecodeStrict parses data, which must hold exactly one JSON value, into v (a non-nil pointer),
// refusing object fields v has no place for. A problem with data returns ErrUnexpectedEnd (empty
// data included), ErrTrailingData, *SyntaxError, *TypeError or *UnknownFieldError; otherwise the
// error passes through unchanged: a field's own parsing failed (a time or UUID in the wrong form),
// or the library refused the data for another reason (nesting too deep).
func DecodeStrict(data []byte, v any) error {
	dec := gojson.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrTrailingData
	}
	return nil
}

// decodeError turns the library's decoding error into this package's.
func decodeError(err error) error {
	var (
		invalidErr *gojson.InvalidUnmarshalError
		syntaxErr  *gojson.SyntaxError
		typeErr    *gojson.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &invalidErr):
		// The library's text only: its error type stays inside this package.
		return fmt.Errorf("%w: %s", ErrInvalidTarget, err.Error())
	case errors.As(err, &syntaxErr):
		return &SyntaxError{Offset: syntaxErr.Offset}
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return ErrUnexpectedEnd
	case errors.As(err, &typeErr):
		return &TypeError{Field: fieldPath(typeErr.Field), Type: typeErr.Type}
	}
	// Neither go-json nor encoding/json has a type for this; a test pins the text.
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		if unquoted, err := strconv.Unquote(name); err == nil {
			name = unquoted
		}
		return &UnknownFieldError{Field: name}
	}
	return err
}

// fieldPath writes the library's field path ("items.0.title") the way validation errors write it
// ("items[0].title"), so replies name a field the same way whichever check failed.
func fieldPath(field string) string {
	var b strings.Builder
	for i, part := range strings.Split(field, ".") {
		if _, err := strconv.Atoi(part); err == nil && i > 0 {
			b.WriteString("[" + part + "]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}
