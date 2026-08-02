// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command agent-ingest scans a machine's local agent transcripts (Antigravity,
// OpenAI Codex, Claude Code) and pushes them to a running Agent Brain Visualizer,
// so trajectories captured anywhere are visible from any machine pointed at the
// same store. It is idempotent: a session already stored with identical content
// is skipped, so it is safe to run repeatedly or on a schedule.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/scottdensmore/agent-brain-visualizer/cli/internal/client"
	"github.com/scottdensmore/agent-brain-visualizer/cli/internal/scan"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	exitOK     = 0 // everything requested was ingested or already up to date
	exitFailed = 1 // the sync ran but something failed (server error, rejected session)
	exitUsage  = 2 // the command was invoked incorrectly
)

const defaultServer = "http://localhost:8080"
const defaultBatchSize = 100

type config struct {
	server    string
	token     string
	sources   []string
	home      string
	dryRun    bool
	jsonOut   bool
	quiet     bool
	batchSize int
	noCache   bool
	// uploadFiles carries the files a transcript attached, so the viewer can
	// preview them from a machine that never had them.
	uploadFiles bool
	cachePath   string // where the scan cache lives; "" disables persistence
}

func main() {
	// Ctrl-C / SIGTERM cancel the context, so in-flight requests and retry
	// backoff waits stop cleanly instead of the process hanging on them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// cli parses arguments and runs the sync, returning the process exit code. It
// takes its streams and environment as parameters so it is fully testable.
func cli(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	cfg, code, done := parseFlags(args, stdout, stderr, getenv)
	if done {
		return code
	}
	return run(ctx, cfg, stdout, stderr)
}

func parseFlags(args []string, stdout, stderr io.Writer, getenv func(string) string) (config, int, bool) {
	fs := flag.NewFlagSet("agent-ingest", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we print usage and errors ourselves, to the right stream
	fs.Usage = func() {}

	var (
		sources     multiFlag
		server      = fs.String("server", envOr(getenv, "AGENT_INGEST_SERVER", defaultServer), "base URL of the visualizer")
		home        = fs.String("home", "", "home directory to scan (default: the current user's)")
		dryRun      = fs.Bool("dry-run", false, "report what would be pushed without pushing")
		jsonOut     = fs.Bool("json", false, "write a machine-readable summary to stdout")
		quiet       = fs.Bool("quiet", false, "suppress progress on stderr")
		batchSize   = fs.Int("batch-size", defaultBatchSize, "sessions per push request")
		noCache     = fs.Bool("no-cache", false, "ignore the local scan cache and re-read and re-hash every transcript")
		noFiles     = fs.Bool("no-upload-files", false, "don't upload the files a transcript attached (they power the viewer's inline preview)")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	fs.Var(&sources, "source", "source to sync; repeatable (default: all)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return config{}, exitOK, true
		}
		fmt.Fprintf(stderr, "agent-ingest: %s\nRun 'agent-ingest --help' for usage.\n", err)
		return config{}, exitUsage, true
	}
	if *showVersion {
		fmt.Fprintf(stdout, "agent-ingest %s\n", version)
		return config{}, exitOK, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agent-ingest: unexpected argument %q\nRun 'agent-ingest --help' for usage.\n", fs.Arg(0))
		return config{}, exitUsage, true
	}

	selected := []string(sources)
	if len(selected) == 0 {
		selected = scan.AllSources
	}
	if err := scan.Validate(selected); err != nil {
		fmt.Fprintf(stderr, "agent-ingest: %s\n", err)
		return config{}, exitUsage, true
	}
	if err := validateServer(*server); err != nil {
		fmt.Fprintf(stderr, "agent-ingest: %s\n", err)
		return config{}, exitUsage, true
	}
	if *batchSize < 1 {
		fmt.Fprintf(stderr, "agent-ingest: --batch-size must be at least 1\n")
		return config{}, exitUsage, true
	}

	return config{
		server:      *server,
		token:       getenv("AGENT_INGEST_TOKEN"),
		sources:     selected,
		home:        *home,
		dryRun:      *dryRun,
		jsonOut:     *jsonOut,
		quiet:       *quiet,
		batchSize:   *batchSize,
		noCache:     *noCache,
		uploadFiles: !*noFiles,
		cachePath:   envOr(getenv, "AGENT_INGEST_CACHE", scan.DefaultCachePath()),
	}, exitOK, false
}

func run(ctx context.Context, cfg config, stdout, stderr io.Writer) int {
	home := cfg.home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "agent-ingest: cannot determine home directory: %s\n", err)
			return exitFailed
		}
		home = h
	}

	// The scan cache lets a corpus that has not changed since the last run skip
	// re-reading and re-hashing every transcript. It is strictly an optimization:
	// a missing, corrupt, or unwritable cache degrades to full hashing, never a
	// failure, and the server-side manifest diff below is always consulted.
	var cache *scan.Cache
	if !cfg.noCache {
		cache = scan.LoadCache(cfg.cachePath)
	}
	sessions, err := scan.ScanWithCache(home, cfg.sources, cache)
	if err != nil {
		fmt.Fprintf(stderr, "agent-ingest: %s\n", err)
		return exitFailed
	}
	// The hashes are valid as soon as the scan finishes, whatever the pushes
	// below do, so persist them now (best-effort; warn, never fail).
	if err := cache.Save(); err != nil {
		fmt.Fprintf(stderr, "agent-ingest: warning: could not save scan cache: %s\n", err)
	}
	bySource := groupBySource(sessions)

	warnCleartextToken(cfg.server, cfg.token, stderr)
	cl := client.New(cfg.server, cfg.token)
	report := newReport(cfg.dryRun)

	for _, source := range cfg.sources {
		// Sorted so batches are chunked in a stable order run over run.
		list := scan.SortedByKey(bySource[source])
		if len(list) == 0 {
			continue // this machine has no sessions for this source
		}
		progress(cfg, stderr, "%s: %d local session(s)", source, len(list))

		manifest, err := cl.Manifest(ctx, source)
		if err != nil {
			// Record the failure against the source so the summary and --json
			// reflect it — a dropped source used to masquerade as "no sessions".
			fmt.Fprintf(stderr, "agent-ingest: %s\n", err)
			report.add(source, sourceReport{Error: err.Error()})
			continue
		}

		changed := changedSessions(list, manifest, cfg.uploadFiles, stderr)
		sr := sourceReport{Skipped: len(list) - len(changed)}

		// Summary sync is a best-effort layer on top of the transcript sync. If the summary manifest
		// can't be fetched — a transient error, or a server too old to expose the endpoint — warn and
		// skip it. It must never block or fail the transcript sync below.
		var pendingSummaries []client.PushSummary
		if summaryManifest, err := cl.SummaryManifest(ctx, source); err != nil {
			fmt.Fprintf(stderr, "agent-ingest: skipping summary sync for %s: %s\n", source, err)
		} else {
			pendingSummaries = changedSummaries(list, manifest, summaryManifest)
		}

		if cfg.dryRun {
			sr.Ingested = len(changed) // "would ingest"
			sr.SummariesSynced = len(pendingSummaries)
			report.add(source, sr)
			continue
		}

		for _, batch := range chunk(changed, cfg.batchSize, maxBatchBytes) {
			r, err := cl.Push(ctx, batch)
			if err != nil {
				fmt.Fprintf(stderr, "agent-ingest: %s\n", err)
				sr.Error = err.Error()
				break
			}
			sr.Ingested += r.Ingested
			sr.Skipped += r.Skipped
			sr.Failed += r.Failed
		}
		// Sync summaries written after their transcript was ingested (they ride along no push). Skip
		// when the transcript push failed — the store is likely down. Unlike the manifest fetch, a
		// push that is attempted and fails is a real failure: surface it (report + exit code), never
		// drop it silently.
		if sr.Error == "" {
			for _, batch := range chunkBy(pendingSummaries, func(s client.PushSummary) int { return len(s.Summary) }, cfg.batchSize, maxBatchBytes) {
				r, err := cl.PushSummaries(ctx, batch)
				if err != nil {
					fmt.Fprintf(stderr, "agent-ingest: %s\n", err)
					sr.Error = err.Error()
					break
				}
				sr.SummariesSynced += r.Ingested
				sr.SummariesFailed += r.Failed
			}
		}
		report.add(source, sr)
	}

	if err := report.write(stdout, cfg.jsonOut); err != nil {
		// A summary that failed to write (a closed pipe, a full disk) must fail
		// the run — a script reading stdout would otherwise see success and no data.
		fmt.Fprintf(stderr, "agent-ingest: %s\n", err)
		return exitFailed
	}
	if report.failed {
		return exitFailed
	}
	return exitOK
}

