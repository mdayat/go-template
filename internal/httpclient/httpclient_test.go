package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// callContext is a call's context with its own deadline well past the client's limits, so a client
// that lost its limit fails the test's own timing check instead of hanging it.
func callContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A service that never answers ends the call at the time limit, not never.
func TestNewEndsCallsAtTheTimeLimit(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	req, err := http.NewRequestWithContext(callContext(t), http.MethodGet, server.URL+"/?token=s3cret", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := New(200 * time.Millisecond).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("call took %s, want it ended near 200ms", took)
	}
	err = WithoutURL(err)
	if ne, ok := errors.AsType[net.Error](err); !ok || !ne.Timeout() {
		t.Errorf("error = %v, want a net.Error whose Timeout() is true", err)
	}
	if strings.Contains(fmt.Sprint(err), "s3cret") || strings.Contains(fmt.Sprint(err), server.URL) {
		t.Errorf("WithoutURL() = %v, want it without the URL", err)
	}
	if !Retryable(err) {
		t.Errorf("Retryable(%v) = false, want true", err)
	}
}

// A service that answers at once and then stalls in the middle of the body is cut off too: the
// limit covers the whole call, reading the reply included.
func TestNewEndsCallsThatStallMidBody(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	req, err := http.NewRequestWithContext(callContext(t), http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := New(200 * time.Millisecond).Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v, want the headers to arrive", err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err == nil {
		t.Error("reading the body succeeded, want it cut off")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("call took %s, want it ended near 200ms", took)
	}
}

// A redirect comes back as the 3xx reply, not followed: Go would repeat a POST as a GET without its
// body, so the call would look delivered when it wasn't, and our headers would go to another host.
func TestNewDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	followed := make(chan struct{}, 1)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed <- struct{}{} }))
	t.Cleanup(elsewhere.Close)
	server := httptest.NewServer(http.RedirectHandler(elsewhere.URL, http.StatusMovedPermanently))
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(callContext(t), http.MethodPost, server.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := New(5 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v, want the 301 reply", err)
	}
	_ = resp.Body.Close()

	err = CheckStatus("webhook", resp.StatusCode)
	if err == nil || Retryable(err) {
		t.Errorf("CheckStatus(%d) = %v, Retryable %v; want an error that isn't retried", resp.StatusCode, err, Retryable(err))
	}
	select {
	case <-followed:
		t.Error("the redirect was followed")
	default:
	}
}

func TestNewRefusesNoTimeLimit(t *testing.T) {
	t.Parallel()
	for _, timeout := range []time.Duration{0, -time.Second} {
		panicked := true
		func() {
			defer func() { _ = recover() }()
			New(timeout)
			panicked = false
		}()
		if !panicked {
			t.Errorf("New(%s) didn't panic, want it to: a client without a time limit can hang forever", timeout)
		}
	}
}

func TestCheckStatusAndRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code          int
		wantErr       bool
		wantRetryable bool
	}{
		{http.StatusOK, false, false},
		{http.StatusNoContent, false, false},
		{http.StatusMultipleChoices, true, false},
		{http.StatusMovedPermanently, true, false},
		{http.StatusBadRequest, true, false},
		{http.StatusUnauthorized, true, false},
		{http.StatusNotFound, true, false},
		{http.StatusConflict, true, false},
		{http.StatusRequestTimeout, true, true},
		{http.StatusTooManyRequests, true, true},
		{http.StatusInternalServerError, true, true},
		{http.StatusBadGateway, true, true},
		{http.StatusServiceUnavailable, true, true},
	}
	for _, tt := range tests {
		err := CheckStatus("webhook", tt.code)
		if (err != nil) != tt.wantErr || Retryable(err) != tt.wantRetryable {
			t.Errorf("CheckStatus(%d) = %v, Retryable %v; want error %v, retryable %v",
				tt.code, err, Retryable(err), tt.wantErr, tt.wantRetryable)
		}
	}
	if err := CheckStatus("webhook", http.StatusBadGateway); err.Error() != "webhook replied 502 Bad Gateway" {
		t.Errorf("CheckStatus(502).Error() = %q", err)
	}
	if wrapped := fmt.Errorf("report: %w", CheckStatus("webhook", http.StatusBadRequest)); Retryable(wrapped) {
		t.Errorf("Retryable(%v) = true, want false: wrapping keeps the status", wrapped)
	}
}

// Only a failed call is worth retrying; an error from before the call is a bug that fails the same
// way every time.
func TestRetryableErrorsWithoutAStatus(t *testing.T) {
	t.Parallel()
	badURL := &url.Error{Op: "parse", URL: "https://exa mple.com/", Err: errors.New(`invalid character " " in host name`)}
	badJSON := &json.UnsupportedTypeError{Type: reflect.TypeFor[chan int]()}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"a failed call", WithoutURL(&url.Error{Op: "Post", URL: "https://h/", Err: errors.New("connection reset by peer")}), true},
		{"a time limit", fmt.Errorf("report: %w", context.DeadlineExceeded), true},
		{"a reply cut off", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"a URL that can't be parsed", badURL, false},
		{"a body that can't be encoded", badJSON, false},
		{"anything else", errors.New("bug"), false},
	}
	for _, tt := range tests {
		if got := Retryable(tt.err); got != tt.want {
			t.Errorf("%s: Retryable(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}
