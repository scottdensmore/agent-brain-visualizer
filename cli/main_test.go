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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scottdensmore/agent-brain-visualizer/cli/internal/client"
	"github.com/scottdensmore/agent-brain-visualizer/cli/internal/scan"
)

// sha reuses the scanner's hash so the test asserts against the real contract.
func sha(s string) string { return scan.Sha256Hex(s) }

// fakeServer stands in for the app's ingest API: it holds a manifest and records
// every pushed batch, so a test can drive the whole scan → diff → push loop.
type fakeServer struct {
	mu                  sync.Mutex
	manifest            map[string]map[string]string // source -> id -> transcript hash
	summaryManifest     map[string]map[string]string // source -> id -> summary hash
	pushed              [][]map[string]any
	pushedSummaries     [][]map[string]any
	failSummaryManifest bool // GET summaries/manifest returns 500
	failSummaryPush     bool // POST summaries reports every item as failed
	srv                 *httptest.Server
}

func newFakeServer() *fakeServer {
	f := &fakeServer{
		manifest:        map[string]map[string]string{},
		summaryManifest: map[string]map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ingest/manifest", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		m := f.manifest[r.URL.Query().Get("source")]
		if m == nil {
			m = map[string]string{}
		}
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/api/ingest/summaries/manifest", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failSummaryManifest {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		m := f.summaryManifest[r.URL.Query().Get("source")]
		if m == nil {
			m = map[string]string{}
		}
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/api/ingest/sessions", func(w http.ResponseWriter, r *http.Request) {
		var batch []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&batch)
		f.mu.Lock()
		f.pushed = append(f.pushed, batch)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int{"ingested": len(batch), "skipped": 0, "failed": 0})
	})
	mux.HandleFunc("/api/ingest/summaries", func(w http.ResponseWriter, r *http.Request) {
		var batch []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&batch)
		f.mu.Lock()
		f.pushedSummaries = append(f.pushedSummaries, batch)
		fail := f.failSummaryPush
		f.mu.Unlock()
		if fail {
			// A 200 that reports the items as failed, exactly as the server does for a bad summary.
			_ = json.NewEncoder(w).Encode(map[string]int{"ingested": 0, "skipped": 0, "failed": len(batch)})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"ingested": len(batch), "skipped": 0, "failed": 0})
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeServer) pushedSummaryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.pushedSummaries {
		n += len(b)
	}
	return n
}

func (f *fakeServer) pushedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.pushed {
		n += len(b)
	}
	return n
}

func (f *fakeServer) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pushed)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedHome(t *testing.T) string {
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex/sessions/2026/06/20/rollout-a.jsonl"), "codex-a")
	write(t, filepath.Join(home, ".claude/projects/p/cc-b.jsonl"), "claude-b")
	return home
}

func runCLI(t *testing.T, args []string, getenv func(string) string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	inner := getenv
	if inner == nil {
		inner = func(string) string { return "" }
	}
	// Point the scan cache at a throwaway path unless the test overrides it, so
	// tests never read or write the real user cache directory.
	cachePath := filepath.Join(t.TempDir(), "scan-cache.json")
	getenv = func(key string) string {
		if v := inner(key); v != "" {
			return v
		}
		if key == "AGENT_INGEST_CACHE" {
			return cachePath
		}
		return ""
	}
	code := cli(context.Background(), args, &out, &errBuf, getenv)
	return code, out.String(), errBuf.String()
}

func TestVersionFlagPrintsTheVersionToStdout(t *testing.T) {
	// version defaults to "dev" and is overridden at release build time via -ldflags; the flag must
	// print whatever it is, so a released binary reports its build.
	code, stdout, _ := runCLI(t, []string{"--version"}, nil)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	want := "agent-ingest " + version + "\n"
	if stdout != want {
		t.Errorf("version output = %q, want %q", stdout, want)
	}
}

