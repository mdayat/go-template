# HTTP API

Handlers stay thin: read the request, call the service, write the reply. Rules about what's valid
live in the service ([errors.md](errors.md)), shared by every client. Where files go:
[structure.md](structure.md).

## Clients, paths and versions

Each kind of client has its routes under its own prefix, in its own package, with its own request
and reply types, all calling the same services. So one client's routes can change without any risk
to another's: a field renamed for the web app can't break the mobile app.

| Client | Path | Package | May change |
|---|---|---|---|
| mobile apps | `/mobile/v1/...` | `internal/api/mobile` | only by adding (below): installs of old versions call it for years |
| web app | `/web/...` | `internal/api/web` | freely, in the same release as the web app, which is deployed with the API |
| admins | `/admin/...` | `internal/api/admin` | freely, with the admin app; every route needs an admin (`admin.RequireAdmin`) |
| your own services | `/internal/...` | `internal/api/internal` | with those services; their own sign-in |
| outside services' webhooks | `/webhooks/<provider>` | `internal/api/webhooks` | as the provider sends; checked by the provider's signature, outside the sign-in group |

The template has the first three; add the others when a project needs them, as a route group in
`NewRouter` (`internal/api/router.go`) with its own middleware. A route both apps need gets a
handler in each package: two thin handlers calling one service, so each reply can change on its own.
The error body (`handler.Problem`) is the one reply type every client shares.

The mobile contract is more than `internal/api/mobile`. It's also everything a mobile request goes
through, so change these only by adding too:

- what every client's handlers share (`internal/api/handler`: the body decoder, error replies,
  paging);
- the replies the router gives before a handler runs: 401, 429 with `Retry-After` and the
  `X-RateLimit-*` headers, 404 and 405 with `Allow`, 503 when a request runs out of time;
- the services' rules: a tighter limit (a shorter title, a smaller page) or a changed error
  `detail` or field `rule` changes what old app versions accept and see, whichever client asked for
  it.

**Changing a web or admin route.** It ships in the same release as its app, but for a few minutes
of every deploy old and new API copies run side by side, and a page already open keeps calling the
old shape until it's reloaded. Rename or remove in two releases: add the new field or route, then
remove the old one once the app no longer uses it.

**Changing a mobile route.** Add a route, a reply field, or an optional request field: old app
versions ignore what they don't know. Never remove, rename or retype a field or route, change what a
field means, or make an optional field required. A change that can't be made that way is a new
version, `/mobile/v2/...`, in its own package (`internal/api/mobile/v2`), served beside v1 until no
old version calls it. To see which do: apps send `User-Agent: <app>/<version>`
(`myapp-ios/3.4.1`), and every request's log line has it as `user_agent.original`. The version is
only in the mobile path; the web and admin apps change with the API.

**Naming.**

- Paths are plural nouns in kebab-case: `/tasks`, `/tasks/{id}`, `/task-exports`. At most one
  level of nesting: `/tasks/{id}/comments`, never deeper; past that, a route of its own with a
  filter (`/comments?task_id=`).
- An action that isn't create, read, change or delete is a noun you create:
  `POST /tasks/{id}/restore`, `POST /task-exports`, never `/exportTasks`.
- Methods: `GET` reads, `POST` creates (or runs an action), `PATCH` changes the fields sent, `PUT`
  replaces the whole thing, `DELETE` removes.
- JSON keys and query parameters are snake_case: `owner_id`, `?older_than_days=`.

## Requests with a JSON body

Read every body through `handler.Decoder`'s `Decode` (`internal/api/handler/decode.go`), never
`json.Unmarshal` directly. It enforces, in order:

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

An ID in a body is a `string` with `validate:"required,uuid"`, parsed with `uuid.MustParse` after
`Decode` has checked it. A `uuid.UUID` field works for good input, but a malformed one gets a vague
400 that doesn't name the field.

Optional fields in an update are pointers: `nil` means "not sent, leave it alone"
(`updateTaskRequest`). A JSON `null` also leaves it alone.

Go's JSON reading (go-json and the standard library alike, with no setting to change it) also:

- matches keys ignoring case: `{"TITLE": "x"}` fills `title`;
- keeps the last value of a repeated key;
- replaces invalid UTF-8 with `U+FFFD`.

## Replies

- Success: JSON with snake_case keys, written with `handler.WriteJSON`. Times are RFC 3339 in UTC.
  `201 Created` sets `Location`; `204 No Content` has no body.
<!-- remove:worker -->
- `202 Accepted` means a job was queued; it replies `{"job_id": "..."}`
  ([worker.md](worker.md#queueing-from-the-api)).
<!-- remove:end -->
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
paths get a 404 problem and wrong methods a 405 with an `Allow` header, signed in or not. Every GET route also
answers HEAD (HTTP requires it; uptime checkers use it). An error reply drops any
headers the handler had set that describe a body (`Content-Encoding`, `Cache-Control`, `ETag`,
...), since they'd misdescribe the problem.

## Signing in

Every route but `/healthz` needs someone signed in: the request carries
`Authorization: Bearer <token>`, and `authenticate` (`internal/api/auth.go`) asks the
`Authenticator` that `cmd/api` passed in whose token it is.

| The token | Reply |
|---|---|
| missing, another scheme, or refused (forged, expired, for another service) | 401 with `WWW-Authenticate: Bearer`, the same detail whatever was wrong; the reason is on the log line |
| can't be checked (the provider's keys never arrived) | 503: our outage, which a client may retry |

The provider only proves who someone is; what they may do is decided on our own data, never on the
token's claims, so changing provider changes no rule. `authenticate` turns the provider's ID for the
person (the token's subject) into our user (`internal/user`, the `users` table), adding them on
their first request; other tables point at `users.id`, never at the provider's ID. Moving to another
provider means pointing each user's `auth_subject` at their new ID there, once; their rows and
rights stay. Making someone an admin (`is_admin`: they see and change everyone's rows) is done by
hand, after their first sign-in (locally, their ID is the `user=` you gave `make dev-token`):

```sql
UPDATE users SET is_admin = true WHERE auth_subject = '<their ID at the provider>';
```

The template's provider is `internal/auth/jwks`: JWTs checked against the keys their issuer
publishes (`AUTH_JWKS_URL`, `AUTH_ISSUER`, `AUTH_AUDIENCE` in `.example.env`). It covers Firebase,
Auth0, Cognito, Keycloak and Supabase, given tokens that name this service in `aud` and are signed
RS256 or ES256: Cognito's ID tokens (its access tokens have no `aud`), Keycloak with an audience
mapper. Another kind (opaque tokens looked up in a table, say) is another package with an
`Authenticate(ctx, token)` method, picked in `cmd/api`; handlers and services don't change.

- For local runs, `make dev-token user=alice` prints a token the local API accepts; run it before
  the API's first start, which needs the key it makes. The API refuses its keys in production.
- A route that must stay open (a webhook from another service, under `/webhooks/<provider>`) goes
  in a group of its own in `NewRouter`, without the `signedIn` middleware, and checks its caller its
  own way.
- Endpoint tests sign in through the real check: `testAPI` signs every request in as
  `testSubject`, with a token from a test issuer (`internal/authtest`), unless the test sets its
  own `Authorization` header. `testUser(t, pool, admin)` records that user (as an admin or not)
  and returns their ID, for seeding rows they own.

### Who may do what

A handler gets the signed-in user with `handler.CurrentUser(r)` and passes it to the service, which
decides: the rules live in one place, the same for every entry point. In the tasks example:

- A user sees and changes only their own tasks. Someone else's is a 404, the same reply as one
  that doesn't exist: a 403 would confirm it does. Replies show the task's `owner_id`.
- An admin (`is_admin`) sees and changes everyone's, tasks from before owners included.
- An action that's no secret but not for everyone is a 403, `apperr.Forbidden`. Admin-only
  routes go under `/admin`, where `admin.RequireAdmin` replies 403 to anyone else.

## Rate limits

Every route but `/healthz` has two limits (`internal/api/ratelimit.go`):

- **Per client IP, signed out** (`RATE_LIMIT_PER_IP`, default 600 a minute): requests without a
  valid sign-in (no token, or a refused one), so a flood of bad tokens is cut off with a 429 instead
  of 401s. Signed-in requests aren't counted per IP: many users can share one (a mobile carrier's,
  an office's). An IPv6 client is counted by its /64, which it usually holds whole.
- **Per signed-in user** (`RATE_LIMIT_PER_USER`, default 120 a minute), wherever they send from.

0 turns a limit off. The defaults are common starting points, not a standard: set them from real
traffic. A limit of N a minute lets a client send N at once, then refills evenly (one every 60/N
seconds). A request past a limit gets a 429 problem reply with `Retry-After`, the seconds until the
next one may go. Every reply a limit checked carries `X-RateLimit-Limit`, `X-RateLimit-Remaining`
and `X-RateLimit-Reset` (when the count is full again, in Unix seconds), for the one limit that
applies: the per-user limit signed in, the per-IP one otherwise. Paths no route matches (404, 405)
aren't limited: they do no work.

**Shared by every copy.** `cmd/api` counts in Valkey (`internal/ratelimit/valkey`, on
[redis_rate](https://github.com/go-redis/redis_rate)): one atomic call per check, timed by Valkey's
clock, so however many copies run, together they never let in more than the limit. Keys are
`rate:api:<hash>`: not the IP or user ID in the clear, though an IPv4 address's hash can be found by
trying every address. Each client active in the last minute holds one small key, in the same Valkey
as everything else the programs share: size its memory for your peak of distinct clients, and alert
on it, since a full Valkey refuses more than counts.

**When Valkey fails.** A call that fails or takes over 250 ms makes that copy count in its own
memory (`ratelimit.Memory`), logging one error: N copies then let in up to N times the limit (twice
that across the turn of a minute), and the API keeps serving. In memory a copy holds at most
100,000 clients a minute; past that, new ones go uncounted. Every 5 s one request tries Valkey
again; when it answers, counting is shared again (logged at info). The rate limits don't need
Valkey to start either; a job queue's connection, if the project has one, does.

**The client's IP.** By default it's whoever opened the connection. Behind a load balancer or CDN
that's the proxy, so every client would share one signed-out limit: set `TRUSTED_PROXIES` to the
proxies' IP ranges. Requests from them then have the client's IP read from `X-Forwarded-For`, right
to left, skipping trusted addresses, so what a client wrote there itself is never believed. A
request that doesn't come from a trusted proxy is counted by its connection, whatever it sends; so
is one from inside a trusted range with no readable `X-Forwarded-For` (the proxy's own, another
service's). The first request with `X-Forwarded-For` from a sender outside the ranges is logged at
warn, once per copy: a sign the setting is missing (or a client forging the header).

Never trust a range clients can send from (the API refuses `0.0.0.0/0` and `::/0`), and never read
the IP from a header yourself: use `middleware.GetClientIP(r.Context())`, the same IP the request
log shows as `client.ip`.

To count somewhere else, give `api.Limits` another `ratelimit.Limiter` in `cmd/api/main.go`: for a
single copy, `ratelimit.NewMemory()`. Handlers and services don't change.

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
ends after 8 s (`RequestTimeout` in `internal/api/router.go`): its queries are cancelled and the
caller gets a 503, "the request took too long". Pass `r.Context()` to everything a handler calls,
or the time limit can't stop it.

On SIGTERM the server stops taking new requests and waits up to 8 s for running ones
(`shutdownTimeout` in `cmd/api/main.go`): each request's context has ended by then, and it's below
Docker's default 10 s before a hard kill, the shortest common wait. Any still running then have
their connections closed, and the process exits.

Work that takes long and doesn't need to finish before the reply (a report, sending email)
doesn't belong in a request: it runs after the reply, outside the request.
<!-- remove:worker -->
It's a background job ([worker.md](worker.md)).
<!-- remove:end -->

An endpoint that needs longer itself (a large upload) sets its own limits in that handler, not a
higher limit for everyone:
`context.WithTimeout(context.WithoutCancel(r.Context()), d)` (a context made from `r.Context()`
alone can't outlive its 8 s; this one also no longer stops when the client leaves), and
`http.ResponseController` for the write deadline.
