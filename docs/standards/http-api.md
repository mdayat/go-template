# HTTP API

Routes live in `internal/api`, one file per resource (`tasks.go`), mounted in `router.go`.
Handlers stay thin: read the request, call the service, write the reply. Rules about what's valid
live in the service ([errors.md](errors.md)).

## Requests with a JSON body

Read every body through `bodyDecoder.decode` (`internal/api/decode.go`), never `json.Unmarshal`
directly. It enforces, in order:

| Problem | Reply |
|---|---|
| `Content-Type` isn't `application/json` | 415 |
| body over 1 MB | 413 |
| empty, not a JSON object, broken JSON, wrong type, unknown field, two JSON values | 400, `detail` says which field or byte |
| a `validate:"..."` struct tag fails | 422, `errors` lists each field by its JSON name |

The content-type check also blocks cross-site form posts: a browser can't send `application/json`
to another site without the site's permission.

Validation tags check **shape** (present, a UUID, at most 100 items). Rules about **meaning**
(a title is unique, an end date is after the start) go in the service, which returns
`apperr.Invalid` or `apperr.Conflict`. The decoder words the common tags (`required`, `email`,
`url`, `uuid`, `oneof`, `min`/`max`/`gte`/`lte`/`len`/`gt`/`lt`); add a case to `ruleMessage` when
you use another.

Optional fields in an update are pointers: `nil` means "not sent, leave it alone"
(`updateTaskRequest`). A JSON `null` also leaves it alone.

Go's JSON reading (go-json and the standard library alike, with no setting to change it) also:

- matches keys ignoring case: `{"TITLE": "x"}` fills `title`;
- keeps the last value of a repeated key;
- replaces invalid UTF-8 with `U+FFFD`.

## Replies

- Success: JSON with snake_case keys, written with `writeJSON`. Times are RFC 3339 in UTC.
  `201 Created` sets `Location`; `204 No Content` has no body.
- Lists are objects, never bare arrays (`{"tasks": [...]}`), so fields can be added later without
  breaking clients. An empty list is `[]`, never `null`.
- Errors: [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details,
  `Content-Type: application/problem+json`:

```json
{"type":"about:blank","title":"Unprocessable Entity","status":422,
 "detail":"the title is invalid","request_id":"dbqsuft4m06s73apk900",
 "errors":[{"field":"title","rule":"max","message":"must be at most 200 characters long"}]}
```

`detail` and `message` are for people; clients branch on `status` and the field's `rule`. Unknown
paths get a 404 problem and wrong methods a 405 with an `Allow` header. Every GET route also
answers HEAD (HTTP requires it; uptime checkers use it). An error reply drops any
headers the handler had set that describe a body (`Content-Encoding`, `Cache-Control`, `ETag`,
...), since they'd misdescribe the problem.

## IDs and paging

- An ID that isn't a valid UUID is a 404, the same as an unknown one: it can't name anything.
- Lists are paged with a cursor: `?limit=` (default 20, at most 100) and `?after=<next_cursor>`
  from the previous page. `next_cursor` is missing on the last page. The cursor is opaque to
  clients; inside, it's the last row's `(created_at, id)`, and the query uses an index on those.
  Unlike `?page=3` (offset), a cursor doesn't skip or repeat rows when rows are added between pages,
  and it stays fast deep into a large table.

## Server settings

Connection timeouts are set in `cmd/api/main.go` (read header 5 s, read 10 s, write 10 s, idle
120 s). Those cut a slow connection but don't stop the handler, so each request's context also
ends after 8 s (`requestTimeout` in `internal/api/router.go`): its queries are cancelled and the
caller gets a 503, "the request took too long". Pass `r.Context()` to everything a handler calls,
or the time limit can't stop it.

On SIGTERM the server stops taking new requests and waits up to 15 s for running ones. Any still
running then have their connections closed, which cancels their contexts, and the process exits.

An endpoint that needs longer (a large upload, a report) sets its own limits in that handler, not
a higher limit for everyone: `context.WithTimeout(context.WithoutCancel(r.Context()), d)` (a
context made from `r.Context()` alone can't outlive its 8 s; this one also no longer stops when the
client leaves), and `http.ResponseController` for the write deadline.