func TestPushesEveryDiscoveredSessionWhenManifestIsEmpty(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := seedHome(t)

	code, stdout, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q", code, stdout)
	}
	if f.pushedCount() != 2 {
		t.Fatalf("pushed %d sessions, want 2", f.pushedCount())
	}
	if !strings.Contains(stdout, "2 ingested") {
		t.Errorf("summary should report 2 ingested; got %q", stdout)
	}
}

func TestPushCarriesAntigravitySummary(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := t.TempDir()
	logs := filepath.Join(home, ".gemini/antigravity-cli/brain/sess-1/.system_generated/logs")
	write(t, filepath.Join(logs, "transcript.jsonl"), "{\"type\":\"USER_INPUT\"}\n")
	write(t, filepath.Join(logs, "summary.json"), "{\"summary\":\"did the thing\"}")

	code, stdout, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "antigravity-cli"}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q", code, stdout)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushed) == 0 || len(f.pushed[0]) == 0 {
		t.Fatal("expected the antigravity session to be pushed")
	}
	if got := f.pushed[0][0]["summary"]; got != "{\"summary\":\"did the thing\"}" {
		t.Errorf("pushed payload should carry the on-disk summary, got %v", got)
	}
}

func TestPushOmitsSummaryWhenAbsent(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := seedHome(t) // codex + claude sessions, neither has a native summary

	code, _, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, batch := range f.pushed {
		for _, s := range batch {
			if _, present := s["summary"]; present {
				t.Errorf("a session with no summary must omit the field, got payload %v", s)
			}
		}
	}
}

func TestSkipsSessionsAlreadyInTheManifest(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := seedHome(t)
	// The server already has codex/rollout-a with the same content, so only the
	// Claude session should be pushed — the manifest diff must prevent duplicates.
	f.manifest["codex"] = map[string]string{"rollout-a": sha("codex-a")}

	code, stdout, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.pushedCount() != 1 {
		t.Fatalf("pushed %d, want 1 (the changed session only)", f.pushedCount())
	}
	if !strings.Contains(stdout, "skipped") {
		t.Errorf("summary should mention skipped; got %q", stdout)
	}
}

// seedAntigravity writes one Antigravity session (transcript + optional summary) under home and
// returns the transcript and summary content so a test can seed the fake manifests.
func seedAntigravity(t *testing.T, home, id, transcript, summary string) {
	t.Helper()
	logs := filepath.Join(home, ".gemini/antigravity-cli/brain", id, ".system_generated/logs")
	write(t, filepath.Join(logs, "transcript.jsonl"), transcript)
	if summary != "" {
		write(t, filepath.Join(logs, "summary.json"), summary)
	}
}

func TestSyncsAPostHocSummaryForAnUnchangedTranscript(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := t.TempDir()
	transcript := "{\"type\":\"USER_INPUT\"}\n"
	summary := "{\"summary\":\"late\"}"
	seedAntigravity(t, home, "sess-1", transcript, summary)
	// The transcript is already stored (manifest matches), and the store has no summary yet.
	f.manifest["antigravity-cli"] = map[string]string{"sess-1": sha(transcript)}

	code, stdout, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "antigravity-cli"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d; stdout=%q", code, stdout)
	}
	if f.pushedCount() != 0 {
		t.Errorf("the unchanged transcript must not be re-pushed, got %d", f.pushedCount())
	}
	if f.pushedSummaryCount() != 1 {
		t.Fatalf("the post-hoc summary should sync on its own, got %d", f.pushedSummaryCount())
	}
	if got := f.pushedSummaries[0][0]["summary"]; got != summary {
		t.Errorf("pushed summary body = %v, want %q", got, summary)
	}
	if !strings.Contains(stdout, "summary(ies) synced") {
		t.Errorf("report should mention the synced summary; got %q", stdout)
	}
}

