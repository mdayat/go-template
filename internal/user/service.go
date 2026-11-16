// Package user holds the people the app serves: our own record of them, apart from whichever
// sign-in provider proves who they are, so changing provider keeps their rows and rights. Other
// tables point at a user's ID.
package user

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mdayat/go-template/internal/db"
	"github.com/mdayat/go-template/internal/postgres"
)

// User is someone who has signed in.
type User struct {
	ID uuid.UUID
	// IsAdmin lets them see and change everyone's data.
	IsAdmin bool
}

// Service finds and records users.
type Service struct {
	queries *db.Queries
}

// NewService returns a Service that runs its queries through queries.
func NewService(queries *db.Queries) *Service {
	return &Service{queries: queries}
}

// SignIn returns the user the sign-in provider knows as subject, recording them on their first
// sign-in. Two first requests at once get the same user. A subject the table can't hold (empty,
// over 255 characters, holding NUL) fails as an unexpected error: the provider vouched for it, so
// it's a bug, not the caller's input.
func (s *Service) SignIn(ctx context.Context, subject string) (User, error) {
	row, err := s.queries.GetUserBySubject(ctx, subject)
	if err == nil {
		return fromRow(row), nil
	}
	if !postgres.IsNotFound(err) {
		return User{}, fmt.Errorf("sign in: find user: %w", err)
	}

	row, err = s.queries.CreateUser(ctx, subject)
	if postgres.IsNotFound(err) {
		// Another request created them between our read and our insert.
		row, err = s.queries.GetUserBySubject(ctx, subject)
		if err != nil {
			return User{}, fmt.Errorf("sign in: read the user another request created: %w", err)
		}
		return fromRow(row), nil
	}
	if err != nil {
		return User{}, fmt.Errorf("sign in: create user: %w", err)
	}
	return fromRow(row), nil
}

func fromRow(row db.User) User {
	return User{ID: row.ID, IsAdmin: row.IsAdmin}
}
