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

package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/pgtest"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
)

func setupTestDB(t *testing.T) (*sql.DB, *pgtest.Server) {
	t.Helper()
	db, srv := pgtest.NewTestDB(t)
	if err := store.ApplySchema(context.Background(), db, store.SchemaSQL()); err != nil {
		t.Fatalf("ApplySchema failed: %v", err)
	}
	return db, srv
}

func makeSession(source, id, steps, hash string) store.Session {
	return store.Session{
		Source:      source,
		ID:          id,
		Title:       "title-" + id,
		SourceMtime: 1000,
		StepsJSON:   steps,
		ContentHash: hash,
	}
}

func makeSessionAt(source, id string, mtime int64) store.Session {
	return store.Session{
		Source:      source,
		ID:          id,
		Title:       "title-" + id,
		SourceMtime: mtime,
		StepsJSON:   "[]",
		ContentHash: "h-" + id,
	}
}

// --- SessionRepository tests (ported from SessionRepositoryTest.java) ---

func TestSessionStoresANewTrajectory(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	wrote, err := sessions.Upsert(makeSession("codex", "a", `[{"type":"MESSAGE"}]`, "h1"))
	if err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}
	if !wrote {
		t.Fatalf("expected Upsert to return true on first write")
	}

	count, err := sessions.CountBySource("codex")
	if err != nil {
		t.Fatalf("CountBySource failed: %v", err)
	}
	if count != 1 {
		t.Errorf("CountBySource = %d, want 1", count)
	}

	manifest, err := sessions.Manifest("codex")
	if err != nil {
		t.Fatalf("Manifest failed: %v", err)
	}
	want := map[string]string{"a": "h1"}
	if !reflect.DeepEqual(manifest, want) {
		t.Errorf("Manifest = %v, want %v", manifest, want)
	}
}

func TestSessionRePushingIdenticalContentIsANoOp(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	wrote, err := sessions.Upsert(makeSession("codex", "a", "[]", "h1"))
	if err != nil || !wrote {
		t.Fatalf("first Upsert = (%v, %v), want (true, nil)", wrote, err)
	}
	// Same content hash: the row must be left alone, and the caller told nothing was written.
	wrote, err = sessions.Upsert(makeSession("codex", "a", "[]", "h1"))
	if err != nil {
		t.Fatalf("second Upsert error: %v", err)
	}
	if wrote {
		t.Errorf("expected second Upsert with identical hash to return false")
	}

	count, err := sessions.CountBySource("codex")
	if err != nil || count != 1 {
		t.Errorf("CountBySource = (%d, %v), want (1, nil)", count, err)
	}
}

func TestSessionRePushingChangedContentUpdatesInPlace(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	if _, err := sessions.Upsert(makeSession("codex", "a", "[]", "h1")); err != nil {
		t.Fatalf("first Upsert failed: %v", err)
	}
	wrote, err := sessions.Upsert(makeSession("codex", "a", `[{"type":"MESSAGE"}]`, "h2"))
	if err != nil || !wrote {
		t.Fatalf("second Upsert = (%v, %v), want (true, nil)", wrote, err)
	}

	count, err := sessions.CountBySource("codex")
	if err != nil || count != 1 {
		t.Errorf("CountBySource = (%d, %v), want (1, nil) — an update must not create a second row", count, err)
	}

	manifest, err := sessions.Manifest("codex")
	if err != nil {
		t.Fatalf("Manifest failed: %v", err)
	}
	want := map[string]string{"a": "h2"}
	if !reflect.DeepEqual(manifest, want) {
		t.Errorf("Manifest = %v, want %v", manifest, want)
	}
}

func TestSessionTheSameIdUnderTwoSourcesIsTwoTrajectories(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	wrote1, err := sessions.Upsert(makeSession("codex", "same", "[]", "h1"))
	if err != nil || !wrote1 {
		t.Fatalf("codex Upsert = (%v, %v), want (true, nil)", wrote1, err)
	}
	wrote2, err := sessions.Upsert(makeSession("claude-code", "same", "[]", "h2"))
	if err != nil || !wrote2 {
		t.Fatalf("claude-code Upsert = (%v, %v), want (true, nil)", wrote2, err)
	}

	if c, err := sessions.CountBySource("codex"); err != nil || c != 1 {
		t.Errorf("CountBySource(codex) = (%d, %v), want (1, nil)", c, err)
	}
	if c, err := sessions.CountBySource("claude-code"); err != nil || c != 1 {
		t.Errorf("CountBySource(claude-code) = (%d, %v), want (1, nil)", c, err)
	}
}

