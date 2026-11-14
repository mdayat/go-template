package webhook

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdayat/go-template/internal/httpclient"
)

// Without a key, no Idempotency-Key header is sent rather than an empty one.
func TestSendWithoutAKeySendsNoHeader(t *testing.T) {
	t.Parallel()
	headers := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	if err := NewHTTPSender(httpclient.New(5*time.Second), server.URL).Send(t.Context(), "", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := <-headers; len(got.Values("Idempotency-Key")) != 0 {
		t.Errorf("Idempotency-Key = %q, want no header", got.Values("Idempotency-Key"))
	}
}

// The error never shows the URL, which can hold a token. A webhook that can't be reached is retried;
// a URL that can't be parsed is a bug a retry can't fix.
func TestSendErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		url           string
		wantRetryable bool
	}{
		// Nothing listens on port 1.
		{"unreachable", "http://127.0.0.1:1/hook?token=s3cret", true},
		{"not a URL", "https://exa mple.com/hook?token=s3cret", false},
	}
	for _, tt := range tests {
		err := NewHTTPSender(httpclient.New(5*time.Second), tt.url).Send(t.Context(), "key", []byte(`{}`))
		if err == nil || strings.Contains(fmt.Sprint(err), "s3cret") {
			t.Errorf("%s: Send() error = %v, want one without the URL", tt.name, err)
		}
		if got := httpclient.Retryable(err); got != tt.wantRetryable {
			t.Errorf("%s: Retryable(%v) = %v, want %v", tt.name, err, got, tt.wantRetryable)
		}
	}
}

func TestDiscardSendsNothing(t *testing.T) {
	t.Parallel()
	if err := (Discard{}).Send(t.Context(), "key", []byte(`{}`)); err != nil {
		t.Errorf("Discard.Send() error = %v, want nil", err)
	}
}
