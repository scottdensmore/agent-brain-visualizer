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

package scan

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byKey(sessions []Session) map[string]Session {
	m := make(map[string]Session, len(sessions))
	for _, s := range sessions {
		m[s.Source+"/"+s.ID] = s
	}
	return m
}

func keys(m map[string]Session) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// The server hashes the hex SHA-256 of the raw UTF-8 bytes it receives, and the
// CLI must reproduce it exactly or the manifest comparison never matches and
// every session re-uploads. This pins the shared vector the server also tests.
func TestSha256HexMatchesTheServerContract(t *testing.T) {
	got := Sha256Hex("abc")
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("Sha256Hex(abc) = %s, want %s", got, want)
	}
}

func TestScanDerivesStableIdsAcrossSources(t *testing.T) {
	home := t.TempDir()

	// Claude Code: id is the .jsonl filename stem (a session UUID).
	write(t, filepath.Join(home, ".claude/projects/-Users-me-proj/cc-uuid-1.jsonl"), "{\"type\":\"user\"}\n")
	// A tool-owned cache directory sits next to the sessions and must be ignored.
	write(t, filepath.Join(home, ".claude/projects/.agybrainviz/cc-uuid-1.summary.json"), "{}")

	// Codex: id is the rollout filename stem.
	write(t, filepath.Join(home, ".codex/sessions/2026/06/20/rollout-x-sess.jsonl"), "{\"type\":\"session_meta\"}\n")

	// Antigravity: id is the session directory name; transcript_full wins over transcript.
	agLogs := filepath.Join(home, ".gemini/antigravity-cli/brain/sess-1/.system_generated/logs")
	write(t, filepath.Join(agLogs, "transcript.jsonl"), "OLD")
	write(t, filepath.Join(agLogs, "transcript_full.jsonl"), "FULL")

	// Antigravity Hub / 2.0: lives under ~/.gemini/antigravity/brain.
	hubLogs := filepath.Join(home, ".gemini/antigravity/brain/hub-1/.system_generated/logs")
	write(t, filepath.Join(hubLogs, "transcript.jsonl"), "HUB")

	sessions, err := Scan(home, AllSources)
	if err != nil {
		t.Fatal(err)
	}
	got := byKey(sessions)

	if len(sessions) != 4 {
		t.Fatalf("expected 4 sessions, got %d: %v", len(sessions), keys(got))
	}
	if _, ok := got["claude-code/cc-uuid-1"]; !ok {
		t.Errorf("missing claude-code session; got %v", keys(got))
	}
	if _, ok := got["codex/rollout-x-sess"]; !ok {
		t.Errorf("missing codex session; got %v", keys(got))
	}
	if _, ok := got["antigravity/hub-1"]; !ok {
		t.Errorf("missing antigravity session; got %v", keys(got))
	}
	ag, ok := got["antigravity-cli/sess-1"]
	if !ok {
		t.Fatalf("missing antigravity-cli session; got %v", keys(got))
	}
	if ag.Raw != "FULL" {
		t.Errorf("antigravity must prefer transcript_full.jsonl, got raw %q", ag.Raw)
	}
}