// changedSessions keeps only the sessions whose content differs from what the
// server already stores, so an unchanged corpus uploads nothing. A session the
// scan cache spared from being read is read here, and only here — sessions the
// server already has never touch the disk beyond a stat.
func changedSessions(
	list []scan.Session,
	manifest map[string]string,
	uploadFiles bool,
	stderr io.Writer,
) []client.PushSession {
	var changed []client.PushSession
	for i := range list {
		s := &list[i]
		if manifest[s.ID] == s.Hash {
			continue
		}
		if err := s.LoadRaw(); err != nil {
			// The transcript vanished or became unreadable between the scan and the
			// push; skip it — like the scanner does — rather than fail the source.
			fmt.Fprintf(stderr, "agent-ingest: warning: skipping %s/%s: %s\n", s.Source, s.ID, err)
			continue
		}
		push := client.PushSession{
			Source:      s.Source,
			ID:          s.ID,
			SourceMtime: s.Mtime,
			Raw:         s.Raw,
			Summary:     s.Summary,
		}
		if uploadFiles {
			push.Files = attachmentsFor(s, stderr)
		}
		changed = append(changed, push)
	}
	return changed
}

// attachmentsFor reads the files a session's transcript explicitly attached, so
// the viewer can preview them from any machine. Skips are reported rather than
// silent: a file left behind because it looks like a secret, or is too large, is
// something the operator should know about.
func attachmentsFor(s *scan.Session, stderr io.Writer) []client.PushFile {
	found, skipped := scan.ReferencedFiles(s.Raw)
	for _, reason := range skipped {
		fmt.Fprintf(stderr, "agent-ingest: not attaching %s\n", reason)
	}
	if len(found) == 0 {
		return nil
	}
	files := make([]client.PushFile, 0, len(found))
	for _, a := range found {
		files = append(files, client.PushFile{Path: a.Path, Content: a.Content})
	}
	return files
}

