# syntax=docker/dockerfile:1

# Keep the Go version equal to the go line in go.mod: an older one refuses to build.
# The compiler runs on the build machine's platform and cross-compiles for the target one (e.g.
# --platform linux/amd64 from an arm64 Mac), instead of running slowly under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# The build can't see .git, so the commit comes in as VERSION (make image sets it).
ARG VERSION=""
# Static binary (no cgo), so the app image needs no C library.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /out/ ./cmd/api ./cmd/worker

# Applies db/migrations to DATABASE_URL. Run it once before starting a new app version:
#   docker run --rm -e DATABASE_URL=... <image>
# Keep the Atlas version equal to the one CI installs (.github/workflows).
FROM arigaio/atlas:1.3.0-community AS migrate
WORKDIR /migrate
COPY deploy/atlas.hcl atlas.hcl
COPY db/migrations/ migrations/
# The Atlas image runs as root; applying migrations only reads these files.
USER 65532:65532
CMD ["migrate", "apply", "--env", "deploy"]

# The worker: runs queued jobs and queues the scheduled ones. It serves no port; run as many as the
# jobs need.
FROM gcr.io/distroless/static-debian13:nonroot AS worker
COPY --from=build /out/worker /worker
ENTRYPOINT ["/worker"]

# The API. Last, so a plain `docker build` builds it. No shell or package manager, and a non-root
# user. Settings come from the environment; there is no .env in the image.
FROM gcr.io/distroless/static-debian13:nonroot AS app
COPY --from=build /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
