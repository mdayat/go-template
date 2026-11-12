# Go Template Repository

This repository serves as a template for speeding up project scaffolding for Go projects. It includes a Dockerfile, golangci-lint configuration, Makefile, example environment variables, zerolog setup, a `config` package, and a GitHub workflow for code quality checks.

## Features

- **Dockerfile**: Pre-configured Dockerfile for containerizing your Go application.
- **.golangci.yml**: Configuration for `golangci-lint`, which runs `revive`, `staticcheck` and other linters.
- **tools/go.mod**: Pins `golangci-lint`, `sqlc` and `govulncheck`, run with `go tool -modfile=tools/go.mod`.
- **Makefile**: `fmt`, `lint`, `test`, `vuln`, `build`, `run`, `db-up` and `db-down`, all on the Go version in `go.mod`.
- **compose.yaml**: Postgres 17 for local runs.
- **.example.env**: Every setting the app reads, with local defaults.
- **zerolog**: A fast and structured logger for Go.
- **`internal/config`**: Loads settings from the environment and reports every missing or invalid one at once.
- **`cmd/api`**: HTTP server with a `/healthz` check and clean shutdown on Ctrl-C or SIGTERM.
- **GitHub Workflow**: Pre-configured GitHub Actions workflow for code quality checks.

## Getting Started

### Prerequisites

- Go 1.21 or newer (`make` downloads the exact version in `go.mod`)
- Docker

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

3. Start Postgres, then run the application:

   ```sh
   make db-up
   make run
   ```

   If another Postgres already uses port 5432, change `POSTGRES_PORT` and the port in
   `DATABASE_URL` in `.env`.