func groupBySource(sessions []scan.Session) map[string][]scan.Session {
	out := map[string][]scan.Session{}
	for _, s := range sessions {
		out[s.Source] = append(out[s.Source], s)
	}
	return out
}

// Transcripts can be large, so a request is bounded by cumulative raw bytes, not just a session
// count that could easily blow past the server's request-size limit. The budget leaves generous
// headroom under that limit (JSON escaping can inflate raw bytes on the wire).
const maxBatchBytes = 16 << 20 // 16 MiB

// chunk groups sessions into requests, flushing when the next session would exceed either the count
// cap or the byte budget. A session larger than the budget is sent alone rather than dropped or
// split; one that large is on its own the only request that can exceed the budget.
func chunk(sessions []client.PushSession, maxCount, maxBytes int) [][]client.PushSession {
	return chunkBy(sessions, func(s client.PushSession) int { return len(s.Raw) }, maxCount, maxBytes)
}

// chunkBy is the shared batching used for both transcripts and summaries: it flushes a batch when the
// next item would exceed the count cap or the byte budget (measured by sizeOf), and sends a single
// over-budget item on its own rather than dropping or splitting it.
func chunkBy[T any](items []T, sizeOf func(T) int, maxCount, maxBytes int) [][]T {
	var batches [][]T
	var current []T
	currentBytes := 0
	for _, item := range items {
		size := sizeOf(item)
		if len(current) > 0 && (len(current) >= maxCount || currentBytes+size > maxBytes) {
			batches = append(batches, current)
			current = nil
			currentBytes = 0
		}
		current = append(current, item)
		currentBytes += size
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// changedSummaries picks the on-disk summaries the store is missing or that changed, but only for
// sessions whose transcript is unchanged — a changed transcript is a full push that already carries
// its summary along. This is what lets a summary written after its transcript was ingested still sync.
func changedSummaries(list []scan.Session, transcriptManifest, summaryManifest map[string]string) []client.PushSummary {
	var out []client.PushSummary
	for _, s := range list {
		if s.Summary == "" {
			continue // no local summary to sync
		}
		if transcriptManifest[s.ID] != s.Hash {
			continue // transcript changed → the summary rides along the full push
		}
		if summaryManifest[s.ID] == scan.Sha256Hex(s.Summary) {
			continue // already stored, unchanged
		}
		out = append(out, client.PushSummary{Source: s.Source, ID: s.ID, Summary: s.Summary})
	}
	return out
}

func progress(cfg config, stderr io.Writer, format string, args ...any) {
	if cfg.quiet || !isTerminal(stderr) {
		return
	}
	fmt.Fprintf(stderr, format+"\n", args...)
}

// isTerminal reports whether w is an interactive terminal, so progress noise
// stays out of pipes and files.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// validateServer rejects a --server that isn't an http(s) URL at parse time, so
// a bad value is a usage error rather than a confusing failure deep in a request.
func validateServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("--server must be an http(s) URL, got %q", server)
	}
	return nil
}

