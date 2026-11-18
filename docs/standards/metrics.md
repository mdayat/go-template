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
  `/web/tasks/{id}`; a path no route matches has its client's prefix, `/web/*`), method and
  status; its count is the number of requests, so 401s, 404s and 429s show too, a client that hung up before the reply as 499, and a reply a panic cut off midway
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
- **Go runtime** (`go.*`): memory and its limit (`go.memory.limit`, only when there is one:
  `GOMEMLIMIT`, or what `startup.LimitMemory` sets from the container's), goroutines, garbage
  collection.

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

## What to alert on

Alert on what users feel (errors, slowness, work that stopped), not on causes: a cause no one feels
yet belongs on a dashboard. Below, each alert's PromQL (as Prometheus stores these metrics: dots
become `_`, units a suffix), when to fire, and where to look next. The rules themselves live where
your alerts are managed; copy these into them. The thresholds are a quiet start: after a week, set
them from your own normal. **Urgent** means someone acts now; the rest can wait for working hours.

`job` is each program's `service.name` (`<project>-<program>`) and `instance` its
`service.instance.id`, as Prometheus names pushed metrics (OTLP); scope each rule to one environment
too, adding `deployment_environment_name="production"` to its metrics, or test traffic dilutes
production's. That label, like `service_version`, exists only if the store promotes the resource
attribute (`promote_resource_attributes` in `local/prometheus.yml`); a stock Prometheus keeps both
on `target_info` alone. With `OTEL_METRICS_EXPORTER=prometheus` (scraped), `instance` is the address
scraped, the same after a restart: count copies by `service_instance_id` on `target_info` instead.

Every program:

- **Stopped sending metrics** (urgent): no runtime metrics for 10 minutes. It's down, or its
  metrics don't arrive (its log warns once a minute when sending fails). A program that's down
  sends nothing, so also check from outside, with an uptime check. One rule per program
  (`<project>-<program>`).

  ```promql
  absent_over_time(go_memory_used_bytes{job="<project>-<program>"}[10m])
  ```

- **Restarting** (urgent): more than 2 copies came and went in 30 minutes. Each start is a new
  `service.instance.id` (`instance`); a crash loop or out-of-memory kills shows as many. Its last
  log lines say why. "Now" is the copies heard from in the last 3 minutes (each sends every
  minute, `OTEL_METRIC_EXPORT_INTERVAL`): a stopped copy's numbers stay readable for 5. The `or`
  counts none running as 0, so a loop with no copy up still fires. A rolling deploy replaces
  every copy, and so does a planned stop or scale-down: keep the threshold above your number of
  copies (two deploys in 30 minutes still fire it). A copy killed or crashed before its first send
  (a minute) never shows, though one that exits sends what it has; the stopped-sending alert covers
  the rest.

  ```promql
  count by (job) (count by (job, instance) (count_over_time(go_memory_used_bytes[30m])))
    - (count by (job) (count by (job, instance) (count_over_time(go_memory_used_bytes[3m])))
       or count by (job) (count by (job, instance) (count_over_time(go_memory_used_bytes[30m]))) * 0)
    > 2
  ```

