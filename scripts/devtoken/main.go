// Command devtoken prints a sign-in token the local API accepts, for calling it by hand: make
// dev-token user=alice. It stands in for a real sign-in provider on your machine only.
//
// It reads AUTH_JWKS_URL, AUTH_ISSUER and AUTH_AUDIENCE as the API does (the environment, then
// .env). AUTH_JWKS_URL must be a file:// path: the first run makes a signing key next to it and
// publishes the key's public half there, so restart the API after it. The key stays in .dev/,
// which git ignores; the API refuses file:// keys in production.
package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/mdayat/go-template/internal/authtest"
	"github.com/mdayat/go-template/internal/config"
)

// tokenLifetime is how long a printed token works: a working day, so one token lasts a session.
const tokenLifetime = 12 * time.Hour

func main() {
	subject := "dev-user"
	if len(os.Args) > 1 && os.Args[1] != "" {
		subject = os.Args[1]
	}
	token, err := run(subject)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dev-token:", err)
		os.Exit(1)
	}
	fmt.Println(token)
}

func run(subject string) (string, error) {
	env, err := config.Env()
	if err != nil {
		return "", err
	}
	r := config.NewReader(env)
	app := r.App()
	keysURL := r.Required("AUTH_JWKS_URL")
	issuer := r.Required("AUTH_ISSUER")
	audience := r.Required("AUTH_AUDIENCE")
	if err := r.Err(); err != nil {
		return "", err
	}
	if app.Env == config.EnvProduction {
		return "", errors.New("APP_ENV is production (its default): tokens there come from the real sign-in provider; set APP_ENV=development for local runs")
	}
	return token(subject, keysURL, issuer, audience)
}

// token signs a token for subject with the key kept next to the keys file keysURL names.
func token(subject, keysURL, issuer, audience string) (string, error) {
	keysPath, err := filePath(keysURL)
	if err != nil {
		return "", err
	}
	key, err := loadOrMakeKey(keysPath)
	if err != nil {
		return "", err
	}
	return authtest.Sign(key, authtest.Claims{
		Subject: subject, Issuer: issuer, Audience: audience, ExpiresAt: time.Now().Add(tokenLifetime),
	})
}

// filePath returns the path a file:// URL names (file://.dev/auth/keys.json is relative to the
// repository root, file:///tmp/keys.json and file://localhost/tmp/keys.json absolute), as the API
// reads it.
func filePath(keysURL string) (string, error) {
	u, err := url.Parse(keysURL)
	if err != nil || u.Scheme != "file" {
		return "", errors.New("AUTH_JWKS_URL must be a file:// path for local tokens, e.g. file://.dev/auth/keys.json")
	}
	if u.Host == "localhost" {
		return u.Path, nil
	}
	return u.Host + u.Path, nil
}

// loadOrMakeKey returns the signing key kept next to keysPath, making it and publishing its public
// half at keysPath on the first run.
func loadOrMakeKey(keysPath string) (authtest.Key, error) {
	keyPath := filepath.Join(filepath.Dir(keysPath), "signing-key.pem")
	raw, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		key, err := parseKey(raw)
		if err != nil {
			return authtest.Key{}, err
		}
		return key, publish(keysPath, key)
	case !errors.Is(err, fs.ErrNotExist):
		return authtest.Key{}, fmt.Errorf("read the signing key: %w", err)
	}

	key, err := authtest.NewKey()
	if err != nil {
		return authtest.Key{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key.Private)
	if err != nil {
		return authtest.Key{}, fmt.Errorf("encode the signing key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keysPath), 0o700); err != nil {
		return authtest.Key{}, err
	}
	// The key ID rides along in the PEM header, so tokens signed later name the published key.
	block := &pem.Block{Type: "PRIVATE KEY", Headers: map[string]string{"Key-Id": key.ID}, Bytes: der}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return authtest.Key{}, err
	}
	if err := publish(keysPath, key); err != nil {
		return authtest.Key{}, err
	}
	fmt.Fprintf(os.Stderr, "dev-token: made a signing key and published it at %s; restart the API to trust it\n", keysPath)
	return key, nil
}

// publish writes key's public half to keysPath, where the API reads it. Rewritten every run, so a
// deleted keys file comes back.
func publish(keysPath string, key authtest.Key) error {
	set, err := authtest.KeySet(key)
	if err != nil {
		return err
	}
	return os.WriteFile(keysPath, set, 0o600) //nolint:gosec // G703: the path is the developer's own AUTH_JWKS_URL setting
}

func parseKey(raw []byte) (authtest.Key, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Headers["Key-Id"] == "" {
		return authtest.Key{}, errors.New("the signing key file is damaged: delete .dev/auth and run again")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return authtest.Key{}, fmt.Errorf("the signing key file is damaged (delete .dev/auth and run again): %w", err)
	}
	private, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return authtest.Key{}, errors.New("the signing key isn't an ECDSA key: delete .dev/auth and run again")
	}
	return authtest.Key{ID: block.Headers["Key-Id"], Private: private}, nil
}