func TestDoesNotResyncASummaryTheStoreAlreadyHas(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := t.TempDir()
	transcript := "{\"type\":\"USER_INPUT\"}\n"
	summary := "{\"summary\":\"already there\"}"
	seedAntigravity(t, home, "sess-1", transcript, summary)
	// Both the transcript and the summary are already stored with matching hashes.
	f.manifest["antigravity-cli"] = map[string]string{"sess-1": sha(transcript)}
	f.summaryManifest["antigravity-cli"] = map[string]string{"sess-1": sha(summary)}

	code, _, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "antigravity-cli"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.pushedSummaryCount() != 0 {
		t.Errorf("an unchanged summary must not re-sync, got %d", f.pushedSummaryCount())
	}
}

func TestAChangedTranscriptCarriesItsSummaryAndDoesNotAlsoSyncItSeparately(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := t.TempDir()
	// The transcript is new (not in the manifest), so it's a full push — the summary rides along and
	// must NOT also be sent through the summary-only path.
	seedAntigravity(t, home, "sess-1", "{\"type\":\"USER_INPUT\"}\n", "{\"summary\":\"new\"}")

	code, _, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "antigravity-cli"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.pushedCount() != 1 {
		t.Fatalf("the new transcript should be pushed once, got %d", f.pushedCount())
	}
	if f.pushedSummaryCount() != 0 {
		t.Errorf("the summary rode along the transcript; it must not sync separately, got %d", f.pushedSummaryCount())
	}
}

func TestSummaryManifestFailureDoesNotBlockTranscriptSync(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	f.failSummaryManifest = true // e.g. a server too old to expose the endpoint
	home := t.TempDir()
	// A new transcript (not in the manifest) that must still ingest despite the summary side failing.
	seedAntigravity(t, home, "sess-1", "{\"type\":\"USER_INPUT\"}\n", "{\"summary\":\"x\"}")

	code, stdout, stderr := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "antigravity-cli"}, nil)
	if code != 0 {
		t.Fatalf("a summary-manifest failure must not fail the run; exit = %d, stderr=%q", code, stderr)
	}
	if f.pushedCount() != 1 {
		t.Errorf("the transcript must still be pushed, got %d", f.pushedCount())
	}
	if !strings.Contains(stderr, "skipping summary sync") {
		t.Errorf("the skipped summary sync should be reported on stderr; got %q", stderr)
	}
	if !strings.Contains(stdout, "1 ingested") {
		t.Errorf("the transcript ingest should be reported; got %q", stdout)
	}
}

func TestSummaryPushFailureIsSurfacedAndFailsTheRun(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	f.failSummaryPush = true
	home := t.TempDir()
	transcript := "{\"type\":\"USER_INPUT\"}\n"
	seedAntigravity(t, home, "sess-1", transcript, "{\"summary\":\"late\"}")
	// Transcript already stored (unchanged), summary missing → the CLI attempts a summary-only push,
	// which the server fails. That must be visible, not silently dropped.
	f.manifest["antigravity-cli"] = map[string]string{"sess-1": sha(transcript)}

	code, stdout, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "antigravity-cli"}, nil)
	if code != exitFailed {
		t.Fatalf("a failed summary push must fail the run; exit = %d", code)
	}
	if !strings.Contains(stdout, "summary(ies) failed") {
		t.Errorf("the report must show the failed summary; got %q", stdout)
	}
}

func TestDryRunPushesNothing(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := seedHome(t)

	code, stdout, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--dry-run"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.pushedCount() != 0 {
		t.Fatalf("dry-run pushed %d sessions, want 0", f.pushedCount())
	}
	if !strings.Contains(strings.ToLower(stdout), "would") {
		t.Errorf("dry-run output should say what it would do; got %q", stdout)
	}
}