- **Memory near its limit**: above 90% of Go's limit for 15 minutes (no limit, no alert). Go is
  collecting garbage hard, which costs CPU, and the next step is the container killing it: take a
  heap profile ([below](#profiles)). Go's limit is itself a share of the container's
  (`AUTOMEMLIMIT`, 0.9 by default), so this fires at about 81% of the container's memory.

  ```promql
  sum by (job, instance) (go_memory_used_bytes) / sum by (job, instance) (go_memory_limit_bytes) > 0.9
  ```

- **Waiting for database connections**: requests spend over 0.1 s a second, all together,
  waiting for a free connection, for 10 minutes. Every connection is busy: slow queries hold
  them, or the pool is too small for the traffic. Counted in time, not waits: many short waits
  for a new connection to open cost no one anything.

  ```promql
  sum by (job) (rate(pgxpool_empty_acquire_wait_time_nanoseconds_total[5m])) / 1e9 > 0.1
  ```

- **Slow queries**: the slowest 5% take over 0.5 s for 10 minutes, with a query every 10 s (a
  program that queries rarely, a daily job's, would otherwise fire on one long query). The dashboard's "Query time p95, by query" names the query; `EXPLAIN` it.

  ```promql
  histogram_quantile(0.95, sum by (le, job) (rate(db_client_operation_duration_seconds_bucket{pgx_operation_type="query"}[5m]))) > 0.5
  and sum by (job) (rate(db_client_operation_duration_seconds_count{pgx_operation_type="query"}[5m])) > 0.1
  ```

- **Failing database calls**: over 5% fail for 10 minutes, with a call every 10 s. Every query
  first takes a connection (`acquire`), so this is failed queries and failed takes per take: an
  unreachable database shows as failed takes, not queries. Postgres is refusing or dropping them;
  the program's error lines say which. A broken unique rule (a duplicate title) counts too, and
  so does a query or wait cancelled because its client hung up (499), so a few are normal. A
  transaction runs several queries on one take, so code that uses them can push the share above
  1.

  ```promql
  sum by (job) (rate(db_client_operation_errors_total{pgx_operation_type=~"query|acquire"}[5m]))
    / sum by (job) (rate(db_client_operation_duration_seconds_count{pgx_operation_type="acquire"}[5m])) > 0.05
  and sum by (job) (rate(db_client_operation_duration_seconds_count{pgx_operation_type="acquire"}[5m])) > 0.1
  ```
<!-- remove:api -->

The API:

- **Server errors** (urgent): over 1% of requests get a 5xx, or a reply a panic cut off midway
  (`error_type` `aborted`, recorded with the status already sent), for 5 minutes, while there's
  traffic (a request every 10 s). The request log's error lines (`level` error) say what failed.

  ```promql
  sum by (job) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m])
                or rate(http_server_request_duration_seconds_count{error_type="aborted"}[5m]))
    / sum by (job) (rate(http_server_request_duration_seconds_count[5m])) > 0.01
  and sum by (job) (rate(http_server_request_duration_seconds_count[5m])) > 0.1
  ```

- **Slow requests**: a route's slowest 5% take over 1 s for 10 minutes, with a request a
  minute on it. Per route: one slow endpoint hides in the overall number. Its trace says where
  the time goes ([tracing.md](tracing.md)).

  ```promql
  histogram_quantile(0.95, sum by (le, job, http_route) (rate(http_server_request_duration_seconds_bucket[5m]))) > 1
  and sum by (job, http_route) (rate(http_server_request_duration_seconds_count[5m])) > 1/60
  ```

Watched on the dashboards, not alerted on: 4xx and 429 replies (a client's mistake, or one
client sending too much), and traffic by route.
<!-- remove:end -->
<!-- remove:worker -->

The worker:

- **A job failed for good**: any job used its last retry in the last 15 minutes. Its
  `job completed` line has the error; the job is gone from the queue. A count's series only
  appears at its first event, already at 1, which `increase` can't see: the second half catches a
  series that appeared in the window, such as the first failure since a restart, through
  `last_over_time` even if that copy has stopped since.

  ```promql
  sum by (job, job_type) (increase(job_run_duration_seconds_count{job_result="failed"}[15m])) > 0
  or count by (job, job_type) (last_over_time(job_run_duration_seconds_count{job_result="failed"}[15m])
    unless job_run_duration_seconds_count{job_result="failed"} offset 15m)
  ```

- **Jobs retrying**: over 20% of runs end in a retry for 15 minutes, with a run every 10 s (one
  retry of a rare job would be 100%; a rare job that keeps failing shows as failed for good).
  Something they call is failing (the database, another service).

  ```promql
  sum by (job) (rate(job_run_duration_seconds_count{job_result="retrying"}[15m]))
    / sum by (job) (rate(job_run_duration_seconds_count[15m])) > 0.2
  and sum by (job) (rate(job_run_duration_seconds_count[15m])) > 0.1
  ```

- **The daily purge didn't succeed** in 25 hours (it runs at midnight UTC): deleted rows pile
  up. Copy this for each job on a timetable, with a window a little longer than its interval. The
  second half counts a first success since a restart, as above, kept by `last_over_time` after
  the next deploy stops that copy; without it the alert fires after every deploy. `absent`
  returns no labels: give the rule its `job` (and environment) as labels of its own. It needs 25
  hours of stored data, and fires in a new environment until the first run.

  ```promql
  absent(
    sum(increase(job_run_duration_seconds_count{job="<project>-worker", job_type="deleted_rows:purge", job_result="succeeded"}[25h])) > 0
    or count(last_over_time(job_run_duration_seconds_count{job="<project>-worker", job_type="deleted_rows:purge", job_result="succeeded"}[25h])
      unless job_run_duration_seconds_count{job="<project>-worker", job_type="deleted_rows:purge", job_result="succeeded"} offset 25h) > 0
  )
  ```

Not covered: a worker that runs but takes no jobs (nothing measures how many wait in the queue;
asynq's `Inspector` can, as a metric of your own), and a job that hangs (a run records nothing
until it ends; its time limit ends it). The purge alert catches a stuck worker within a day.
<!-- remove:end -->
<!-- remove:valkey -->

Valkey:

- **Command errors** (urgent): over 5% of a client's commands fail for 5 minutes, with a command a
  second. What that client (`pool_name`) is for stops working. `status` `nil` (a key that isn't
  there) is normal, and so is one `NOSCRIPT` error per script after Valkey or a copy starts (each
  script is loaded on its first use): too few to reach 5% of a busy client. A client that sends
  few commands reads one error as a big share, so the minimum leaves it out: its failures show as
  errors of whatever called it.

  ```promql
  sum by (job, pool_name) (rate(db_client_connections_use_time_milliseconds_count{status="error"}[5m]))
    / sum by (job, pool_name) (rate(db_client_connections_use_time_milliseconds_count[5m])) > 0.05
  and sum by (job, pool_name) (rate(db_client_connections_use_time_milliseconds_count[5m])) > 1
  ```
<!-- remove:end -->

Watched on the dashboards, not alerted on: goroutine counts.

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

## Profiles

Metrics say a program is slow or growing; a profile says where: which functions use the CPU, what
holds memory, what every goroutine is waiting on. Go records them itself
([`net/http/pprof`](https://pkg.go.dev/net/http/pprof)); the long-running programs serve them
only when `PPROF_ADDR` is set, on that port of their own (`startup.ServeProfiles`).

```sh
PPROF_ADDR=127.0.0.1:6060 go run ./cmd/<program>
go tool pprof -http=:8081 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'   # CPU
go tool pprof -http=:8081 'http://127.0.0.1:6060/debug/pprof/heap'                 # memory
curl 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=2'                      # every goroutine
```

- **Only on this machine**: `PPROF_ADDR` must be `127.0.0.1`, `::1` or `localhost` with a port,
  or the program refuses to start. Profiles show the program's insides (its stacks, what's in
  memory), so they never go on the network: reach a server's with a port forward
  (`kubectl port-forward pod/<pod> 6060`), or in plain Docker from a container sharing its network
  (`docker run --rm --network container:<name> curlimages/curl ...`; `-p` can't reach
  127.0.0.1 inside), and unset it once done. Programs sharing one network (an API and a worker in
  one pod) need different ports.
- **Off by default**: a port nobody uses is one more to secure. When on, the program logs
  `profiling on` at warn at start.
- A profile over time (`?seconds=`, whole seconds: CPU, 30 by default; a trace, which takes
  fractions; or a heap or block profile's difference over that time) lasts up to 60 seconds, and
  slows the program a little meanwhile. Some pages pause it outright: `goroutine?debug=2` while it
  writes every goroutine's stack, `heap?gc=1` for a full garbage collection. Only GET is served
  (and POST for `symbol`, up to 1 MiB). `cmdline` isn't: it shows the program's arguments.

