package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDetach(t *testing.T) {
	t.Parallel()
	shutdown, giveUp := context.WithCancel(t.Context())
	defer giveUp()
	requestCtx, clientLeft := context.WithCancel(WithShutdown(t.Context(), shutdown))
	r := httptest.NewRequestWithContext(requestCtx, "POST", "/upload", nil)

	long, cancel := Detach(r, time.Hour)
	defer cancel()
	clientLeft()
	if long.Err() != nil {
		t.Fatalf("the long context ended with its request: %v", long.Err())
	}
	giveUp()
	select {
	case <-long.Done():
	case <-time.After(time.Second):
		t.Fatal("the long context outlived the server giving up on shutdown")
	}

	short, cancel := Detach(httptest.NewRequestWithContext(t.Context(), "POST", "/upload", nil), 10*time.Millisecond)
	defer cancel()
	select {
	case <-short.Done():
	case <-time.After(time.Second):
		t.Fatal("the long context outlived its own time limit")
	}
}
