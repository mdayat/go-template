// Package jwks signs people in with JSON Web Tokens, checked against the public keys their issuer
// publishes as a JWK Set. Firebase, Auth0, Cognito, Keycloak and Supabase all issue such tokens; a
// token must name this service in "aud" and be signed RS256 or ES256, which some need set up
// (Cognito: its ID tokens, not access tokens; Keycloak: an audience mapper). Only this package
// (and the test issuer) imports the JWT libraries (lint: depguard).
package jwks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/auth"
	"github.com/mdayat/go-template/internal/httpclient"
)

// Config says whose tokens to accept.
type Config struct {
	// KeysURL is the issuer's JWK Set: an https:// URL; for local runs an http:// one (a provider's
	// emulator) or a file:// path (make dev-token writes one).
	KeysURL string
	// Issuer must equal a token's "iss".
	Issuer string
	// Audience must be in a token's "aud": a token the issuer made for another service is refused.
	Audience string
}

// Authenticator checks tokens against the issuer's keys.
type Authenticator struct {
	keys   keyfunc.Keyfunc
	parser *jwt.Parser
}

// New returns an Authenticator for cfg. For an https:// KeysURL it fetches the keys with client and
// refreshes them hourly in the background until ctx ends, logging failures to logger. It doesn't
// fail when the issuer can't be reached: Authenticate reports that until the keys arrive, so the
// rest of the API keeps serving.
func New(ctx context.Context, cfg Config, client *http.Client, logger zerolog.Logger) (*Authenticator, error) {
	if cfg.KeysURL == "" || cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("jwks: keys URL, issuer and audience are all required")
	}
	u, err := url.Parse(cfg.KeysURL)
	if err != nil {
		return nil, errors.New("jwks: the keys URL can't be parsed")
	}

	var keys keyfunc.Keyfunc
	switch u.Scheme {
	case "file":
		raw, err := os.ReadFile(filePath(u))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("jwks: no keys file at %s: run make dev-token first", filePath(u))
		}
		if err != nil {
			return nil, fmt.Errorf("jwks: read the keys file: %w", err)
		}
		if keys, err = keyfunc.NewJWKSetJSON(raw); err != nil {
			return nil, fmt.Errorf("jwks: read the keys file: %w", err)
		}
	case "https", "http":
		keys, err = keyfunc.NewDefaultOverrideCtx(ctx, []string{cfg.KeysURL}, keyfunc.Override{
			Client: client,
			// A token signed with a key we don't have makes it fetch the keys again, at most once in
			// 5 minutes (the library's default). This caps how long a request may spend on that fetch
			// (by default up to a minute, past the request's own time limit); without a fetch to spare
			// the token is refused, so made-up key IDs can't hold requests open.
			RateLimitWaitMax: 3 * time.Second,
			RefreshErrorHandlerFunc: func(string) func(context.Context, error) {
				return func(_ context.Context, err error) {
					logger.Error().Err(httpclient.WithoutURL(err)).Msg("fetch sign-in keys")
				}
			},
		})
		if err != nil {
			return nil, fmt.Errorf("jwks: %w", httpclient.WithoutURL(err))
		}
	default:
		return nil, errors.New("jwks: the keys URL must be https://, http:// or file://")
	}

	return &Authenticator{
		keys: keys,
		parser: jwt.NewParser(
			// Only the asymmetric algorithms issuers publish keys for. The JWT library already refuses
			// "none" and a public key used as a shared secret (HS256); this is a second lock on both.
			jwt.WithValidMethods([]string{"RS256", "ES256"}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
			// Clocks differ a little between the issuer and us.
			jwt.WithLeeway(30*time.Second),
		),
	}, nil
}

// Authenticate returns who token belongs to. A token it refuses is an auth.ErrInvalidToken; any
// other error means it couldn't check tokens at all (the issuer's keys never arrived).
func (a *Authenticator) Authenticate(ctx context.Context, token string) (auth.Identity, error) {
	var claims jwt.RegisteredClaims
	if _, err := a.parser.ParseWithClaims(token, &claims, a.keys.KeyfuncCtx(ctx)); err != nil {
		if !a.haveKeys(ctx) {
			return auth.Identity{}, fmt.Errorf("no sign-in keys fetched yet: %w", err)
		}
		return auth.Identity{}, fmt.Errorf("%w: %w", auth.ErrInvalidToken, err)
	}
	if claims.Subject == "" {
		return auth.Identity{}, fmt.Errorf("%w: no subject", auth.ErrInvalidToken)
	}
	return auth.Identity{Subject: claims.Subject}, nil
}

// filePath is the path a file:// URL names: file://.dev/keys.json is relative (to where the program
// runs), file:///tmp/keys.json and file://localhost/tmp/keys.json absolute.
func filePath(u *url.URL) string {
	if u.Host == "localhost" {
		return u.Path
	}
	return u.Host + u.Path
}

// haveKeys reports whether any of the issuer's keys have arrived. Without them every token would
// look invalid, which is our outage, not the caller's fault.
func (a *Authenticator) haveKeys(ctx context.Context) bool {
	keys, err := a.keys.Storage().KeyReadAll(ctx)
	return err == nil && len(keys) > 0
}
