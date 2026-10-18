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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/ai"
	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/insights"
	"github.com/scottdensmore/agent-brain-visualizer/internal/pgtest"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
	"github.com/scottdensmore/agent-brain-visualizer/internal/testhttp"
	"github.com/scottdensmore/agent-brain-visualizer/internal/transcript"
)

type testServerEnv struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	pg     *pgtest.Server
	home   string
}

func newTestServerEnv(t *testing.T) *testServerEnv {
	t.Helper()
	db, pg := pgtest.NewTestDB(t)
	if err := store.ApplySchema(context.Background(), db, store.SchemaSQL()); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	home := t.TempDir()
	srv := New(Dependencies{
		Version:  "test-version",
		UserHome: home,
		DB:       db,
	})
	ts := testhttp.NewServer(srv)
	t.Cleanup(ts.Close)
	return &testServerEnv{
		t:    t,
		srv:  srv,
		ts:   ts,
		pg:   pg,
		home: home,
	}
}

func (e *testServerEnv) seedSession(source, id, title, stepsJSON, summaryJSON string, mtime int64) {
	e.t.Helper()
	_, err := e.srv.Sessions.Upsert(store.Session{
		Source:      source,
		ID:          id,
		Title:       title,
		SourceMtime: mtime,
		StepsJSON:   stepsJSON,
		ContentHash: store.Sha256Hex(stepsJSON + ":" + id),
	})
	if err != nil {
		e.t.Fatalf("seedSession(%s, %s): %v", source, id, err)
	}
	if summaryJSON != "" {
		_, err := e.srv.Summaries.Upsert(source, id, summaryJSON, title)
		if err != nil {
			e.t.Fatalf("seedSummary(%s, %s): %v", source, id, err)
		}
	}
}

func (e *testServerEnv) get(path string) (int, string) {
	e.t.Helper()
	resp, err := http.Get(e.ts.URL + path)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *testServerEnv) getJSON(path string) (int, map[string]any) {
	e.t.Helper()
	status, body := e.get(path)
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		e.t.Fatalf("GET %s invalid JSON (%d): %v\nbody: %s", path, status, err, body)
	}
	return status, m
}

func (e *testServerEnv) postJSON(path, body string) (int, string) {
	e.t.Helper()
	resp, err := http.Post(e.ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *testServerEnv) deleteReq(path string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodDelete, e.ts.URL+path, nil)
	if err != nil {
		e.t.Fatalf("NewRequest DELETE %s: %v", path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("DELETE %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// --- HealthControllerTest ---

func TestLivenessIsUpAndReportsTheBuildVersion(t *testing.T) {
	env := newTestServerEnv(t)
	status, body := env.getJSON("/health")
	if status != http.StatusOK || body["status"] != "UP" || body["version"] != "test-version" {
		t.Fatalf("unexpected /health (%d): %+v", status, body)
	}
}

func TestReadinessIsUpWhenTheStoreIsReachable(t *testing.T) {
	env := newTestServerEnv(t)
	status, body := env.getJSON("/health/ready")
	if status != http.StatusOK || body["status"] != "UP" || body["db"] != "UP" {
		t.Fatalf("unexpected /health/ready (%d): %+v", status, body)
	}
}

func TestReadinessIs503WithDownBodyWhenTheStoreIsUnreachable(t *testing.T) {
	env := newTestServerEnv(t)
	env.srv.DBPing = func(ctx context.Context) error {
		return errors.New("store is down")
	}
	status, body := env.getJSON("/health/ready")
	if status != http.StatusServiceUnavailable || body["status"] != "DOWN" || body["db"] != "DOWN" {
		t.Fatalf("unexpected /health/ready (%d): %+v", status, body)
	}
}

// --- ApiAuthFilterTest ---

func TestApiAuthFilter(t *testing.T) {
	const token = "api-s3cret"

	// Letting every request through when no token is configured
	if st, _ := GuardAPI(config.NewApiConfig("", false), ""); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}
	if st, _ := GuardAPI(config.NewApiConfig("   ", false), "Bearer whatever"); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}

	// Accepts configured bearer token
	if st, _ := GuardAPI(config.NewApiConfig(token, false), "Bearer "+token); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}

	// Rejects missing, wrong, prefix, non-bearer, or ingest token
	for _, bad := range []string{"", "Bearer nope", "Bearer api-s3c", "Basic " + token, token, "Bearer some-ingest-token"} {
		st, body := GuardAPI(config.NewApiConfig(token, false), bad)
		if st != http.StatusUnauthorized || !strings.Contains(body["error"], "API_TOKEN") {
			t.Fatalf("GuardAPI(%q) = %d %+v, want 401", bad, st, body)
		}
	}

	// Fails closed when auth is required but no token is configured
	requireNoToken := config.NewApiConfig("", true)
	for _, hdr := range []string{"", "Bearer anything"} {
		st, body := GuardAPI(requireNoToken, hdr)
		if st != http.StatusServiceUnavailable || body["error"] != "The API is unavailable." {
			t.Fatalf("GuardAPI(requireNoToken, %q) = %d %+v, want 503", hdr, st, body)
		}
	}

	// Requiring auth does not change valid token path
	requireWithToken := config.NewApiConfig(token, true)
	if st, _ := GuardAPI(requireWithToken, "Bearer "+token); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}
	if st, _ := GuardAPI(requireWithToken, "Bearer nope"); st != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", st)
	}
}

