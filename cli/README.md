# agent-ingest

A small Go command that scans a machine for local agent transcripts — Antigravity,
OpenAI Codex, and Claude Code — and pushes them to a running
[Agent Brain Visualizer](../README.md), so trajectories captured on any machine are
visible from any other machine pointed at the same store.

The visualizer is the only thing that talks to the database; this CLI is just an
HTTP client for its `/api/ingest` endpoints. It stays deliberately thin — it locates
files, reads them, and uploads them verbatim. **The server parses them**, so the CLI
never has to understand any tool's transcript format.

When a source keeps its own AI analysis on disk (Antigravity's per-session
`summary.json`), the CLI uploads it so a summary computed on one machine reaches the
shared store without a recompute. A summary rides along with its transcript when the
transcript is pushed; a summary that appears **after** its transcript was already ingested
(a common case, since it's written when the session ends) is synced on its own. The CLI
diffs local summaries against a separate summary manifest and sends only the ones the store
is missing or that changed — without re-uploading the transcript — so a post-hoc summary
still lands.

## Install

Download the binary for your platform from the
[latest release](https://github.com/scottdensmore/antigravity-brain-visualizer/releases/latest) —
assets are named `agent-ingest-<os>-<arch>` (`linux`/`macos`/`windows`, `amd64`/`arm64`):

```bash
# Linux/macOS — pick your platform, e.g. macos-arm64 for Apple silicon:
curl -sSL -o agent-ingest \
  https://github.com/scottdensmore/antigravity-brain-visualizer/releases/latest/download/agent-ingest-macos-arm64
chmod +x agent-ingest
./agent-ingest --version
```

Or build from source (Go 1.23+). Stamp the version so `--version` reports which build you're running:

```bash
cd cli
go build -ldflags "-X main.version=$(git describe --tags --always)" -o agent-ingest .
```

(A plain `go build -o agent-ingest .` works too; it just reports `dev` as the version.)

## Use

```bash
# Push everything this machine has to a local visualizer:
agent-ingest

# A remote visualizer, only Claude Code sessions:
agent-ingest --server https://viz.example.com --source claude-code

# See what would happen, without uploading (still queries the manifest):
agent-ingest --dry-run
```

## Pointing it at a shared visualizer

The common setup is one always-on machine running the visualizer — a Mac mini, a NAS, a
spare box — with every laptop and desktop pushing its transcripts there. Two things have
to line up: the address, and the ingest token the server requires.

Ask the server's owner for its address and the value of `INGEST_TOKEN` (it is a different
secret from `API_TOKEN`, which the web UI asks you for). Then, on each machine that
should push:

```bash
# Put both in your shell profile so every run picks them up.
export AGENT_INGEST_SERVER=http://mini.local:8080
export AGENT_INGEST_TOKEN=<the server's INGEST_TOKEN>

agent-ingest
```

Or per-invocation, with the token still coming from the environment so it never lands in
shell history or the process list:

```bash
AGENT_INGEST_TOKEN=... agent-ingest --server http://mini.local:8080
```

Check it before trusting a schedule:

```bash
agent-ingest --server http://mini.local:8080 --dry-run
```

`--dry-run` still contacts the server for the manifest, so it proves the address, the
token, and the network path all work — without uploading anything. A wrong or missing
token fails with a 401 rather than silently pushing nothing.

**The address.** Use whatever the server answers to on your network: `mini.local` if
Bonjour/mDNS resolves it, otherwise its LAN IP (`http://192.168.1.50:8080`). Give the
server a static or reserved DHCP address if you are putting this in a schedule — a
machine that changes IP will fail every run until the config is updated. `https://` works
the same way if the server is behind a TLS-terminating proxy.

**After upgrading a server** to a version that stores attached files, run once with
`--no-cache` from each machine. Sessions already in the store are skipped as unchanged,
so without it their attached files never get uploaded and their previews stay broken:

```bash
agent-ingest --no-cache
```

Running it again uploads nothing: each session is keyed by a stable `(source, id)`
and skipped when the server already has identical content, so it is safe to run on a
schedule (`cron`, a `launchd` timer, …).

Repeat runs are also cheap locally: the CLI keeps a small scan cache (see
`AGENT_INGEST_CACHE` below) mapping each session to the size, mtime, and content hash
seen on the previous run, so unchanged transcripts are not re-read and re-hashed — only
stat'ed. The cache is purely an optimization: it never affects what the server receives
(the server-side manifest diff is always consulted), and a missing, corrupt, or
unwritable cache just falls back to full hashing. `--no-cache` forces that full re-hash.

## Attached files

Antigravity transcripts record the files a prompt attached (`@[name] is a [File]:`), and the viewer
turns them into clickable previews. Those files live on the machine that ran the agent, so a
visualizer running anywhere else could not show them — it read its own disk. `agent-ingest` now
uploads them with the session, and the server serves that copy.

Only files the transcript explicitly references are sent — a path that merely appears in tool output
is not. Each must be under 256 KiB, at most 25 travel with one session, and anything that looks like
credential material is refused whatever the transcript says: `.env` and friends, `*.pem`, `*.key`,
`id_rsa`, `oauth_creds.json`, and paths under `.ssh`, `.aws`, or `.gnupg`. Every skip is reported on
stderr, so a file left behind is visible rather than silent.

Pass `--no-upload-files` to send none; the preview then works only on the machine that has the files.

### Flags

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--server URL` | `http://localhost:8080` (or `$AGENT_INGEST_SERVER`) | Base URL of the visualizer. |
| `--source NAME` | all | Source to sync; repeatable. One of `antigravity-cli`, `antigravity-ide`, `codex`, `claude-code`. |
| `--home DIR` | current user's home | Home directory to scan (mainly for testing). |
| `--batch-size N` | `100` | Sessions per push request. |
| `--no-cache` | off | Ignore the local scan cache and re-read and re-hash every transcript. |
| `--no-upload-files` | off | Don't upload the files a transcript attached. They power the viewer's inline preview, so it falls back to reading the server's own disk without them. |
| `--dry-run` | off | Report what would be pushed, without pushing. |
| `--json` | off | Write a machine-readable summary to stdout. |
| `--quiet` | off | Suppress progress on stderr. |
| `--version` | | Print the version and exit. |
| `-h`, `--help` | | Show help and exit. |

### Environment

| Variable | Meaning |
| -------- | ------- |
| `AGENT_INGEST_SERVER` | Default for `--server`. |
| `AGENT_INGEST_CACHE` | Path of the scan cache file (default: `agent-ingest/scan-cache.json` under the OS user cache directory, e.g. `~/.cache/agent-ingest/scan-cache.json` on Linux). |
| `AGENT_INGEST_TOKEN` | Bearer token, sent when the server sets `INGEST_TOKEN`. Passed via the environment (not a flag) so it never lands in shell history or the process list. |

### Exit codes

| Code | Meaning |
| ---- | ------- |
| `0` | Success — everything was ingested or already up to date. |
| `1` | The sync ran but something failed (server error, a rejected session). |
| `2` | The command was invoked incorrectly. |

The human summary and `--json` go to **stdout**; progress and errors go to **stderr**,
so the tool composes cleanly in scripts and pipelines. The `--json` object carries an
`ok` boolean (false when any source failed) alongside per-source counts and an `error`
message for any source that could not be synced, so a script can detect failure without
parsing the exit code.

## Scheduling

Because a re-run uploads only what changed and exits `0` when there's nothing to do,
`agent-ingest` is meant to run unattended on a timer. A few things make it well-behaved in a
scheduler:

- The final summary always prints to **stdout** — redirect it to a log file (as below) or to
  `/dev/null`. The per-source progress chatter is silenced automatically whenever output isn't
  a terminal (i.e. under any scheduler), so `--quiet` is optional; it just forces the same on a
  terminal.
- Errors go to **stderr** and set a non-zero **exit code** — wire those to your alerting.
- Keep `AGENT_INGEST_TOKEN` out of the job definition where you can (an env file with tight
  permissions), since it grants write access to the store.

**cron** (Linux/macOS) — every 30 minutes, token sourced from a private file, output logged.
A crontab entry is a single line (cron has no `\` continuation), and because the file is
_sourced_ it must `export` the token so the child process inherits it — so `token.env` here
holds `export AGENT_INGEST_TOKEN=...`:

```cron
*/30 * * * * . "$HOME/.config/agent-ingest/token.env" && /usr/local/bin/agent-ingest --server https://viz.example.com >> "$HOME/.local/state/agent-ingest.log" 2>&1
```

**systemd timer** (Linux) — a `oneshot` service plus a timer, as user units:

```ini
# ~/.config/systemd/user/agent-ingest.service
[Unit]
Description=Push local agent transcripts to the Agent Brain Visualizer

[Service]
Type=oneshot
Environment=AGENT_INGEST_SERVER=https://viz.example.com
# EnvironmentFile wants a bare KEY=VALUE line (no "export"): AGENT_INGEST_TOKEN=...
# (so it's NOT the same file as the cron token.env above, which is sourced and exports).
EnvironmentFile=%h/.config/agent-ingest/token.env
ExecStart=/usr/local/bin/agent-ingest
```

```ini
# ~/.config/systemd/user/agent-ingest.timer
[Unit]
Description=Sync agent transcripts every 30 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=30min
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl --user enable --now agent-ingest.timer
# For runs while you're logged out: sudo loginctl enable-linger "$USER"
```

A failed sync marks the service failed (`systemctl --user status agent-ingest`), which your
usual unit monitoring can pick up.

**launchd** (macOS) — a LaunchAgent that runs every 30 minutes:

```xml
<!-- ~/Library/LaunchAgents/com.example.agent-ingest.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.example.agent-ingest</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/agent-ingest</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>AGENT_INGEST_SERVER</key><string>https://viz.example.com</string>
    <key>AGENT_INGEST_TOKEN</key><string>REPLACE_ME</string>
  </dict>
  <key>StartInterval</key><integer>1800</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardErrorPath</key><string>/tmp/agent-ingest.log</string>
</dict>
</plist>
```

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.agent-ingest.plist
# (older macOS: launchctl load ~/Library/LaunchAgents/com.example.agent-ingest.plist)
```

launchd has no env file, so the token lives in the plist — keep it readable only by you
(`chmod 600`; note its value is still visible to anything running as you, e.g. via
`launchctl print`).

## Test

```bash
cd cli
go test ./...
```

The tests are hermetic: they scan a temporary home and push to an in-process HTTP
stub, so no real agent files or running server are needed.
