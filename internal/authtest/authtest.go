// Package authtest is a sign-in provider for tests and local runs: it issues tokens and publishes
// its public key the way a real issuer does, so the real check (internal/auth/jwks) runs on them.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/json"
)

// The issuer and audience test tokens carry, and test Authenticators expect.
const (
	IssuerName = "https://issuer.example.test"
	Audience   = "go-template"
)

// Key is a signing key and the ID its tokens name in their header.
type Key struct {
	ID      string
	Private *ecdsa.PrivateKey
}

// NewKey makes a P-256 key: ES256, fast to make, so every test can have its own.
func NewKey() (Key, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("make signing key: %w", err)
	}
	return Key{ID: rand.Text(), Private: private}, nil
}

// KeySet returns the JWK Set publishing the public half of keys, as an issuer serves it.
func KeySet(keys ...Key) ([]byte, error) {
	type jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		Use string `json:"use"`
	}
	set := struct {
		Keys []jwk `json:"keys"`
	}{Keys: []jwk{}}
	for _, k := range keys {
		public, err := k.Private.PublicKey.ECDH()
		if err != nil {
			return nil, fmt.Errorf("publish key: %w", err)
		}
		point := public.Bytes() // 0x04, then X and Y, 32 bytes each
		enc := base64.RawURLEncoding.EncodeToString
		set.Keys = append(set.Keys, jwk{
			Kty: "EC", Crv: "P-256", X: enc(point[1:33]), Y: enc(point[33:65]), Kid: k.ID, Alg: "ES256", Use: "sig",
		})
	}
	return json.Marshal(set)
}

// Claims are what a token says. Zero times mean "now" (IssuedAt) and "in an hour" (ExpiresAt).
type Claims struct {
	Subject   string
	Issuer    string
	Audience  string
	IssuedAt  time.Time
	ExpiresAt time.Time
	NoExpiry  bool // leaves out "exp": a token that would never run out
}

// Sign returns a token for c, signed with k.
func Sign(k Key, c Claims) (string, error) {
	now := time.Now()
	if c.IssuedAt.IsZero() {
		c.IssuedAt = now
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = now.Add(time.Hour)
	}
	claims := jwt.RegisteredClaims{
		Subject:   c.Subject,
		Issuer:    c.Issuer,
		Audience:  jwt.ClaimStrings{c.Audience},
		IssuedAt:  jwt.NewNumericDate(c.IssuedAt),
		ExpiresAt: jwt.NewNumericDate(c.ExpiresAt),
	}
	if c.NoExpiry {
		claims.ExpiresAt = nil
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = k.ID
	return token.SignedString(k.Private)
}

// Issuer is a test's own sign-in provider, publishing its key over HTTP.
type Issuer struct {
	tb      testing.TB
	keysURL string

	mu  sync.Mutex
	key Key
}

// NewIssuer starts an issuer that serves its key set until tb ends.
func NewIssuer(tb testing.TB) *Issuer {
	tb.Helper()
	i := &Issuer{tb: tb, key: mustKey(tb)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i.mu.Lock()
		set, err := KeySet(i.key)
		i.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(set)
	}))
	tb.Cleanup(server.Close)
	i.keysURL = server.URL
	return i
}

// Config is the jwks.Config that accepts this issuer's tokens.
func (i *Issuer) Config() jwks.Config {
	return jwks.Config{KeysURL: i.keysURL, Issuer: IssuerName, Audience: Audience}
}

// Token returns a valid token for subject; each change applies before signing (Expired,
// ForAudience...).
func (i *Issuer) Token(subject string, changes ...func(*Claims)) string {
	i.tb.Helper()
	c := Claims{Subject: subject, Issuer: IssuerName, Audience: Audience}
	for _, change := range changes {
		change(&c)
	}
	i.mu.Lock()
	key := i.key
	i.mu.Unlock()
	return mustSign(i.tb, key, c)
}

// TokenSignedBy returns a valid-looking token for subject signed with k, which the issuer doesn't
// publish unless it rotated to it.
func (i *Issuer) TokenSignedBy(k Key, subject string) string {
	i.tb.Helper()
	return mustSign(i.tb, k, Claims{Subject: subject, Issuer: IssuerName, Audience: Audience})
}

// Rotate replaces the published key with a new one and returns it, as issuers do from time to time.
func (i *Issuer) Rotate() Key {
	i.tb.Helper()
	k := mustKey(i.tb)
	i.mu.Lock()
	i.key = k
	i.mu.Unlock()
	return k
}

// Expired makes a token that ran out an hour ago.
func Expired(c *Claims) {
	c.IssuedAt = time.Now().Add(-2 * time.Hour)
	c.ExpiresAt = time.Now().Add(-time.Hour)
}

// NoExpiry makes a token that never runs out.
func NoExpiry(c *Claims) { c.NoExpiry = true }

// ForAudience makes a token meant for another service.
func ForAudience(audience string) func(*Claims) {
	return func(c *Claims) { c.Audience = audience }
}

// FromIssuer makes a token naming another issuer.
func FromIssuer(issuer string) func(*Claims) {
	return func(c *Claims) { c.Issuer = issuer }
}

func mustKey(tb testing.TB) Key {
	tb.Helper()
	k, err := NewKey()
	if err != nil {
		tb.Fatal(err)
	}
	return k
}

func mustSign(tb testing.TB, k Key, c Claims) string {
	tb.Helper()
	token, err := Sign(k, c)
	if err != nil {
		tb.Fatalf("sign token: %v", err)
	}
	return token
}
