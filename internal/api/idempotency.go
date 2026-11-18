package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/apperr"
	"github.com/mdayat/go-template/internal/idempotency"
)

const (
	// idempotencyHeader carries the client's key for an action, the same on every try of it.
	idempotencyHeader = "Idempotency-Key"
	// replayedHeader marks a reply sent again for a retry, not made by running the request.
	replayedHeader = "Idempotent-Replayed"
	// idempotencyHold is how long a request holds its key while it runs: well past RequestTimeout,
	// so a retry can't run beside it. If the program dies mid-request the hold runs out and a retry
	// runs again. An endpoint that works longer (handler.Detach) needs a longer hold.
	idempotencyHold = time.Minute
	// idempotencyKeep is how long a reply is kept for retries: a day, longer than any client keeps
	// retrying one action.
	idempotencyKeep = 24 * time.Hour
	// maxKeptReply is the largest reply body kept. A larger one is kept as "done, too large to
	// keep": the request still never runs twice. It bounds what one key costs Valkey's memory for a
	// day: a signed-in user can make as many keys as their rate limit lets them.
	maxKeptReply = 64 << 10
)

// keepReplies lets a client retry a POST safely (see docs/standards/http-api.md): a POST with an
// Idempotency-Key runs once per signed-in user and key, and a retry with the same key and request
// gets the first reply back, marked Idempotent-Replayed, without running again. Requests without
// the header, and other methods, which already give the same result twice, pass straight through.
//
// Whatever reply the handler wrote is kept, even if its client has gone or its time ran out: the
// work may be saved, so a retry must not run it again. The key is freed only when nothing was
// written and the request was cut off, or on a 5xx (our failure; a retry may succeed), 401 or 403
// (who's signed in, and what they may do, can change), or a panic (a bug, even mid-reply). When store can't be reached a keyed request
// gets a 503 rather than run unguarded. A keyed body is read whole first, to fingerprint it, so it
// has the JSON endpoints' 1 MB limit.
func keepReplies(store idempotency.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientKey := r.Header.Get(idempotencyHeader)
			if r.Method != http.MethodPost || clientKey == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !validIdempotencyKey(clientKey) {
				handler.WriteError(w, r, &handler.StatusError{
					Status: http.StatusBadRequest,
					Detail: "Idempotency-Key must be 1 to 255 visible ASCII characters, like a UUID",
				})
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, handler.MaxBodyBytes))
			switch _, tooLarge := errors.AsType[*http.MaxBytesError](err); {
			case tooLarge:
				// The same replies the decoder gives (handler.Decode).
				handler.WriteError(w, r, &handler.StatusError{Status: http.StatusRequestEntityTooLarge, Detail: "request body must be at most 1 MB"})
				return
			case err != nil:
				// A client that left gets no reply (WriteError); one that sent less than it said, a 400.
				handler.WriteError(w, r, fmt.Errorf("read request body: %w: %w", err, apperr.BadRequest("request body can't be read")))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			key := handler.CurrentUser(r).ID.String() + "\x00" + clientKey
			claim, err := store.Begin(r.Context(), key, fingerprint(r, body), idempotencyHold)
			switch {
			case err != nil:
				// A client that left meanwhile gets no reply and is logged as gone, even when Valkey's
				// own time limit ended the call (go-redis doesn't stop for a canceled context mid-call).
				if ctxErr := r.Context().Err(); ctxErr != nil {
					err = errors.Join(err, ctxErr)
				}
				// The cause goes on the log line; the client sees only the StatusError.
				handler.WriteError(w, r, fmt.Errorf("%w: %w", err, &handler.StatusError{
					Status: http.StatusServiceUnavailable,
					Detail: "the Idempotency-Key can't be checked right now; try again",
				}))
			case claim.Mismatch:
				handler.WriteError(w, r, &handler.StatusError{
					Status: http.StatusUnprocessableEntity,
					Detail: "this Idempotency-Key was used for a different request; use a new key for a new action",
				})
			case claim.Running:
				w.Header().Set("Retry-After", "1")
				handler.WriteError(w, r, &handler.StatusError{
					Status: http.StatusConflict,
					Detail: "a request with this Idempotency-Key is still running; try again shortly",
				})
			case claim.Reply != nil:
				replay(w, r, *claim.Reply)
			default:
				runAndKeep(w, r, next, store, key, claim.Token)
			}
		})
	}
}