func TestSessionTheManifestIsScopedToItsSource(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, _ = sessions.Upsert(makeSession("codex", "a", "[]", "h1"))
	_, _ = sessions.Upsert(makeSession("claude-code", "b", "[]", "h2"))

	mCodex, err := sessions.Manifest("codex")
	if err != nil || !reflect.DeepEqual(mCodex, map[string]string{"a": "h1"}) {
		t.Errorf("Manifest(codex) = (%v, %v), want map[a:h1]", mCodex, err)
	}
	mClaude, err := sessions.Manifest("claude-code")
	if err != nil || !reflect.DeepEqual(mClaude, map[string]string{"b": "h2"}) {
		t.Errorf("Manifest(claude-code) = (%v, %v), want map[b:h2]", mClaude, err)
	}
	mAgy, err := sessions.Manifest("antigravity-cli")
	if err != nil || len(mAgy) != 0 {
		t.Errorf("Manifest(antigravity-cli) = (%v, %v), want empty", mAgy, err)
	}
}

func TestSessionCountAndManifestAreEmptyForAnUnknownSource(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	if c, err := sessions.CountBySource("nope"); err != nil || c != 0 {
		t.Errorf("CountBySource(nope) = (%d, %v), want (0, nil)", c, err)
	}
	if m, err := sessions.Manifest("nope"); err != nil || len(m) != 0 {
		t.Errorf("Manifest(nope) = (%v, %v), want empty", m, err)
	}
}

