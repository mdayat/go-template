package main

import (
	"testing"

	"github.com/mdayat/go-template/internal/webhook"
)

// No PURGE_WEBHOOK_URL means no reports, not a client with an empty URL failing every purge.
func TestPurgeReportsWithoutAURLSendNothing(t *testing.T) {
	t.Parallel()
	if r := purgeReports(""); r != (webhook.Discard{}) {
		t.Errorf("purgeReports(\"\") = %T, want webhook.Discard", r)
	}
	if r := purgeReports("https://hooks.example.com/purged"); r == (webhook.Discard{}) {
		t.Errorf("purgeReports(a URL) = %T, want *webhook.HTTPSender", r)
	}
}
