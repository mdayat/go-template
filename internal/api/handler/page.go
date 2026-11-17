package handler

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mdayat/go-template/internal/apperr"
)

// DefaultPageSize is a list's page size when the request has no ?limit=.
const DefaultPageSize = 20

// PageLimit reads ?limit=, DefaultPageSize when absent. Whether it's in range is the service's
// rule; maxSize only words the reply to a limit that isn't a number.
func PageLimit(r *http.Request, maxSize int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return DefaultPageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, apperr.BadRequest(fmt.Sprintf("limit must be a whole number from 1 to %d", maxSize))
	}
	return n, nil
}

// Cursor is where a page of a list ends: lists are paged on (created_at, id), newest first.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// EncodeCursor makes the ?after= value for the page after c. It's opaque to clients: base64url of
// "<created_at RFC 3339>|<id>".
func EncodeCursor(c Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()))
}

// DecodeCursor reads ?after=, a cursor EncodeCursor made; nil when there's none.
func DecodeCursor(r *http.Request) (*Cursor, error) {
	s := r.URL.Query().Get("after")
	if s == "" {
		return nil, nil //nolint:nilnil // no cursor is the first page, not an error
	}
	invalid := apperr.BadRequest("after is not a cursor from a previous page")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, invalid
	}
	rawTime, rawID, found := strings.Cut(string(raw), "|")
	if !found {
		return nil, invalid
	}
	createdAt, err := time.Parse(time.RFC3339Nano, rawTime)
	if err != nil {
		return nil, invalid
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		return nil, invalid
	}
	return &Cursor{CreatedAt: createdAt, ID: id}, nil
}

// PathUUID reads the {name} path segment as a UUID. One that isn't a UUID can't name a row, so it's
// a 404 like any other unknown ID: "<what> not found".
func PathUUID(r *http.Request, name, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.UUID{}, apperr.NotFound(what + " not found")
	}
	return id, nil
}
