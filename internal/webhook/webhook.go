// Package webhook tells another system about events by POSTing JSON to a URL it gave us. It's the
// template's example of calling an outside service: a client with a time limit, failures sorted
// into retryable or not, an idempotency key, no URL in errors. Code that reports defines the small
// interface it needs where it's used, and cmd/ passes in HTTPSender or Discard.
package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/mdayat/go-template/internal/httpclient"
)

// service names the webhook in errors, never its URL: a URL can hold a token.
const service = "webhook"

// HTTPSender POSTs events to one URL.
type HTTPSender struct {
	client *http.Client
	url    string
}

// NewHTTPSender returns a Sender that POSTs to url with client (httpclient.New).
func NewHTTPSender(client *http.Client, url string) *HTTPSender {
	return &HTTPSender{client: client, url: url}
}

// Send POSTs body, a JSON event, with idempotencyKey as its Idempotency-Key header. The key is the
// same on every try of one piece of work (a job's ID), so the receiver can drop repeats; "" sends
// none. Any 2xx reply is a success; check a failure with httpclient.Retryable.
func (s *HTTPSender) Send(ctx context.Context, idempotencyKey string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		// Not wrapped: the parse error quotes the URL. Settings check it at startup, so this is a bug.
		return fmt.Errorf("build %s request: the URL can't be parsed", service)
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", service, httpclient.WithoutURL(err))
	}
	defer resp.Body.Close()
	// Read what's left (up to 64 KB) so the connection can be reused; the body itself isn't needed.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return httpclient.CheckStatus(service, resp.StatusCode)
}

// Discard sends nothing, for when no webhook is set up.
type Discard struct{}

// Send does nothing.
func (Discard) Send(context.Context, string, []byte) error { return nil }
