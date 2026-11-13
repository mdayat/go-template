.DEFAULT_GOAL := run

# Run every command on the exact Go version in go.mod (the one CI uses), even when a newer Go is
# installed, so code can't quietly depend on something only the newer version has.
export GOTOOLCHAIN := go$(shell sed -n 's/^go //p' go.mod)

# Linter, sqlc and govulncheck are pinned in tools/go.mod, apart from the app's own dependencies.
TOOL := go tool -modfile=tools/go.mod

# migrate-apply uses DATABASE_URL from the environment, else from .env, the same order the app uses.
# (Not `include .env`: a value from an included file would beat the environment.) Reads the value
# the way the app's .env loader does: KEY=value, KEY="value", KEY='value', export KEY=value, and
# for unquoted values a trailing " # comment" and spaces are dropped. (HASH: a bare # in a Makefile
# starts a comment.)
HASH := \#
export DATABASE_URL ?= $(shell sed -nE 's/^(export[[:space:]]+)?DATABASE_URL=//p' .env 2>/dev/null | tail -n 1 \
	| sed -E -e "s/^\"([^\"]*)\".*/\1/;t" -e "s/^'([^']*)'.*/\1/;t" -e "s/[[:space:]]+$(HASH).*//" -e "s/[[:space:]]+$$//")

.PHONY: fmt lint test test-db-clean vuln build rename image run run-worker db-up db-down generate \
	check-generated migrate-diff migrate-apply

fmt:
	$(TOOL) golangci-lint fmt ./...

lint:
	$(TOOL) golangci-lint run ./...

# Tests get their own databases on the compose Postgres (TEST_DATABASE_URL) and queue jobs on the
# compose Valkey (TEST_REDIS_URL), so both must be up.
test: db-up
	go test -race -shuffle=on ./...

# Drop the databases failed tests kept, and masters an interrupted run left. Not while tests run.
test-db-clean:
	docker compose exec -T postgres psql -U postgres -Atc \
		"SELECT format('DROP DATABASE %I;', datname) FROM pg_database WHERE datname LIKE 'gotest\_%'" \
		| docker compose exec -T postgres psql -U postgres -q

vuln:
	$(TOOL) govulncheck ./...

build: lint
	go build -o api ./cmd/api
	go build -o worker ./cmd/worker

# Run once in a project copied from the template: make rename module=example.com/you/app
# Renames the module in both go.mod files, Go imports and the linter's import grouping, and sets
# IMAGE to the module's last part (lowercase, as Docker requires; a /v2-style suffix skipped).
# README links are left to edit by hand.
rename:
	@test -n "$(module)" || { echo "usage: make rename module=<new module path>"; exit 1; }
	@old=$$(go list -m) && \
		image=$$(echo "$(module)" | sed -E 's|/v[0-9]+$$||; s|.*/||' | tr '[:upper:]' '[:lower:]') && \
		go mod edit -module "$(module)" && \
		(cd tools && go mod edit -module "$(module)/tools") && \
		grep -rlF --include='*.go' --include='.golangci.yml' "$$old" . \
			| xargs perl -pi -e "s|\Q$$old\E|$(module)|g" && \
		perl -pi -e "s|^IMAGE \\?= .*|IMAGE ?= $$image|" Makefile && \
		$(TOOL) golangci-lint fmt ./... && \
		echo "Renamed $$old to $(module)."

IMAGE ?= go-template

# The API image, the worker image, and the image that applies migrations (see Dockerfile).
image:
	docker build --build-arg VERSION=$$(git describe --always --dirty) -t $(IMAGE) .
	docker build --build-arg VERSION=$$(git describe --always --dirty) --target worker -t $(IMAGE)-worker .
	docker build --target migrate -t $(IMAGE)-migrate .

run:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

db-up:
	docker compose up -d --wait

db-down:
	docker compose down

# Regenerate internal/db from db/queries and db/schema.sql.
generate:
	$(TOOL) sqlc generate

# Fails if a generated file is out of date: internal/db (sqlc), atlas.sum, migrations that don't
# build db/schema.sql, or an untidy go.mod. Needs Docker. Atlas's schema diff exits 0 even when the
# schemas differ, so its output is checked instead.
check-generated:
	$(TOOL) sqlc diff
	atlas migrate validate --env local
	@out=$$(atlas schema diff --env local --from file://db/migrations --to file://db/schema.sql 2>&1); \
		echo "$$out" | grep -qx "Schemas are synced, no changes to be made." || { \
		echo "$$out"; echo "db/schema.sql differs from db/migrations: make migrate-diff name=<what_changed>"; exit 1; }
	go mod tidy -diff
	cd tools && go mod tidy -diff

# Write a migration for the difference between db/schema.sql and db/migrations (needs Docker).
migrate-diff:
	@test -n "$(name)" || { echo "usage: make migrate-diff name=<what_changed>"; exit 1; }
	atlas migrate diff --env local $(name)

migrate-apply:
	@test -n "$$DATABASE_URL" || { echo "DATABASE_URL is not set (in the environment or .env)"; exit 1; }
	atlas migrate apply --env local