func TestJSONOutputIsMachineReadableOnStdout(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := seedHome(t)

	code, stdout, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--json"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var parsed struct {
		Total struct{ Ingested, Skipped, Failed int } `json:"total"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("--json stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if parsed.Total.Ingested != 2 {
		t.Errorf("total.ingested = %d, want 2", parsed.Total.Ingested)
	}
}

func TestSelectsASingleSource(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := seedHome(t)

	code, _, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "codex"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.pushedCount() != 1 {
		t.Fatalf("pushed %d, want only the 1 codex session", f.pushedCount())
	}
}

func TestServerErrorExitsNonZero(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	home := seedHome(t)

	code, _, stderr := runCLI(t, []string{"--server", down.URL, "--home", home}, nil)
	if code == 0 {
		t.Fatal("a failed sync must exit non-zero")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("a failure should explain itself on stderr")
	}
}

func TestUnknownSourceIsAUsageError(t *testing.T) {
	code, _, stderr := runCLI(t, []string{"--source", "borg"}, nil)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 for a usage error", code)
	}
	if !strings.Contains(stderr, "borg") {
		t.Errorf("stderr should name the bad source; got %q", stderr)
	}
}

func TestHelpGoesToStdoutAndExitsZero(t *testing.T) {
	code, stdout, _ := runCLI(t, []string{"--help"}, nil)
	if code != 0 {
		t.Fatalf("--help exit = %d, want 0", code)
	}
	for _, want := range []string{"agent-ingest", "--server", "--dry-run", "--no-cache", "AGENT_INGEST_TOKEN", "AGENT_INGEST_CACHE"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

func TestAFailedSourceIsReportedNotHiddenAsNoSessions(t *testing.T) {
	// Regression: a source whose manifest fetch fails used to be dropped from the
	// report, so stdout printed "No sessions found" (and --json hid the failure)
	// even though sessions existed and the run exited non-zero.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	home := seedHome(t) // has a codex and a claude-code session

	code, stdout, stderr := runCLI(t, []string{"--server", srv.URL, "--home", home}, nil)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if strings.Contains(stdout, "No sessions found") {
		t.Errorf("must not claim 'no sessions' when sessions exist and the sync failed; stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "codex") || !strings.Contains(stdout, "claude-code") {
		t.Errorf("errored sources must appear in the summary; stdout=%q", stdout)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Errorf("the failure should also explain itself on stderr")
	}
}

func TestJSONMarksFailureWithOkFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	home := seedHome(t)

	code, stdout, _ := runCLI(t, []string{"--server", srv.URL, "--home", home, "--json"}, nil)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var parsed struct {
		OK      bool `json:"ok"`
		Sources map[string]struct {
			Error string `json:"error"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, stdout)
	}
	if parsed.OK {
		t.Errorf("ok must be false when the sync failed")
	}
	if parsed.Sources["codex"].Error == "" {
		t.Errorf("an errored source must carry an error message in --json; got %s", stdout)
	}
}

func TestInvalidServerIsAUsageError(t *testing.T) {
	// A bad invocation value is a usage error (exit 2), not a deep runtime failure.
	code, _, stderr := runCLI(t, []string{"--server", "", "--home", t.TempDir()}, nil)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 for an empty --server", code)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("an invalid --server should explain itself on stderr")
	}
	code, _, _ = runCLI(t, []string{"--server", "not-a-url", "--home", t.TempDir()}, nil)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 for a schemeless --server", code)
	}
}

func TestCommaSeparatedSourcesAreAccepted(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := seedHome(t)

	code, _, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "codex,claude-code"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.pushedCount() != 2 {
		t.Fatalf("pushed %d, want both comma-listed sources (2)", f.pushedCount())
	}
}

