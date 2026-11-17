package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/httpclient"
)

// A token is accepted by the real check reading the keys file it published; later runs reuse the
// key, and a deleted keys file comes back.
func TestTokenIsAcceptedWithThePublishedKeys(t *testing.T) {
	t.Parallel()
	keysURL := "file://" + filepath.Join(t.TempDir(), "auth", "keys.json")
	cfg := jwks.Config{KeysURL: keysURL, Issuer: "https://dev.local", Audience: "app"}

	first, err := token("alice", keysURL, cfg.Issuer, cfg.Audience)
	if err != nil {
		t.Fatalf("token() error = %v", err)
	}
	if err := os.Remove(keysURL[len("file://"):]); err != nil {
		t.Fatal(err)
	}
	second, err := token("bob", keysURL, cfg.Issuer, cfg.Audience)
	if err != nil {
		t.Fatalf("second token() error = %v", err)
	}

	authn, err := jwks.New(t.Context(), cfg, httpclient.New(time.Second, tracenoop.NewTracerProvider()), zerolog.Nop())
	if err != nil {
		t.Fatalf("jwks.New() error = %v", err)
	}
	for subject, tok := range map[string]string{"alice": first, "bob": second} {
		got, err := authn.Authenticate(t.Context(), tok)
		if err != nil || got.Subject != subject {
			t.Errorf("Authenticate(%s's token) = %+v, %v; want %s", subject, got, err, subject)
		}
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(keysURL[len("file://"):]), "signing-key.pem"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("signing key = %v, %v; want a file only its owner can read", info, err)
	}
}

func TestTokenNeedsAKeysFile(t *testing.T) {
	t.Parallel()
	if _, err := token("alice", "https://keys.example.com/jwks.json", "i", "a"); err == nil {
		t.Error("token() with an https:// keys URL error = nil, want one: there's no local key to sign with")
	}
}

// Tokens in production come from the real provider; a dev key there would let anyone sign in.
// Not parallel: it sets the environment.
func TestRunRefusesProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("AUTH_JWKS_URL", "file://"+filepath.Join(t.TempDir(), "keys.json"))
	t.Setenv("AUTH_ISSUER", "https://dev.local")
	t.Setenv("AUTH_AUDIENCE", "app")

	if tok, err := run("alice"); err == nil {
		t.Errorf("run() = %q, nil; want an error in production", tok)
	}
}