func TestScanPopulatesHashAndMtime(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex/sessions/2026/06/20/r.jsonl"), "hello")

	sessions, err := Scan(home, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.Hash != Sha256Hex("hello") {
		t.Errorf("Hash = %s, want hash of the file content", s.Hash)
	}
	if s.Mtime <= 0 {
		t.Errorf("Mtime = %d, want the file's modification time in epoch millis", s.Mtime)
	}
}

func TestScanSelectsOnlyRequestedSources(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex/sessions/2026/r.jsonl"), "c")
	write(t, filepath.Join(home, ".claude/projects/p/cc.jsonl"), "j")

	sessions, err := Scan(home, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Source != "codex" {
		t.Fatalf("expected only the codex session, got %v", keys(byKey(sessions)))
	}
}

func TestScanSkipsEmptyTranscripts(t *testing.T) {
	home := t.TempDir()
	// An empty file is a session that never really started; the server lists none such.
	write(t, filepath.Join(home, ".claude/projects/p/empty.jsonl"), "")

	sessions, err := Scan(home, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected empty transcript to be skipped, got %d", len(sessions))
	}
}

func TestScanReturnsNothingWhenNoAgentDirsExist(t *testing.T) {
	sessions, err := Scan(t.TempDir(), AllSources)
	if err != nil {
		t.Fatalf("a missing agent dir is normal, not an error: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected no sessions, got %d", len(sessions))
	}
}

func TestUnknownSourceIsRejected(t *testing.T) {
	if _, err := Scan(t.TempDir(), []string{"borg"}); err == nil {
		t.Fatal("expected an error for an unknown source")
	}
}

func TestScanSkipsUnreadableEntriesRatherThanFailing(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex/sessions/good.jsonl"), "ok")
	// A dangling symlink is an unreadable entry (stat fails). One such file must
	// not abort the whole scan — this tool runs unattended over a user's home.
	if err := os.Symlink(filepath.Join(home, "missing"), filepath.Join(home, ".codex/sessions/bad.jsonl")); err != nil {
		t.Skip("symlinks unsupported on this platform")
	}

	sessions, err := Scan(home, []string{"codex"})
	if err != nil {
		t.Fatalf("one unreadable entry must not fail the scan: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "good" {
		t.Fatalf("expected only the readable session, got %v", keys(byKey(sessions)))
	}
}

func TestScanKeepsTheFirstOfDuplicateStemsAndWarns(t *testing.T) {
	home := t.TempDir()
	// The same filename stem in two project directories would collide on
	// (source, id) and last-writer-wins on the server; the first must win and the
	// skipped file must be named, never silently dropped or silently clobbered.
	write(t, filepath.Join(home, ".claude/projects/proj-a/cc-uuid-1.jsonl"), "first")
	write(t, filepath.Join(home, ".claude/projects/proj-b/cc-uuid-1.jsonl"), "second")

	var warned strings.Builder
	warnWriter = &warned
	defer func() { warnWriter = os.Stderr }()

	sessions, err := Scan(home, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("a duplicate stem must keep exactly 1 session, got %d", len(sessions))
	}
	if sessions[0].Raw != "first" {
		t.Errorf("the first file walked must win (its id may already be stored), got %q", sessions[0].Raw)
	}
	for _, want := range []string{"cc-uuid-1", "proj-a", "proj-b"} {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("the warning must identify the id and both paths; missing %q in %q", want, warned.String())
		}
	}
}

func TestScanDuplicateStemAfterAnEmptyFileIsNotACollision(t *testing.T) {
	home := t.TempDir()
	// The first file with the stem is empty (never really a session), so the
	// second is the real one and must be kept without a warning.
	write(t, filepath.Join(home, ".claude/projects/proj-a/cc-uuid-1.jsonl"), "")
	write(t, filepath.Join(home, ".claude/projects/proj-b/cc-uuid-1.jsonl"), "real")

	var warned strings.Builder
	warnWriter = &warned
	defer func() { warnWriter = os.Stderr }()

	sessions, err := Scan(home, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Raw != "real" {
		t.Fatalf("the non-empty file must be kept, got %v", keys(byKey(sessions)))
	}
	if warned.Len() != 0 {
		t.Errorf("an empty file is not a collision; got warning %q", warned.String())
	}
}

func TestScanReadsAntigravitySummaryJSON(t *testing.T) {
	home := t.TempDir()
	// Antigravity writes its own AI analysis next to the transcript; the CLI carries it
	// so a summary computed on this machine reaches the shared store.
	logs := filepath.Join(home, ".gemini/antigravity-cli/brain/sess-1/.system_generated/logs")
	write(t, filepath.Join(logs, "transcript.jsonl"), "{\"type\":\"USER_INPUT\"}\n")
	summary := "{\"summary\":\"did the thing\"}"
	write(t, filepath.Join(logs, "summary.json"), summary)

	sessions, err := Scan(home, []string{"antigravity-cli"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Summary != summary {
		t.Errorf("Summary = %q, want the on-disk summary.json", sessions[0].Summary)
	}
}

func TestScanHasNoSummaryWhenNoneOnDisk(t *testing.T) {
	home := t.TempDir()
	logs := filepath.Join(home, ".gemini/antigravity-cli/brain/sess-1/.system_generated/logs")
	write(t, filepath.Join(logs, "transcript.jsonl"), "{\"type\":\"USER_INPUT\"}\n")

	sessions, err := Scan(home, []string{"antigravity-cli"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Summary != "" {
		t.Fatalf("a session with no summary.json must carry an empty Summary, got %q", sessions[0].Summary)
	}
}

func TestScanIgnoresMalformedSummaryJSON(t *testing.T) {
	home := t.TempDir()
	// The server stores the summary in a jsonb column, so a non-JSON file would fail the
	// whole push. Drop it here rather than poison the batch.
	logs := filepath.Join(home, ".gemini/antigravity-cli/brain/sess-1/.system_generated/logs")
	write(t, filepath.Join(logs, "transcript.jsonl"), "{\"type\":\"USER_INPUT\"}\n")
	write(t, filepath.Join(logs, "summary.json"), "not json at all")

	sessions, err := Scan(home, []string{"antigravity-cli"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Summary != "" {
		t.Fatalf("a malformed summary.json must be dropped, got %q", sessions[0].Summary)
	}
}

func TestScanFlatSourcesCarryNoSummary(t *testing.T) {
	home := t.TempDir()
	// Codex and Claude Code have no native on-disk summary — the visualizer computes theirs.
	write(t, filepath.Join(home, ".codex/sessions/r.jsonl"), "hello")
	write(t, filepath.Join(home, ".claude/projects/p/cc.jsonl"), "{\"type\":\"user\"}\n")

	sessions, err := Scan(home, []string{"codex", "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Summary != "" {
			t.Errorf("%s/%s should carry no summary, got %q", s.Source, s.ID, s.Summary)
		}
	}
}

func TestScanHashesUTF8CoercedContentSoItMatchesTheServer(t *testing.T) {
	home := t.TempDir()
	// A transcript with an invalid UTF-8 byte, as embedded tool output can produce.
	// The server stores the JSON-decoded (U+FFFD-coerced) form, so the scanner must
	// hash that same form — hashing the raw bytes would never match the manifest.
	raw := "line\xffend"
	write(t, filepath.Join(home, ".codex/sessions/r.jsonl"), raw)

	sessions, err := Scan(home, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	coerced := strings.ToValidUTF8(raw, "�")
	if sessions[0].Raw != coerced {
		t.Errorf("Raw should be UTF-8-coerced before sending")
	}
	if sessions[0].Hash != Sha256Hex(coerced) {
		t.Errorf("Hash must be over the coerced content the server will store")
	}
}