func TestNonUTF8TranscriptIsIngestedOnceThenSkipped(t *testing.T) {
	// End-to-end guard for the hash contract: a server that hashes the raw it
	// receives exactly as the Java server does (sha256 of the received bytes). A
	// transcript with an invalid UTF-8 byte must be pushed once and then recognized
	// as unchanged — otherwise it re-uploads on every run.
	stored := map[string]string{}
	var mu sync.Mutex
	var pushes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ingest/manifest" {
			mu.Lock()
			_ = json.NewEncoder(w).Encode(stored)
			mu.Unlock()
			return
		}
		if r.URL.Path == "/api/ingest/summaries/manifest" {
			_, _ = w.Write([]byte("{}")) // this test has no summaries
			return
		}
		var batch []struct{ ID, Raw string }
		_ = json.NewDecoder(r.Body).Decode(&batch)
		mu.Lock()
		for _, s := range batch {
			pushes++
			sum := sha256.Sum256([]byte(s.Raw)) // what Ingestor.sha256 does on the server
			stored[s.ID] = hex.EncodeToString(sum[:])
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ingested":1,"skipped":0,"failed":0}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	write(t, filepath.Join(home, ".codex/sessions/2026/06/20/r.jsonl"), "line\xffend")

	args := []string{"--server", srv.URL, "--home", home, "--source", "codex"}
	if code, _, _ := runCLI(t, args, nil); code != 0 {
		t.Fatalf("first run exit = %d", code)
	}
	if code, _, _ := runCLI(t, args, nil); code != 0 {
		t.Fatalf("second run exit = %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if pushes != 1 {
		t.Fatalf("pushed %d times; a non-UTF-8 transcript must upload once then skip", pushes)
	}
}

// rewriteKeepingStat replaces path's content with a same-length string and
// restores the original mtime, so only an actual re-read could observe the new
// content — the non-flaky way to tell whether the scan trusted its cache.
func rewriteKeepingStat(t *testing.T, path, content string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(content)) != info.Size() {
		t.Fatalf("rewriteKeepingStat needs same-length content: %d != %d", len(content), info.Size())
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// cacheEnv pins the scan cache to one path so it persists across runCLI calls
// within a test (runCLI otherwise gives every call a throwaway cache).
func cacheEnv(t *testing.T) func(string) string {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "scan-cache.json")
	return func(k string) string {
		if k == "AGENT_INGEST_CACHE" {
			return cachePath
		}
		return ""
	}
}

func TestScanCacheSkipsRehashingUnchangedFilesAcrossRuns(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := t.TempDir()
	path := filepath.Join(home, ".codex/sessions/r.jsonl")
	write(t, path, "hello")
	// The server already has this exact content, so nothing should ever upload.
	f.manifest["codex"] = map[string]string{"r": sha("hello")}

	env := cacheEnv(t)
	args := []string{"--server", f.srv.URL, "--home", home, "--source", "codex"}
	if code, _, _ := runCLI(t, args, env); code != 0 {
		t.Fatalf("priming run exit = %d", code)
	}

	// Same size and mtime, different bytes. A run that re-hashed would see a
	// changed session and push it; one that trusts the cache keeps the old hash,
	// which still matches the manifest, and pushes nothing.
	rewriteKeepingStat(t, path, "world")

	code, stdout, _ := runCLI(t, args, env)
	if code != 0 {
		t.Fatalf("second run exit = %d", code)
	}
	if f.pushedCount() != 0 {
		t.Fatalf("pushed %d session(s); a stat-identical file must ride the cache and skip", f.pushedCount())
	}
	if !strings.Contains(stdout, "1 skipped") {
		t.Errorf("the cached session should still be reported as skipped; stdout=%q", stdout)
	}
}

func TestNoCacheFlagForcesAFullRehash(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := t.TempDir()
	path := filepath.Join(home, ".codex/sessions/r.jsonl")
	write(t, path, "hello")
	f.manifest["codex"] = map[string]string{"r": sha("hello")}

	env := cacheEnv(t)
	args := []string{"--server", f.srv.URL, "--home", home, "--source", "codex"}
	if code, _, _ := runCLI(t, args, env); code != 0 {
		t.Fatalf("priming run exit = %d", code)
	}
	rewriteKeepingStat(t, path, "world")

	// --no-cache must ignore the primed cache, re-hash the file, see it differs
	// from the manifest, and push the current content.
	if code, _, _ := runCLI(t, append([]string{"--no-cache"}, args...), env); code != 0 {
		t.Fatalf("--no-cache run exit = %d", code)
	}
	if f.pushedCount() != 1 {
		t.Fatalf("pushed %d session(s), want 1: --no-cache must re-read the changed file", f.pushedCount())
	}
	f.mu.Lock()
	raw := f.pushed[0][0]["raw"]
	f.mu.Unlock()
	if raw != "world" {
		t.Errorf("pushed raw = %q, want the current on-disk content", raw)
	}
}

func TestChunkSplitsByByteBudgetNotJustCount(t *testing.T) {
	// Transcripts are large, so a batch is capped by cumulative bytes as well as count, to stay under
	// the server's request-size limit.
	s := func(raw string) client.PushSession { return client.PushSession{Raw: raw} }
	batches := chunk([]client.PushSession{s("aaaa"), s("bbbb"), s("cccc")}, 100, 10)
	// aaaa(4)+bbbb(4)=8 ≤ 10; +cccc would be 12 > 10 → flush, then cccc alone.
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 1 {
		t.Fatalf("got %d batches with sizes %v; want [2,1]", len(batches), sizesOf(batches))
	}
}

func TestChunkSendsAnOversizedSessionInItsOwnRequest(t *testing.T) {
	s := func(raw string) client.PushSession { return client.PushSession{Raw: raw} }
	// The middle session alone exceeds the budget; it must still be sent (in its own request) rather
	// than dropped.
	batches := chunk([]client.PushSession{s("aa"), s("xxxxxxxxxx"), s("bb")}, 100, 5)
	if len(batches) != 3 {
		t.Fatalf("got %d batches %v; an oversized session must go alone", len(batches), sizesOf(batches))
	}
	if batches[1][0].Raw != "xxxxxxxxxx" {
		t.Errorf("the oversized session should be the lone middle batch")
	}
}

func TestChunkStillRespectsTheCountCap(t *testing.T) {
	s := func(raw string) client.PushSession { return client.PushSession{Raw: raw} }
	batches := chunk([]client.PushSession{s("a"), s("b"), s("c")}, 2, 1<<30)
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 1 {
		t.Fatalf("count cap ignored: got sizes %v", sizesOf(batches))
	}
}

func sizesOf(batches [][]client.PushSession) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = len(b)
	}
	return out
}

