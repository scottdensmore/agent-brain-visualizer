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

package insights

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scottdensmore/agent-brain-visualizer/internal/ai"
	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/pgtest"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
)

// --- ErrorNormalizer Tests ---

func TestCollapsesQuotedValuesSoSameFailureGroups(t *testing.T) {
	a := "Error: ENOENT: no such file or directory, open '/Users/alice/project/a.txt'"
	b := "Error: ENOENT: no such file or directory, open '/home/bob/work/b.txt'"
	if NormalizeError(a) != NormalizeError(b) {
		t.Fatalf("NormalizeError(a)=%q != NormalizeError(b)=%q", NormalizeError(a), NormalizeError(b))
	}
	if !strings.Contains(NormalizeError(a), "'<v>'") {
		t.Fatalf("expected '<v>' in %q", NormalizeError(a))
	}
}

func TestCollapsesBarePaths(t *testing.T) {
	if got := NormalizeError("cannot open /Users/a/b/c.txt"); got != "cannot open <path>" {
		t.Fatalf("got %q, want %q", got, "cannot open <path>")
	}
	if NormalizeError("cannot open /Users/a/b/c.txt") != NormalizeError("cannot open /var/tmp/other/z.log") {
		t.Fatalf("expected bare paths to collapse to same key")
	}
}

func TestCollapsesNumbersButKeepsAlphanumericCodes(t *testing.T) {
	if got := NormalizeError("listen EADDRINUSE :::3000"); got != "listen EADDRINUSE :::<n>" {
		t.Fatalf("got %q, want %q", got, "listen EADDRINUSE :::<n>")
	}
	ts := NormalizeError("error TS2304: Cannot find name 'foo'")
	if !strings.Contains(ts, "TS2304") || !strings.Contains(ts, "'<v>'") {
		t.Fatalf("unexpected ts=%q", ts)
	}
}

func TestCollapsesHexAndUuid(t *testing.T) {
	if got := NormalizeError("segfault at 0xdeadbeef"); got != "segfault at <hex>" {
		t.Fatalf("got %q, want %q", got, "segfault at <hex>")
	}
	if got := NormalizeError("run 550e8400-e29b-41d4-a716-446655440000 failed"); got != "run <id> failed" {
		t.Fatalf("got %q, want %q", got, "run <id> failed")
	}
}

func TestPrefersAStrongErrorLineOverANoisyFirstLine(t *testing.T) {
	blob := "$ ./run.sh\nProcess exited with code 2\nError: connection refused"
	if got := NormalizeError(blob); got != "Error: connection refused" {
		t.Fatalf("got %q, want %q", got, "Error: connection refused")
	}
}

func TestFallsBackToExitLineWhenNoStrongSignal(t *testing.T) {
	blob := "some output\nProcess exited with code 137"
	if got := NormalizeError(blob); got != "Process exited with code <n>" {
		t.Fatalf("got %q, want %q", got, "Process exited with code <n>")
	}
}

func TestStripsAntigravityStepPrefixAndMetadata(t *testing.T) {
	blob := "Encountered error in step execution: The thing broke\nCreated At: 2026-06-19"
	if got := NormalizeError(blob); got != "The thing broke" {
		t.Fatalf("got %q, want %q", got, "The thing broke")
	}
}

func TestBoundsOutputToAMaxLength(t *testing.T) {
	huge := "Error: " + strings.Repeat("x", 5000)
	key := NormalizeError(huge)
	if len(key) > 100 {
		t.Fatalf("len(key) = %d > 100", len(key))
	}
	if !strings.HasPrefix(key, "Error:") {
		t.Fatalf("expected prefix 'Error:', got %q", key)
	}
}

func TestReturnsUnknownForBlankInput(t *testing.T) {
	if got := NormalizeError(""); got != "Unknown error" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeError("   \n  "); got != "Unknown error" {
		t.Fatalf("got %q", got)
	}
}

// --- SessionCollector Tests ---

