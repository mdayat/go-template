# Go Template Repository

This repository serves as a template for speeding up project scaffolding for Go projects. It includes a Dockerfile, revive configuration, Makefile, example environment variables, zerolog setup, an `env` package, and a GitHub workflow for code quality checks.

## Features

- **Dockerfile**: Pre-configured Dockerfile for containerizing your Go application.
- **revive.toml**: Configuration file for `revive`, a fast Go linter.
- **Makefile**: Automate various tasks such as building and running the application.
- **.example.env**: Example environment file to manage application settings.
- **zerolog**: A fast and structured logger for Go.
- **`env` package**: For managing environment variables.
- **GitHub Workflow**: Pre-configured GitHub Actions workflow for code quality checks.

## Getting Started

### Prerequisites

- Go
- Docker
- [revive](https://github.com/mgechev/revive)

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
