# syntax=docker/dockerfile:1
#
# Two-stage build for the Agent Brain Visualizer: compile the fat jar under a full JDK, then run it
# on a slim JRE. Self-contained — you don't need Java or Gradle on the host. Used by the `app`
# service in docker-compose.yml (`docker compose --profile full up -d --build`).

# --- build stage -----------------------------------------------------------
FROM eclipse-temurin:25-jdk AS build
WORKDIR /src
# Stamp the image with a version. build.gradle reads GITHUB_REF_NAME for the app version (surfaced
# at GET /health and `--version`); compose passes a release tag or `git describe` as APP_VERSION.
# Defaults to "docker" for an unlabelled local build. Use a tag or `git describe` — a value with a
# slash or space (e.g. a branch name) becomes the jar filename and breaks the `cp` glob below.
ARG APP_VERSION=docker
ENV GITHUB_REF_NAME=$APP_VERSION
COPY . .
# The cache mount keeps Gradle's downloaded dependencies across builds, so a code
# change rebuilds the jar without re-fetching the world.
RUN --mount=type=cache,target=/root/.gradle \
    ./gradlew --no-daemon clean shadowJar -x test \
    && cp build/libs/*-all.jar /app.jar

# --- runtime stage ---------------------------------------------------------
# The base tags float on their major version deliberately: rebuilding picks up JRE security patches.
# Pin to a digest if you need bit-for-bit reproducible images.
FROM eclipse-temurin:25-jre AS runtime
WORKDIR /app
COPY --from=build /app.jar app.jar

# Run as an unprivileged user, not root. The app only reads the jar and talks to Postgres — it needs
# no write access to the image.
RUN useradd --system --uid 10001 app
USER app

EXPOSE 8080

# application.yml binds loopback only, which is right for a host process but wrong here: inside the
# container's own network namespace nothing else could reach it, so the published port would be dead.
# In a container the boundary is Docker's port publishing — docker-compose.yml publishes to
# 127.0.0.1, and the production override keeps it there behind a TLS-terminating proxy.
ENV MICRONAUT_SERVER_HOST=0.0.0.0

# Same probe as the `app` healthcheck in docker-compose.yml (which takes precedence under compose),
# so the image also self-reports health under plain `docker run` or other orchestrators. The JRE
# image ships bash but no curl/wget, hence bash's /dev/tcp for a minimal HTTP request. Liveness
# (not readiness) on purpose: the app serves the UI even when the store is down, so a database blip
# must not mark the container unhealthy.
HEALTHCHECK --interval=10s --timeout=5s --retries=12 --start-period=40s \
    CMD ["bash", "-c", "exec 3<>/dev/tcp/localhost/8080 && printf 'GET /health HTTP/1.0\\r\\n\\r\\n' >&3 && grep -q '200 OK' <&3"]

# Configuration comes from the environment (compose sets DATABASE_URL, credentials, and the optional
# AI keys), so ignore any stray .env that lands in the working directory.
ENTRYPOINT ["java", "-Ddotenv.enabled=false", "-jar", "app.jar"]
