# Traces

A trace follows one piece of work (a request, a job run) through everything it does, as
[OpenTelemetry](https://opentelemetry.io/docs/concepts/signals/traces/) records it: each step is a
**span** (a name, a start, a length, and what it was about), and the steps hang under the one that
started them. Where logs say what happened ([logging.md](logging.md)) and metrics say how often
and how fast ([metrics.md](metrics.md)), a trace says where one slow request's time went: which
query, which call. Where traces go is a setting, as for metrics, so the store can change without
code changes.

## What's traced

Only a request or a job run starts a trace. Everything else is a span inside one, or isn't
recorded: database and key-value store clients record every call they make, even their own
background ones (a job queue polls every second), and each would otherwise be a trace of its own.

<!-- remove:api -->
- **Each request** (API; [otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp),
  `internal/api/telemetry.go`): a span named by its route (`GET /tasks/{id}`; `HTTP` for a method
  HTTP doesn't define), with the method, status, user agent, the route pattern as its path (never
  the path itself, which can hold an ID or a token) and the client's IP as
  [`TRUSTED_PROXIES`](http-api.md#rate-limits) finds it (else the connection's, as the request
  log has it). The connection's own address and port and the method as sent are there too: the
  client's IP and user agent are the only personal data a span holds, the same the request log
  keeps. A 5xx or a reply cut off by a panic marks it failed. The health check isn't traced.
  - Every caller counts as outside: a trace a client sends (its `traceparent` header) is linked to
    the request's span, never continued, so no client can make us keep its requests or choose the
    trace IDs in our logs. Behind your own services that do send theirs, drop
    `WithPublicEndpointFn` in `observeRequests`.
<!-- remove:end -->
<!-- remove:worker -->
- **Each job** (worker): queueing it is a span (`send deleted_rows:purge`), and its trace goes with
  the job in its headers, never its payload (`internal/queue/valkey`). Each run is a span
  (`process deleted_rows:purge`, `internal/worker/logging.go`) in that same trace, so a request and
  the job it queued are one trace, retries included. A job on a timetable, or queued outside a kept
  trace, starts its own. A failed run's span is marked failed, with the error's message (never its
  stack).
<!-- remove:end -->
- **Each query** ([otelpgx](https://github.com/exaring/otelpgx), `startup.ConnectDB`): named by
  its sqlc name (`query GetTask`; `prepare GetTask` the first time a connection runs it), with its
  SQL but not its values. A failed query's span holds Postgres's error message, which can quote a
  value it refused (`invalid input syntax for type uuid: "..."`).
<!-- remove:valkey -->
- **Each Valkey command** ([redisotel](https://github.com/redis/go-redis/tree/master/extra/redisotel),
  `startup.ConnectRedis`, `startup.RedisFailFast`): named by the command (`set`), with the pool's
  name; never its arguments, which hold a queued job's payload.
<!-- remove:end -->
- **Each call to an outside service** (`internal/httpclient`): a span named by the method
  (`POST`), with the host, port and the reply's status; never the URL, which often holds a key.
  The trace isn't sent to the service: it isn't ours.
- **Log lines** of a kept trace carry its `trace.id` and `span.id`: search the logs for a trace's
  ID to find what it logged.

## Where they go

Set by OpenTelemetry's standard variables, read in `internal/telemetry`:

| Setting | What happens |
|---|---|
| `OTEL_TRACES_EXPORTER` unset, `otlp` | Pushed as OTLP over HTTP to `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` (or `OTEL_EXPORTER_OTLP_ENDPOINT`; default `http://localhost:4318`). |
| `OTEL_TRACES_EXPORTER=none` | Off: nothing is recorded, at next to no cost. `OTEL_SDK_DISABLED=true` turns off metrics and traces both. An empty value is unset: on. |
| `OTEL_TRACES_SAMPLER_ARG` | The share of traces kept, from 0 to 1 (default 1, all). Decided once per trace, so a trace is kept whole or not at all. |

Traces are only pushed: a trace is a record of each step, sent once, unlike a metric's running
total that can be read any number of times. Finished spans wait in memory and go out in batches,
every few seconds; at most 2048 wait, and past that new ones are dropped rather than use more
memory. On shutdown what's left is sent, within a second.

`console` (traces printed among the log lines) and an `OTEL_TRACES_SAMPLER` other than
`parentbased_traceidratio` or `parentbased_always_on` (what the sampler above is; any other would
be ignored) stop the program at start, like any bad setting. A send that
fails is logged at warn, at most once a minute, as for metrics.

**In production**, send to a collector or a trace store (Grafana Tempo, Jaeger, a vendor) with
the endpoint setting; see [metrics.md](metrics.md#where-they-go) for why a collector. Keeping
every trace costs storage that grows with traffic: lower `OTEL_TRACES_SAMPLER_ARG` once traffic is
high.

**Locally**: `make telemetry-up` starts Jaeger with the metrics stack; `.env` copied from
`.example.env` already points at it. See traces at http://localhost:16686, or in Grafana's
Explore (http://localhost:3000, the Jaeger source; sign in as admin/admin, as the open view can't
use Explore). Jaeger keeps the newest 10,000 in memory.

## Rules

- **Take the `trace.TracerProvider` passed in**, from `telemetry.Start` in `cmd/`; never
  OpenTelemetry's global one. Lint refuses the global and the SDK outside `internal/telemetry`.
- **Ready-made instrumentation first**, as for metrics; check what a library records before
  adding it. Not OpenTelemetry's ready-made HTTP client transport: it records the whole URL and
  sends our trace to whoever we call; `internal/httpclient` does neither.
- **Never personal data or secrets** in a span's name or attributes beyond the client's IP and
  user agent that requests carry: no user's details, tokens, payloads, query values or URLs with
  keys. Names stay few and fixed (a route pattern, a sqlc name,
  a job type), never an ID or a path.
- **A span of your own** for a slow step that isn't a query or a call: start it from the context
  you were given and end it on every path:

  ```go
  ctx, span := tracer.Start(ctx, "report.render")
  defer span.End()
  if err != nil {
  	span.RecordError(err)
  	span.SetStatus(codes.Error, "render failed")
  }
  ```

  Outside a request or job it's dropped, by design.

## Tests

`internal/spantest` records in memory, every span kept: give `spantest.New().Provider` to the code,
then check `Named(name)`, a span's parent, kind and `spantest.Attributes`.
`noop.NewTracerProvider()` (`go.opentelemetry.io/otel/trace/noop`) records nothing, for tests about
something else.