// --- IngestAuthFilterTest ---

func TestIngestAuthFilter(t *testing.T) {
	const token = "s3cret-token"

	if st, _ := GuardIngest(config.NewIngestConfig("", false), ""); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}
	if st, _ := GuardIngest(config.NewIngestConfig("   ", false), "Bearer whatever"); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}
	if st, _ := GuardIngest(config.NewIngestConfig(token, false), "Bearer "+token); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}

	for _, bad := range []string{"", "Bearer nope", "Bearer s3cret", "Basic " + token, token} {
		st, body := GuardIngest(config.NewIngestConfig(token, false), bad)
		if st != http.StatusUnauthorized || !strings.Contains(body["error"], "INGEST_TOKEN") {
			t.Fatalf("GuardIngest(%q) = %d %+v, want 401", bad, st, body)
		}
	}

	requireNoToken := config.NewIngestConfig("", true)
	for _, hdr := range []string{"", "Bearer anything"} {
		st, body := GuardIngest(requireNoToken, hdr)
		if st != http.StatusServiceUnavailable || body["error"] != "Ingest is unavailable." {
			t.Fatalf("GuardIngest(requireNoToken, %q) = %d %+v, want 503", hdr, st, body)
		}
	}

	requireWithToken := config.NewIngestConfig(token, true)
	if st, _ := GuardIngest(requireWithToken, "Bearer "+token); st != 0 {
		t.Fatalf("expected allow, got %d", st)
	}
	if st, _ := GuardIngest(requireWithToken, "Bearer nope"); st != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", st)
	}
}

func TestHttpAuthFiltersIntegration(t *testing.T) {
	env := newTestServerEnv(t)
	env.srv.ApiConfig = config.NewApiConfig("api-secret", true)
	env.srv.IngestConfig = config.NewIngestConfig("ingest-secret", true)

	// /health and static UI are NOT guarded by API or Ingest filters
	if st, _ := env.get("/health"); st != http.StatusOK {
		t.Fatalf("expected /health 200, got %d", st)
	}
	if st, body := env.get("/index.html"); st != http.StatusOK || !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("expected /index.html 200, got %d", st)
	}

	// Unauthenticated API and Ingest requests get 401
	if st, _ := env.get("/api/brain/conversations"); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/brain/conversations, got %d", st)
	}
	if st, _ := env.get("/api/ingest/manifest?source=codex"); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/ingest/manifest, got %d", st)
	}

	// Authenticated requests succeed, and tokens do not cross-unlock
	reqAPI, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/brain/conversations", nil)
	reqAPI.Header.Set("Authorization", "Bearer api-secret")
	respAPI, err := http.DefaultClient.Do(reqAPI)
	if err != nil || respAPI.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with api-secret, got %v %v", respAPI, err)
	}
	respAPI.Body.Close()

	reqCross, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/brain/conversations", nil)
	reqCross.Header.Set("Authorization", "Bearer ingest-secret")
	respCross, err := http.DefaultClient.Do(reqCross)
	if err != nil || respCross.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with ingest-secret on API route, got %v %v", respCross, err)
	}
	respCross.Body.Close()
}

