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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/pgtest"
	"github.com/scottdensmore/agent-brain-visualizer/internal/server"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
	"github.com/scottdensmore/agent-brain-visualizer/internal/testhttp"
	"github.com/scottdensmore/agent-brain-visualizer/internal/transcript"
)

func TestCLIArgsVersionAndHelp(t *testing.T) {
	// 1. --version and -v
	for _, flag := range []string{"--version", "-v"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flag}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(%q) exit = %d, stderr = %s", flag, code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "agy-brain-viz ") {
			t.Fatalf("unexpected version output: %q", stdout.String())
		}
	}

	// 2. --help and -h
	for _, flag := range []string{"--help", "-h"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flag}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(%q) exit = %d", flag, code)
		}
		if !strings.Contains(stdout.String(), "Usage: agy-brain-viz") {
			t.Fatalf("unexpected help output: %q", stdout.String())
		}
	}

	// 3. Unknown flag returns exit code 2
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--unknown-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("expected exit code 2 for unknown flag, got %d", code)
	}

	// 4. -D JVM system property compatibility flags
	opts, err := parseArgs([]string{
		"-Dmicronaut.server.port=9099",
		"-Dmicronaut.server.host=0.0.0.0",
		"-Ddotenv.enabled=false",
		"-Duser.home=/tmp/custom-home",
	})
	if err != nil {
		t.Fatalf("parseArgs failed: %v", err)
	}
	if opts.portOverride == nil || *opts.portOverride != 9099 || opts.hostOverride != "0.0.0.0" || opts.dotenvEnabled == nil || *opts.dotenvEnabled || opts.userHome != "/tmp/custom-home" {
		t.Fatalf("unexpected parsed opts: %+v", opts)
	}

	// 5. Malformed -D flag without '=' fails
	if _, err := parseArgs([]string{"-Dmissing_equals"}); err == nil {
		t.Fatalf("expected error for malformed -D flag")
	}
}