func newTestInsightsEnv(t *testing.T) (*SessionCollector, *store.SessionRepository, *store.SummaryRepository) {
	t.Helper()
	db, _ := pgtest.NewTestDB(t)
	if err := store.ApplySchema(context.Background(), db, store.SchemaSQL()); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	sessionRepo := store.NewSessionRepository(db)
	summaryRepo := store.NewSummaryRepository(db)
	return NewSessionCollector(sessionRepo), sessionRepo, summaryRepo
}

func TestCollectReadsStoredSessionsAndCachedSummariesForFlavor(t *testing.T) {
	collector, sessionRepo, summaryRepo := newTestInsightsEnv(t)

	_, err := sessionRepo.Upsert(store.Session{
		Source:      "codex",
		ID:          "cx-1",
		Title:       "Title 1",
		SourceMtime: 2000,
		StepsJSON:   `[{"type":"USER_INPUT","content":"hello codex"}]`,
		ContentHash: "h1",
	})
	if err != nil {
		t.Fatalf("Upsert cx-1: %v", err)
	}
	_, err = sessionRepo.Upsert(store.Session{
		Source:      "codex",
		ID:          "cx-2",
		Title:       "Title 2",
		SourceMtime: 1000,
		StepsJSON:   `[{"type":"USER_INPUT","content":"older"}]`,
		ContentHash: "h2",
	})
	if err != nil {
		t.Fatalf("Upsert cx-2: %v", err)
	}
	_, err = summaryRepo.Upsert("codex", "cx-1", `{"shortTitle":"Cached Title","summary":"ok"}`, "Cached Title")
	if err != nil {
		t.Fatalf("Upsert summary: %v", err)
	}

	collected, err := collector.Collect("codex")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if collected.TotalSessionCount != 2 || len(collected.Sessions) != 2 {
		t.Fatalf("unexpected collected: %+v", collected)
	}
	if collected.Sessions[0].ID != "cx-1" || len(collected.Sessions[0].Steps) != 1 || collected.Sessions[0].CachedSummary["shortTitle"] != "Cached Title" {
		t.Fatalf("unexpected Sessions[0]: %+v", collected.Sessions[0])
	}
	if collected.Sessions[1].ID != "cx-2" || collected.Sessions[1].CachedSummary != nil {
		t.Fatalf("unexpected Sessions[1]: %+v", collected.Sessions[1])
	}
}

// --- FleetInsights & InsightsService Tests ---

func sampleInsightsSessions() []Session {
	var steps1 []map[string]any
	_ = json.Unmarshal([]byte(`[`+
		`{"type":"USER_INPUT","content":"Build the project","created_at":"2026-04-17T10:00:00Z"},`+
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Bash"},{"name":"Bash"},{"name":"Bash"}]},`+
		`{"type":"FUNCTION_OUTPUT","status":"ERROR","content":"Exit code 1: Compilation failed\nmore details"},`+
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Read"}],"created_at":"2026-04-17T10:01:00Z"}`+
		`]`), &steps1)

	var summary1 map[string]any
	_ = json.Unmarshal([]byte(`{"shortTitle":"Fix Build Failure","summary":"Fixed build","flow":["run","fix"],`+
		`"issues":[{"error":"Gradle daemon lock","circumvention":"Stopped daemon"}],`+
		`"recommendations":["Pin Java 25 in toolchain"]}`), &summary1)

	var steps2 []map[string]any
	_ = json.Unmarshal([]byte(`[`+
		`{"type":"USER_INPUT","content":"Add unit tests"},`+
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Bash"},{"name":"Write"}]}`+
		`]`), &steps2)

	return []Session{
		{ID: "s1", Steps: steps1, CachedSummary: summary1},
		{ID: "s2", Steps: steps2, CachedSummary: nil},
	}
}

