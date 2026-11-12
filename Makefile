.DEFAULT_GOAL := run

# Run every command on the exact Go version in go.mod (the one CI uses), even when a newer Go is
# installed, so code can't quietly depend on something only the newer version has.
export GOTOOLCHAIN := go$(shell sed -n 's/^go //p' go.mod)

# Linter, sqlc and govulncheck are pinned in tools/go.mod, apart from the app's own dependencies.
TOOL := go tool -modfile=tools/go.mod

.PHONY: fmt lint test vuln build run

fmt:
	$(TOOL) golangci-lint fmt ./...

lint:
	$(TOOL) golangci-lint run ./...

test:
	go test -race -shuffle=on ./...

vuln:
	$(TOOL) govulncheck ./...

build: lint
	go build -o app .

run:
	go run .
