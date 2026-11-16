package main

import (
	"errors"
	"net/url"

	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/config"
)

// settings are what the API reads at startup; .example.env describes each.
type settings struct {
	app         config.App
	httpAddr    string
	databaseURL string
	auth        jwks.Config // whose sign-in tokens to accept
	// remove:worker
	redisURL string // the job queue
	// remove:end
}

// loadSettings reads the API's settings from sources, the first winning, and reports every missing
// or invalid one in one error.
func loadSettings(sources ...config.Source) (settings, error) {
	r := config.NewReader(sources...)
	s := settings{
		app:         r.App(),
		httpAddr:    r.String("HTTP_ADDR", ":8080"),
		databaseURL: r.Required("DATABASE_URL"),
		auth: jwks.Config{
			KeysURL:  r.Required("AUTH_JWKS_URL"),
			Issuer:   r.Required("AUTH_ISSUER"),
			Audience: r.Required("AUTH_AUDIENCE"),
		},
		// remove:worker
		redisURL: r.Required("REDIS_URL"),
		// remove:end
	}
	if err := errors.Join(r.Err(), checkKeysURL(s.app.Env, s.auth.KeysURL)); err != nil {
		return settings{}, err
	}
	return s, nil
}

// checkKeysURL refuses, in production, keys from anywhere but https://: a local file (make
// dev-token's) or plain http:// would let whoever can write the file, or sits on the network, sign
// in as anyone. Its error doesn't quote the value.
func checkKeysURL(env, keysURL string) error {
	if keysURL == "" {
		return nil // reported as missing already
	}
	u, err := url.Parse(keysURL)
	switch {
	case err == nil && u.Scheme == "https" && u.Host != "":
		return nil
	case err == nil && (u.Scheme == "file" || u.Scheme == "http") && env != config.EnvProduction:
		return nil
	case env == config.EnvProduction:
		return errors.New("AUTH_JWKS_URL must be an https:// URL in production (APP_ENV defaults to production; set APP_ENV=development for local runs)")
	default:
		return errors.New("AUTH_JWKS_URL must be an https://, http:// or file:// URL")
	}
}