// warnCleartextToken flags a bearer token about to be sent over cleartext HTTP
// to a non-loopback host, where anyone on the network path can read it. It only
// warns — plain http on a trusted network is a legitimate setup — but the
// exposure must never be silent.
func warnCleartextToken(server, token string, stderr io.Writer) {
	if token == "" {
		return
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	fmt.Fprintf(stderr, "agent-ingest: warning: sending the bearer token unencrypted over http to %s; use an https --server\n", host)
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `agent-ingest — push local agent transcripts to an Agent Brain Visualizer

Usage:
  agent-ingest [flags]

It scans this machine for Antigravity, OpenAI Codex, and Claude Code sessions and
uploads them to the visualizer's ingest API. Sessions already stored with the same
content are skipped, so running it repeatedly (e.g. on a schedule) is safe. For a source
that keeps its own AI summary on disk (Antigravity), that summary is uploaded with the
transcript, or synced on its own on a later run if it was written after the transcript.

Flags:
  --server URL      base URL of the visualizer (default %q; or $AGENT_INGEST_SERVER)
  --source NAME     source to sync; repeatable (default: all). One of:
                    %s
  --home DIR        home directory to scan (default: the current user's)
  --batch-size N    max sessions per push request (default %d; a request is also
                    capped at ~16 MiB of transcript, and one larger session is
                    sent on its own)
  --no-cache        ignore the local scan cache and re-read and re-hash every
                    transcript. The cache only skips local hashing of files
                    whose size and mtime are unchanged; the server's manifest
                    is always consulted, so this is rarely needed.
  --dry-run         report what would be pushed, without pushing (still queries
                    the server's manifest to compute the diff)
  --json            write a machine-readable summary to stdout
  --quiet           suppress progress on stderr
  --version         print the version and exit
  -h, --help        show this help and exit

Environment:
  AGENT_INGEST_SERVER   default for --server
  AGENT_INGEST_CACHE    path of the scan cache file (default: agent-ingest/
                        scan-cache.json under the OS user cache directory)
  AGENT_INGEST_TOKEN    bearer token, sent when the server sets INGEST_TOKEN.
                        Passed via the environment (not a flag) so it never lands
                        in shell history or the process list.

Exit codes:
  0  success (everything was ingested or already up to date)
  1  the sync ran but something failed
  2  the command was invoked incorrectly
`, defaultServer, strings.Join(scan.AllSources, ", "), defaultBatchSize)
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	// Accept both a repeated flag and a comma-separated list.
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

// sourceReport is the outcome for one source. Error is set when the source could
// not be synced, so a failure is never silently dropped from the summary.
type sourceReport struct {
	Ingested        int    `json:"ingested"`
	Skipped         int    `json:"skipped"`
	Failed          int    `json:"failed"`
	SummariesSynced int    `json:"summariesSynced,omitempty"`
	SummariesFailed int    `json:"summariesFailed,omitempty"`
	Error           string `json:"error,omitempty"`
}

func (s sourceReport) errored() bool { return s.Error != "" || s.Failed > 0 || s.SummariesFailed > 0 }

// report accumulates per-source outcomes and renders the summary.
type report struct {
	dryRun               bool
	order                []string
	perSrc               map[string]sourceReport
	total                client.Result
	totalSummaries       int
	totalSummariesFailed int
	failed               bool
}

func newReport(dryRun bool) *report {
	return &report{dryRun: dryRun, perSrc: map[string]sourceReport{}}
}

func (r *report) add(source string, sr sourceReport) {
	if _, seen := r.perSrc[source]; !seen {
		r.order = append(r.order, source)
	}
	r.perSrc[source] = sr
	r.total.Ingested += sr.Ingested
	r.total.Skipped += sr.Skipped
	r.total.Failed += sr.Failed
	r.totalSummaries += sr.SummariesSynced
	r.totalSummariesFailed += sr.SummariesFailed
	if sr.errored() {
		r.failed = true
	}
}

func (r *report) write(stdout io.Writer, jsonOut bool) error {
	if jsonOut {
		payload := map[string]any{
			"ok":     !r.failed,
			"dryRun": r.dryRun,
			// The total mirrors the per-source shape (which carries summariesSynced/Failed), so a
			// script reading .total sees the same fields as .sources.*.
			"total": sourceReport{
				Ingested:        r.total.Ingested,
				Skipped:         r.total.Skipped,
				Failed:          r.total.Failed,
				SummariesSynced: r.totalSummaries,
				SummariesFailed: r.totalSummariesFailed,
			},
			"sources": r.perSrc,
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return fmt.Errorf("writing JSON summary: %w", err)
		}
		return nil
	}

	if len(r.order) == 0 {
		fmt.Fprintln(stdout, "No sessions found to ingest.")
		return nil
	}
	verb := "ingested"
	if r.dryRun {
		verb = "would ingest"
	}
	width := len("total")
	for _, s := range r.order {
		width = max(width, len(s))
	}
	for _, s := range r.order {
		sr := r.perSrc[s]
		if sr.Error != "" {
			fmt.Fprintf(stdout, "%-*s  error: %s\n", width, s, sr.Error)
			continue
		}
		fmt.Fprintf(stdout, "%-*s  %d %s, %d skipped, %d failed%s\n", width, s, sr.Ingested, verb, sr.Skipped, sr.Failed, summarySuffix(sr.SummariesSynced, sr.SummariesFailed, r.dryRun))
	}
	fmt.Fprintf(stdout, "%-*s  %d %s, %d skipped, %d failed%s\n", width, "total", r.total.Ingested, verb, r.total.Skipped, r.total.Failed, summarySuffix(r.totalSummaries, r.totalSummariesFailed, r.dryRun))
	return nil
}

// summarySuffix appends the standalone-summary tally to a report line, but only when there is one, so
// the common no-summary case reads exactly as before. Failures are always shown — a summary that was
// pushed and rejected must never be dropped silently from the report.
func summarySuffix(synced, failed int, dryRun bool) string {
	var parts []string
	if synced > 0 {
		if dryRun {
			parts = append(parts, fmt.Sprintf("%d summary(ies) would sync", synced))
		} else {
			parts = append(parts, fmt.Sprintf("%d summary(ies) synced", synced))
		}
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d summary(ies) failed", failed))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}
