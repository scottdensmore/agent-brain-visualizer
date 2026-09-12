# Agent Brain Visualizer

Agent Brain Visualizer is a Micronaut web application for inspecting JSONL
execution transcripts from Antigravity, OpenAI Codex, and Claude Code. The
application normalizes the source-specific formats, stores trajectories in
Postgres, and renders them as an interactive timeline with analysis and
cross-session tools.

## Architecture

- **Backend:** Java 25, Micronaut 5 on Netty, and compile-time Micronaut Serde.
  The main class is `io.github.glaforge.agybrainviz.Application`.
- **Persistence:** Plain JDBC through Micronaut HikariCP and PostgreSQL. There
  is no ORM. `SchemaBootstrap` applies
  `src/main/resources/db/schema.sql` at startup. The schema stores sessions,
  summaries, attached session files, and evaluation runs.
- **AI:** LangChain4j 1.18 with Google Gemini and Ollama providers. `AI_PROVIDER`
  selects `gemini` (the default) or `ollama`; the application remains usable
  without an AI provider, but summary and AI-assisted features are unavailable.
- **Frontend:** Zero-build vanilla JavaScript, HTML, and CSS. ES modules live
  under `src/main/resources/public/modules/`. Runtime dependencies are pinned
  `marked` and `highlight.js` CDN assets plus the vendored DOMPurify bundle.
- **Ingest CLI:** `cli/` contains the standalone Go `agent-ingest` binary. It
  scans local Antigravity, Codex, and Claude Code transcripts, diffs against
  server manifests, uploads changed sessions and summaries, and can upload
  explicitly attached files for inline previews.

## Application capabilities

The backend exposes controllers for:

- session lists, normalized transcripts, and attached-file previews (`/api/brain`)
- AI summaries and progress (`/api/analysis`)
- trajectory and summary manifests plus batch ingestion (`/api/ingest`)
- fleet insights and drill-downs (`/api/insights`)
- reusable workflow, skill, and `AGENTS.md` mining (`/api/mine`)
- deterministic analysis evaluation, saved runs, comparisons, and CSV export
  support (`/api/eval`)
- prompt-variant comparison using the evaluation score (`/api/optimize`)
- liveness and database readiness probes (`/health`, `/health/ready`)

Supported stored sources are `antigravity-cli`, `antigravity-ide`,
`antigravity`, `codex`, and `claude-code`. The CLI scans Antigravity under
`~/.gemini`, Codex under `~/.codex/sessions`, and Claude Code under
`~/.claude/projects`.

## Local development

The repository pins its toolchain in `mise.toml`:

- Java `temurin-25`
- Node.js `22`
- Go `1.26`

Install those tools with `mise install`, or use `mise exec --` before commands
when the tools are not active in the shell.

### Run the application on the host

Postgres is provided by the default compose file and is published only on
`127.0.0.1:55433`:

```bash
docker compose up -d postgres
mise exec -- ./gradlew run
```

The host application listens on `http://localhost:8080`. It still serves the
UI if Postgres is unavailable; store-backed requests return `503` and
`/health/ready` reports the database as down.

For a containerized app and database, use the `full` profile:

```bash
docker compose --profile full up -d --build
```

That stack is available at `http://localhost:8200`. The LAN and production
overlays are `docker-compose.lan.yml` and `docker-compose.prod.yml`; read the
deployment sections in `README.md` before exposing the service beyond the
loopback interface.

### Configuration

Copy `.env.example` to `.env` for local configuration. Real environment
variables override values from `.env`; `-Ddotenv.enabled=false` disables dotenv
loading and `-Ddotenv.path=<path>` selects another file.

AI configuration:

- Gemini: set `AI_PROVIDER=gemini` and `GEMINI_API_KEY`; optionally set
  `GEMINI_MODEL`.
- Ollama: set `AI_PROVIDER=ollama`; optionally set `OLLAMA_BASE_URL` and
  `OLLAMA_MODEL`.

Store and server configuration includes `DATABASE_URL`, `POSTGRES_USER`,
`POSTGRES_PASSWORD`, `MICRONAUT_SERVER_HOST`, `MICRONAUT_SERVER_PORT`, and
`LOG_APPENDER` (`CONSOLE_TEXT` or `CONSOLE_JSON`).

