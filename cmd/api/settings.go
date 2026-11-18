package main

import (
	"errors"
	"net/netip"
	"net/url"
	"time"

	"github.com/mdayat/go-template/internal/api"
	"github.com/mdayat/go-template/internal/auth/jwks"
	"github.com/mdayat/go-template/internal/config"
	"github.com/mdayat/go-template/internal/startup"
)

// settings are what the API reads at startup; .example.env describes each.
type settings struct {
	app         config.App
	httpAddr    string
	databaseURL string
	dbLimits    startup.DBLimits
	auth        jwks.Config // whose sign-in tokens to accept
	limits      api.Limits  // all but Limiter, which run sets
	redisURL    string      // the Valkey that every copy of the API shares
	pprofAddr   string      // where to serve profiles; "" serves none
}

// loadSettings reads the API's settings from sources, the first winning, and reports every missing
// or invalid one in one error.
func loadSettings(sources ...config.Source) (settings, error) {
	r := config.NewReader(sources...)
	s := settings{
		app:         r.App(),
		httpAddr:    r.String("HTTP_ADDR", ":8080"),
		databaseURL: r.Required("DATABASE_URL"),
		// Above api.RequestTimeout, so a slow request gets the API's own 503 and its query is
		// cancelled with it; these catch what that cancel misses. No request needs a transaction open
		// longer.
		dbLimits: startup.DBLimits{
			Statement:         r.Duration("DB_STATEMENT_TIMEOUT", 10*time.Second),
			IdleInTransaction: r.Duration("DB_IDLE_IN_TRANSACTION_TIMEOUT", 10*time.Second),
		},
		auth: jwks.Config{
			KeysURL:  r.Required("AUTH_JWKS_URL"),
			Issuer:   r.Required("AUTH_ISSUER"),
			Audience: r.Required("AUTH_AUDIENCE"),
		},
		limits: api.Limits{
			TrustedProxies: r.IPRanges("TRUSTED_PROXIES"),
			PerIP:          r.Count("RATE_LIMIT_PER_IP", 600),
			PerUser:        r.Count("RATE_LIMIT_PER_USER", 120),
		},
		redisURL:  r.Required("REDIS_URL"),
		pprofAddr: r.String("PPROF_ADDR", ""),
	}
	err := errors.Join(r.Err(), checkKeysURL(s.app.Env, s.auth.KeysURL), checkTrustedProxies(s.limits.TrustedProxies),
		startup.CheckProfileAddr(s.pprofAddr))
	if err != nil {
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

// checkTrustedProxies refuses ranges that would let any client pick its IP (0.0.0.0/0, ::/0: every
// sender is "a proxy", so X-Forwarded-For is always believed), and IPv4 ranges written as IPv6
// (::ffff:10.0.0.0/104), which never match: chi compares a proxy's address in its IPv4 form.
func checkTrustedProxies(ranges []string) error {
	for _, r := range ranges {
		// The reader has already refused a list with an entry that doesn't parse, and returned none.
		prefix := netip.MustParsePrefix(r)
		switch {
		case prefix.Bits() == 0:
			return errors.New("TRUSTED_PROXIES must not hold 0.0.0.0/0 or ::/0: any client could then pick its IP")
		case prefix.Addr().Is4In6():
			return errors.New("TRUSTED_PROXIES must write IPv4 ranges as IPv4 (10.0.0.0/8, not ::ffff:10.0.0.0/104)")
		}
	}
	return nil
}