func TestBatchSizeSplitsIntoSeparateRequests(t *testing.T) {
	f := newFakeServer()
	defer f.srv.Close()
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex/sessions/a.jsonl"), "a")
	write(t, filepath.Join(home, ".codex/sessions/b.jsonl"), "b")
	write(t, filepath.Join(home, ".codex/sessions/c.jsonl"), "c")

	code, _, _ := runCLI(t, []string{"--server", f.srv.URL, "--home", home, "--source", "codex", "--batch-size", "2"}, nil)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.pushedCount() != 3 {
		t.Fatalf("pushed %d sessions, want 3", f.pushedCount())
	}
	if f.batchCount() != 2 {
		t.Fatalf("sent %d requests, want 2 (3 sessions at batch-size 2)", f.batchCount())
	}
}

func TestCleartextTokenOverHTTPWarnsOnlyForNonLoopbackHosts(t *testing.T) {
	cases := []struct {
		name   string
		server string
		token  string
		warn   bool
	}{
		{"token over http to a remote host", "http://brain.example.com:8080", "s3cret", true},
		{"token over https", "https://brain.example.com", "s3cret", false},
		{"token over http to localhost", "http://localhost:8080", "s3cret", false},
		{"token over http to 127.0.0.1", "http://127.0.0.1:8080", "s3cret", false},
		{"no token over http to a remote host", "http://brain.example.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			warnCleartextToken(tc.server, tc.token, &stderr)
			if got := strings.Contains(stderr.String(), "warning"); got != tc.warn {
				t.Errorf("warned = %v, want %v; stderr=%q", got, tc.warn, stderr.String())
			}
		})
	}
}

func TestTokenComesFromTheEnvironment(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_, _ = w.Write([]byte(`{"ingested":0,"skipped":0,"failed":0}`))
	}))
	defer srv.Close()
	home := seedHome(t)
	env := func(k string) string {
		if k == "AGENT_INGEST_TOKEN" {
			return "s3cret"
		}
		return ""
	}

	code, _, _ := runCLI(t, []string{"--server", srv.URL, "--home", home, "--source", "codex"}, env)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the env token", gotAuth)
	}
}