// --- BrainControllerTest ---

func TestBrainListConversations(t *testing.T) {
	env := newTestServerEnv(t)

	// Empty page for a flavor with no sessions
	st, page := env.getJSON("/api/brain/conversations?flavor=nothing-here")
	if st != http.StatusOK || int(page["total"].(float64)) != 0 || len(page["items"].([]any)) != 0 {
		t.Fatalf("unexpected empty page: %+v", page)
	}

	// Stored sessions newest first
	env.seedSession("list-flavor", "older", "Old work", "[]", "", 1000)
	env.seedSession("list-flavor", "newer", "New work", "[]", "", 2000)

	_, page = env.getJSON("/api/brain/conversations?flavor=list-flavor")
	items := page["items"].([]any)
	if len(items) != 2 || int(page["total"].(float64)) != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["id"] != "newer" || first["summary"] != "New work" || first["updatedAt"] != "2000" || second["id"] != "older" {
		t.Fatalf("unexpected items order: %+v", items)
	}

	// Scoped to requested flavor
	env.seedSession("codex", "c1", "codex work", "[]", "", 1000)
	env.seedSession("claude-code", "cc1", "claude work", "[]", "", 1000)
	_, codexPage := env.getJSON("/api/brain/conversations?flavor=codex")
	codexItems := codexPage["items"].([]any)
	if len(codexItems) != 1 || codexItems[0].(map[string]any)["id"] != "c1" {
		t.Fatalf("unexpected codex items: %+v", codexItems)
	}

	// Paging with limit and offset, clamping oversized limit, and offset past end
	env.seedSession("paged", "a", "A", "[]", "", 3000)
	env.seedSession("paged", "b", "B", "[]", "", 2000)
	env.seedSession("paged", "c", "C", "[]", "", 1000)

	_, p1 := env.getJSON("/api/brain/conversations?flavor=paged&limit=2&offset=0")
	if len(p1["items"].([]any)) != 2 || int(p1["total"].(float64)) != 3 || p1["items"].([]any)[0].(map[string]any)["id"] != "a" {
		t.Fatalf("unexpected p1: %+v", p1)
	}
	_, p2 := env.getJSON("/api/brain/conversations?flavor=paged&limit=2&offset=2")
	if len(p2["items"].([]any)) != 1 || p2["items"].([]any)[0].(map[string]any)["id"] != "c" {
		t.Fatalf("unexpected p2: %+v", p2)
	}
	_, clamped := env.getJSON("/api/brain/conversations?flavor=paged&limit=999999")
	if int(clamped["limit"].(float64)) != MaxLimit {
		t.Fatalf("expected limit=%d, got %v", MaxLimit, clamped["limit"])
	}
	_, pastEnd := env.getJSON("/api/brain/conversations?flavor=paged&limit=10&offset=50")
	if len(pastEnd["items"].([]any)) != 0 || int(pastEnd["total"].(float64)) != 3 {
		t.Fatalf("unexpected pastEnd: %+v", pastEnd)
	}
}

func TestBrainTranscript(t *testing.T) {
	env := newTestServerEnv(t)

	// Missing transcript returns []
	st, body := env.get("/api/brain/conversations/missing-id/transcript?flavor=nothing")
	if st != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("expected [], got %d %q", st, body)
	}

	// Stored transcript returns steps
	steps := `[{"type":"USER_INPUT","content":"hi"},{"type":"MESSAGE","content":"ok"}]`
	env.seedSession("t-flavor", "conv-1", "t", steps, "", 1000)

	st, body = env.get("/api/brain/conversations/conv-1/transcript?flavor=t-flavor")
	if st != http.StatusOK {
		t.Fatalf("status=%d", st)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil || len(arr) != 2 || arr[0]["type"] != "USER_INPUT" || arr[1]["content"] != "ok" {
		t.Fatalf("unexpected transcript: %s", body)
	}
}

