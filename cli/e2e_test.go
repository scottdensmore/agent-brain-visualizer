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

	appconfig "github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/pgtest"
	"github.com/scottdensmore/agent-brain-visualizer/internal/server"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
	"github.com/scottdensmore/agent-brain-visualizer/internal/testhttp"
)

func TestAgentIngestAgainstRealGoVisualizerServer(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()

	// Seed Antigravity, Codex, and Claude Code sessions on disk
	agyID := "cli-to-server-agy-1"
	agyBrainDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", agyID)
	agyLogsDir := filepath.Join(agyBrainDir, ".system_generated", "logs")
	if err := os.MkdirAll(agyLogsDir, 0o755); err != nil {
		t.Fatalf("mkdir agy: %v", err)
	}
	attachedPath := filepath.Join(agyBrainDir, "walkthrough.md")
	attachedContent := "# Walkthrough\n\nVerified CLI -> Go Server E2E.\n"
	if err := os.WriteFile(attachedPath, []byte(attachedContent), 0o644); err != nil {
		t.Fatalf("write attached file: %v", err)
	}
	agyTranscript := fmt.Sprintf("{\"type\":\"USER_INPUT\",\"content\":\"<USER_REQUEST>\\nSee walkthrough\\n</USER_REQUEST>\"}\n@[walkthrough.md] is a [File]:\n%s\n", attachedPath)
	if err := os.WriteFile(filepath.Join(agyLogsDir, "transcript.jsonl"), []byte(agyTranscript), 0o644); err != nil {
		t.Fatalf("write agy transcript: %v", err)
	}

	codexID := "rollout-2026-06-20T15-00-00-cli2srv"
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "06", "20")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("mkdir codex: %v", err)
	}
	codexTranscript := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Codex CLI E2E prompt"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, codexID+".jsonl"), []byte(codexTranscript), 0o644); err != nil {
		t.Fatalf("write codex transcript: %v", err)
	}

	claudeID := "22222222-3333-4444-5555-666666666666"
	claudeDir := filepath.Join(home, ".claude", "projects", "-Users-test-proj")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir claude: %v", err)
	}
	claudeTranscript := `{"type":"user","timestamp":"2026-06-20T16:00:00Z","message":{"role":"user","content":"Claude Code CLI E2E prompt"}}` + "\n"
	if err := os.WriteFile(filepath.Join(claudeDir, claudeID+".jsonl"), []byte(claudeTranscript), 0o644); err != nil {
		t.Fatalf("write claude transcript: %v", err)
	}

	// Start the real Go server backed by pgtest
	db, pgSrv := pgtest.NewTestDB(t)
	defer pgSrv.Close()
	store.BootstrapSchema(ctx, db)

	const ingestToken = "ingest-e2e-token"
	srv := server.New(server.Dependencies{
		Version:      "1.0.0-e2e",
		UserHome:     t.TempDir(), // empty server userHome so file preview must come from session_files DB table!
		DB:           db,
		ApiConfig:    appconfig.NewApiConfig("", false),
		IngestConfig: appconfig.NewIngestConfig(ingestToken, true),
		AiConfig:     appconfig.NewAiConfig("gemini", "dummy-key", "gemini-2.5-flash", "http://localhost:11434", "qwen3:8b"),
	})
	httpSrv := testhttp.NewServer(srv)
	defer httpSrv.Close()

	getenv := func(k string) string {
		if k == "AGENT_INGEST_TOKEN" {
			return ingestToken
		}
		return ""
	}

	type cliJSONReport struct {
		OK    bool `json:"ok"`
		Total struct {
			Ingested        int `json:"ingested"`
			Skipped         int `json:"skipped"`
			Failed          int `json:"failed"`
			SummariesSynced int `json:"summariesSynced"`
		} `json:"total"`
	}

	// 1. First CLI sync pushes all 3 sessions + attached file
	var stdout, stderr bytes.Buffer
	code := cli(ctx, []string{"--server", httpSrv.URL, "--home", home, "--json"}, &stdout, &stderr, getenv)
	if code != exitOK {
		t.Fatalf("first cli sync exit = %d, stderr = %s, stdout = %s", code, stderr.String(), stdout.String())
	}
	var report1 cliJSONReport
	if err := json.Unmarshal(stdout.Bytes(), &report1); err != nil {
		t.Fatalf("unmarshal report1: %v (%s)", err, stdout.String())
	}
	if !report1.OK || report1.Total.Ingested != 3 || report1.Total.Failed != 0 {
		t.Fatalf("unexpected report1: %+v", report1)
	}

	// 2. Second CLI sync sees all 3 in server manifest -> 3 skipped, 0 ingested
	stdout.Reset()
	stderr.Reset()
	code = cli(ctx, []string{"--server", httpSrv.URL, "--home", home, "--json"}, &stdout, &stderr, getenv)
	if code != exitOK {
		t.Fatalf("second cli sync exit = %d, stderr = %s", code, stderr.String())
	}
	var report2 cliJSONReport
	_ = json.Unmarshal(stdout.Bytes(), &report2)
	if !report2.OK || report2.Total.Skipped != 3 || report2.Total.Ingested != 0 {
		t.Fatalf("unexpected report2: %+v", report2)
	}

	// 3. Write a post-hoc summary.json on disk for agyID -> third CLI sync pushes only the standalone summary!
	postHocSummary := `{"shortTitle":"Post-Hoc CLI Summary","summary":"Synced later without re-uploading transcript."}`
	if err := os.WriteFile(filepath.Join(agyLogsDir, "summary.json"), []byte(postHocSummary), 0o644); err != nil {
		t.Fatalf("write post-hoc summary: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = cli(ctx, []string{"--server", httpSrv.URL, "--home", home, "--json"}, &stdout, &stderr, getenv)
	if code != exitOK {
		t.Fatalf("third cli sync exit = %d, stderr = %s", code, stderr.String())
	}
	var report3 cliJSONReport
	_ = json.Unmarshal(stdout.Bytes(), &report3)
	if !report3.OK || report3.Total.Skipped != 3 || report3.Total.Ingested != 0 || report3.Total.SummariesSynced != 1 {
		t.Fatalf("expected 1 standalone summary synced on third run, got %+v", report3)
	}

	// 4. Verify the server serves the attached file from session_files (even though server userHome is empty!)
	fileResp, err := http.Get(fmt.Sprintf("%s/api/brain/file?id=%s&flavor=antigravity-cli&path=%s", httpSrv.URL, agyID, url.QueryEscape(attachedPath)))
	if err != nil {
		t.Fatalf("GET /api/brain/file: %v", err)
	}
	defer fileResp.Body.Close()
	fileBytes, _ := io.ReadAll(fileResp.Body)
	if fileResp.StatusCode != http.StatusOK || string(fileBytes) != attachedContent {
		t.Fatalf("expected attached walkthrough.md from store, got %d: %q", fileResp.StatusCode, string(fileBytes))
	}

	// 5. Verify the server serves the post-hoc summary from the summary store
	sumResp, err := http.Post(fmt.Sprintf("%s/api/analysis/conversations/%s/summarize?flavor=antigravity-cli", httpSrv.URL, agyID), "application/json", nil)
	if err != nil {
		t.Fatalf("POST summarize: %v", err)
	}
	defer sumResp.Body.Close()
	sumBytes, _ := io.ReadAll(sumResp.Body)
	if !strings.Contains(string(sumBytes), "Post-Hoc CLI Summary") {
		t.Fatalf("expected Post-Hoc CLI Summary from summarize endpoint, got %s", string(sumBytes))
	}
}