func TestSessionListsConversationsNewestFirstScopedToSource(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, _ = sessions.Upsert(makeSessionAt("codex", "old", 1000))
	_, _ = sessions.Upsert(makeSessionAt("codex", "new", 2000))
	_, _ = sessions.Upsert(makeSessionAt("claude-code", "other", 3000))

	list, err := sessions.ListConversations("codex", 100, 0)
	if err != nil {
		t.Fatalf("ListConversations failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
	if list[0]["id"] != "new" {
		t.Errorf("list[0].id = %q, want %q", list[0]["id"], "new")
	}
	if list[1]["id"] != "old" {
		t.Errorf("list[1].id = %q, want %q", list[1]["id"], "old")
	}
	if list[0]["summary"] != "title-new" {
		t.Errorf("list[0].summary = %q, want %q", list[0]["summary"], "title-new")
	}
	if list[0]["updatedAt"] != "2000" {
		t.Errorf("list[0].updatedAt = %q, want %q", list[0]["updatedAt"], "2000")
	}
}

func TestSessionListConversationsHonoursLimitAndOffsetForStablePaging(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, _ = sessions.Upsert(makeSessionAt("codex", "a", 3000)) // newest
	_, _ = sessions.Upsert(makeSessionAt("codex", "b", 2000))
	_, _ = sessions.Upsert(makeSessionAt("codex", "c", 1000)) // oldest

	page1, err := sessions.ListConversations("codex", 2, 0)
	if err != nil {
		t.Fatalf("page1 error: %v", err)
	}
	if len(page1) != 2 || page1[0]["id"] != "a" || page1[1]["id"] != "b" {
		t.Fatalf("page1 = %v, want [a, b]", page1)
	}

	page2, err := sessions.ListConversations("codex", 2, 2)
	if err != nil {
		t.Fatalf("page2 error: %v", err)
	}
	if len(page2) != 1 || page2[0]["id"] != "c" {
		t.Fatalf("page2 = %v, want [c]", page2)
	}
}

func TestSessionListBreaksTiesByIDOnEqualUpdatedAt(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, _ = sessions.Upsert(makeSessionAt("codex", "z", 1000))
	_, _ = sessions.Upsert(makeSessionAt("codex", "a", 1000))
	_, _ = sessions.Upsert(makeSessionAt("codex", "m", 1000))

	list, err := sessions.ListConversations("codex", 10, 0)
	if err != nil {
		t.Fatalf("ListConversations failed: %v", err)
	}
	if len(list) != 3 || list[0]["id"] != "a" || list[1]["id"] != "m" || list[2]["id"] != "z" {
		t.Fatalf("list order = %v, want [a, m, z]", list)
	}
}

func TestSessionListPrefersTheCachedAiShortTitleOverTheSessionTitle(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)
	summaries := store.NewSummaryRepository(db)

	_, _ = sessions.Upsert(store.Session{
		Source:      "codex",
		ID:          "a",
		Title:       "Long raw first prompt…",
		SourceMtime: 1000,
		StepsJSON:   "[]",
		ContentHash: "h",
	})
	if _, err := summaries.Upsert("codex", "a", `{"summary":"s"}`, "Fixed the parser"); err != nil {
		t.Fatalf("summaries.Upsert failed: %v", err)
	}

	list, err := sessions.ListConversations("codex", 100, 0)
	if err != nil {
		t.Fatalf("ListConversations failed: %v", err)
	}
	if list[0]["summary"] != "Fixed the parser" {
		t.Errorf("list[0].summary = %q, want %q", list[0]["summary"], "Fixed the parser")
	}
}

func TestSessionListFallsBackToTheSessionTitleWhenNoSummaryIsCached(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, _ = sessions.Upsert(makeSessionAt("codex", "a", 1000))
	list, err := sessions.ListConversations("codex", 100, 0)
	if err != nil {
		t.Fatalf("ListConversations failed: %v", err)
	}
	if list[0]["summary"] != "title-a" {
		t.Errorf("list[0].summary = %q, want %q", list[0]["summary"], "title-a")
	}
}

func TestSessionStepsReturnsTheStoredArrayOrNotFound(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, _ = sessions.Upsert(makeSession("codex", "a", `[{"type":"MESSAGE"}]`, "h1"))

	steps, ok, err := sessions.Steps("codex", "a")
	if err != nil || !ok || !strings.Contains(steps, "MESSAGE") {
		t.Errorf("Steps(codex, a) = (%q, %v, %v), want containing MESSAGE", steps, ok, err)
	}

	missing, ok, err := sessions.Steps("codex", "missing")
	if err != nil || ok || missing != "" {
		t.Errorf("Steps(codex, missing) = (%q, %v, %v), want (\"\", false, nil)", missing, ok, err)
	}
}

func TestSessionExistsReflectsWhetherASessionIsStored(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, _ = sessions.Upsert(makeSession("codex", "a", "[]", "h1"))

	if exists, err := sessions.Exists("codex", "a"); err != nil || !exists {
		t.Errorf("Exists(codex, a) = (%v, %v), want (true, nil)", exists, err)
	}
	if exists, err := sessions.Exists("codex", "b"); err != nil || exists {
		t.Errorf("Exists(codex, b) = (%v, %v), want (false, nil)", exists, err)
	}
	if exists, err := sessions.Exists("claude-code", "a"); err != nil || exists {
		t.Errorf("Exists(claude-code, a) = (%v, %v), want (false, nil)", exists, err)
	}
}

func TestSessionCollectReturnsStepsAndJoinedSummaryNewestFirstUpToLimit(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)
	summaries := store.NewSummaryRepository(db)

	_, _ = sessions.Upsert(makeSessionAt("codex", "old", 1000))
	_, _ = sessions.Upsert(makeSessionAt("codex", "mid", 2000))
	_, _ = sessions.Upsert(makeSessionAt("codex", "new", 3000))
	_, _ = summaries.Upsert("codex", "new", `{"summary":"s"}`, "t")

	collected, err := sessions.Collect("codex", 2)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(collected) != 2 {
		t.Fatalf("len(collected) = %d, want 2", len(collected))
	}
	if collected[0].ID != "new" {
		t.Errorf("collected[0].ID = %q, want %q", collected[0].ID, "new")
	}
	if collected[1].ID != "mid" {
		t.Errorf("collected[1].ID = %q, want %q", collected[1].ID, "mid")
	}
	if !collected[0].HasSummary || !strings.Contains(collected[0].SummaryJSON, "summary") {
		t.Errorf("collected[0] summary = (%v, %q), want summary present", collected[0].HasSummary, collected[0].SummaryJSON)
	}
	if collected[1].HasSummary || collected[1].SummaryJSON != "" {
		t.Errorf("collected[1] summary = (%v, %q), want no summary", collected[1].HasSummary, collected[1].SummaryJSON)
	}
}