func TestBrainFilePreviewSecurityAndAttachments(t *testing.T) {
	env := newTestServerEnv(t)
	geminiDir := filepath.Join(env.home, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// 1. Serves stored session file attachment first even if path is not on local disk
	env.seedSession("antigravity-cli", "sess-attached", "Attached", "[]", "", 1000)
	if err := env.srv.SessionFiles.ReplaceForSession("antigravity-cli", "sess-attached", []store.IngestFile{
		{Path: "/remote/machine/.gemini/brain/sess-attached/plan.md", Content: "# Stored Plan"},
	}); err != nil {
		t.Fatalf("ReplaceForSession: %v", err)
	}
	st, body := env.get("/api/brain/file?id=sess-attached&path=" + url.QueryEscape("/remote/machine/.gemini/brain/sess-attached/plan.md"))
	if st != http.StatusOK || body != "# Stored Plan" {
		t.Fatalf("expected stored attachment, got %d %q", st, body)
	}

	// 2. Serves allowed file inside ~/.gemini
	notesFile := filepath.Join(geminiDir, "notes.txt")
	_ = os.WriteFile(notesFile, []byte("hello file"), 0o644)
	st, body = env.get("/api/brain/file?path=" + url.QueryEscape(notesFile))
	if st != http.StatusOK || body != "hello file" {
		t.Fatalf("expected 'hello file', got %d %q", st, body)
	}

	// 3. Serves session data under brain subtree (.json allowed under brain/)
	brainState := filepath.Join(geminiDir, "antigravity-cli", "brain", "sess-1", "state.json")
	_ = os.MkdirAll(filepath.Dir(brainState), 0o755)
	_ = os.WriteFile(brainState, []byte(`{"step":1}`), 0o644)
	st, body = env.get("/api/brain/file?path=" + url.QueryEscape(brainState))
	if st != http.StatusOK || body != `{"step":1}` {
		t.Fatalf("expected brain state.json, got %d %q", st, body)
	}

	// 4. Forbidden (403) for credentials at the sandbox root
	for _, name := range []string{"oauth_creds.json", "google_accounts.json", "settings.json", "mcp-oauth-tokens.json", ".env"} {
		credPath := filepath.Join(geminiDir, name)
		_ = os.WriteFile(credPath, []byte("refresh_token=SUPER-SECRET"), 0o644)
		st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(credPath))
		if st != http.StatusForbidden {
			t.Fatalf("%s must return 403 Forbidden, got %d", name, st)
		}
	}

	// 5. Forbidden (403) for dot-directory inside sandbox
	dotCred := filepath.Join(geminiDir, ".credentials", "token.json")
	_ = os.MkdirAll(filepath.Dir(dotCred), 0o755)
	_ = os.WriteFile(dotCred, []byte("token"), 0o644)
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(dotCred)); st != http.StatusForbidden {
		t.Fatalf("expected 403 for dot directory, got %d", st)
	}

	// 6. Forbidden (403) for symlink inside sandbox aimed at credentials
	linkToCreds := filepath.Join(geminiDir, "harmless.txt")
	_ = os.Symlink(filepath.Join(geminiDir, "oauth_creds.json"), linkToCreds)
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(linkToCreds)); st != http.StatusForbidden {
		t.Fatalf("expected 403 for symlink to credentials, got %d", st)
	}

	// 7. Unauthorized (401) for path outside ~/.gemini, traversal, sibling prefix, and escaping symlink
	outsideFile := filepath.Join(env.home, "outside-secret.txt")
	_ = os.WriteFile(outsideFile, []byte("secret"), 0o644)
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(outsideFile)); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for outside path, got %d", st)
	}

	traversal := geminiDir + string(filepath.Separator) + ".." + string(filepath.Separator) + "outside-secret.txt"
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(traversal)); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for path traversal, got %d", st)
	}

	siblingFile := filepath.Join(env.home, ".gemini-evil", "loot.txt")
	_ = os.MkdirAll(filepath.Dir(siblingFile), 0o755)
	_ = os.WriteFile(siblingFile, []byte("loot"), 0o644)
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(siblingFile)); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for sibling .gemini-evil, got %d", st)
	}

	escapeLink := filepath.Join(geminiDir, "innocent-looking.txt")
	_ = os.Symlink(outsideFile, escapeLink)
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(escapeLink)); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for escaping symlink, got %d", st)
	}

	// 8. Not found (404) for missing file or directory inside ~/.gemini
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(filepath.Join(geminiDir, "does-not-exist.txt"))); st != http.StatusNotFound {
		t.Fatalf("expected 404 for missing file, got %d", st)
	}
	subDir := filepath.Join(geminiDir, "a-directory")
	_ = os.MkdirAll(subDir, 0o755)
	if st, _ := env.get("/api/brain/file?path=" + url.QueryEscape(subDir)); st != http.StatusNotFound {
		t.Fatalf("expected 404 for directory, got %d", st)
	}
}

