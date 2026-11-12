package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/goccy/go-json"

	"github.com/mdayat/go-template/internal/apperr"
)

const maxBodyBytes = 1 << 20 // 1 MB

// bodyDecoder reads JSON request bodies. Make one with newBodyDecoder and share it: the validator
// caches what it learns about each struct type.
type bodyDecoder struct {
	validate *validator.Validate
}

func newBodyDecoder() *bodyDecoder {
	validate := validator.New(validator.WithRequiredStructEnabled())
	// Report fields by the JSON names the client sent, not the Go field names.
	validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			return ""
		}
		return name
	})
	return &bodyDecoder{validate: validate}
}

// decode reads r's JSON body into dst (a pointer to a struct) and validates it with dst's
// `validate` tags. A body the client got wrong returns an error that replies 400, 413, 415 or 422
// with a message saying what to fix; any other error is a bug on our side.
func (d *bodyDecoder) decode(w http.ResponseWriter, r *http.Request, dst any) error {
	// Requiring application/json also blocks cross-site HTML forms, which can't send it.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &statusError{status: http.StatusUnsupportedMediaType, detail: "Content-Type must be application/json"}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return &statusError{status: http.StatusRequestEntityTooLarge, detail: "request body must be at most 1 MB"}
		}
		// The client sent less than it announced, or the connection broke.
		return fmt.Errorf("read request body: %w: %w", err, apperr.BadRequest("request body can't be read"))
	}
	// Only an object fits a struct. Checked here because the decoder accepts `null` as "no
	// change", which would let a body of null through as an empty request.
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return apperr.BadRequest("request body is empty")
	}
	if trimmed[0] != '{' {
		return apperr.BadRequest("request body must be a JSON object")
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// One JSON value only: trailing data usually means a client bug.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperr.BadRequest("request body must hold a single JSON value")
	}

	if err := d.validate.Struct(dst); err != nil {
		var fieldErrs validator.ValidationErrors
		if !errors.As(err, &fieldErrs) {
			return fmt.Errorf("validate request body: %w", err)
		}
		fields := make([]apperr.FieldError, 0, len(fieldErrs))
		for _, fe := range fieldErrs {
			fields = append(fields, apperr.FieldError{Field: fieldPath(fe), Rule: fe.Tag(), Message: ruleMessage(fe)})
		}
		return apperr.Invalid("request body is invalid", fields...)
	}
	return nil
}

// decodeError turns a JSON decoding error into a reply the client can act on.
func decodeError(err error) error {
	var (
		syntaxErr  *json.SyntaxError
		typeErr    *json.UnmarshalTypeError
		invalidErr *json.InvalidUnmarshalError
	)
	switch {
	case errors.As(err, &invalidErr):
		return fmt.Errorf("decode request body: %w", err) // dst isn't a pointer: our bug
	case errors.As(err, &syntaxErr):
		return apperr.BadRequest(fmt.Sprintf("request body is not valid JSON (at byte %d)", syntaxErr.Offset))
	case errors.Is(err, io.ErrUnexpectedEOF):
		return apperr.BadRequest("request body is not valid JSON (it ends too early)")
	case errors.As(err, &typeErr):
		return apperr.BadRequest(fmt.Sprintf("field %q must be %s", jsonPath(typeErr.Field), jsonTypeName(typeErr.Type)))
	}
	// Neither go-json nor encoding/json has a type for this; a test pins the text.
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		if unquoted, err := strconv.Unquote(name); err == nil {
			name = unquoted
		}
		return apperr.BadRequest(fmt.Sprintf("unknown field %q", name))
	}
	return apperr.BadRequest("request body can't be read")
}

// jsonTypeName names t the way a JSON client thinks of it.
func jsonTypeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Map, reflect.Struct:
		return "an object"
	default:
		return "a valid value"
	}
}

// jsonPath writes go-json's field path ("items.0.title") the way validation errors do
// ("items[0].title"), so the 400 and 422 replies name a field the same way.
func jsonPath(field string) string {
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

// fieldPath is the field's JSON path without the top struct's name: "items[0].name".
func fieldPath(fe validator.FieldError) string {
	_, path, ok := strings.Cut(fe.Namespace(), ".")
	if !ok {
		return fe.Field()
	}
	return path
}

// ruleMessage says what a failed validate rule needs, for the rules a template project is likely to
// use; any other rule gets a generic message, and its name is still in the reply's "rule".
func ruleMessage(fe validator.FieldError) string {
	param := fe.Param()
	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be an email address"
	case "url":
		return "must be a URL"
	case "uuid", "uuid4":
		return "must be a UUID"
	case "oneof":
		return "must be one of: " + strings.Join(strings.Fields(param), ", ")
	case "min", "gte":
		return "must be at least " + sizeOf(fe.Kind(), param)
	case "max", "lte":
		return "must be at most " + sizeOf(fe.Kind(), param)
	case "len":
		return "must be exactly " + sizeOf(fe.Kind(), param)
	case "gt":
		return "must be more than " + sizeOf(fe.Kind(), param)
	case "lt":
		return "must be less than " + sizeOf(fe.Kind(), param)
	default:
		return "is invalid"
	}
}

// sizeOf words a min/max limit: a length for strings and lists, a value for numbers.
func sizeOf(kind reflect.Kind, n string) string {
	switch kind {
	case reflect.String:
		return n + " characters long"
	case reflect.Slice, reflect.Array, reflect.Map:
		return n + " items"
	default:
		return n
	}
}