func TestSessionRejectsInvalidJSONB(t *testing.T) {
	db, _ := setupTestDB(t)
	sessions := store.NewSessionRepository(db)

	_, err := sessions.Upsert(makeSession("codex", "bad", "not-valid-json", "h1"))
	if err == nil {
		t.Fatalf("expected invalid JSON steps to fail ::jsonb validation")
	}
	if _, ok := store.IsStoreUnavailable(err); !ok {
		t.Errorf("expected *StoreUnavailableError, got %T: %v", err, err)
	}
}

// --- SummaryRepository tests (ported from SummaryRepositoryTest.java) ---

func TestSummaryFindReturnsWhatWasWritten(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	if _, err := summaries.Upsert("codex", "a", `{"shortTitle":"Fixed build","summary":"s"}`, "Fixed build"); err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	found, ok, err := summaries.Find("codex", "a")
	if err != nil || !ok {
		t.Fatalf("Find = (%q, %v, %v), want found", found, ok, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(found), &parsed); err != nil {
		t.Fatalf("Unmarshal found summary failed: %v", err)
	}
	if parsed["shortTitle"] != "Fixed build" {
		t.Errorf("shortTitle = %v, want %q", parsed["shortTitle"], "Fixed build")
	}
}

func TestSummaryUpsertReplacesAnExistingSummary(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	_, _ = summaries.Upsert("codex", "a", `{"summary":"first"}`, "t1")
	_, _ = summaries.Upsert("codex", "a", `{"summary":"second"}`, "t2")

	found, ok, err := summaries.Find("codex", "a")
	if err != nil || !ok || !strings.Contains(found, "second") {
		t.Errorf("Find = (%q, %v, %v), want containing 'second'", found, ok, err)
	}
}

func TestSummaryFindIsEmptyForAnUnknownSession(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	found, ok, err := summaries.Find("codex", "nope")
	if err != nil || ok || found != "" {
		t.Errorf("Find(codex, nope) = (%q, %v, %v), want (\"\", false, nil)", found, ok, err)
	}
}

func TestSummaryDeleteRemovesTheCachedSummary(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	_, _ = summaries.Upsert("codex", "a", `{"summary":"s"}`, "t")
	if err := summaries.Delete("codex", "a"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, ok, err := summaries.Find("codex", "a"); err != nil || ok {
		t.Errorf("Find after Delete = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestSummarySummariesAreKeyedBySourceAndId(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	_, _ = summaries.Upsert("codex", "same", `{"summary":"codex"}`, "t")
	_, _ = summaries.Upsert("claude-code", "same", `{"summary":"claude"}`, "t")

	f1, ok1, _ := summaries.Find("codex", "same")
	f2, ok2, _ := summaries.Find("claude-code", "same")
	if !ok1 || !strings.Contains(f1, "codex") {
		t.Errorf("Find(codex, same) = %q, want codex", f1)
	}
	if !ok2 || !strings.Contains(f2, "claude") {
		t.Errorf("Find(claude-code, same) = %q, want claude", f2)
	}
}

func TestSummaryUpsertReportsWhetherItWroteAndSkipsUnchangedContent(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	if wrote, err := summaries.Upsert("codex", "a", `{"summary":"one"}`, "t"); err != nil || !wrote {
		t.Fatalf("first write = (%v, %v), want (true, nil)", wrote, err)
	}
	if wrote, err := summaries.Upsert("codex", "a", `{"summary":"one"}`, "t"); err != nil || wrote {
		t.Fatalf("identical content write = (%v, %v), want (false, nil)", wrote, err)
	}
	if wrote, err := summaries.Upsert("codex", "a", `{"summary":"two"}`, "t"); err != nil || !wrote {
		t.Fatalf("changed content write = (%v, %v), want (true, nil)", wrote, err)
	}
}

func TestSummaryManifestPublishesTheContentHashPerSession(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	_, _ = summaries.Upsert("codex", "a", `{"summary":"one"}`, "t")
	_, _ = summaries.Upsert("codex", "b", `{"summary":"two"}`, "t")
	_, _ = summaries.Upsert("claude-code", "c", `{"summary":"other"}`, "t")

	// Also insert a legacy row with NULL content_hash and verify Manifest excludes it.
	if _, err := db.Exec(`INSERT INTO summaries (source, session_id, summary, short_title, content_hash, updated_at) VALUES ($1, $2, $3::jsonb, $4, $5, now())`, "codex", "legacy", `{"summary":"old"}`, "t", nil); err != nil {
		t.Fatalf("insert legacy summary failed: %v", err)
	}

	manifest, err := summaries.Manifest("codex")
	if err != nil {
		t.Fatalf("Manifest failed: %v", err)
	}
	if len(manifest) != 2 {
		t.Fatalf("len(manifest) = %d, want 2", len(manifest))
	}
	if manifest["a"] != store.Sha256Hex(`{"summary":"one"}`) {
		t.Errorf("manifest[a] = %q, want %q", manifest["a"], store.Sha256Hex(`{"summary":"one"}`))
	}
	if manifest["b"] != store.Sha256Hex(`{"summary":"two"}`) {
		t.Errorf("manifest[b] = %q, want %q", manifest["b"], store.Sha256Hex(`{"summary":"two"}`))
	}
}

func TestSummaryBlankTitleDoesNotWipeAnExistingShortTitle(t *testing.T) {
	db, _ := setupTestDB(t)
	summaries := store.NewSummaryRepository(db)

	if _, err := summaries.Upsert("codex", "a", `{"summary":"first"}`, "Nice Title"); err != nil {
		t.Fatalf("first Upsert failed: %v", err)
	}
	// A summary-only push carries no title; it must not erase the label set earlier.
	if _, err := summaries.Upsert("codex", "a", `{"summary":"second"}`, ""); err != nil {
		t.Fatalf("second Upsert failed: %v", err)
	}

	var stored sql.NullString
	if err := db.QueryRow(`SELECT short_title FROM summaries WHERE source = $1 AND session_id = $2`, "codex", "a").Scan(&stored); err != nil {
		t.Fatalf("QueryRow short_title failed: %v", err)
	}
	if !stored.Valid || stored.String != "Nice Title" {
		t.Errorf("short_title = %v, want 'Nice Title'", stored)
	}
}

// --- SessionFileRepository tests ---

func TestSessionFileRepositoryLifecycleAndTransactionIsolation(t *testing.T) {
	db, _ := setupTestDB(t)
	filesRepo := store.NewSessionFileRepository(db)

	initial := []store.IngestFile{
		{Path: "/workspace/README.md", Content: "# Hello"},
		{Path: "/workspace/main.go", Content: "package main"},
	}
	if err := filesRepo.ReplaceForSession("antigravity", "sess-1", initial); err != nil {
		t.Fatalf("ReplaceForSession failed: %v", err)
	}

	content, ok, err := filesRepo.Find("antigravity", "sess-1", "/workspace/README.md")
	if err != nil || !ok || content != "# Hello" {
		t.Errorf("Find README.md = (%q, %v, %v), want ('# Hello', true, nil)", content, ok, err)
	}

	// Replace with a new file set; old file should be removed and new one stored.
	updated := []store.IngestFile{
		{Path: "/workspace/main.go", Content: "package main\nfunc main() {}"},
		{Path: "/workspace/go.mod", Content: "module example"},
	}
	if err := filesRepo.ReplaceForSession("antigravity", "sess-1", updated); err != nil {
		t.Fatalf("second ReplaceForSession failed: %v", err)
	}

	if _, ok, err := filesRepo.Find("antigravity", "sess-1", "/workspace/README.md"); err != nil || ok {
		t.Errorf("expected old attachment /workspace/README.md to be removed, got (%v, %v)", ok, err)
	}
	if c, ok, err := filesRepo.Find("antigravity", "sess-1", "/workspace/main.go"); err != nil || !ok || c != "package main\nfunc main() {}" {
		t.Errorf("Find updated main.go = (%q, %v, %v)", c, ok, err)
	}
	if c, ok, err := filesRepo.Find("antigravity", "sess-1", "/workspace/go.mod"); err != nil || !ok || c != "module example" {
		t.Errorf("Find go.mod = (%q, %v, %v)", c, ok, err)
	}

	// DeleteForSession removes all attachments for that session.
	if err := filesRepo.DeleteForSession("antigravity", "sess-1"); err != nil {
		t.Fatalf("DeleteForSession failed: %v", err)
	}
	if _, ok, err := filesRepo.Find("antigravity", "sess-1", "/workspace/main.go"); err != nil || ok {
		t.Errorf("expected attachment to be deleted after DeleteForSession, got (%v, %v)", ok, err)
	}
}

// --- EvalRunRepository tests (ported from EvalRunRepositoryTest.java) ---

func makeEvalReport(flavor string, avgScore float64, judge *store.JudgeSummary) store.EvalReport {
	return store.EvalReport{
		Flavor:            flavor,
		ModelLabel:        "gemini · gemini-3.5-flash",
		SessionCount:      10,
		SampledSessions:   10,
		EvaluatedSessions: 5,
		AvgScore:          avgScore,
		CheckPassRates:    []store.NameCount{{Name: "schema-complete", Count: 4}},
		WorstCases:        []store.EvalCaseResult{},
		Judge:             judge,
	}
}

func judgeNotRun() *store.JudgeSummary {
	return &store.JudgeSummary{Ran: false, Note: "n", Cases: []store.JudgedCase{}}
}

func TestEvalRunSavesAndListsNewestFirst(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	if _, err := repo.SaveAt(makeEvalReport("codex", 70.0, judgeNotRun()), "2026-07-09T12:00:00Z"); err != nil {
		t.Fatalf("first Save failed: %v", err)
	}
	if _, err := repo.SaveAt(makeEvalReport("codex", 80.0, judgeNotRun()), "2026-07-09T12:00:01Z"); err != nil {
		t.Fatalf("second Save failed: %v", err)
	}

	runs, err := repo.List("codex")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].AvgScore != 80.0 || runs[1].AvgScore != 70.0 {
		t.Errorf("runs AvgScore = [%v, %v], want [80, 70]", runs[0].AvgScore, runs[1].AvgScore)
	}
	if runs[0].Judged {
		t.Errorf("expected Judged = false")
	}
	if runs[0].Flavor != "codex" {
		t.Errorf("Flavor = %q, want codex", runs[0].Flavor)
	}
}

func TestEvalRunFiltersByFlavor(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	_, _ = repo.Save(makeEvalReport("codex", 70.0, judgeNotRun()))
	_, _ = repo.Save(makeEvalReport("claude-code", 90.0, judgeNotRun()))

	codex, err := repo.List("codex")
	if err != nil || len(codex) != 1 || codex[0].Flavor != "codex" {
		t.Fatalf("List(codex) = (%v, %v), want 1 codex run", codex, err)
	}
	claude, err := repo.List("claude-code")
	if err != nil || len(claude) != 1 || claude[0].AvgScore != 90.0 {
		t.Fatalf("List(claude-code) = (%v, %v), want AvgScore 90.0", claude, err)
	}
}

func TestEvalRunRoundTripsTheCheckPassRateTally(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	_, _ = repo.Save(makeEvalReport("codex", 70.0, judgeNotRun()))

	runs, err := repo.List("codex")
	if err != nil || len(runs) != 1 {
		t.Fatalf("List failed: %v", err)
	}
	rates := runs[0].CheckPassRates
	if len(rates) != 1 || rates[0].Name != "schema-complete" || rates[0].Count != 4 {
		t.Errorf("CheckPassRates = %v, want [{schema-complete 4}]", rates)
	}
}

func TestEvalRunCapturesJudgeAveragesWhenJudged(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	judge := &store.JudgeSummary{
		Ran:              true,
		JudgedSessions:   3,
		AvgFaithfulness:  4.5,
		AvgActionability: 4.0,
		AvgClarity:       3.5,
		Cases:            []store.JudgedCase{},
	}
	_, _ = repo.Save(makeEvalReport("codex", 75.0, judge))

	runs, err := repo.List("codex")
	if err != nil || len(runs) != 1 {
		t.Fatalf("List failed: %v", err)
	}
	run := runs[0]
	if !run.Judged || run.JudgedSessions != 3 || run.AvgFaithfulness != 4.5 || run.AvgActionability != 4.0 || run.AvgClarity != 3.5 {
		t.Errorf("unexpected judged snapshot: %+v", run)
	}
}

func TestEvalRunCapsHistoryToMaxKeepingNewest(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	n := store.EvalRunMaxRuns + 10
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		stamp := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := repo.SaveAt(makeEvalReport("codex", float64(i), judgeNotRun()), stamp); err != nil {
			t.Fatalf("SaveAt(%d) failed: %v", i, err)
		}
	}

	runs, err := repo.List("codex")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(runs) != store.EvalRunMaxRuns {
		t.Fatalf("len(runs) = %d, want %d", len(runs), store.EvalRunMaxRuns)
	}
	if runs[0].AvgScore != float64(n-1) {
		t.Errorf("newest AvgScore = %v, want %v", runs[0].AvgScore, float64(n-1))
	}
}

func TestEvalRunListIsEmptyWhenNothingSaved(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	runs, err := repo.List("codex")
	if err != nil || len(runs) != 0 {
		t.Errorf("List(codex) = (%v, %v), want empty", runs, err)
	}
}

func TestEvalRunDefaultsBlankFlavorAndModelSoTheSnapshotStaysListable(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	saved, err := repo.Save(store.EvalReport{})
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if saved.Flavor != "unknown" || saved.ModelLabel != "unknown" {
		t.Errorf("saved = (%q, %q), want ('unknown', 'unknown')", saved.Flavor, saved.ModelLabel)
	}
	runs, err := repo.List("unknown")
	if err != nil || len(runs) != 1 {
		t.Errorf("List(unknown) = (%v, %v), want 1 run", runs, err)
	}
}

func TestEvalRunDeletesBySavedAtAndLeavesOthers(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	a, _ := repo.SaveAt(makeEvalReport("codex", 70.0, judgeNotRun()), "2026-07-09T12:00:00Z")
	b, _ := repo.SaveAt(makeEvalReport("codex", 80.0, judgeNotRun()), "2026-07-09T12:00:01Z")

	deleted, err := repo.Delete(a.SavedAt)
	if err != nil || deleted != 1 {
		t.Fatalf("Delete(a) = (%d, %v), want (1, nil)", deleted, err)
	}
	remaining, err := repo.List("codex")
	if err != nil || len(remaining) != 1 || remaining[0].SavedAt != b.SavedAt {
		t.Fatalf("remaining = %v, want [%s]", remaining, b.SavedAt)
	}

	if d, err := repo.Delete("2000-01-01T00:00:00Z"); err != nil || d != 0 {
		t.Errorf("Delete(unknown) = (%d, %v), want (0, nil)", d, err)
	}
	if d, err := repo.Delete(""); err != nil || d != 0 {
		t.Errorf("Delete(blank) = (%d, %v), want (0, nil)", d, err)
	}
}

func TestEvalRunStampsAParseableServerSideSavedAt(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	saved, err := repo.Save(makeEvalReport("codex", 50.0, judgeNotRun()))
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if strings.TrimSpace(saved.SavedAt) == "" {
		t.Fatalf("SavedAt is blank")
	}
	if _, err := time.Parse(time.RFC3339Nano, saved.SavedAt); err != nil {
		t.Errorf("SavedAt %q is not valid RFC3339Nano: %v", saved.SavedAt, err)
	}
}

func TestEvalRunTwoRunsSharingAnInstantBothPersistAndBothDelete(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	shared := "2026-07-09T12:00:00Z"
	_, _ = repo.SaveAt(makeEvalReport("codex", 70.0, judgeNotRun()), shared)
	_, _ = repo.SaveAt(makeEvalReport("codex", 80.0, judgeNotRun()), shared)
	_, _ = repo.SaveAt(makeEvalReport("codex", 90.0, judgeNotRun()), "2026-07-09T12:00:01Z")

	if runs, _ := repo.List("codex"); len(runs) != 3 {
		t.Fatalf("len(runs) = %d, want 3", len(runs))
	}
	if d, err := repo.Delete(shared); err != nil || d != 2 {
		t.Fatalf("Delete(shared) = (%d, %v), want (2, nil)", d, err)
	}
	remaining, _ := repo.List("codex")
	if len(remaining) != 1 || remaining[0].AvgScore != 90.0 {
		t.Errorf("remaining = %v, want 1 run with AvgScore 90.0", remaining)
	}
}

func TestEvalRunOrdersRunsSharingAnInstantByInsertionSoTheNewestStillLeads(t *testing.T) {
	db, _ := setupTestDB(t)
	repo := store.NewEvalRunRepository(db)

	shared := "2026-07-09T12:00:00Z"
	_, _ = repo.SaveAt(makeEvalReport("codex", 70.0, judgeNotRun()), shared)
	_, _ = repo.SaveAt(makeEvalReport("codex", 80.0, judgeNotRun()), shared)

	runs, err := repo.List("codex")
	if err != nil || len(runs) != 2 {
		t.Fatalf("List failed: %v", err)
	}
	if runs[0].AvgScore != 80.0 {
		t.Errorf("runs[0].AvgScore = %v, want 80.0", runs[0].AvgScore)
	}
}

// --- SchemaBootstrap, Sha256Hex, and StoreUnavailableError tests ---

func TestSchemaBootstrapAndSha256Hex(t *testing.T) {
	db, _ := pgtest.NewTestDB(t)

	// Calling BootstrapSchema multiple times is idempotent.
	store.BootstrapSchema(context.Background(), db)
	store.BootstrapSchema(context.Background(), db)

	if err := store.ApplySchema(context.Background(), nil, store.SchemaSQL()); err == nil {
		t.Errorf("ApplySchema(nil) succeeded, want error")
	}

	gotHash := store.Sha256Hex("hello")
	wantHash := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if gotHash != wantHash {
		t.Errorf("Sha256Hex('hello') = %q, want %q", gotHash, wantHash)
	}
}

func TestStoreUnavailableErrorWhenServerClosed(t *testing.T) {
	db, srv := setupTestDB(t)
	sessions := store.NewSessionRepository(db)
	summaries := store.NewSummaryRepository(db)
	files := store.NewSessionFileRepository(db)
	evals := store.NewEvalRunRepository(db)

	// Shut down the wire server to simulate database failure.
	srv.Close()

	// BootstrapSchema must not panic when the database is unreachable.
	store.BootstrapSchema(context.Background(), db)

	checkUnavailable := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s succeeded on closed server, want StoreUnavailableError", name)
			return
		}
		uErr, ok := store.IsStoreUnavailable(err)
		if !ok || uErr == nil {
			t.Errorf("%s returned %T (%v), want *StoreUnavailableError", name, err, err)
		}
	}

	_, err := sessions.Upsert(makeSession("codex", "a", "[]", "h1"))
	checkUnavailable("sessions.Upsert", err)

	_, err = sessions.Manifest("codex")
	checkUnavailable("sessions.Manifest", err)

	_, err = sessions.ListConversations("codex", 10, 0)
	checkUnavailable("sessions.ListConversations", err)

	_, _, err = sessions.Steps("codex", "a")
	checkUnavailable("sessions.Steps", err)

	_, err = sessions.Exists("codex", "a")
	checkUnavailable("sessions.Exists", err)

	_, err = sessions.Collect("codex", 10)
	checkUnavailable("sessions.Collect", err)

	_, err = sessions.CountBySource("codex")
	checkUnavailable("sessions.CountBySource", err)

	_, _, err = summaries.Find("codex", "a")
	checkUnavailable("summaries.Find", err)

	_, err = summaries.Upsert("codex", "a", `{"s":1}`, "t")
	checkUnavailable("summaries.Upsert", err)

	_, err = summaries.Manifest("codex")
	checkUnavailable("summaries.Manifest", err)

	err = summaries.Delete("codex", "a")
	checkUnavailable("summaries.Delete", err)

	err = files.ReplaceForSession("codex", "a", []store.IngestFile{{Path: "/a", Content: "x"}})
	checkUnavailable("files.ReplaceForSession", err)

	_, _, err = store.NewSessionFileRepository(nil).Find("codex", "a", "/a")
	checkUnavailable("files.Find(nil db)", err)

	err = files.DeleteForSession("codex", "a")
	checkUnavailable("files.DeleteForSession", err)

	_, err = evals.Save(makeEvalReport("codex", 50.0, judgeNotRun()))
	checkUnavailable("evals.Save", err)

	_, err = evals.List("codex")
	checkUnavailable("evals.List", err)

	_, err = evals.Delete("2026-07-09T12:00:00Z")
	checkUnavailable("evals.Delete", err)
}
