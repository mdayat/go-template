# Go Template Repository

This repository serves as a template for speeding up project scaffolding for Go projects. It includes a Dockerfile, golangci-lint configuration, Makefile, example environment variables, zerolog setup, an `env` package, and a GitHub workflow for code quality checks.

## Features

- **Dockerfile**: Pre-configured Dockerfile for containerizing your Go application.
- **.golangci.yml**: Configuration for `golangci-lint`, which runs `revive`, `staticcheck` and other linters.
- **tools/go.mod**: Pins `golangci-lint`, `sqlc` and `govulncheck`, run with `go tool -modfile=tools/go.mod`.
- **Makefile**: `fmt`, `lint`, `test`, `vuln`, `build` and `run`, all on the Go version in `go.mod`.
- **.example.env**: Example environment file to manage application settings.
- **zerolog**: A fast and structured logger for Go.
- **`env` package**: For managing environment variables.
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

3. Build or run the application:
   ```sh
   make build
   make run
   ```