func TestAggregateFleetAggregatesToolsErrorsDurationsAndSummaries(t *testing.T) {
	report := AggregateFleet("antigravity-cli", 2, sampleInsightsSessions())

	if report.Flavor != "antigravity-cli" {
		t.Fatalf("Flavor = %q", report.Flavor)
	}
	if report.SessionCount != 2 || report.SampledSessions != 2 {
		t.Fatalf("counts = %d / %d", report.SessionCount, report.SampledSessions)
	}
	if report.AnalyzedSessions != 1 {
		t.Fatalf("AnalyzedSessions = %d, want 1", report.AnalyzedSessions)
	}
	if report.ToolCallTotal != 6 {
		t.Fatalf("ToolCallTotal = %d, want 6", report.ToolCallTotal)
	}
	if report.SessionsWithErrors != 1 || report.CleanSessions != 1 {
		t.Fatalf("SessionsWithErrors=%d CleanSessions=%d", report.SessionsWithErrors, report.CleanSessions)
	}
	if report.AvgToolsPerSession != 3.0 {
		t.Fatalf("AvgToolsPerSession = %v, want 3.0", report.AvgToolsPerSession)
	}
	if report.AvgDurationSeconds != 60.0 {
		t.Fatalf("AvgDurationSeconds = %v, want 60.0", report.AvgDurationSeconds)
	}
	if len(report.TopTools) == 0 || report.TopTools[0] != (NameCount{Name: "Bash", Count: 4}) {
		t.Fatalf("TopTools = %+v", report.TopTools)
	}
	if len(report.TopErrors) != 1 || report.TopErrors[0] != (NameCount{Name: "Exit code <n>: Compilation failed", Count: 1}) {
		t.Fatalf("TopErrors = %+v", report.TopErrors)
	}
	if len(report.TopIssues) != 1 || report.TopIssues[0] != (NameCount{Name: "Gradle daemon lock", Count: 1}) {
		t.Fatalf("TopIssues = %+v", report.TopIssues)
	}
	if len(report.TopRecommendations) != 1 || report.TopRecommendations[0] != (NameCount{Name: "Pin Java 25 in toolchain", Count: 1}) {
		t.Fatalf("TopRecommendations = %+v", report.TopRecommendations)
	}
}

func TestSessionsForAndInsightsServiceDrilldown(t *testing.T) {
	sessions := sampleInsightsSessions()

	byTool := SessionsFor("tool", "Bash", sessions)
	if len(byTool) != 2 || byTool[0] != (SessionRef{ID: "s1", Title: "Fix Build Failure"}) || byTool[1] != (SessionRef{ID: "s2", Title: "Add unit tests"}) {
		t.Fatalf("byTool = %+v", byTool)
	}
	byErr := SessionsFor("error", "Exit code <n>: Compilation failed", sessions)
	if len(byErr) != 1 || byErr[0].ID != "s1" {
		t.Fatalf("byErr = %+v", byErr)
	}
	byIssue := SessionsFor("issue", "Gradle daemon lock", sessions)
	if len(byIssue) != 1 || byIssue[0].ID != "s1" {
		t.Fatalf("byIssue = %+v", byIssue)
	}
	byRec := SessionsFor("recommendation", "Pin Java 25 in toolchain", sessions)
	if len(byRec) != 1 || byRec[0].ID != "s1" {
		t.Fatalf("byRec = %+v", byRec)
	}
	byWorkflow := SessionsFor("workflow", "Bash → Bash", sessions)
	if len(byWorkflow) != 1 || byWorkflow[0].ID != "s1" {
		t.Fatalf("byWorkflow = %+v", byWorkflow)
	}
	if empty := SessionsFor("unknown", "Bash", sessions); len(empty) != 0 {
		t.Fatalf("expected empty for unknown category, got %+v", empty)
	}

	collector, sessionRepo, _ := newTestInsightsEnv(t)
	_, _ = sessionRepo.Upsert(store.Session{
		Source:      "codex",
		ID:          "cx-1",
		Title:       "T",
		SourceMtime: 1000,
		StepsJSON:   `[{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Bash"}]}]`,
		ContentHash: "h1",
	})
	svc := NewInsightsService(collector)

	rep, err := svc.ForFlavor("codex")
	if err != nil {
		t.Fatalf("ForFlavor: %v", err)
	}
	if rep.SessionCount != 1 || rep.ToolCallTotal != 1 {
		t.Fatalf("unexpected rep: %+v", rep)
	}
	dd, err := svc.Drilldown("codex", "tool", "Bash")
	if err != nil {
		t.Fatalf("Drilldown: %v", err)
	}
	if dd.TotalMatches != 1 || len(dd.Sessions) != 1 || dd.Sessions[0].ID != "cx-1" {
		t.Fatalf("unexpected Drilldown: %+v", dd)
	}
}

