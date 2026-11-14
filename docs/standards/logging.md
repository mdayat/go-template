# Logging

[zerolog](https://github.com/rs/zerolog), JSON lines in production (`APP_ENV=production`, the
default), readable colored lines in development. Field names follow the
[Elastic Common Schema](https://www.elastic.co/guide/en/ecs/current/index.html), so log tools
recognise them.

## One summary line each

Each piece of work a program handles ends with exactly one summary line. A failure carries its error on
that line, and a panic its stack: there is never a second line for the same failure.

<!-- remove:api -->
### Requests

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
  stack.
- A successful `/healthz` isn't logged: load balancers call it every few seconds.

<!-- remove:end -->
<!-- remove:worker -->
### Jobs

One `job completed` line per run, with the error on it ([worker.md](worker.md#logging)).

<!-- remove:end -->

## Levels

| Level | When |
|---|---|
| error | a failure that's ours, or a panic: something is broken and needs a look |
| info | everything that went as expected, a caller's mistake included |
| warn | something odd that still worked; rare |
| debug | detail for local runs (`LOG_LEVEL=debug`); off in production |

Don't log a caller's mistake at error: alerts on error lines would fire for every typo a user
makes.
<!-- remove:api -->
In the API a 5xx reply is error, a 4xx info.
<!-- remove:end -->

## Never log

Request or response bodies, headers (`Authorization`, cookies), query strings (they carry tokens
and emails), passwords, keys, or personal data. If you need an ID from a body, log that one
field.
<!-- remove:api -->
The request's summary line logs only the path, never the query.
<!-- remove:end -->

## Adding fields

Inside a piece of work, log through its own logger (`zerolog.Ctx(ctx)`), so the line carries its
ID:

```go
zerolog.Ctx(ctx).Info().Str("task_id", id.String()).Msg("task archived")
```

Most code needs no log at all: return the error and let the summary line carry it
([errors.md](errors.md)). Log on its own line only an event worth finding later that isn't an
error (a payment confirmed, a job started).

**Trap:** never call `zerolog.Ctx(ctx).UpdateContext(...)` where `ctx` may not hold a logger of
its own (code called from elsewhere, a test). `zerolog.Ctx` then returns the shared default logger,
and the field sticks to every later line of the whole process.
<!-- remove:api -->
`recordError` in `internal/api/problem.go` guards against exactly this.
<!-- remove:end -->

## Startup, shutdown and libraries

Startup and shutdown use the global `log.Logger`, each program's lines carrying the build's
`version`. Libraries' own messages go into the same JSON log, marked with `source`.
<!-- remove:api -->
- The API: `api listening` (with `addr`), `shutting down`, `api stopped cleanly`; Go's HTTP server
  (a failed accept, a handler writing headers twice) as `source: net/http`.
<!-- remove:end -->
<!-- remove:worker -->
- The worker: `worker started`, `shutting down`, `worker stopped cleanly`; the queue client as
  `source: go-redis`, and the job runner as `source: asynq`.
<!-- remove:end -->
