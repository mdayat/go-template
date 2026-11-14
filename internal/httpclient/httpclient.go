// Package httpclient builds the HTTP clients that call outside services, and sorts their failures
// into ones a retry may fix and ones it can't. Go's default client (http.Get, http.DefaultClient)
// has no time limit, so a stuck service would hold a request or a job forever; lint refuses it.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// New returns a client whose every call ends after timeout, from connecting to reading the whole
// reply. Each call's context can end it sooner. One client per outside service, kept for the
// program's life: it reuses connections.
//
// It doesn't follow redirects: a 3xx reply is returned as it is (CheckStatus makes it an error).
// Go would otherwise repeat a POST as a GET without its body after a 301, 302 or 303, so the call
// would seem to work while nothing was delivered, and carry our headers to another host.
func New(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		panic("httpclient: a time limit is required")
	}
	// Connecting and the TLS handshake get their own, shorter limit, so a host that's down fails
	// fast instead of using up the whole call's time.
	step := min(timeout, 5*time.Second)
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: step, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: step,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			ForceAttemptHTTP2:   true,
		},
	}
}

// StatusError is a reply whose status says the call failed.
type StatusError struct {
	Service string // who we called, e.g. "webhook"
	Code    int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s replied %d %s", e.Service, e.Code, http.StatusText(e.Code))
}

// CheckStatus returns a StatusError unless code is 2xx.
func CheckStatus(service string, code int) error {
	if code >= 200 && code < 300 {
		return nil
	}
	return &StatusError{Service: service, Code: code}
}

// Retryable reports whether trying the failed call again may work: the call itself failed (no
// connection, a time limit, a reply cut off) or the reply was 408, 429 or 5xx. Anything else fails
// the same way every time: any other 4xx or 3xx says the request is wrong, and an error from before
// the call (a body that can't be encoded, a URL that can't be parsed) is a bug.
func Retryable(err error) bool {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if _, isCall := errors.AsType[*CallError](err); isCall {
		return true
	}
	// Go's other *url.Errors (a URL that can't be parsed) also count as net.Errors, but no call was
	// made: client.Do's own are CallErrors once WithoutURL has turned them.
	if _, isURL := errors.AsType[*url.Error](err); isURL {
		return false
	}
	_, isNet := errors.AsType[net.Error](err)
	return isNet
}

// CallError is a call that failed before a reply came back (client.Do's error), without the URL.
type CallError struct {
	Method string
	Err    error
}

func (e *CallError) Error() string { return e.Method + ": " + e.Err.Error() }

func (e *CallError) Unwrap() error { return e.Err }

// WithoutURL returns client.Do's error as a CallError, without the URL, which can hold a key or
// token. The cause stays inside, so a time limit is still a context.DeadlineExceeded or a
// net.Error. Only for client.Do: other *url.Errors (a URL that can't be parsed) aren't calls.
func WithoutURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return &CallError{Method: ue.Op, Err: ue.Err}
	}
	return err
}
