# Errors

Two rules hold everything up:

1. **Services say what went wrong; entry points decide the response.** A service returns an
   `apperr` kind; the HTTP layer turns the kind into a status code, and a worker would turn the
   same kind into "retry" or "don't". Services never know about HTTP.
2. **Each error is logged once, where the request ends.** Inner code wraps and returns; it never
   logs an error it also returns. The HTTP layer puts the error on the request's one summary line
   ([logging.md](logging.md)).

## Expected failures: `internal/apperr`

| Kind | Meaning | HTTP |
|---|---|---|
| `apperr.BadRequest(detail)` | the input can't be read (bad JSON, a bad cursor) | 400 |
| `apperr.Invalid(detail, fields...)` | readable, but breaks a rule; `fields` say which | 422 |
| `apperr.NotFound(detail)` | the thing doesn't exist | 404 |
| `apperr.Conflict(detail)` | clashes with current state (a duplicate) | 409 |

- `detail` and each field's message are **shown to the caller**, so they never hold internal
  details (SQL, IDs of other users, hostnames).
- Add context by wrapping, not by editing the detail:
  `fmt.Errorf("get task %s: %w", id, apperr.NotFound("task not found"))`. The wrapped text goes to
  the log; only the detail goes to the caller.
- Any error that isn't an `apperr` is **unexpected** (a bug or an outage): the caller gets a 500
  with the fixed text `internal server error`, and the full error goes to the log.
- Add a kind only when some caller must react to it differently from the existing ones (sign-in
  would add an unauthorized kind). `problemFor` (`internal/api/problem.go`) maps each kind; the
  linter fails until a new kind has its own case.

## Turning driver errors into kinds

Only the service that ran the query knows what a database error means, so the mapping happens
there, once: `pgx.ErrNoRows` becomes `NotFound`, a unique violation on a known constraint becomes
`Conflict`. Handlers never look at driver errors. See `notFoundOr` and `conflictOr` in
`internal/task/service.go`. Match a unique violation by its constraint name, so a new unique rule
isn't silently reported as the wrong conflict.

## Handlers

Handlers have the type `func(w, r) error` and are wrapped with `Handle`:

```go
r.Get("/tasks/{id}", Handle(tr.get))
```

`Handle` writes the reply for a returned error ([RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)
problem JSON, see [http-api.md](http-api.md)) and records the error for the summary line. A
handler that has started its reply can't change the status any more; its error is still logged.

Some failures exist only in HTTP (body too large: 413, wrong content type: 415). They're
`statusError`s inside `internal/api`, not `apperr` kinds, because no service could cause them.

## Panics

A panic in a handler is caught by the logging middleware: the caller gets a 500 problem reply, and
the summary line gets the panic and its stack. If the reply had already started, the connection is
cut instead, so the client can't mistake half a reply for a whole one. Panics are for bugs only;
return errors for everything else.

## The client went away

If the client disconnects, the request's context is cancelled and queries fail with
`context.Canceled`. That's not a server failure: nothing is written, and the summary line shows
status 499 at info level, not a 5xx at error level.

## Too slow

Every request's context ends after 8 s ([http-api.md](http-api.md#server-settings)). A handler
whose query is cut off by it returns the `context.DeadlineExceeded` error as usual; the caller gets
a 503 "the request took too long", and the summary line logs it at error level, since a slow
endpoint is our problem.

## Don't

- Log an error and also return it: it shows up twice and the counts lie.
- Return `err.Error()` text to a caller: it can hold SQL, hosts or other users' data.
- Retry every error. Only a few database errors are worth retrying; see
  [database.md](database.md#transactions-and-retries).
- `panic` on bad input or a failed call.
