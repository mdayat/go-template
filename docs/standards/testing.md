# Testing

The goal: a change that breaks something that used to work fails a test before it merges. Not a
coverage number: covered lines can still be wrong, and a test that can't fail proves nothing.

## What to test, and how

- **Through the real entry point, against a real database.** An endpoint test sends a request to
  the real router (`NewRouter`) with the real service on a real Postgres, then checks two things:
  **the reply and the rows**. Example: `internal/api/tasks_test.go`.
- **Don't mock our own code.** A mock of our own service tests the mock. Fake only what we can't
  run locally: a payment provider, an email service, another company's API.
- **Every success and every error path** a caller can reach: each status code an endpoint can
  return, with the rows unchanged after a failure.
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

## Style

- `t.Parallel()` in every test and subtest, unless the test changes process-wide state.
- Table tests for several inputs through one path; one test per path otherwise.
- Compare with `cmp.Diff(want, got)` from [go-cmp](https://github.com/google/go-cmp), and print
  `(-want +got)`. No assertion libraries.
- Failure messages say what was called, what came back and what was expected:
  `t.Errorf("status = %d, want 201; body %s", rec.Code, rec.Body)`.
- `t.Fatal` when the rest of the test can't run (no reply to decode); `t.Error` otherwise, so one
  run shows every problem.
- Helpers call `t.Helper()` and take `t` first. Tests use `t.Context()`, not
  `context.Background()`. — lint (thelper, usetesting, tparallel)
- Don't assert on text that comes from a library (Go's JSON errors, driver messages): it changes
  between versions. Our own messages, status codes and field codes are fine to assert.

## Prove the test can fail

After writing a test, break the rule it guards (comment out the check, flip the comparison) and
run it: it must fail. The broken code must still compile, or the "failure" proves nothing. A test
that passes either way is deleted or fixed.

## Running

```sh
make test   # starts Postgres, then go test -race -shuffle=on ./...
```

`-race` finds data races; `-shuffle=on` runs tests in random order, so a test that depends on
another one's leftovers fails. To repeat a flaky-looking test:
`go test -race -count=20 -run TestName ./internal/api/`.
