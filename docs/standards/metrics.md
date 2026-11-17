# Metrics

Each program measures itself with [OpenTelemetry](https://opentelemetry.io/docs/concepts/signals/metrics/):
running numbers (counts, times) that a metrics store such as Prometheus keeps over time and Grafana
charts. OpenTelemetry only measures and sends; where the numbers go is a setting, so changing store
(Prometheus, Grafana Cloud, a vendor) changes no code. Logs say what happened to one request or job
([logging.md](logging.md)); metrics say how many, how fast and how full, over time; traces say
where one request's or job's time went ([tracing.md](tracing.md)).

## What's recorded

Ready-made instrumentation where it exists; our own only where it doesn't. Every metric carries
the program's `service.name` (`<project>-api`, `<project>-worker`), `service.version`,
`deployment.environment.name` and `service.instance.id` (new each start, so copies running at once
don't overwrite each other's numbers).

<!-- remove:api -->
- **HTTP requests** (API; [otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp),
  `internal/api/telemetry.go`): `http.server.request.duration` (seconds) by `http.route` (the pattern,
  `/tasks/{id}`), method and status; its count is the number of requests, so 401s, 404s and 429s
  show too, a client that hung up before the reply as 499, and a reply a panic cut off midway
  with `error.type` `aborted`. The health check isn't recorded, nor otelhttp's body-size histograms
  (dropped in `internal/telemetry`).
<!-- remove:end -->
<!-- remove:worker -->
- **Jobs** (worker; our own, as asynq has none; `internal/worker/logging.go`): `job.run.duration`
  (seconds) by `job.type`, `job.queue` and `job.result` (`succeeded`, `retrying`, `failed` for
  good), recorded where each run's log line is written, with the same result.
<!-- remove:end -->
- **Database, from the program's side** ([otelpgx](https://github.com/exaring/otelpgx),
  `startup.ConnectDB`): `db.client.operation.duration` (seconds) per query, named by its sqlc name
  (`db.operation.name` = `GetTask`), and the pool's use (`pgxpool.*`, pool named `main`:
  connections in use, idle, waits for a free one). The duration also times connecting, preparing
  and taking a connection (`pgx.operation.type`): count queries with `pgx.operation.type` `query`,
  or each is counted more than once. Postgres's own exporter sees the server's side; only these see
  time spent waiting for a connection or on the network, per program and copy.
<!-- remove:valkey -->
- **Valkey, from the program's side** ([redisotel](https://github.com/redis/go-redis/tree/master/extra/redisotel),
  `startup.ConnectRedis`, `startup.RedisFailFast`): each command's time
  (`db.client.connections.use_time`, milliseconds) and the connections' use, by `pool.name`: what the
  client is for, as `cmd/` names it.
<!-- remove:end -->
- **Go runtime** (`go.*`): memory, goroutines, garbage collection.

## Where they go

Set by OpenTelemetry's standard variables, which it reads itself (`internal/telemetry`), not
through `internal/config`:

| `OTEL_METRICS_EXPORTER` | What happens |
|---|---|
| unset, `otlp` | Pushed every `OTEL_METRIC_EXPORT_INTERVAL` ms (default 60000) to `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` (or `OTEL_EXPORTER_OTLP_ENDPOINT`; default `http://localhost:4318`), as OTLP over HTTP. |
| `none` | Off: nothing is recorded, at next to no cost. `OTEL_SDK_DISABLED=true` turns off metrics and traces both. An empty value is unset: on. |
| `prometheus` | Served for pulling at `http://OTEL_EXPORTER_PROMETHEUS_HOST:OTEL_EXPORTER_PROMETHEUS_PORT/metrics` (default `localhost:9464`: in a container, set the host to `0.0.0.0`, or a scraper outside can't reach it). Give each program on one machine its own port. |

`OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` override what the program is called. A wrong
setting (an unknown exporter, `console`, which would print metrics among the log lines, a bad
`OTEL_RESOURCE_ATTRIBUTES`, a port already in use) stops the program at start, like any bad
setting. On unless set to `none`, so a deploy that forgets the settings still sends to the usual
local address, and says so when nothing is there: a push that fails is logged at warn
(`source: opentelemetry`), at most once a minute (with `errors_skipped`, how many weren't), and
the program carries on. On shutdown a pushing program sends what's left, within a second. Each start is a new
`service.instance.id`, so its series are new: mind the store's series limits if copies restart
often.

**In production**, push to a collector next to the programs (the
[OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) or Grafana Alloy) rather than
straight to the store, once there are several services, more than one store, or traces: it holds
the store's address and keys, retries while the store is down, and can drop or rename metrics in
one place. For one small service, pushing straight to the store is fine. Only the endpoint setting
changes, never the code.

**Locally**: `make telemetry-up` starts Prometheus (receiving pushes), Jaeger (traces) and Grafana
at http://localhost:3000 with a dashboard per part, from `local/grafana/dashboards/`. `.env` copied
from `.example.env` already points at them, every 10 s; start the programs. `make telemetry-down`
stops them.

In Prometheus, names have `.` turned into `_` and the unit added: `db.client.operation.duration`
is `db_client_operation_duration_seconds_bucket`, `_count` and `_sum`. When pushed, `service.name`
is the `job` label and `service.instance.id` the `instance` label; when pulled, those come from the
scrape config.

## Rules

- **Never a personal or unbounded value in an attribute**: no user ID, raw path, IP, SQL or
  anything a client chooses. Each distinct set of attribute values is a separate series kept by the
  store; a value per user or per request multiplies them without limit (and puts personal data in
  the store). Use the route pattern, not the path; the sqlc query name, not the SQL. Check what a
  library records from the request, not only what you add.
<!-- remove:api -->
- otelhttp takes `server.address` from the client's `Host` header unless given a fixed name;
  `internal/api/telemetry.go` gives one.
<!-- remove:end -->
- **Take the `metric.MeterProvider` passed in**, from `telemetry.Start` in `cmd/`; never
  OpenTelemetry's global one, which tests running in parallel would share. Lint refuses the global
  and the SDK outside `internal/telemetry`.
- **Ready-made instrumentation first** ([OpenTelemetry's registry](https://opentelemetry.io/ecosystem/registry/)):
  a library's own names follow OpenTelemetry's conventions, which dashboards and alerts expect.
- **Your own metric**: named with dots, `<area>.<thing>` (`payment.charge.duration`), in the
  unit's base (seconds, bytes), with a description, created once where its code is built. A time
  is a histogram with buckets for its unit: the SDK's default buckets are made for milliseconds. A
  histogram's count is already the number of events: don't add a counter for it.
<!-- remove:worker -->
  The worker's `job.run.duration` (`internal/worker/logging.go`) is the example.
<!-- remove:end -->

## Tests

`internal/metrictest` records in memory: give `metrictest.New().Provider` to the code, then check
`Collect(t).Find(name, attributes)`. `noop.NewMeterProvider()` (`go.opentelemetry.io/otel/metric/noop`)
records nothing, for tests about something else.
