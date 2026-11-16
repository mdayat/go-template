package jwks_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"

	"github.com/mdayat/go-template/internal/auth"
	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/authtest"
	"github.com/mdayat/go-template/internal/httpclient"
)

func newAuthenticator(t *testing.T, cfg jwks.Config) *jwks.Authenticator {
	t.Helper()
	a, err := jwks.New(t.Context(), cfg, httpclient.New(5*time.Second), zerolog.Nop())
	if err != nil {
		t.Fatalf("jwks.New() error = %v", err)
	}
	return a
}

func TestAuthenticateAcceptsAValidToken(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	a := newAuthenticator(t, issuer.Config())

	got, err := a.Authenticate(t.Context(), issuer.Token("alice"))

	if err != nil || got != (auth.Identity{Subject: "alice"}) {
		t.Errorf("Authenticate() = %+v, %v; want alice", got, err)
	}
}

// Every way a token can be wrong is refused as an invalid token (401), never as an outage.
func TestAuthenticateRefuses(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	valid := issuer.Token("alice")
	header, payload, _ := strings.Cut(valid, ".")
	otherPayload := strings.Split(issuer.Token("mallory"), ".")[1]
	unpublished, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	// Signed with a shared secret: an issuer's public key must never be usable as one (the classic
	// algorithm confusion).
	hs256, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: "alice", Issuer: authtest.IssuerName, Audience: jwt.ClaimStrings{authtest.Audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{
		Subject: "alice", Issuer: authtest.IssuerName, Audience: jwt.ClaimStrings{authtest.Audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"expired", issuer.Token("alice", authtest.Expired)},
		{"never running out", issuer.Token("alice", authtest.NoExpiry)},
		{"for another service", issuer.Token("alice", authtest.ForAudience("another-service"))},
		{"from another issuer", issuer.Token("alice", authtest.FromIssuer("https://evil.example.test"))},
		{"signed by a key the issuer doesn't publish", issuer.TokenSignedBy(unpublished, "alice")},
		{"payload swapped", header + "." + otherPayload + "." + strings.SplitN(payload, ".", 2)[1]},
		{"signed with a shared secret", hs256},
		{"not signed at all", unsigned},
		{"no subject", issuer.Token("")},
		{"not a token", "not-a-token"},
	}
	a := newAuthenticator(t, issuer.Config())
	for _, tt := range tests {
		got, err := a.Authenticate(t.Context(), tt.token)
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: Authenticate() = %+v, %v; want an auth.ErrInvalidToken", tt.name, got, err)
		}
	}
}

// An issuer rotating its key is followed: a token naming the new key makes the keys be fetched
// again.
func TestAuthenticateFollowsAKeyRotation(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	a := newAuthenticator(t, issuer.Config())
	if _, err := a.Authenticate(t.Context(), issuer.Token("alice")); err != nil {
		t.Fatalf("before rotation: Authenticate() error = %v", err)
	}

	issuer.Rotate()
	got, err := a.Authenticate(t.Context(), issuer.Token("alice"))

	if err != nil || got.Subject != "alice" {
		t.Errorf("after rotation: Authenticate() = %+v, %v; want alice", got, err)
	}
}

// Fetching the keys again for an unknown key ID happens at most once in a while, so a stream of
// made-up IDs is refused at once instead of holding requests open.
func TestAuthenticateDoesNotWaitToRefetchKeys(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	a := newAuthenticator(t, issuer.Config())
	unpublished, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	// Spends the one refetch the limit allows.
	if _, err := a.Authenticate(t.Context(), issuer.TokenSignedBy(unpublished, "alice")); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("first unknown key: Authenticate() error = %v, want an auth.ErrInvalidToken", err)
	}

	start := time.Now()
	_, err = a.Authenticate(t.Context(), issuer.TokenSignedBy(unpublished, "alice"))

	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("second unknown key: Authenticate() error = %v, want an auth.ErrInvalidToken", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("second unknown key took %s, want it refused at once", took)
	}
}