// --- PatternMiner & MinerService Tests ---

func sampleMinerSessions() []Session {
	var steps []map[string]any
	_ = json.Unmarshal([]byte(`[`+
		`{"type":"USER_INPUT","content":"Run Playwright E2E"},`+
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Read"},{"name":"Edit"},{"name":"Bash"}]}`+
		`]`), &steps)

	var summary map[string]any
	_ = json.Unmarshal([]byte(`{"shortTitle":"Playwright E2E","summary":"Ran tests","flow":["test"],`+
		`"issues":[{"error":"Playwright browser missing","circumvention":"Ran npx playwright install"}],`+
		`"recommendations":["Install Playwright browsers in setup script"]}`), &summary)

	return []Session{
		{ID: "s1", Steps: steps, CachedSummary: summary},
		{ID: "s2", Steps: steps, CachedSummary: summary},
	}
}

func TestMinePatternsExtractsSequencesFixesAndRecommendations(t *testing.T) {
	sessions := sampleMinerSessions()
	p := MinePatterns(sessions)

	if p.AnalyzedSessions != 2 {
		t.Fatalf("AnalyzedSessions = %d, want 2", p.AnalyzedSessions)
	}
	if len(p.ToolSequences) == 0 || p.ToolSequences[0] != (NameCount{Name: "Read → Edit → Bash", Count: 2}) {
		t.Fatalf("unexpected ToolSequences: %+v", p.ToolSequences)
	}
	if len(p.FailureFixes) != 1 || p.FailureFixes[0] != (FixPair{Error: "Playwright browser missing", Fix: "Ran npx playwright install", Count: 2}) {
		t.Fatalf("unexpected FailureFixes: %+v", p.FailureFixes)
	}
	if len(p.Recommendations) != 1 || p.Recommendations[0] != (NameCount{Name: "Install Playwright browsers in setup script", Count: 2}) {
		t.Fatalf("unexpected Recommendations: %+v", p.Recommendations)
	}

	digest := BuildEvidenceDigest(p)
	for _, want := range []string{
		"<untrusted_evidence_",
		"RECURRING TOOL SEQUENCES",
		"Read → Edit → Bash (2 sessions)",
		"RECURRING FAILURE → FIX PAIRS:",
		"Error: Playwright browser missing | Fix: Ran npx playwright install (2 sessions)",
		"RECOMMENDATIONS FROM PRIOR ANALYSES:",
		"Install Playwright browsers in setup script (2 sessions)",
	} {
		if !strings.Contains(digest, want) {
			t.Fatalf("expected %q in digest:\n%s", want, digest)
		}
	}
}