func TestEndToEndFullStackLifecycle(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()

	// --- 1. Populate multi-source agent fixtures ---
	agy1ID := "e2e-agy-session-1"
	agy1BrainDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", agy1ID)
	if err := os.MkdirAll(agy1BrainDir, 0o755); err != nil {
		t.Fatalf("mkdir agy1: %v", err)
	}
	planPath := filepath.Join(agy1BrainDir, "implementation_plan.md")
	planContent := "# Implementation Plan\n\nStep 1: Port to Go.\nStep 2: Run E2E tests.\n"
	if err := os.WriteFile(planPath, []byte(planContent), 0o644); err != nil {
		t.Fatalf("write plan.md: %v", err)
	}
	agy1Transcript := strings.Join([]string{
		fmt.Sprintf(`{"type":"USER_INPUT","content":"<USER_REQUEST>\nReview [implementation_plan.md](file://%s) and fix build\n</USER_REQUEST>"}`, planPath),
		`{"type":"PLANNER_RESPONSE","thinking":"Checking build","tool_calls":[{"name":"run_command","arguments":{"CommandLine":"go test ./..."}}]}`,
		`{"type":"TOOL_RESPONSE","status":"ERROR","content":"exit status 1: undefined: foo"}`,
		`{"type":"PLANNER_RESPONSE","thinking":"Fixing foo","tool_calls":[{"name":"replace_file_content","arguments":{"TargetFile":"foo.go"}},{"name":"run_command","arguments":{"CommandLine":"go test -v ./..."}}]}`,
		`{"type":"TOOL_RESPONSE","status":"DONE","content":"PASS"}`,
		`{"type":"PLANNER_RESPONSE","content":"Fixed the build and verified tests pass."}`,
	}, "\n") + "\n"
	agy1Summary := `{"shortTitle":"Fix Go Build","flow":["Read plan","Ran go test","Fixed foo.go","Verified tests"],"agentActions":[{"action":"run_command","description":"Ran go test"}],"issues":[{"error":"exit status 1: undefined: foo","circumvention":"Defined foo in foo.go"}],"recommendations":["Add pre-commit go vet hook"],"summary":"The agent reviewed the implementation plan, encountered a build error in foo.go, fixed the symbol, and verified all tests pass."}`

	agy2ID := "e2e-agy-session-2"
	agy2Transcript := strings.Join([]string{
		`{"type":"USER_INPUT","content":"<USER_REQUEST>\nRun the test suite and fix any failures\n</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"run_command","arguments":{"CommandLine":"go test ./..."}}]}`,
		`{"type":"TOOL_RESPONSE","status":"ERROR","content":"exit status 1: undefined: bar"}`,
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"replace_file_content","arguments":{"TargetFile":"bar.go"}},{"name":"run_command","arguments":{"CommandLine":"go test -v ./..."}}]}`,
		`{"type":"TOOL_RESPONSE","status":"DONE","content":"PASS"}`,
		`{"type":"PLANNER_RESPONSE","content":"All tests are passing now."}`,
	}, "\n") + "\n"

	// Sensitive credential file inside ~/.gemini (must be blocked by /api/brain/file with 403)
	if err := os.WriteFile(filepath.Join(home, ".gemini", "oauth_creds.json"), []byte(`{"refresh_token":"secret"}`), 0o600); err != nil {
		t.Fatalf("write oauth_creds: %v", err)
	}

	codexID := "rollout-2026-06-20T12-00-00-e2ecodex"
	codexTranscript := strings.Join([]string{
		`{"timestamp":"2026-06-20T12:00:00Z","type":"session_meta","payload":{"id":"e2ecodex","cwd":"/workspace"}}`,
		`{"timestamp":"2026-06-20T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect main.go"}]}}`,
		`{"timestamp":"2026-06-20T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call_1","arguments":"{\"cmd\":\"ls -la\"}"}}`,
		`{"timestamp":"2026-06-20T12:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"Process exited with code 0\nOutput:\nmain.go"}}`,
		`{"timestamp":"2026-06-20T12:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Found main.go in the workspace."}]}}`,
	}, "\n") + "\n"

	claudeID := "11111111-2222-3333-4444-555555555555"
	claudeTranscript := strings.Join([]string{
		`{"type":"user","timestamp":"2026-06-20T13:00:00Z","message":{"role":"user","content":"Check git status"}}`,
		`{"type":"assistant","timestamp":"2026-06-20T13:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git status"}}]}}`,
		`{"type":"user","timestamp":"2026-06-20T13:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"On branch main","is_error":false}]}}`,
		`{"type":"assistant","timestamp":"2026-06-20T13:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"The working tree is on branch main."}]}}`,
	}, "\n") + "\n"

	// --- 2. Start Mock Ollama HTTP Server (over testhttp.NewServer) ---
	ollamaServer := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		bodyStr := string(bodyBytes)
		w.Header().Set("Content-Type", "application/json")

		var replyJSON string
		switch {
		case strings.Contains(bodyStr, "whenToUse"):
			replyJSON = `{"skills":[{"name":"go-test-fix","whenToUse":"When go test fails with undefined symbol","body":"1. Run go test\n2. Fix symbol\n3. Re-run go test -v"}],"agentsRules":[{"rule":"Always run go test -v ./... after editing Go files","rationale":"Repeated in 2 sessions"}],"toolingGaps":["Automated symbol import fixer"]}`
		case strings.Contains(bodyStr, "faithfulness"):
			replyJSON = `{"faithfulness":5,"actionability":4,"clarity":5,"comment":"Accurate and actionable summary."}`
		default:
			replyJSON = `{"shortTitle":"AI Generated Title","flow":["User requested test run","Agent ran go test","Agent fixed bar.go","Verified tests pass"],"agentActions":[{"action":"run_command","description":"Executed go test"}],"issues":[{"error":"exit status 1: undefined: bar","circumvention":"Fixed bar.go"}],"recommendations":["Run go vet before testing"],"summary":"The agent ran the test suite, resolved the undefined bar symbol in bar.go, and confirmed all tests pass."}`
		}

		envelope := map[string]any{
			"message": map[string]any{
				"content": replyJSON,
			},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer ollamaServer.Close()

	// --- 3. Start Go Visualizer Server backed by PostgreSQL wire server (pgtest + pgdriver) ---
	db, pgSrv := pgtest.NewTestDB(t)
	defer pgSrv.Close()
	store.BootstrapSchema(ctx, db)

	const apiToken = "e2e-api-token-secret"
	const ingestToken = "e2e-ingest-token-secret"

	aiCfg := config.NewAiConfig("ollama", "", "", ollamaServer.URL, "qwen3:8b")
	apiCfg := config.NewApiConfig(apiToken, true)
	ingestCfg := config.NewIngestConfig(ingestToken, true)

	srv := server.New(server.Dependencies{
		Version:      "1.2.3-e2e",
		UserHome:     home,
		DB:           db,
		ApiConfig:    apiCfg,
		IngestConfig: ingestCfg,
		AiConfig:     aiCfg,
	})
	httpSrv := testhttp.NewServer(srv)
	defer httpSrv.Close()

	getRaw := func(path string, bearer string) (int, string, http.Header) {
		req, err := http.NewRequest(http.MethodGet, httpSrv.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	postRaw := func(path, body, bearer string) (int, string) {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+path, r)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// --- 4. Ingest Sessions & Attachments ---
	batch := []transcript.IngestSession{
		{
			Source:      "antigravity-cli",
			ID:          agy1ID,
			SourceMtime: 1000,
			Raw:         agy1Transcript,
			Summary:     agy1Summary,
			Files: []store.IngestFile{
				{Path: planPath, Content: planContent},
			},
		},
		{
			Source:      "antigravity-cli",
			ID:          agy2ID,
			SourceMtime: 2000,
			Raw:         agy2Transcript,
		},
		{
			Source:      "codex",
			ID:          codexID,
			SourceMtime: 3000,
			Raw:         codexTranscript,
		},
		{
			Source:      "claude-code",
			ID:          claudeID,
			SourceMtime: 4000,
			Raw:         claudeTranscript,
		},
	}
	batchBytes, _ := json.Marshal(batch)
	st, pushBody := postRaw("/api/ingest/sessions", string(batchBytes), ingestToken)
	if st != http.StatusOK {
		t.Fatalf("POST /api/ingest/sessions status = %d: %s", st, pushBody)
	}
	var pushRes transcript.IngestResult
	if err := json.Unmarshal([]byte(pushBody), &pushRes); err != nil {
		t.Fatalf("unmarshal pushRes: %v", err)
	}
	if pushRes.Ingested != 4 || pushRes.Skipped != 0 || pushRes.Failed != 0 {
		t.Fatalf("unexpected first push result: %+v", pushRes)
	}

	// Re-pushing the exact same batch is idempotent (all 4 skipped)
	st, repushBody := postRaw("/api/ingest/sessions", string(batchBytes), ingestToken)
	if st != http.StatusOK {
		t.Fatalf("repush status = %d", st)
	}
	_ = json.Unmarshal([]byte(repushBody), &pushRes)
	if pushRes.Ingested != 0 || pushRes.Skipped != 4 || pushRes.Failed != 0 {
		t.Fatalf("unexpected idempotent repush result: %+v", pushRes)
	}

	// --- 5. Verify Health & Static Assets (unauthenticated even when API_REQUIRE_AUTH=true) ---
	status, healthBody, _ := getRaw("/health", "")
	if status != http.StatusOK || !strings.Contains(healthBody, `"version":"1.2.3-e2e"`) {
		t.Fatalf("unexpected /health: %d %s", status, healthBody)
	}
	status, readyBody, _ := getRaw("/health/ready", "")
	if status != http.StatusOK || !strings.Contains(readyBody, `"db":"UP"`) {
		t.Fatalf("unexpected /health/ready: %d %s", status, readyBody)
	}
	for _, asset := range []string{"/", "/index.html", "/app.js", "/style.css", "/modules/utils.js"} {
		st, body, _ := getRaw(asset, "")
		if st != http.StatusOK || len(body) == 0 {
			t.Fatalf("static asset %s returned status %d (len=%d)", asset, st, len(body))
		}
	}

	// Unauthenticated API call is rejected with 401
	if st, _, _ := getRaw("/api/brain/conversations", ""); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /api/brain/conversations, got %d", st)
	}

	// --- 6. Verify Conversations List, Search, and Transcripts across all 3 flavors ---
	for _, tc := range []struct {
		flavor    string
		wantTotal int
		wantID    string
	}{
		{"antigravity-cli", 2, agy1ID},
		{"codex", 1, codexID},
		{"claude-code", 1, claudeID},
	} {
		st, body, _ := getRaw("/api/brain/conversations?flavor="+tc.flavor, apiToken)
		if st != http.StatusOK {
			t.Fatalf("conversations(%s) status = %d: %s", tc.flavor, st, body)
		}
		var page server.ConversationPage
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("unmarshal page(%s): %v", tc.flavor, err)
		}
		if page.Total != tc.wantTotal {
			t.Fatalf("conversations(%s) total = %d, want %d", tc.flavor, page.Total, tc.wantTotal)
		}
		if !strings.Contains(body, tc.wantID) {
			t.Fatalf("conversations(%s) missing %s: %s", tc.flavor, tc.wantID, body)
		}

		// Fetch transcript
		st, trBody, _ := getRaw(fmt.Sprintf("/api/brain/conversations/%s/transcript?flavor=%s", tc.wantID, tc.flavor), apiToken)
		if st != http.StatusOK || !strings.Contains(trBody, "USER_INPUT") {
			t.Fatalf("transcript(%s/%s) status=%d body=%s", tc.flavor, tc.wantID, st, trBody)
		}
	}

	// --- 7. Verify Attached File Preview & Security Guards ---
	st, fileBody, _ := getRaw(fmt.Sprintf("/api/brain/file?id=%s&flavor=antigravity-cli&path=%s", agy1ID, url.QueryEscape(planPath)), apiToken)
	if st != http.StatusOK || fileBody != planContent {
		t.Fatalf("expected attached plan.md content, got %d: %q", st, fileBody)
	}
	// Credential file inside ~/.gemini is blocked with 403
	credsPath := filepath.Join(home, ".gemini", "oauth_creds.json")
	if st, _, _ := getRaw("/api/brain/file?path="+url.QueryEscape(credsPath), apiToken); st != http.StatusForbidden {
		t.Fatalf("expected 403 for oauth_creds.json, got %d", st)
	}
	// Path outside ~/.gemini is blocked with 401
	if st, _, _ := getRaw("/api/brain/file?path=/etc/passwd", apiToken); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for /etc/passwd, got %d", st)
	}

	// --- 8. Verify AI Summarization (Cached & Fresh LLM Call) ---
	st, sum1Body := postRaw(fmt.Sprintf("/api/analysis/conversations/%s/summarize?flavor=antigravity-cli", agy1ID), "", apiToken)
	if st != http.StatusOK || !strings.Contains(sum1Body, "Fix Go Build") {
		t.Fatalf("expected cached summary for agy1, got %d: %s", st, sum1Body)
	}
	st, sum2Body := postRaw(fmt.Sprintf("/api/analysis/conversations/%s/summarize?flavor=antigravity-cli", agy2ID), "", apiToken)
	if st != http.StatusOK || !strings.Contains(sum2Body, "AI Generated Title") {
		t.Fatalf("expected AI generated summary for agy2, got %d: %s", st, sum2Body)
	}

	// Standalone summary manifest now lists both agy1 and agy2
	st, sumManifestBody, _ := getRaw("/api/ingest/summaries/manifest?source=antigravity-cli", ingestToken)
	if st != http.StatusOK {
		t.Fatalf("summary manifest status = %d", st)
	}
	var sumManifest map[string]string
	_ = json.Unmarshal([]byte(sumManifestBody), &sumManifest)
	if len(sumManifest) != 2 {
		t.Fatalf("expected 2 summaries in manifest, got %+v", sumManifest)
	}

	// --- 9. Verify Fleet Insights & Drilldown ---
	st, insBody, _ := getRaw("/api/insights?flavor=antigravity-cli", apiToken)
	if st != http.StatusOK || !strings.Contains(insBody, `"sessionCount":2`) || !strings.Contains(insBody, `"analyzedSessions":2`) {
		t.Fatalf("unexpected /api/insights: %d %s", st, insBody)
	}
	st, drillBody, _ := getRaw("/api/insights/sessions?flavor=antigravity-cli&category=tool&key=run_command", apiToken)
	if st != http.StatusOK || !strings.Contains(drillBody, agy1ID) || !strings.Contains(drillBody, agy2ID) {
		t.Fatalf("unexpected /api/insights/sessions: %d %s", st, drillBody)
	}

	// --- 10. Verify Pattern Miner (/api/mine) ---
	st, mineBody, _ := getRaw("/api/mine?flavor=antigravity-cli", apiToken)
	if st != http.StatusOK || !strings.Contains(mineBody, "go-test-fix") || !strings.Contains(mineBody, "run_command → replace_file_content → run_command") {
		t.Fatalf("unexpected /api/mine: %d %s", st, mineBody)
	}

	// --- 11. Verify Evaluation Harness (/api/eval & /api/eval/runs) ---
	st, evalBody, _ := getRaw("/api/eval?flavor=antigravity-cli&judge=true", apiToken)
	if st != http.StatusOK || !strings.Contains(evalBody, `"evaluatedSessions":2`) || !strings.Contains(evalBody, `"judgedSessions":2`) {
		t.Fatalf("unexpected /api/eval: %d %s", st, evalBody)
	}
	st, saveRunBody := postRaw("/api/eval/runs", evalBody, apiToken)
	if st != http.StatusOK {
		t.Fatalf("unexpected POST /api/eval/runs: %d %s", st, saveRunBody)
	}
	st, runsBody, _ := getRaw("/api/eval/runs?flavor=antigravity-cli", apiToken)
	if st != http.StatusOK {
		t.Fatalf("unexpected GET /api/eval/runs: %d %s", st, runsBody)
	}
	var snapshots []store.EvalRunSnapshot
	if err := json.Unmarshal([]byte(runsBody), &snapshots); err != nil || len(snapshots) != 1 {
		t.Fatalf("expected 1 persisted eval run snapshot, got %v (%s)", err, runsBody)
	}
	if snapshots[0].SavedAt == "" || snapshots[0].EvaluatedSessions != 2 {
		t.Fatalf("unexpected snapshot: %+v", snapshots[0])
	}

	// --- 12. Verify Prompt Optimizer (/api/optimize & POST /api/optimize) ---
	st, defBody, _ := getRaw("/api/optimize", apiToken)
	if st != http.StatusOK || !strings.Contains(defBody, "instruction") {
		t.Fatalf("unexpected GET /api/optimize: %d %s", st, defBody)
	}
	optReq, _ := json.Marshal(map[string]any{
		"flavor":          "antigravity-cli",
		"instructionA":    "Be concise.",
		"instructionB":    "Be thorough and list every action.",
		"sampleSize":      1,
		"labelA":          "Concise",
		"labelB":          "Thorough",
	})
	st, optBody := postRaw("/api/optimize", string(optReq), apiToken)
	if st != http.StatusOK || !strings.Contains(optBody, `"sampleSize":1`) || !strings.Contains(optBody, `"avgScore":100`) {
		t.Fatalf("unexpected POST /api/optimize: %d %s", st, optBody)
	}

	// --- 13. Verify Eval Run Deletion ---
	delReq, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/eval/runs?savedAt=%s", httpSrv.URL, url.QueryEscape(snapshots[0].SavedAt)), nil)
	delReq.Header.Set("Authorization", "Bearer "+apiToken)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE eval run: %v", err)
	}
	delBody, _ := io.ReadAll(delResp.Body)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK || !strings.Contains(string(delBody), `"deleted":1`) {
		t.Fatalf("expected 200 with deleted:1 on DELETE /api/eval/runs, got %d %s", delResp.StatusCode, string(delBody))
	}
	st, runsAfterDel, _ := getRaw("/api/eval/runs?flavor=antigravity-cli", apiToken)
	if st != http.StatusOK || strings.TrimSpace(runsAfterDel) != "[]" {
		t.Fatalf("expected empty eval runs after delete, got %d %s", st, runsAfterDel)
	}
}
