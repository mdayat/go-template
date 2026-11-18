package main

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
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
	// shutdownDelay is how long the API keeps serving after SIGTERM with /readyz failing, so load
	// balancers stop sending it requests before it stops taking them.
	shutdownDelay time.Duration
	// allowedOrigins are the sites whose web pages may call the API from a browser; none: no other
	// site's.
	allowedOrigins []string
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
			MaxConns:          r.Count("DB_MAX_CONNS", 10),
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
		redisURL:      r.Required("REDIS_URL"),
		pprofAddr:     r.String("PPROF_ADDR", ""),
		shutdownDelay: r.Duration("SHUTDOWN_DELAY", 0),
	}
	origins, originsErr := readOrigins(r.String("CORS_ALLOWED_ORIGINS", ""), s.app.Env)
	s.allowedOrigins = origins
	err := errors.Join(r.Err(), checkKeysURL(s.app.Env, s.auth.KeysURL), checkTrustedProxies(s.limits.TrustedProxies),
		startup.CheckProfileAddr(s.pprofAddr), checkShutdownDelay(s.shutdownDelay), startup.CheckMaxConns(s.dbLimits.MaxConns),
		originsErr)
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

// maxShutdownDelay is the longest SHUTDOWN_DELAY: load balancers notice a failing /readyz within
// seconds, and the platform kills a program that takes longer to stop than it allows.
const maxShutdownDelay = time.Minute

func checkShutdownDelay(d time.Duration) error {
	if d > maxShutdownDelay {
		return fmt.Errorf("SHUTDOWN_DELAY must be at most %s, got %s", maxShutdownDelay, d)
	}
	return nil
}

// readOrigins reads CORS_ALLOWED_ORIGINS, a comma-separated list of sites written as a browser
// sends them in Origin: scheme, host and a port if not the scheme's own (https://app.example.com,
// http://localhost:5173), with nothing after. Each must be named: a "*" anywhere is a wildcard to
// rs/cors ("https://*" lets a page on any site read the API's replies, "https://*example.com"
// also evilexample.com). In production only https:// will do: a page served over plain http can be
// changed by anyone on its network. An entry a browser never sends (a default port, :443; a
// non-ASCII host, sent in its xn-- form) would never match, so it's refused too. Returned
// lowercase, as browsers send them.
func readOrigins(value, env string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var origins []string
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		u, err := url.Parse(entry)
		switch {
		case entry == "*":
			return nil, errors.New("CORS_ALLOWED_ORIGINS must name each site, not *")
		// Anything besides scheme://host[:port] (a path, a trailing /, ?, #, user@) changes the text.
		case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			strings.ToLower(entry) != u.Scheme+"://"+strings.ToLower(u.Host):
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS: %q isn't a site like https://app.example.com (scheme and host, nothing after)", entry)
		case !isOriginHost(u):
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS: %q must name one site as a browser writes it: a name of letters, digits, - and ., or an IP address in its shortest form, and a port only if not the scheme's own (no *)", entry)
		case u.Scheme == "http" && env == config.EnvProduction:
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS: %q must be https:// in production", entry)
		}
		origins = append(origins, strings.ToLower(entry))
	}
	return origins, nil
}

// isOriginHost reports whether u's host and port are written as a browser writes them in Origin:
// an IP address in its shortest form (127.0.0.1, [::1]; not 127.1 or [0:0:0:0:0:0:0:1]), or a
// name of ASCII letters, digits, "-" and "." whose last part isn't a number (a browser reads
// 127.000.000.001 as an address and rewrites it); and a port, if any, from 1 to 65535, without
// leading zeros, and not the scheme's own.
func isOriginHost(u *url.URL) bool {
	if port := u.Port(); strings.HasSuffix(u.Host, ":") || port != "" && !isOriginPort(port, u.Scheme) {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.String() == host && !ip.Is4In6()
	}
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for _, c := range host {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	last := host[strings.LastIndex(host, ".")+1:]
	return strings.TrimLeft(last, "0123456789") != "" && !strings.HasPrefix(last, "0x")
}

// isOriginPort reports whether port is one a browser keeps in Origin for scheme.
func isOriginPort(port, scheme string) bool {
	n, err := strconv.Atoi(port)
	defaultPort := map[string]int{"https": 443, "http": 80}[scheme]
	return err == nil && n >= 1 && n <= 65535 && port[0] != '0' && n != defaultPort
}