func TestMinerServiceForFlavorInvokesAdvisorAndReturnsReport(t *testing.T) {
	collector, sessionRepo, summaryRepo := newTestInsightsEnv(t)
	for _, s := range sampleMinerSessions() {
		stepsBytes, _ := json.Marshal(s.Steps)
		sumBytes, _ := json.Marshal(s.CachedSummary)
		_, _ = sessionRepo.Upsert(store.Session{
			Source:      "claude-code",
			ID:          s.ID,
			Title:       "Playwright E2E",
			SourceMtime: 1000,
			StepsJSON:   string(stepsBytes),
			ContentHash: "h-" + s.ID,
		})
		_, _ = summaryRepo.Upsert("claude-code", s.ID, string(sumBytes), "Playwright E2E")
	}

	cfg := config.NewAiConfig("gemini", "test-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	advisor := ai.NewMinerAdvisorService(ai.ChatModelFunc(func(ctx context.Context, sys, usr string, schema map[string]any) (string, error) {
		p := ai.MiningProposal{
			Skills:      []ai.SkillProposal{{Name: "run-playwright", WhenToUse: "When running E2E tests", Body: "## Steps\n1. Install browsers"}},
			AgentsRules: []ai.AgentsRule{{Rule: "Run playwright install first", Rationale: "Avoids missing browser error"}},
			ToolingGaps: []string{"Add pre-test hook"},
		}
		b, _ := json.Marshal(p)
		return string(b), nil
	}))
	svc := NewMinerService(collector, advisor, cfg)

	report, err := svc.ForFlavor(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("ForFlavor: %v", err)
	}
	if report.Flavor != "claude-code" || report.SessionCount != 2 || !report.AiGenerated || report.Note != "" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.Skills) != 1 || len(report.AgentsRules) != 1 || len(report.ToolingGaps) != 1 {
		t.Fatalf("unexpected proposal: %+v", report)
	}
}

