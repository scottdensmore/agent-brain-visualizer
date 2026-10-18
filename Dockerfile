# syntax=docker/dockerfile:1
#
# Two-stage build for the Agent Brain Visualizer: compile the static Go server binary (with the
# embedded web UI and SQL schema) and the agent-ingest CLI, then run the server in a minimal
# runtime image. Self-contained — you don't need Go on the host. Used by the `app` service in
# docker-compose.yml (`docker compose --profile full up -d --build`).

# --- build stage -----------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
# Stamp the image with a version. main.go reads GITHUB_REF_NAME or -X main.version for the app
# version (surfaced at GET /health and `--version`); compose passes a release tag or `git describe`
# as APP_VERSION. Defaults to "docker" for an unlabelled local build.
ARG APP_VERSION=docker
ENV GITHUB_REF_NAME=$APP_VERSION
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /agy-brain-viz . \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /agent-ingest ./cli

# --- runtime stage ---------------------------------------------------------
FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=build /agy-brain-viz /app/agy-brain-viz
COPY --from=build /agent-ingest /usr/local/bin/agent-ingest

USER app

EXPOSE 8080

# The server binds loopback (127.0.0.1) by default, which is right for a host process but wrong
# inside a container's own network namespace where nothing else could reach it. In a container the
# boundary is Docker's port publishing — docker-compose.yml publishes to 127.0.0.1, and the
# production override keeps it there behind a TLS-terminating proxy.
ENV MICRONAUT_SERVER_HOST=0.0.0.0

# Liveness probe (not readiness) on purpose: the app serves the UI even when the store is down, so a
# database blip must not mark the container unhealthy.
HEALTHCHECK --interval=10s --timeout=5s --retries=12 --start-period=10s \
    CMD ["wget", "-q", "-O", "-", "http://localhost:8080/health"]

# Configuration comes from the environment (compose sets DATABASE_URL, credentials, and the optional
# AI keys), so ignore any stray .env that lands in the working directory.
ENTRYPOINT ["/app/agy-brain-viz", "-Ddotenv.enabled=false"]
