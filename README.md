# Go Template Repository

This repository serves as a template for speeding up project scaffolding for Go projects. It includes a Dockerfile, golangci-lint configuration, Makefile, example environment variables, zerolog setup, a `config` package, and a GitHub workflow for code quality checks.

## Features

- **Dockerfile**: Pre-configured Dockerfile for containerizing your Go application.
- **.golangci.yml**: Configuration for `golangci-lint`, which runs `revive`, `staticcheck` and other linters.
- **tools/go.mod**: Pins `golangci-lint`, `sqlc` and `govulncheck`, run with `go tool -modfile=tools/go.mod`.
- **Makefile**: `fmt`, `lint`, `test`, `test-db-clean`, `vuln`, `build`, `run`, `db-up`, `db-down`,
  `generate`, `migrate-diff` and `migrate-apply`, all on the Go version in `go.mod`.
- **Database**: `db/schema.sql` is the schema as it should be. [Atlas](https://atlasgo.io)
  (Community edition) writes migrations from it into `db/migrations`, and
  [sqlc](https://sqlc.dev) turns the queries in `db/queries` into typed Go in `internal/db`.
- **Example feature, tasks**: `internal/task` holds the rules (titles trimmed, 1–200 characters,
  unique) and turns database errors into `apperr` kinds; `internal/api/tasks.go` serves
  `POST/GET /tasks` (newest first, paged with `?limit=` and `?after=<next_cursor>`) and
  `GET/PATCH/DELETE /tasks/{id}`. Copy its shape for new features, then delete it.
- **compose.yaml**: Postgres 17 for local runs and tests.
- **`internal/testdb`**: `testdb.New(t)` gives a test its own database with every migration applied,
  copied from a master database built once per package, so tests run in parallel without sharing
  rows. A failed test's database is kept and its name logged. `internal/api/tasks_test.go` shows the
  style: real router, real Postgres, checking the reply and the rows.
- **.example.env**: Every setting the app reads, with local defaults.
- **zerolog**: A fast and structured logger for Go.
- **`internal/config`**: Loads settings from the environment and reports every missing or invalid one at once.
- **`cmd/api`**: HTTP server with a `/healthz` check and clean shutdown on Ctrl-C or SIGTERM.
- **`internal/api`**: Routes, plus one JSON log line per request with a request ID (also sent back as
  `X-Request-Id`). Query strings, headers and bodies are never logged. Handlers return errors;
  `Handle` turns them into [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) replies, and panics
  become 500 replies with the stack on the request's log line. JSON request bodies are read with a
  1 MB cap, unknown fields rejected, and checked with `validate` struct tags; failures name the
  JSON field and the rule.
- **`internal/apperr`**: The kinds of expected failure (bad request, invalid, not found, conflict)
  services return, with a message safe to show callers. Any other error is a 500.
- **GitHub Workflow**: Pre-configured GitHub Actions workflow for code quality checks.

## Getting Started

### Prerequisites

- Go 1.21 or newer (`make` downloads the exact version in `go.mod`)
- Docker
- [Atlas](https://atlasgo.io) (migrations and tests), Community edition: `brew install atlas`
  (Homebrew's build is the Apache-2.0 Community one; `atlas version` says "community")

### Installation

1. Clone the repository:

   ```sh
   git clone https://github.com/mdayat/go-template.git
   cd go-template
   ```

2. Copy the example environment file:

   ```sh
   cp .example.env .env
   ```

3. Start Postgres, apply the migrations, then run the application:

   ```sh
   make db-up
   make migrate-apply
   make run
   ```

### Running tests

`make test` starts Postgres and runs every test. Tests use `TEST_DATABASE_URL` from the
environment or `.env` (see `.example.env`), and refuse any host but localhost. A failed test's
database is kept for a look; `make test-db-clean` drops them all.

### Changing the database

1. Edit `db/schema.sql` (and the queries in `db/queries`).
2. `make migrate-diff name=<what_changed>` writes the migration into `db/migrations`. Commit it with
   `atlas.sum`, which Atlas uses to detect edited or missing migration files.
3. `make generate` updates `internal/db`; `make migrate-apply` applies the migration locally.

   If another Postgres already uses port 5432, change `POSTGRES_PORT` and the port in
   `DATABASE_URL` in `.env`.