func TestMinerServiceHandlesNoEvidenceUnconfiguredAndLlmFailure(t *testing.T) {
	collector, sessionRepo, summaryRepo := newTestInsightsEnv(t)
	cfg := config.NewAiConfig("gemini", "test-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	svc := NewMinerService(collector, nil, cfg)

	emptyReport, err := svc.ForFlavor(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("ForFlavor empty: %v", err)
	}
	if emptyReport.AiGenerated || !strings.Contains(emptyReport.Note, "Not enough recurring patterns yet") {
		t.Fatalf("unexpected emptyReport: %+v", emptyReport)
	}

	for _, s := range sampleMinerSessions() {
		stepsBytes, _ := json.Marshal(s.Steps)
		sumBytes, _ := json.Marshal(s.CachedSummary)
		_, _ = sessionRepo.Upsert(store.Session{
			Source:      "claude-code",
			ID:          s.ID,
			Title:       "Playwright E2E",
			SourceMtime: 1000,
			StepsJSON:   string(stepsBytes),
			ContentHash: "h-" + s.ID,
		})
		_, _ = summaryRepo.Upsert("claude-code", s.ID, string(sumBytes), "Playwright E2E")
	}

	unconfCfg := config.NewAiConfig("gemini", "", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	unconfSvc := NewMinerService(collector, nil, unconfCfg)
	unconfReport, err := unconfSvc.ForFlavor(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("ForFlavor unconf: %v", err)
	}
	if unconfReport.AiGenerated || !strings.Contains(unconfReport.Note, "Configure an AI provider") {
		t.Fatalf("unexpected unconfReport: %+v", unconfReport)
	}

	failAdvisor := ai.NewMinerAdvisorService(ai.ChatModelFunc(func(ctx context.Context, sys, usr string, schema map[string]any) (string, error) {
		return "", errors.New("quota exceeded")
	}))
	failSvc := NewMinerService(collector, failAdvisor, cfg)
	failReport, err := failSvc.ForFlavor(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("ForFlavor fail: %v", err)
	}
	if failReport.AiGenerated || !strings.Contains(failReport.Note, "AI proposal was unavailable") || len(failReport.FailureFixes) != 1 {
		t.Fatalf("unexpected failReport: %+v", failReport)
	}
}

// --- EvalScorer & EvalService Tests ---

func sampleGoodSession() Session {
	var steps []map[string]any
	_ = json.Unmarshal([]byte(`[`+
		`{"type":"USER_INPUT","content":"Add caching to the API"},`+
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Read"},{"name":"Edit"}]},`+
		`{"type":"FUNCTION_OUTPUT","status":"ERROR","content":"Test failed"}`+
		`]`), &steps)

	var summary map[string]any
	_ = json.Unmarshal([]byte(`{`+
		`"shortTitle":"Add API Response Caching",`+
		`"summary":"Implemented an in-memory cache for the API controller and updated unit tests to verify cache hits and eviction behavior.",`+
		`"flow":["Read existing controller implementation","Added cache layer and updated tests","Verified all tests pass"],`+
		`"issues":[{"error":"Initial test failed due to stale mock","circumvention":"Reset mock state in @BeforeEach"}],`+
		`"recommendations":["Configure explicit TTL values in application.properties to avoid stale data in production"]`+
		`}`), &summary)

	return Session{ID: "good-1", Steps: steps, CachedSummary: summary}
}

func TestScoreSessionAllChecksPassForWellFormedSummary(t *testing.T) {
	s := sampleGoodSession()
	result := ScoreSession(s.ID, s.Steps, s.CachedSummary)

	if result.Score != 100 || len(result.Passed) != 6 || len(result.Failed) != 0 || result.Title != "Add API Response Caching" {
		t.Fatalf("unexpected ScoreSession result: %+v", result)
	}
}

func TestScoreSessionFlagsWeakOrDegenerateSummary(t *testing.T) {
	var steps []map[string]any
	_ = json.Unmarshal([]byte(`[`+
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Bash"}]},`+
		`{"type":"FUNCTION_OUTPUT","status":"ERROR","content":"boom"}`+
		`]`), &steps)

	var badSummary map[string]any
	_ = json.Unmarshal([]byte(`{`+
		`"shortTitle":"",`+
		`"summary":"loop loop loop loop loop loop",`+
		`"flow":[],`+
		`"issues":[],`+
		`"recommendations":[]`+
		`}`), &badSummary)

	result := ScoreSession("bad-1", steps, badSummary)
	for _, wantFail := range []string{
		"schema-complete",
		"has-recommendations",
		"not-degenerate",
		"error-coverage",
	} {
		if !slices.Contains(result.Failed, wantFail) {
			t.Fatalf("expected check %q in Failed=%v", wantFail, result.Failed)
		}
	}
}

func TestEvalServiceForFlavorAggregatesScoresAndRunsOptionalJudge(t *testing.T) {
	collector, sessionRepo, summaryRepo := newTestInsightsEnv(t)
	good := sampleGoodSession()
	stepsBytes, _ := json.Marshal(good.Steps)
	sumBytes, _ := json.Marshal(good.CachedSummary)
	_, _ = sessionRepo.Upsert(store.Session{
		Source:      "antigravity-cli",
		ID:          good.ID,
		Title:       "Good",
		SourceMtime: 2000,
		StepsJSON:   string(stepsBytes),
		ContentHash: "h1",
	})
	_, _ = summaryRepo.Upsert("antigravity-cli", good.ID, string(sumBytes), "Add API Response Caching")

	_, _ = sessionRepo.Upsert(store.Session{
		Source:      "antigravity-cli",
		ID:          "unanalyzed",
		Title:       "Unanalyzed",
		SourceMtime: 1000,
		StepsJSON:   `[]`,
		ContentHash: "h2",
	})

	cfg := config.NewAiConfig("gemini", "test-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	svc := NewEvalService(collector, cfg, nil)

	reportNoJudge, err := svc.ForFlavor(context.Background(), "antigravity-cli", false)
	if err != nil {
		t.Fatalf("ForFlavor(false): %v", err)
	}
	if reportNoJudge.ModelLabel != "gemini · gemini-2.5-flash" ||
		reportNoJudge.SessionCount != 2 ||
		reportNoJudge.SampledSessions != 2 ||
		reportNoJudge.EvaluatedSessions != 1 ||
		reportNoJudge.AvgScore != 100.0 ||
		reportNoJudge.Judge == nil ||
		reportNoJudge.Judge.Ran ||
		len(reportNoJudge.CheckPassRates) != 6 {
		t.Fatalf("unexpected reportNoJudge: %+v", reportNoJudge)
	}

	var judgeCall int32
	svc.Judge = ai.NewAnalysisJudgeService(ai.ChatModelFunc(func(ctx context.Context, sys, usr string, schema map[string]any) (string, error) {
		n := atomic.AddInt32(&judgeCall, 1)
		switch n {
		case 1:
			return `{"faithfulness":5,"actionability":4,"clarity":5,"comment":"Strictest call."}`, nil
		case 2:
			return `{"faithfulness":4,"actionability":4,"clarity":4,"comment":"Fair."}`, nil
		default:
			return `{"faithfulness":3,"actionability":4,"clarity":3,"comment":"Pragmatic."}`, nil
		}
	}))

	reportWithJudge, err := svc.ForFlavor(context.Background(), "antigravity-cli", true)
	if err != nil {
		t.Fatalf("ForFlavor(true): %v", err)
	}
	j := reportWithJudge.Judge
	if j == nil || !j.Ran || j.JudgedSessions != 1 || j.AvgFaithfulness != 4.0 || j.AvgActionability != 4.0 || j.AvgClarity != 4.0 {
		t.Fatalf("unexpected JudgeSummary: %+v", j)
	}
	if len(j.Cases) != 1 || j.Cases[0].Samples != 3 || j.Cases[0].PanelMin > j.Cases[0].PanelMax {
		t.Fatalf("unexpected JudgedCase: %+v", j.Cases)
	}
}

func TestEvalServiceHandlesJudgeFailureAndNoCandidates(t *testing.T) {
	collector, sessionRepo, summaryRepo := newTestInsightsEnv(t)
	cfg := config.NewAiConfig("gemini", "test-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	svc := NewEvalService(collector, cfg, nil)

	emptyJudge, err := svc.ForFlavor(context.Background(), "antigravity-cli", true)
	if err != nil {
		t.Fatalf("ForFlavor empty: %v", err)
	}
	if emptyJudge.Judge == nil || emptyJudge.Judge.Ran || !strings.Contains(emptyJudge.Judge.Note, "No analyzed sessions") {
		t.Fatalf("unexpected emptyJudge: %+v", emptyJudge.Judge)
	}

	good := sampleGoodSession()
	stepsBytes, _ := json.Marshal(good.Steps)
	sumBytes, _ := json.Marshal(good.CachedSummary)
	_, _ = sessionRepo.Upsert(store.Session{
		Source:      "antigravity-cli",
		ID:          good.ID,
		Title:       "Good",
		SourceMtime: 1000,
		StepsJSON:   string(stepsBytes),
		ContentHash: "h1",
	})
	_, _ = summaryRepo.Upsert("antigravity-cli", good.ID, string(sumBytes), "Add API Response Caching")

	svc.Judge = ai.NewAnalysisJudgeService(ai.ChatModelFunc(func(ctx context.Context, sys, usr string, schema map[string]any) (string, error) {
		return "", errors.New("judge offline")
	}))
	failedJudge, err := svc.ForFlavor(context.Background(), "antigravity-cli", true)
	if err != nil {
		t.Fatalf("ForFlavor failedJudge: %v", err)
	}
	if failedJudge.Judge == nil || failedJudge.Judge.Ran || !strings.Contains(failedJudge.Judge.Note, "unavailable") {
		t.Fatalf("expected unavailable Judge note, got %+v", failedJudge.Judge)
	}
}

func TestModelLabelReflectsActiveProvider(t *testing.T) {
	collector, _, _ := newTestInsightsEnv(t)

	ollamaCfg := config.NewAiConfig("ollama", "", "gemini-2.5-flash", "http://localhost:11434", "gemma3:12b")
	ollamaEval := NewEvalService(collector, ollamaCfg, nil)
	if got := ollamaEval.ModelLabel(); got != "ollama · gemma3:12b" {
		t.Fatalf("ModelLabel = %q, want ollama · gemma3:12b", got)
	}

	geminiCfg := config.NewAiConfig("gemini", "k", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	geminiEval := NewEvalService(collector, geminiCfg, nil)
	if got := geminiEval.ModelLabel(); got != "gemini · gemini-2.5-flash" {
		t.Fatalf("ModelLabel = %q, want gemini · gemini-2.5-flash", got)
	}
}

// --- OptimizeService Tests ---

func seedOptimizeSession(t *testing.T, sessionRepo *store.SessionRepository, id string) {
	t.Helper()
	steps := `[` +
		`{"type":"USER_INPUT","content":"Fix failing test"},` +
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"Bash"}]},` +
		`{"type":"FUNCTION_OUTPUT","status":"ERROR","content":"AssertionError"}` +
		`]`
	_, err := sessionRepo.Upsert(store.Session{
		Source:      "antigravity-cli",
		ID:          id,
		Title:       "Fix failing test",
		SourceMtime: 1000,
		StepsJSON:   steps,
		ContentHash: store.Sha256Hex(steps + id),
	})
	if err != nil {
		t.Fatalf("Upsert session: %v", err)
	}
}

func TestCompareScoresTwoVariants(t *testing.T) {
	collector, sessionRepo, _ := newTestInsightsEnv(t)
	seedOptimizeSession(t, sessionRepo, "s1")

	cfg := config.NewAiConfig("gemini", "test-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")

	weakA := ai.AnalysisResponse{
		ShortTitle:      "",
		Summary:         "Short",
		Flow:            []string{},
		Issues:          []ai.Issue{},
		Recommendations: []string{},
	}
	strongB := ai.AnalysisResponse{
		ShortTitle:      "Fix Failing Unit Test",
		Summary:         "Investigated the failing Gradle test suite, identified the root cause in the assertion, and updated the test fixture.",
		Flow:            []string{"Ran ./gradlew test", "Inspected failure output", "Updated test assertion"},
		Issues:          []ai.Issue{{Error: "AssertionError in test", Circumvention: "Updated expected value"}},
		Recommendations: []string{"Run ./gradlew test --info locally before pushing changes to catch assertion mismatches early"},
	}

	analyzer := ai.NewVariantAnalyzerService(ai.ChatModelFunc(func(ctx context.Context, sys, usr string, schema map[string]any) (string, error) {
		if strings.Contains(usr, "Prompt B") {
			b, _ := json.Marshal(strongB)
			return string(b), nil
		}
		b, _ := json.Marshal(weakA)
		return string(b), nil
	}))
	optSvc := NewOptimizeService(collector, analyzer, cfg)

	report, err := optSvc.Compare(context.Background(), "antigravity-cli", 2, "Prompt A", "Prompt B")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.SampleSize != 1 || report.Note != "" || report.B.AvgScore <= report.A.AvgScore || report.B.Scored != 1 || report.A.Scored != 1 {
		t.Fatalf("unexpected Compare report: %+v", report)
	}
}

func TestCompareHandlesNoSessionsAndUnconfiguredAi(t *testing.T) {
	collector, _, _ := newTestInsightsEnv(t)
	cfg := config.NewAiConfig("gemini", "test-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	optSvc := NewOptimizeService(collector, nil, cfg)

	empty, err := optSvc.Compare(context.Background(), "codex", 2, "A", "B")
	if err != nil {
		t.Fatalf("Compare empty: %v", err)
	}
	if empty.SampleSize != 0 || !strings.Contains(empty.Note, "No sessions with a transcript") {
		t.Fatalf("unexpected empty report: %+v", empty)
	}

	unconf := config.NewAiConfig("gemini", "", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b")
	unconfSvc := NewOptimizeService(collector, nil, unconf)
	unconfReport, err := unconfSvc.Compare(context.Background(), "codex", 2, "A", "B")
	if err != nil {
		t.Fatalf("Compare unconf: %v", err)
	}
	if !strings.Contains(unconfReport.Note, "Configure an AI provider") {
		t.Fatalf("unexpected unconfReport: %+v", unconfReport)
	}
}