// --- IngestControllerTest ---

func TestIngestController(t *testing.T) {
	env := newTestServerEnv(t)
	raw := "{\"type\":\"USER_INPUT\",\"content\":\"hello\",\"created_at\":\"2026-06-19T10:00:00Z\"}\n"

	makeBatch := func(source, id, rawContent string) string {
		b, _ := json.Marshal([]transcript.IngestSession{
			{
				Source:      source,
				ID:          id,
				SourceMtime: 1700000000000,
				Raw:         rawContent,
			},
		})
		return string(b)
	}

	// 1. Pushes a trajectory and reports it in the manifest
	st, resBody := env.postJSON("/api/ingest/sessions", makeBatch("antigravity-cli", "s1", raw))
	if st != http.StatusOK {
		t.Fatalf("status=%d body=%s", st, resBody)
	}
	var res transcript.IngestResult
	_ = json.Unmarshal([]byte(resBody), &res)
	if res.Ingested != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("unexpected IngestResult: %+v", res)
	}

	_, manifest := env.getJSON("/api/ingest/manifest?source=antigravity-cli")
	if manifest["s1"] != store.Sha256Hex(raw) {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}

	// 2. Re-pushing is a no-op over HTTP
	_, againBody := env.postJSON("/api/ingest/sessions", makeBatch("antigravity-cli", "s1", raw))
	_ = json.Unmarshal([]byte(againBody), &res)
	if res.Ingested != 0 || res.Skipped != 1 {
		t.Fatalf("unexpected re-push IngestResult: %+v", res)
	}

	// 3. Accepts a large transcript (>11 MB)
	bigTranscript := `{"type":"USER_INPUT","content":"` + strings.Repeat("x", 11*1024*1024) + `"}` + "\n"
	st, bigBody := env.postJSON("/api/ingest/sessions", makeBatch("antigravity-cli", "big", bigTranscript))
	_ = json.Unmarshal([]byte(bigBody), &res)
	if st != http.StatusOK || res.Ingested != 1 || res.Failed != 0 {
		t.Fatalf("expected large transcript ingested, status=%d res=%+v", st, res)
	}

	// 4. Pushes a summary on its own and reports it in summary manifest
	summaryJSON := `{"summary":"post hoc"}`
	sumBatch, _ := json.Marshal([]transcript.IngestSummary{
		{Source: "antigravity-cli", ID: "s1", Summary: summaryJSON},
	})
	st, sumBody := env.postJSON("/api/ingest/summaries", string(sumBatch))
	_ = json.Unmarshal([]byte(sumBody), &res)
	if st != http.StatusOK || res.Ingested != 1 {
		t.Fatalf("unexpected summary push result: %d %+v", st, res)
	}
	_, sumManifest := env.getJSON("/api/ingest/summaries/manifest?source=antigravity-cli")
	if sumManifest["s1"] != store.Sha256Hex(summaryJSON) {
		t.Fatalf("unexpected summary manifest: %+v", sumManifest)
	}

	// 5. Unknown source manifests are empty; unknown source in ingest batch is counted as failed
	_, unknownManifest := env.getJSON("/api/ingest/manifest?source=nothing-here")
	if len(unknownManifest) != 0 {
		t.Fatalf("expected empty manifest, got %+v", unknownManifest)
	}
	_, unknownSumManifest := env.getJSON("/api/ingest/summaries/manifest?source=nothing-here")
	if len(unknownSumManifest) != 0 {
		t.Fatalf("expected empty summary manifest, got %+v", unknownSumManifest)
	}
	_, borgBody := env.postJSON("/api/ingest/sessions", makeBatch("borg", "s1", raw))
	_ = json.Unmarshal([]byte(borgBody), &res)
	if res.Ingested != 0 || res.Failed != 1 {
		t.Fatalf("expected failed=1 for unknown source, got %+v", res)
	}
}

