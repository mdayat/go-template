# Calling outside services

An outside service is anything we call over the network that isn't our own: another
company's API (payments, email, storage), a webhook, another team's service. These calls fail in
ways our own code doesn't: they hang, they're down for a minute, they limit how often we call, and
a call that timed out on our side may still have worked on theirs.

The example is `internal/webhook`, which POSTs events to a URL another system gave us, with its
tests; it stays whichever programs a project keeps.
<!-- remove:worker -->
The worker's purge job uses it: after deleting old done tasks, it reports to `PURGE_WEBHOOK_URL`
(optional).
<!-- remove:end -->

## Behind an interface, passed in from `cmd/`

Code that calls a service defines a small interface for what it needs, where it uses it, never
taking the HTTP client or the provider's SDK. `cmd/` builds the
implementation (`webhook.NewHTTPSender`) and passes it in, so moving to another provider is a new
implementation, and a setting that's not filled in can pass one that does nothing
(`webhook.Discard`).
<!-- remove:worker -->
The worker's is `worker.EventSender`, and `cmd/worker` picks its implementation.
<!-- remove:end -->

The implementation lives in a package of its own, named after what it does (`internal/webhook`,
`internal/mailer`), not after the provider.

## Every call has a time limit

Go's default HTTP client (`http.Get`, `http.DefaultClient`) waits forever: one stuck service then
holds a request, a database connection or a job until the process restarts. So:

- Build the client with `httpclient.New(timeout)`. It limits the whole call, from connecting to
  reading the reply, and connecting and the TLS handshake on their own. Lint (forbidigo) refuses
  the default client and transport; a client you build yourself (`&http.Client{}`) isn't caught,
  so review catches that one.
- It doesn't follow redirects: Go would repeat a POST as a GET without its body after a 301, 302 or
  303, so a "moved" service would look delivered while nothing was, and our headers would go to
  another host. A 3xx is an error that isn't retried; fix the URL.
- One client per service, built once in `cmd/` and kept: it reuses connections.
- Pass `ctx` to every call (`http.NewRequestWithContext`), so a request's or a job's own time limit
  ends the call too.
- Pick the limit from the service's normal speed, well inside the caller's own limit: a job with 5
  minutes can give a webhook 10 seconds; a request with 8 seconds can give a call 2 or 3.

## Retries

**In a request, don't retry.** The caller is waiting: return the error and let the caller decide.

<!-- remove:worker -->
**In a job, don't retry inside the handler.** Return the error and the queue retries the whole job
with growing delays. A failure a retry can't fix is marked `queue.ErrNoRetry`, so the job fails at
once instead of using up its tries:

```go
if err := tj.purgeReports.Send(ctx, run.ID, event); err != nil {
	if !httpclient.Retryable(err) {
		return fmt.Errorf("report purge: %w: %w", err, queue.ErrNoRetry)
	}
	return fmt.Errorf("report purge: %w", err)
}
```

<!-- remove:end -->
`httpclient.Retryable` sorts failures the same way for every service, for whoever retries (a
client, a queue):

| Failure | Retried? |
|---|---|
| no connection, a time limit, a reply cut off | yes |
| 408, 429, 5xx | yes |
| any other 4xx, or a 3xx | no: our request is wrong, and it'd be wrong every time |
| an error before the call (a body that can't be encoded, a URL that can't be parsed) | no: a bug |

<!-- remove:worker -->
A 429's `Retry-After` isn't honoured yet: the job is retried with the queue's own growing delays.

A report whose job runs out of tries is lost: the purge has already happened, and the job fails
for good. That's fine for the purge, since the next day's report says the same thing; work whose
report must arrive (a payment's receipt) writes it to an outbox table in the same transaction and
sends it from a job of its own ([worker.md](worker.md)).

<!-- remove:end -->
## Every call may happen twice

Work gets repeated (a job runs again after any failure, a client sends a request again), and a
call that timed out may have worked on the other side. So:

- **The outside call comes last**, after the database work, and the database work is safe to
  repeat.
- **Send what's true after a repeat too**, not a change that a repeat would undo or double.
- **Send an idempotency key** when the service accepts one: the same on every try of one piece of
  work, so the service can drop the repeat (the `Idempotency-Key` header in `internal/webhook`).
  Payments and emails must have one.

<!-- remove:worker -->
The worker's purge does all three. It deletes by a condition, so a second run deletes nothing. It
reports "done tasks older than 30 days are gone", not how many it deleted: after a failed report the
job runs again, deletes nothing, and a count would say 0. Its key is the job's ID
(`queue.RunFrom(ctx)`), the same on every try.

<!-- remove:end -->

## Errors and logs

- Name the service and the status in errors (`webhook replied 503 Service Unavailable`), never the
  URL: it can hold a key or token (`httpclient.WithoutURL` strips it from the client's errors).
- Never log request or reply bodies, headers or query strings: they hold personal data and keys.
- A setting holding a URL is checked when the program starts (`config.Reader.OptionalHTTPURL`); its
  error names the setting, not the value.
- Close every reply body (lint: bodyclose), and read what's left of it first, even when you don't
  need it, so the connection can be reused.

## Tests

Fake the outside service, not our code: an `httptest.Server` stands in for it, and the test checks
what we sent (method, headers, body, idempotency key) and what our code does with each kind of
reply: success, a failure that's retried, one that isn't, a redirect, and a service too slow to
answer. Give a test of a time limit a deadline of its own, so a missing limit fails the test
instead of hanging it. See `internal/httpclient` and `internal/webhook`'s tests.
<!-- remove:worker -->
The purge's are `TestPurgeDoneTasksReportsToTheWebhook` and its neighbours in
`internal/worker/tasks_test.go`.
<!-- remove:end -->