// An issuer that can't be reached doesn't stop the program starting, and isn't the caller's fault:
// the error isn't an invalid token, so the API answers 503, not 401.
func TestAuthenticateWithoutKeysIsAnOutage(t *testing.T) {
	t.Parallel()
	issuer := authtest.NewIssuer(t)
	down := httptest.NewServer(nil)
	down.Close()
	cfg := issuer.Config()
	cfg.KeysURL = down.URL

	a := newAuthenticator(t, cfg)
	_, err := a.Authenticate(t.Context(), issuer.Token("alice"))

	if err == nil || errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Authenticate() error = %v, want one that isn't an auth.ErrInvalidToken", err)
	}
	if strings.Contains(err.Error(), down.URL) {
		t.Errorf("Authenticate() error = %v, want it without the keys URL", err)
	}
}

// Once an issuer that was down at start is back, a token brings its keys: no restart needed. (While
// it's down, each token can try at most once in 5 minutes; this one is the first.)
func TestAuthenticateRecoversWhenTheIssuerIsBack(t *testing.T) {
	t.Parallel()
	key, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	set, err := authtest.KeySet(key)
	if err != nil {
		t.Fatal(err)
	}
	var up atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(set)
	}))
	t.Cleanup(server.Close)
	token, err := authtest.Sign(key, authtest.Claims{Subject: "alice", Issuer: authtest.IssuerName, Audience: authtest.Audience})
	if err != nil {
		t.Fatal(err)
	}
	a := newAuthenticator(t, jwks.Config{KeysURL: server.URL, Issuer: authtest.IssuerName, Audience: authtest.Audience})

	up.Store(true)
	got, err := a.Authenticate(t.Context(), token)

	if err != nil || got.Subject != "alice" {
		t.Errorf("Authenticate() after the issuer came back = %+v, %v; want alice", got, err)
	}
}

// For local runs the keys can come from a file (make dev-token writes it).
func TestAuthenticateWithKeysFromAFile(t *testing.T) {
	t.Parallel()
	key, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	set, err := authtest.KeySet(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, set, 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := authtest.Sign(key, authtest.Claims{Subject: "alice", Issuer: authtest.IssuerName, Audience: authtest.Audience})
	if err != nil {
		t.Fatal(err)
	}

	for _, keysURL := range []string{"file://" + path, "file://localhost" + path} {
		a := newAuthenticator(t, jwks.Config{KeysURL: keysURL, Issuer: authtest.IssuerName, Audience: authtest.Audience})
		got, err := a.Authenticate(t.Context(), token)

		if err != nil || got.Subject != "alice" {
			t.Errorf("%s: Authenticate() = %+v, %v; want alice", keysURL, got, err)
		}
	}
}

func TestNewRefuses(t *testing.T) {
	t.Parallel()
	ok := jwks.Config{KeysURL: "https://keys.example.test", Issuer: authtest.IssuerName, Audience: authtest.Audience}
	tests := []struct {
		name   string
		change func(*jwks.Config)
	}{
		{"no keys URL", func(c *jwks.Config) { c.KeysURL = "" }},
		{"no issuer", func(c *jwks.Config) { c.Issuer = "" }},
		{"no audience", func(c *jwks.Config) { c.Audience = "" }},
		{"another scheme", func(c *jwks.Config) { c.KeysURL = "ftp://keys.example.test" }},
		{"a keys file that isn't there", func(c *jwks.Config) { c.KeysURL = "file:///no/such/keys.json" }},
	}
	for _, tt := range tests {
		cfg := ok
		tt.change(&cfg)
		if a, err := jwks.New(t.Context(), cfg, httpclient.New(time.Second), zerolog.Nop()); err == nil {
			t.Errorf("%s: jwks.New() = %v, want an error", tt.name, a)
		}
	}
}
