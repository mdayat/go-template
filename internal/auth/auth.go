// Package auth holds what every sign-in provider shares: who a request comes from, as the provider
// proved it. Checking a token is a provider's job, in a package of its own (internal/auth/jwks, or
// another with the same shape); what the person may do is decided on our own data, never by the
// provider, so changing provider changes no rule.
package auth

import "errors"

// Identity is who signed in, as the provider knows them.
type Identity struct {
	// Subject is the provider's stable ID for the person (a JWT's "sub", Firebase's uid). It's unique
	// only within one provider.
	Subject string
}

// ErrInvalidToken marks a token the provider refused: forged, expired, meant for another service,
// or unreadable. Any other error from a provider means it couldn't check the token at all.
var ErrInvalidToken = errors.New("invalid token")
