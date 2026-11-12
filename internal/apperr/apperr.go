// Package apperr defines the kinds of failure our code reports, apart from how they're delivered.
// Services say what went wrong; each entry point (HTTP API, worker) decides the response: a status
// code, a retry or not. An error without a kind is unexpected: a bug or an outage.
package apperr

// Kind says what went wrong.
type Kind int

// The kinds an entry point maps to a response. Add one only when some caller must react to it
// differently from the others.
const (
	KindBadRequest Kind = iota + 1 // the input can't be read (malformed JSON, wrong types)
	KindInvalid                    // the input is readable but breaks a rule; Fields says which
	KindNotFound
	KindConflict // the request clashes with current state (a duplicate, a stale version)
)

// Error is an expected failure. Its Detail is safe to show the caller, so it never holds internal
// details; add those by wrapping it: fmt.Errorf("get task %s: %w", id, apperr.NotFound("task not found")).
type Error struct {
	Kind   Kind
	Detail string
	Fields []FieldError
}

// FieldError is one input field that broke a rule.
type FieldError struct {
	Field   string // the name the caller sent, e.g. the JSON key
	Rule    string // a stable code a client can match on, e.g. "required"
	Message string // safe to show, e.g. "is required"
}

func (e *Error) Error() string {
	return e.Detail
}

// BadRequest reports input that can't be read.
func BadRequest(detail string) error {
	return &Error{Kind: KindBadRequest, Detail: detail}
}

// Invalid reports input that breaks a rule.
func Invalid(detail string, fields ...FieldError) error {
	return &Error{Kind: KindInvalid, Detail: detail, Fields: fields}
}

// NotFound reports that the thing asked for doesn't exist.
func NotFound(detail string) error {
	return &Error{Kind: KindNotFound, Detail: detail}
}

// Conflict reports a request that clashes with current state.
func Conflict(detail string) error {
	return &Error{Kind: KindConflict, Detail: detail}
}
