# Testing

The goal: a change that breaks something that used to work fails a test before it merges. Not a
coverage number: covered lines can still be wrong, and a test that can't fail proves nothing.

## What to test, and how

- **Through the real entry point, against a real database.** An endpoint test sends a request to
  the real router with the real services on a real Postgres (`testAPI` in
  `internal/api/main_test.go` builds them), then checks two things: **the reply and the rows**.
  Example: `internal/api/tasks_test.go`. A job test runs the job through the real handlers
  (`testWorker` in `internal/worker/main_test.go`) and checks **the error and the rows**.
- **A service's rules are tested on the service**, on a real database: every input a rule
  accepts or refuses, the exact limits, and the rows after. Example: `internal/task/service_test.go`.
  The rules stay tested whichever entry points a project keeps. Endpoint and job tests then check
  what the caller gets: one case per distinct reply or outcome, not every input that gives the
  same one.
- **Don't mock our own code.** A mock of our own service tests the mock. Fake only what we can't
  run locally: a payment provider, an email service, another company's API. Fake it with an
  `httptest.Server` and check what we send to it ([outside-calls.md](outside-calls.md#tests)).
- **Every success and every error path** a caller can reach: each reply an endpoint can return
  (status, detail, field errors), with the rows unchanged after a failure.
- **Unit tests for tricky logic only** (parsing, calculations), as plain table tests; see
  `internal/api/decode_test.go`.
- **The cases that break in production**: two requests at once (`TestCreateTaskSameTitleAtOnce`),
  paging with equal timestamps, a database that's down, input at the exact limit.

## A database per test

`testdb.New(t)` returns a pool on a database that only this test uses, with every migration
applied. It's copied from a master database built once per package, which takes about 35 ms. Tests don't share rows, so they can run in parallel and in any order, and each one
creates the rows it needs.

- Each package that uses it has `func TestMain(m *testing.M) { testdb.Main(m) }` (drops the master
  at the end).
- Insert seed rows directly with SQL (`seedTask`), not through the endpoint under test, so a broken
  create endpoint doesn't fail every other test.
- A failed test's database is kept and its name printed, so you can look inside; `make
  test-db-clean` drops them.
- `TEST_DATABASE_URL` must point at a local Postgres (`.example.env`); anything else is refused.
  If it's unset, tests fail rather than skip: a skipped test looks like a passing one.

## The job queue in tests

Tests that queue jobs use a real Valkey, through `internal/testqueue`: `testqueue.New(t)` connects
to `TEST_REDIS_URL`, which must be a local Valkey and a database other than 0, the one the app
uses, so a worker running on your machine never picks up a test's job. Tests share that database,
so each one finds its own job by ID and deletes it: `testqueue.Take(t, client, jobs.Queue, id)`.
Like `TEST_DATABASE_URL`, an unset or refused `TEST_REDIS_URL` fails the tests. Job handlers
themselves don't need Valkey; see [worker.md](worker.md#tests).

## Style

- `t.Parallel()` in every test and subtest, unless the test changes process-wide state.
- Table tests for several inputs through one path; one test per path otherwise.
- Compare with `cmp.Diff(want, got)` from [go-cmp](https://github.com/google/go-cmp), and print
  `(-want +got)`. No assertion libraries.
- Failure messages say what was called, what came back and what was expected:
  `t.Errorf("POST /tasks status = %d, want 201; body %s", rec.Code, rec.Body)`. In a loop without
  subtests, the message names the input, and the loop uses `t.Error` so every input is checked.
- `t.Fatal` when the rest of the test can't run (no reply to decode); `t.Error` otherwise, so one
  run shows every problem.
- Helpers every feature's tests use (`testAPI`, `send`, `readProblem`, `seedTime`) live in
  `internal/api/main_test.go`, not in one feature's test file, so removing a feature can't break
  the others.
- The test decides pass or fail. Helpers build things and fetch results (`seedTask`, `tasksInDB`,
  `readProblem`) and stop the test only when that setup itself fails; the test compares the result
  with `cmp.Diff` and reports, so each case decides its own message and whether to stop.
- Helpers call `t.Helper()` and take `t` first. Tests use `t.Context()`, not
  `context.Background()`. — lint (thelper, usetesting, tparallel)
- Test names are `Test` + what's tested, without underscores, also for unexported functions
  (`TestLocalURLRefusesOtherHosts`).
- Don't assert on text that comes from a library (Go's JSON errors, driver messages): it changes
  between versions. Our own messages, status codes and field codes are fine to assert.

## Prove the test can fail

After writing a test, break the rule it guards (comment out the check, flip the comparison) and
run it: it must fail. The broken code must still compile, or the "failure" proves nothing. A test
that passes either way is deleted or fixed.

## Running

```sh
make test   # starts Postgres and Valkey, then go test -race -shuffle=on ./...
```

`-race` finds data races; `-shuffle=on` runs tests in random order, so a test that depends on
another one's leftovers fails. To repeat a flaky-looking test:
`go test -race -count=20 -run TestName ./internal/api/`.