// runAndKeep runs the request holding key, then keeps its reply or frees the key.
func runAndKeep(w http.ResponseWriter, r *http.Request, next http.Handler, store idempotency.Store, key, token string) {
	// The capture sits under chi's wrapper, which handlers check for a started reply, and sees every
	// byte the handler writes, even those a gone client never got.
	capture := &captureWriter{ResponseWriter: w}
	ww := middleware.NewWrapResponseWriter(capture, r.ProtoMajor)
	// The key must be settled even when the request's context has ended (a timeout, a client that
	// left), and even if the handler panics: deferred, on a context of its own.
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	kept := false
	defer func() {
		if !kept {
			if err := store.Release(endCtx, key, token); err != nil {
				noteIdempotencyError(r, err)
			}
		}
	}()

	next.ServeHTTP(ww, r)

	status := ww.Status()
	switch {
	case status == 0 && r.Context().Err() != nil:
		return // cut off before any reply: freed by the deferred Release
	case status == 0:
		status = http.StatusOK // nothing written: net/http sends an empty 200
	case status >= http.StatusInternalServerError, status == http.StatusUnauthorized, status == http.StatusForbidden:
		return
	}
	reply := idempotency.Reply{Status: status, Header: map[string]string{}, Body: capture.body.Bytes(), TooLarge: capture.body.overflow}
	if reply.TooLarge {
		reply.Body = nil
	}
	for _, h := range []string{"Content-Type", "Location"} {
		if v := ww.Header().Get(h); v != "" {
			reply.Header[h] = v
		}
	}
	if err := store.Finish(endCtx, key, token, reply, idempotencyKeep); err != nil {
		noteIdempotencyError(r, err)
		return
	}
	kept = true
}

// captureWriter keeps a copy of the reply's body as the handler writes it.
type captureWriter struct {
	http.ResponseWriter
	body cappedBuffer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	_, _ = c.body.Write(p)
	return c.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection (read deadlines, flushing).
func (c *captureWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// Flush sends what's written so far, when the connection can.
func (c *captureWriter) Flush() {
	_ = http.NewResponseController(c.ResponseWriter).Flush()
}

// replay sends a kept reply again. One too large to keep gets a 409 saying the request ran, with its
// Location, if any, to fetch the result from.
func replay(w http.ResponseWriter, r *http.Request, reply idempotency.Reply) {
	w.Header().Set(replayedHeader, "true")
	hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Bool("idempotent_replay", true)
	})
	if reply.TooLarge {
		if loc := reply.Header["Location"]; loc != "" {
			w.Header().Set("Location", loc)
		}
		handler.WriteError(w, r, &handler.StatusError{
			Status: http.StatusConflict,
			Detail: "the request with this Idempotency-Key already ran; its reply was too large to keep",
		})
		return
	}
	for h, v := range reply.Header {
		w.Header().Set(h, v)
	}
	if reply.Header["Content-Type"] != "" {
		// As WriteJSON sets on every reply: a reply echoing client input isn't read as HTML.
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	w.WriteHeader(reply.Status)
	// A write error means the client is gone; there's no one left to tell.
	_, _ = w.Write(reply.Body)
}

// noteIdempotencyError puts on the request's log line that its reply wasn't kept, or its key not
// freed (the reply has gone out already, so this is all that's left to do).
func noteIdempotencyError(r *http.Request, err error) {
	hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("idempotency_error", err.Error())
	})
}

// fingerprint is what a request asks, to tell a retry from another request reusing its key.
func fingerprint(r *http.Request, body []byte) string {
	h := sha256.New()
	_, _ = io.WriteString(h, r.Method+" "+r.URL.RequestURI()+"\n")
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// validIdempotencyKey reports whether key is 1 to 255 visible ASCII characters.
func validIdempotencyKey(key string) bool {
	if len(key) > 255 {
		return false
	}
	for i := range len(key) {
		if key[i] < '!' || key[i] > '~' {
			return false
		}
	}
	return key != ""
}

// cappedBuffer keeps what's written to it, up to maxKeptReply; past that it notes the overflow and
// keeps nothing more.
type cappedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.overflow || b.Len()+len(p) > maxKeptReply {
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
