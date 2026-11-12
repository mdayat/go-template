# Logging

[zerolog](https://github.com/rs/zerolog), JSON lines in production (`APP_ENV=production`, the
default), readable colored lines in development. Field names follow the
[Elastic Common Schema](https://www.elastic.co/guide/en/ecs/current/index.html), so log tools
recognise them.

## One line per request

`logRequests` (`internal/api/logging.go`) writes exactly one line when a request ends:

```json
{"level":"info","request_id":"dbqsuft4m06s73apk900",
 "http":{"request":{"method":"POST"},"response":{"status_code":201,"body":{"bytes":166}},"route":"/tasks"},
 "url":{"path":"/tasks"},"user_agent":{"original":"curl/8.7.1"},
 "client":{"ip":"192.168.65.1","port":46088},"duration_ms":10.6,"message":"request completed"}
```

- `http.route` is the route pattern (`/tasks/{id}`), so requests group by endpoint; `url.path` is
  the actual path.
- `request_id` is new for every request and sent back as the `X-Request-Id` header, so a user's
  bug report can be matched to its line. A client's own `X-Request-Id` is ignored: it could be
  forged.
- A failed request carries `error.message` on the same line. A panic adds the panic and its
  stack. There is never a second line for the same failure.
- A successful `/healthz` isn't logged: load balancers call it every few seconds.

## Levels

| Level | When |
|---|---|
| error | a 5xx reply or a panic: something is broken and needs a look |
| info | every other request, 4xx included (the caller's mistake, not ours) |
| warn | something odd that still worked; rare |
| debug | detail for local runs (`LOG_LEVEL=debug`); off in production |

Don't log a 4xx at error: alerts on error lines would fire for every typo a user makes.

## Never log

Request or response bodies, headers (`Authorization`, cookies), query strings (they carry tokens
and emails), passwords, keys, or personal data. The summary line logs only the path, never the
query. If you need an ID from a body, log that one field.

## Adding fields

Inside a request, log through the request's logger, so the line carries `request_id`:

```go
zerolog.Ctx(r.Context()).Info().Str("task_id", id.String()).Msg("task archived")
```

Most code needs no log at all: return the error and let the summary line carry it
([errors.md](errors.md)). Log on its own line only an event worth finding later that isn't an
error (a payment confirmed, a job started).

**Trap:** never call `zerolog.Ctx(ctx).UpdateContext(...)` where `ctx` may not hold a request
logger (a background job, a test). `zerolog.Ctx` then returns the shared default logger, and the
field sticks to every later line of the whole process. `recordError` in
`internal/api/problem.go` guards against exactly this.

## Outside requests

Startup and shutdown use the global `log.Logger`: `api listening` (with `addr` and the build's
`version`), `shutting down`, `api stopped cleanly`. Errors from Go's HTTP server itself (a failed
accept, a handler writing headers twice) go through `api.ServerErrorLog` into the same JSON log.