// --- AnalysisControllerTest ---

func TestAnalysisController(t *testing.T) {
	env := newTestServerEnv(t)

	// 1. Progress returns sentinel when no analysis running
	st, prog := env.getJSON("/api/analysis/conversations/unknown-id/progress")
	if st != http.StatusOK || prog["phase"] != "" || int(prog["progress"].(float64)) != -1 {
		t.Fatalf("unexpected progress: %+v", prog)
	}

	// 2. Summarize returns error when API key missing
	env.srv.AiConfig = config.NewAiConfig("gemini", "", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	_, noKeyBody := env.postJSON("/api/analysis/conversations/any-id/summarize", "")
	if !strings.Contains(noKeyBody, "GEMINI_API_KEY") {
		t.Fatalf("expected GEMINI_API_KEY message, got %s", noKeyBody)
	}

	// 3. Configure AI and mock AnalyzerService
	env.srv.AiConfig = config.NewAiConfig("gemini", "test-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	env.srv.Orchestrator.Config = env.srv.AiConfig

	var analyzeCalls int32
	var consolidateCalls int32
	var inFlight int32
	var peakInFlight int32
	var delayMs int32
	var failAnalyze atomic.Bool
	var failConsolidate atomic.Bool

	var startedCh chan struct{}
	var releaseCh chan struct{}
	var chMu sync.Mutex

	chunkResp := ai.AnalysisResponse{
		ShortTitle:      "Chunk Title",
		Flow:            []string{"chunk flow"},
		AgentActions:    []transcript.AgentAction{},
		Issues:          []ai.Issue{},
		Recommendations: []string{},
		Summary:         "chunk summary",
	}
	var currentResp atomic.Value
	currentResp.Store(chunkResp)

	env.srv.Orchestrator.Analyzer = ai.NewAnalyzerService(ai.ChatModelFunc(func(ctx context.Context, sys, usr string, schema map[string]any) (string, error) {
		if strings.Contains(usr, "Please consolidate them into a single") {
			atomic.AddInt32(&consolidateCalls, 1)
			if failConsolidate.Load() {
				return "", errors.New("consolidation failed")
			}
			b, _ := json.Marshal(ai.AnalysisResponse{
				ShortTitle: "Final Title",
				Flow:       []string{"final flow"},
				Summary:    "final summary",
			})
			return string(b), nil
		}

		atomic.AddInt32(&analyzeCalls, 1)
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			prev := atomic.LoadInt32(&peakInFlight)
			if cur <= prev || atomic.CompareAndSwapInt32(&peakInFlight, prev, cur) {
				break
			}
		}
		defer atomic.AddInt32(&inFlight, -1)

		if d := atomic.LoadInt32(&delayMs); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		if failAnalyze.Load() {
			return "", errors.New("analyze failed")
		}

		chMu.Lock()
		sCh := startedCh
		rCh := releaseCh
		chMu.Unlock()
		if sCh != nil {
			select {
			case sCh <- struct{}{}:
			default:
			}
			<-rCh
		}

		b, _ := json.Marshal(currentResp.Load().(ai.AnalysisResponse))
		return string(b), nil
	}))

	// 4. Missing transcript returns "No transcript found."
	_, missingBody := env.postJSON("/api/analysis/conversations/no-transcript-here/summarize", "")
	if !strings.Contains(missingBody, "No transcript found.") {
		t.Fatalf("expected No transcript found, got %s", missingBody)
	}

	// 5. Codex & Claude Code sessions summarize and cache
	normSteps := `[{"type":"USER_INPUT","content":"do it"},{"type":"FUNCTION_CALL","source":"MODEL","tool_calls":[{"name":"exec_command","args":{"command":"ls"}}]}]`
	env.seedSession("codex", "cx-1", "t", normSteps, "", 1000)
	_, cxBody := env.postJSON("/api/analysis/conversations/cx-1/summarize?flavor=codex&force=true", "")
	if !strings.Contains(cxBody, "chunk summary") || atomic.LoadInt32(&analyzeCalls) != 1 {
		t.Fatalf("unexpected codex summarize: %s (calls=%d)", cxBody, atomic.LoadInt32(&analyzeCalls))
	}
	_, cxCached := env.postJSON("/api/analysis/conversations/cx-1/summarize?flavor=codex", "")
	if !strings.Contains(cxCached, "Chunk Title") || atomic.LoadInt32(&analyzeCalls) != 1 {
		t.Fatalf("expected cached codex summary without extra LLM call, calls=%d", atomic.LoadInt32(&analyzeCalls))
	}

	// 6. Many-chunk transcript stays within concurrency bound (<= 20) and analyzes every chunk
	atomic.StoreInt32(&analyzeCalls, 0)
	atomic.StoreInt32(&peakInFlight, 0)
	atomic.StoreInt32(&delayMs, 25)
	env.srv.Orchestrator.TokenCounter = &ai.TokenCounter{
		EstimateFn: func(text string) int { return 100001 },
	}
	var manySteps bytes.Buffer
	manySteps.WriteString("[")
	for i := 0; i < 40; i++ {
		if i > 0 {
			manySteps.WriteString(",")
		}
		fmt.Fprintf(&manySteps, `{"type":"USER_INPUT","content":"line %d"}`, i)
	}
	manySteps.WriteString("]")
	env.seedSession("antigravity-cli", "many-chunks", "t", manySteps.String(), "", 1000)

	_, manyBody := env.postJSON("/api/analysis/conversations/many-chunks/summarize?force=true", "")
	if !strings.Contains(manyBody, "Final Title") {
		t.Fatalf("unexpected many-chunks body: %s", manyBody)
	}
	if gotCalls := atomic.LoadInt32(&analyzeCalls); gotCalls != 40 {
		t.Fatalf("analyzeCalls = %d, want 40", gotCalls)
	}
	if peak := atomic.LoadInt32(&peakInFlight); peak > 20 {
		t.Fatalf("peakInFlight = %d > 20", peak)
	}
	atomic.StoreInt32(&delayMs, 0)
	env.srv.Orchestrator.TokenCounter = ai.NewTokenCounter(env.srv.AiConfig)

	// 7. Concurrent request for same conversation reports "already running"
	env.seedSession("antigravity-cli", "concurrent-sess", "t", `[{"type":"USER_INPUT","content":"do the thing"}]`, "", 1000)
	chMu.Lock()
	startedCh = make(chan struct{}, 1)
	releaseCh = make(chan struct{})
	chMu.Unlock()

	doneCh := make(chan string, 1)
	go func() {
		_, b := env.postJSON("/api/analysis/conversations/concurrent-sess/summarize?force=true", "")
		doneCh <- b
	}()
	<-startedCh

	_, secondBody := env.postJSON("/api/analysis/conversations/concurrent-sess/summarize?force=true", "")
	if !strings.Contains(secondBody, "already running") {
		t.Fatalf("expected 'already running', got %s", secondBody)
	}
	close(releaseCh)
	firstBody := <-doneCh
	if !strings.Contains(firstBody, "chunk summary") {
		t.Fatalf("expected first request to finish with chunk summary, got %s", firstBody)
	}
}

// --- EvalControllerTest, InsightsControllerTest, MineControllerTest, OptimizeControllerTest ---

func TestEvalInsightsMineAndOptimizeControllers(t *testing.T) {
	env := newTestServerEnv(t)

	stepsErr := `[{"type":"USER_INPUT","source":"USER_EXPLICIT","content":"fix the parser","created_at":"2026-06-19T10:00:00Z"},` +
		`{"type":"PLANNER_RESPONSE","source":"MODEL","created_at":"2026-06-19T10:00:05Z","tool_calls":[{"name":"Read","arguments":{}},{"name":"Edit","arguments":{}},{"name":"Bash","arguments":{}}]},` +
		`{"type":"ERROR_MESSAGE","status":"ERROR","error":"NullPointerException at X","created_at":"2026-06-19T10:00:10Z"}]`
	sumGood := `{"shortTitle":"Fixed build","summary":"Updated the JDK and it passed.","flow":["Read config"],"recommendations":["Pin the JDK with mise"],"issues":[{"error":"NPE in parser","circumvention":"added null check"}]}`

	env.seedSession("antigravity-cli", "s-good", "t-good", stepsErr, sumGood, 1000)
	env.seedSession("antigravity-cli", "s-poor", "t-poor", stepsErr, `{"summary":""}`, 2000)

	// 1. InsightsController
	st, ins := env.getJSON("/api/insights?flavor=antigravity-cli")
	if st != http.StatusOK || int(ins["sessionCount"].(float64)) != 2 || int(ins["analyzedSessions"].(float64)) != 2 {
		t.Fatalf("unexpected insights: %+v", ins)
	}
	st, dd := env.getJSON("/api/insights/sessions?flavor=antigravity-cli&category=error&key=" + url.QueryEscape("NullPointerException at X"))
	if st != http.StatusOK || int(dd["totalMatches"].(float64)) != 2 {
		t.Fatalf("unexpected drilldown: %+v", dd)
	}

	// 2. MineController (degrades without AI key)
	st, mine := env.getJSON("/api/mine?flavor=antigravity-cli")
	if st != http.StatusOK || mine["aiGenerated"] != false || !strings.Contains(mine["note"].(string), "Configure an AI provider") {
		t.Fatalf("unexpected mine: %+v", mine)
	}

	// 3. EvalController + run history save/list/delete
	st, evalRep := env.getJSON("/api/eval?flavor=antigravity-cli")
	if st != http.StatusOK || int(evalRep["sessionCount"].(float64)) != 2 || int(evalRep["evaluatedSessions"].(float64)) != 2 {
		t.Fatalf("unexpected eval: %+v", evalRep)
	}
	evalBytes, _ := json.Marshal(evalRep)
	st, savedStr := env.postJSON("/api/eval/runs", string(evalBytes))
	if st != http.StatusOK {
		t.Fatalf("save eval run failed: %d %s", st, savedStr)
	}
	var savedSnap store.EvalRunSnapshot
	_ = json.Unmarshal([]byte(savedStr), &savedSnap)
	if savedSnap.Flavor != "antigravity-cli" || savedSnap.SavedAt == "" {
		t.Fatalf("unexpected savedSnap: %+v", savedSnap)
	}

	st, listStr := env.get("/api/eval/runs?flavor=antigravity-cli")
	var history []store.EvalRunSnapshot
	_ = json.Unmarshal([]byte(listStr), &history)
	if st != http.StatusOK || len(history) != 1 {
		t.Fatalf("unexpected eval runs history: %+v", history)
	}

	st, delStr := env.deleteReq("/api/eval/runs?savedAt=" + url.QueryEscape(savedSnap.SavedAt))
	if st != http.StatusOK || !strings.Contains(delStr, `"deleted":1`) {
		t.Fatalf("unexpected delete eval run response: %d %s", st, delStr)
	}

	// 4. OptimizeController
	st, optDef := env.getJSON("/api/optimize")
	if st != http.StatusOK || int(optDef["maxSample"].(float64)) != insights.MaxSample || !strings.Contains(strings.ToLower(optDef["instruction"].(string)), "analyze") {
		t.Fatalf("unexpected /api/optimize defaults: %+v", optDef)
	}
	st, optCmpStr := env.postJSON("/api/optimize", `{"flavor":"antigravity-cli","sampleSize":3,"instructionA":"a","instructionB":"b"}`)
	if st != http.StatusOK || !strings.Contains(optCmpStr, "Configure an AI provider") {
		t.Fatalf("unexpected POST /api/optimize: %d %s", st, optCmpStr)
	}
}

// --- StoreUnavailableHandler Test ---

func TestStoreUnavailableReturns503WithHelpfulMessage(t *testing.T) {
	env := newTestServerEnv(t)
	// Close the in-memory Postgres server to simulate a database outage
	env.pg.Close()

	st, body := env.getJSON("/api/brain/conversations")
	if st != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when store is down, got %d (%+v)", st, body)
	}
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "The trajectory store is unavailable") || !strings.Contains(errMsg, "docker compose up -d") {
		t.Fatalf("unexpected 503 error message: %q", errMsg)
	}
}