The read/compute API and ingest API have separate optional bearer tokens:
`API_TOKEN`/`API_REQUIRE_AUTH` and `INGEST_TOKEN`/`INGEST_REQUIRE_AUTH`.
Defaults are intended for loopback development. Set distinct tokens before
binding the app to another interface. The CLI reads its ingest token from
`AGENT_INGEST_TOKEN` rather than a command-line argument.

## Testing and formatting

Backend tests use JUnit 5 and Testcontainers with a real PostgreSQL instance.
Frontend unit tests use Vitest and jsdom. Playwright drives end-to-end tests
against a packaged fat jar and a real Postgres store. CLI tests use Go's test
runner.

Typical checks are:

```bash
mise exec -- ./gradlew build       # backend tests, compilation, and Spotless
npm ci                              # once, for frontend and e2e tests
npm test                            # frontend unit tests
(cd cli && go test ./...)           # CLI tests
(cd cli && go vet ./...)            # CLI static checks
(cd cli && go build ./...)          # CLI build
```

For end-to-end tests, start the database, build the fat jar, and run Playwright
from the repository root:

```bash
docker compose up -d postgres
mise exec -- ./gradlew shadowJar -x test --no-daemon
mise exec -- npm run e2e
```

`npm run e2e` uses port `8099`, seeds fixtures, and disables dotenv loading.
When reusing a local server, stop the old process after changing the jar or
fixture home.

Run `mise exec -- ./gradlew spotlessApply` to format Java and frontend
JavaScript, and `mise exec -- ./gradlew spotlessCheck` to verify formatting.
The frontend itself has no build step.

## Native and release builds

The Gradle build also provides the Shadow fat jar, Micronaut AOT, Dockerfile
generation, and GraalVM native-image support:

```bash
mise exec -- ./gradlew shadowJar
mise exec -- ./gradlew nativeCompile
```

The native executable is written to
`build/native/nativeCompile/agy-brain-viz`. CI additionally smoke-tests the
native store path and the full Docker compose profile. Release workflows build
native application binaries and cross-platform `agent-ingest` binaries.

## Code conventions

### Java

- Use constructor injection for Micronaut beans and keep shared beans
  stateless or thread-safe.
- Use records for data carriers where appropriate.
- Use explicit imports; do not use wildcard imports or fully qualified class
  names in source.
- Use prepared statements and short-lived JDBC resources with
  try-with-resources. Keep schema changes idempotent and aligned with the
  repository queries.
- Keep blocking JDBC and filesystem work off Netty's event loop.
- Treat native-image compatibility as a correctness requirement: avoid
  unregistered reflection and use `@Serdeable` for JSON boundary types.

### JavaScript and CSS

- Use modern ES modules and browser APIs; do not add a frontend framework or
  build system.
- Keep rendering and API interactions isolated in the existing modules.
- Treat transcript, file, and model content as untrusted. Preserve the existing
  DOMPurify boundary and do not interpolate unsanitized content into HTML.
- Use CSS variables for theme values and keep the responsive layout accessible
  across viewport sizes.

### Go CLI

- Keep `agent-ingest` composable: diagnostics go to stderr and JSON summaries
  go to stdout.
- Check and wrap errors, close files and response bodies, use bounded HTTP
  requests, and keep the scan cache optional and recoverable.
- Run `gofmt`, `go vet ./...`, and `go test ./...` for CLI changes.

## Important files

- `build.gradle` — Micronaut, LangChain4j, JDBC, Testcontainers, native-image,
  and Spotless configuration.
- `src/main/java/io/github/glaforge/agybrainviz/` — backend controllers,
  normalizers, services, repositories, and API records.
- `src/main/resources/application.yml` — server and datasource defaults.
- `src/main/resources/db/schema.sql` — database schema bootstrap.
- `src/main/resources/public/` — the static frontend and its modules.
- `cli/` — Go ingest CLI and unit tests.
- `e2e/` — Playwright scenarios and seeded fixtures.
- `src/test/java/` and `src/test/js/` — backend and frontend unit tests.
- `docker-compose.yml`, `docker-compose.lan.yml`, and
  `docker-compose.prod.yml` — local, LAN, and production container overlays.
- `.env.example` — documented configuration template; keep secrets out of the
  repository.

Read `README.md` and `cli/README.md` for user-facing installation,
deployment, API, and scheduling details.
