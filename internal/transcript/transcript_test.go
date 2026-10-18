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

package transcript

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
)

// =============================================================================
// AntigravityPathsTest
// =============================================================================

func TestDefaultFlavorIsTheAntigravityCli(t *testing.T) {
	if DefaultFlavor != "antigravity-cli" {
		t.Fatalf("got %q, want %q", DefaultFlavor, "antigravity-cli")
	}
}

func TestGeminiRootIsDotGeminiUnderTheCurrentHome(t *testing.T) {
	root := GeminiRoot()
	if filepath.Base(root) != ".gemini" {
		t.Fatalf("expected GeminiRoot to end with .gemini, got %q", root)
	}
	resolved := ResolveGeminiDir("/home/testuser")
	if resolved != filepath.Join("/home/testuser", ".gemini") {
		t.Fatalf("got %q, want %q", resolved, filepath.Join("/home/testuser", ".gemini"))
	}
}

func TestDeniesCredentialShapedPathsInTheSandbox(t *testing.T) {
	sandbox := "/home/u/.gemini"
	deniedPaths := []string{
		"oauth_creds.json",
		"google_accounts.json",
		"settings.json",
		"mcp-oauth-tokens.json",
		"OAUTH_CREDS.JSON",
		"anything-at-all.json",
		".env",
		".hidden",
		".credentials/token.json",
		"antigravity-cli/.env",
		"antigravity-cli/brain/.secret/notes.txt",
	}
	for _, rel := range deniedPaths {
		candidate := filepath.Join(sandbox, rel)
		if !IsPreviewDenied(sandbox, candidate) {
			t.Errorf("%s must not be previewable", rel)
		}
	}
}

func TestAllowsSessionDataUnderTheFlavorSubtrees(t *testing.T) {
	sandbox := "/home/u/.gemini"
	allowedPaths := []string{
		"config.txt",
		"notes.md",
		"antigravity-cli/brain/sess-1/state.json",
		"antigravity-cli/brain/sess-1/transcript.jsonl",
		"codex/brain/rollout.json",
	}
	for _, rel := range allowedPaths {
		candidate := filepath.Join(sandbox, rel)
		if IsPreviewDenied(sandbox, candidate) {
			t.Errorf("%s should still be previewable", rel)
		}
	}
}

func TestDeniesAPathThatIsNotInsideTheSandboxAtAll(t *testing.T) {
	sandbox := "/home/u/.gemini"
	if !IsPreviewDenied(sandbox, "/etc/passwd") {
		t.Fatal("a path outside the sandbox has no business being previewable")
	}
}

func TestTreatsTheDotJsonRuleAsRootOnly(t *testing.T) {
	sandbox := "/home/u/.gemini"
	if !IsPreviewDenied(sandbox, filepath.Join(sandbox, "creds.json")) {
		t.Fatal("creds.json at root must be denied")
	}
	if IsPreviewDenied(sandbox, filepath.Join(sandbox, "sub/creds.json")) {
		t.Fatal("sub/creds.json must be allowed")
	}
}

// =============================================================================
// TranscriptParserTest
// =============================================================================

func TestDeduplicateCollapsesConsecutiveDuplicates(t *testing.T) {
	got := DeduplicateSequence([]string{"a", "a", "a", "b"})
	want := []string{"a (repeated 3 times)", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeduplicateLeavesNonConsecutiveDuplicatesIntact(t *testing.T) {
	got := DeduplicateSequence([]string{"a", "b", "a"})
	want := []string{"a", "b", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeduplicateHandlesTrailingRun(t *testing.T) {
	got := DeduplicateSequence([]string{"x", "y", "y"})
	want := []string{"x", "y (repeated 2 times)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeduplicateReturnsEmptyForEmpty(t *testing.T) {
	got := DeduplicateSequence([]string{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestParseSequencesEmitsNothingForEmptyInput(t *testing.T) {
	got := ParseSequences(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty sequences, got %v", got)
	}
}

func TestParseSequencesStartsNewSequenceOnEachUserInput(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"first"}`,
		`{"type":"USER_INPUT","content":"second"}`,
	}
	seqs := ParseSequences(lines)
	if len(seqs) != 2 {
		t.Fatalf("expected 2 sequences, got %d", len(seqs))
	}
	if seqs[0][0] != "USER REQUEST: first" {
		t.Errorf("got %q, want %q", seqs[0][0], "USER REQUEST: first")
	}
	if seqs[1][0] != "USER REQUEST: second" {
		t.Errorf("got %q, want %q", seqs[1][0], "USER REQUEST: second")
	}
}

func TestParseSequencesTreatsUserExplicitSourceAsUserInput(t *testing.T) {
	lines := []string{`{"source":"USER_EXPLICIT","content":"hello"}`}
	seqs := ParseSequences(lines)
	if len(seqs) != 1 || seqs[0][0] != "USER REQUEST: hello" {
		t.Fatalf("unexpected sequences: %v", seqs)
	}
}

func TestParseSequencesTruncatesLongUserContentTo2000Chars(t *testing.T) {
	longContent := strings.Repeat("x", 2500)
	lines := []string{`{"type":"USER_INPUT","content":"` + longContent + `"}`}
	seqs := ParseSequences(lines)
	want := "USER REQUEST: " + strings.Repeat("x", 2000)
	if seqs[0][0] != want {
		t.Fatalf("got length %d, want length %d", len(seqs[0][0]), len(want))
	}
}

func TestParseSequencesExtractsAgentToolCalls(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"do it"}`,
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"edit","arguments":{"toolAction":"write","TargetFile":"/a.txt"}}]}`,
	}
	seqs := ParseSequences(lines)
	if len(seqs) != 1 || len(seqs[0]) != 2 {
		t.Fatalf("unexpected sequences: %v", seqs)
	}
	if seqs[0][1] != "AGENT ACTION: [edit] write -> /a.txt" {
		t.Fatalf("got %q", seqs[0][1])
	}
}

func TestParseSequencesFallsBackToCommandLineWhenNoTargetFile(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"do it"}`,
		`{"source":"MODEL","tool_calls":[{"name":"run","arguments":{"toolAction":"exec","CommandLine":"ls -la"}}]}`,
	}
	seqs := ParseSequences(lines)
	if seqs[0][1] != "AGENT ACTION: [run] exec -> ls -la" {
		t.Fatalf("got %q", seqs[0][1])
	}
}

func TestParseSequencesCapturesErrorEvents(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"go"}`,
		`{"type":"ERROR_MESSAGE","error":"boom","content":"NullPointerException here"}`,
	}
	seqs := ParseSequences(lines)
	if seqs[0][1] != "SYSTEM EVENT/ERROR: NullPointerException here" {
		t.Fatalf("got %q", seqs[0][1])
	}
}

func TestParseSequencesSkipsBlankAndMalformedLines(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"go"}`,
		"",
		"   ",
		"this is not json",
	}
	seqs := ParseSequences(lines)
	if len(seqs) != 1 || len(seqs[0]) != 1 {
		t.Fatalf("unexpected sequences: %v", seqs)
	}
}

func TestParseSequencesIgnoresAgentResponseWhenToolCallsNotAnArray(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"go"}`,
		`{"type":"PLANNER_RESPONSE","tool_calls":"not-an-array"}`,
	}
	seqs := ParseSequences(lines)
	if len(seqs) != 1 || len(seqs[0]) != 1 {
		t.Fatalf("unexpected sequences: %v", seqs)
	}
}

func TestParseSequencesIgnoresUnrelatedLines(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"go"}`,
		`{"type":"SOMETHING_ELSE","source":"SYSTEM","content":"noise"}`,
	}
	seqs := ParseSequences(lines)
	if len(seqs) != 1 || len(seqs[0]) != 1 {
		t.Fatalf("unexpected sequences: %v", seqs)
	}
}

func TestParseSequencesDeduplicatesWithinASequence(t *testing.T) {
	lines := []string{
		`{"type":"USER_INPUT","content":"go"}`,
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"run","arguments":{"CommandLine":"ls"}}]}`,
		`{"type":"PLANNER_RESPONSE","tool_calls":[{"name":"run","arguments":{"CommandLine":"ls"}}]}`,
	}
	seqs := ParseSequences(lines)
	if seqs[0][1] != "AGENT ACTION: [run]  -> ls (repeated 2 times)" {
		t.Fatalf("got %q", seqs[0][1])
	}
}

func TestSplitKeepsSingleChunkWhenWithinBudget(t *testing.T) {
	chunks := SplitIntoSafeChunks([]string{"a", "b", "c"}, func(string) int { return 5 }, 100)
	if len(chunks) != 1 || !reflect.DeepEqual(chunks[0], []string{"a", "b", "c"}) {
		t.Fatalf("unexpected chunks: %v", chunks)
	}
}

func TestSplitDividesWhenOverBudget(t *testing.T) {
	counter := func(s string) int {
		if strings.Contains(s, "\n") {
			return 1000
		}
		return 1
	}
	chunks := SplitIntoSafeChunks([]string{"a", "b", "c", "d"}, counter, 10)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) != 1 {
			t.Fatalf("expected chunk of size 1, got %v", chunk)
		}
	}
}

func TestSplitNeverDividesASingleLineEvenIfOverBudget(t *testing.T) {
	chunks := SplitIntoSafeChunks([]string{"huge"}, func(string) int { return 1_000_000 }, 10)
	if len(chunks) != 1 || !reflect.DeepEqual(chunks[0], []string{"huge"}) {
		t.Fatalf("unexpected chunks: %v", chunks)
	}
}

func TestSplitUsesLengthHeuristicWhenCounterPanics(t *testing.T) {
	counter := func(string) int {
		panic("estimator unavailable")
	}
	lines := []string{strings.Repeat("a", 20), strings.Repeat("b", 20)}
	chunks := SplitIntoSafeChunks(lines, counter, 5)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
}

// =============================================================================
// ClaudeCodeAdapterTest
// =============================================================================

const (
	claudeSummaryLine = `{"type":"summary","summary":"Refactor the parser","leafUuid":"x"}`
	claudeUserString  = `{"type":"user","timestamp":"2026-06-07T00:44:40.324Z","message":{"role":"user","content":"Fix the bug"}}`
	claudeAssistant   = `{"type":"assistant","timestamp":"2026-06-07T00:44:45.000Z","message":{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"Let me think","signature":"s"},` +
		`{"type":"text","text":"On it."},` +
		`{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/a.txt"}}]}}`
	claudeToolResultOK = `{"type":"user","timestamp":"2026-06-07T00:44:46.000Z","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents","is_error":false}]}}`
	claudeToolResultErr = `{"type":"user","timestamp":"2026-06-07T00:44:47.000Z","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_2","content":"boom","is_error":true}]}}`
)

func TestClaudeMapsUserStringToUserInput(t *testing.T) {
	steps := ClaudeCodeToTimelineSteps([]string{claudeUserString})
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
	s := steps[0]
	if s["type"] != "USER_INPUT" || s["source"] != "USER_EXPLICIT" || s["content"] != "Fix the bug" || s["created_at"] != "2026-06-07T00:44:40.324Z" {
		t.Fatalf("unexpected step: %#v", s)
	}
}

func TestClaudeExpandsAssistantBlocksInOrder(t *testing.T) {
	s := ClaudeCodeToTimelineSteps([]string{claudeAssistant})
	if len(s) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(s))
	}
	if s[0]["type"] != "REASONING" || s[0]["thinking"] != "Let me think" {
		t.Fatalf("unexpected reasoning step: %#v", s[0])
	}
	if s[1]["type"] != "MESSAGE" || s[1]["content"] != "On it." {
		t.Fatalf("unexpected message step: %#v", s[1])
	}
	if s[2]["type"] != "FUNCTION_CALL" {
		t.Fatalf("unexpected function call step: %#v", s[2])
	}
	tools := extractToolCalls(s[2])
	if len(tools) != 1 || tools[0]["name"] != "Read" {
		t.Fatalf("unexpected tool calls: %#v", tools)
	}
	args, _ := tools[0]["args"].(map[string]any)
	if args["file_path"] != "/a.txt" {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestClaudeMapsToolResultToFunctionOutput(t *testing.T) {
	s := ClaudeCodeToTimelineSteps([]string{claudeToolResultOK})[0]
	if s["type"] != "FUNCTION_OUTPUT" || s["source"] != "TOOL" || s["content"] != "file contents" {
		t.Fatalf("unexpected step: %#v", s)
	}
	if _, hasStatus := s["status"]; hasStatus {
		t.Fatalf("expected no status on ok tool result, got %#v", s["status"])
	}
}

func TestClaudeFlagsToolResultErrorAsError(t *testing.T) {
	s := ClaudeCodeToTimelineSteps([]string{claudeToolResultErr})[0]
	if s["status"] != "ERROR" {
		t.Fatalf("expected status ERROR, got %#v", s["status"])
	}
}

func TestClaudeSkipsNonConversationLines(t *testing.T) {
	attachment := `{"type":"attachment","timestamp":"t","attachment":{}}`
	system := `{"type":"system","timestamp":"t"}`
	steps := ClaudeCodeToTimelineSteps([]string{claudeSummaryLine, attachment, system})
	if len(steps) != 0 {
		t.Fatalf("expected 0 steps, got %d", len(steps))
	}
}

func TestClaudePreservesOrderAcrossLines(t *testing.T) {
	s := ClaudeCodeToTimelineSteps([]string{claudeUserString, claudeAssistant, claudeToolResultOK})
	if len(s) != 5 {
		t.Fatalf("expected 5 steps, got %d", len(s))
	}
	if s[0]["type"] != "USER_INPUT" || s[4]["type"] != "FUNCTION_OUTPUT" {
		t.Fatalf("unexpected step order: %#v", s)
	}
}

func TestClaudeSkipsMalformedLines(t *testing.T) {
	s := ClaudeCodeToTimelineSteps([]string{"not json", "", claudeUserString})
	if len(s) != 1 {
		t.Fatalf("expected 1 step, got %d", len(s))
	}
}

func TestClaudeDerivesSummaryFromSummaryLine(t *testing.T) {
	got, ok := ClaudeCodeDeriveShortTitle([]string{claudeSummaryLine, claudeUserString})
	if !ok || got != "Refactor the parser" {
		t.Fatalf("got (%q, %v), want (\"Refactor the parser\", true)", got, ok)
	}
}

func TestClaudeDerivesSummaryFromFirstUserPromptWhenNoSummaryLine(t *testing.T) {
	got, ok := ClaudeCodeDeriveShortTitle([]string{claudeAssistant, claudeUserString})
	if !ok || got != "Fix the bug" {
		t.Fatalf("got (%q, %v), want (\"Fix the bug\", true)", got, ok)
	}
}

func TestClaudeBuildsAnalysisSequences(t *testing.T) {
	seqs := ClaudeCodeToAnalysisSequences([]string{
		claudeUserString,
		claudeAssistant,
		claudeToolResultOK,
		claudeToolResultErr,
	})
	if len(seqs) != 1 {
		t.Fatalf("expected 1 sequence, got %d", len(seqs))
	}
	lines := seqs[0]
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "USER REQUEST: Fix the bug") {
		t.Errorf("missing user request: %s", joined)
	}
	if !strings.Contains(joined, "ASSISTANT: On it.") {
		t.Errorf("missing assistant message: %s", joined)
	}
	if !strings.Contains(joined, "AGENT ACTION: [Read] /a.txt") {
		t.Errorf("missing agent action: %s", joined)
	}
	if !strings.Contains(joined, "SYSTEM EVENT/ERROR:") {
		t.Errorf("missing error event: %s", joined)
	}
	if strings.Contains(joined, "file contents") {
		t.Errorf("successful tool output should be omitted: %s", joined)
	}
}

func TestClaudeProducesValidJsonArray(t *testing.T) {
	norm := ClaudeCodeNormalizer{}
	rawJSON := norm.ToStepsJSON([]string{claudeUserString, claudeAssistant})
	var arr []any
	if err := json.Unmarshal([]byte(rawJSON), &arr); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(arr) != 4 {
		t.Fatalf("expected 4 items, got %d", len(arr))
	}
}

// =============================================================================
// CodexAdapterTest
// =============================================================================

const (
	codexSessionMeta      = `{"type":"session_meta","timestamp":"2026-06-20T19:48:24.520Z","payload":{"id":"abc","cwd":"/repo"}}`
	codexUserMsg          = `{"type":"response_item","timestamp":"2026-06-20T19:51:13.373Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the bug"}]}}`
	codexAssistantMsg     = `{"type":"response_item","timestamp":"2026-06-20T19:51:21.937Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"On it."}]}}`
	codexFunctionCall     = `{"type":"response_item","timestamp":"2026-06-20T19:51:22.000Z","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status\"}","call_id":"call_1"}}`
	codexFunctionOutputOK = `{"type":"response_item","timestamp":"2026-06-20T19:51:22.100Z","payload":{"type":"function_call_output","call_id":"call_1","output":"Process exited with code 0\nOutput:\nclean"}}`
	codexFunctionOutputFl = `{"type":"response_item","timestamp":"2026-06-20T19:51:23.000Z","payload":{"type":"function_call_output","call_id":"call_2","output":"Process exited with code 1\nboom"}}`
	codexEventUserMessage = `{"type":"event_msg","timestamp":"2026-06-20T19:51:13.537Z","payload":{"type":"user_message","message":"Review the branch diff"}}`
)

func TestCodexMapsUserMessageToUserInput(t *testing.T) {
	s := CodexToTimelineSteps([]string{codexUserMsg})[0]
	if s["type"] != "USER_INPUT" || s["source"] != "USER_EXPLICIT" || s["content"] != "Fix the bug" || s["created_at"] != "2026-06-20T19:51:13.373Z" {
		t.Fatalf("unexpected step: %#v", s)
	}
}

func TestCodexMapsAssistantMessageToModelMessage(t *testing.T) {
	s := CodexToTimelineSteps([]string{codexAssistantMsg})[0]
	if s["type"] != "MESSAGE" || s["source"] != "MODEL" || s["content"] != "On it." {
		t.Fatalf("unexpected step: %#v", s)
	}
}

func TestCodexMapsFunctionCallToToolCallWithParsedArgs(t *testing.T) {
	s := CodexToTimelineSteps([]string{codexFunctionCall})[0]
	if s["type"] != "FUNCTION_CALL" || s["source"] != "MODEL" {
		t.Fatalf("unexpected step: %#v", s)
	}
	tool := extractToolCalls(s)[0]
	if tool["name"] != "exec_command" {
		t.Fatalf("unexpected tool name: %#v", tool["name"])
	}
	args, _ := tool["args"].(map[string]any)
	if args["cmd"] != "git status" {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestCodexMapsFunctionOutputToToolOutput(t *testing.T) {
	s := CodexToTimelineSteps([]string{codexFunctionOutputOK})[0]
	if s["type"] != "FUNCTION_OUTPUT" || s["source"] != "TOOL" {
		t.Fatalf("unexpected step: %#v", s)
	}
	if !strings.Contains(s["content"].(string), "clean") {
		t.Fatalf("expected content to contain clean, got %q", s["content"])
	}
	if _, hasStatus := s["status"]; hasStatus {
		t.Fatal("a successful command must not be flagged as an error")
	}
}

func TestCodexFlagsNonZeroExitCodeAsError(t *testing.T) {
	s := CodexToTimelineSteps([]string{codexFunctionOutputFl})[0]
	if s["status"] != "ERROR" {
		t.Fatalf("expected status ERROR, got %#v", s["status"])
	}
}

func TestCodexDoesNotFlagErrorWhenOutputMerelyEchoesTheExitPhrase(t *testing.T) {
	echoed := `{"type":"response_item","timestamp":"t","payload":{"type":"function_call_output","call_id":"c","output":"log: see 'Process exited with code 1' in docs\nProcess exited with code 0"}}`
	s := CodexToTimelineSteps([]string{echoed})[0]
	if _, hasStatus := s["status"]; hasStatus {
		t.Fatalf("unexpected error status: %#v", s["status"])
	}
}

func TestCodexStringifiesObjectFunctionOutput(t *testing.T) {
	objOutput := `{"type":"response_item","timestamp":"t","payload":{"type":"function_call_output","call_id":"c","output":{"stdout":"ok"}}}`
	s := CodexToTimelineSteps([]string{objOutput})[0]
	content := s["content"].(string)
	if !strings.Contains(content, "stdout") || !strings.Contains(content, "ok") {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestCodexSkipsSessionMetaAndEmptyReasoning(t *testing.T) {
	emptyReasoning := `{"type":"response_item","timestamp":"t","payload":{"type":"reasoning","summary":[]}}`
	steps := CodexToTimelineSteps([]string{codexSessionMeta, emptyReasoning, codexEventUserMessage})
	if len(steps) != 0 {
		t.Fatalf("meta, empty reasoning, and event_msg are not timeline steps: %#v", steps)
	}
}

func TestCodexEmitsReasoningStepWhenSummaryHasText(t *testing.T) {
	reasoning := `{"type":"response_item","timestamp":"t","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"Thinking hard"}]}}`
	s := CodexToTimelineSteps([]string{reasoning})[0]
	if s["type"] != "REASONING" || s["thinking"] != "Thinking hard" {
		t.Fatalf("unexpected reasoning step: %#v", s)
	}
}

func TestCodexPreservesOrderAcrossMixedLines(t *testing.T) {
	steps := CodexToTimelineSteps([]string{
		codexSessionMeta,
		codexUserMsg,
		codexAssistantMsg,
		codexFunctionCall,
		codexFunctionOutputOK,
	})
	if len(steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(steps))
	}
	if steps[0]["type"] != "USER_INPUT" ||
		steps[1]["type"] != "MESSAGE" ||
		steps[2]["type"] != "FUNCTION_CALL" ||
		steps[3]["type"] != "FUNCTION_OUTPUT" {
		t.Fatalf("unexpected order: %#v", steps)
	}
}

func TestCodexSkipsMalformedLines(t *testing.T) {
	steps := CodexToTimelineSteps([]string{"not json", "", codexUserMsg})
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
}

func TestCodexDerivesSummaryFromCleanUserMessageEvent(t *testing.T) {
	got, ok := CodexDeriveShortTitle([]string{codexUserMsg, codexEventUserMessage})
	if !ok || got != "Review the branch diff" {
		t.Fatalf("got (%q, %v), want (\"Review the branch diff\", true)", got, ok)
	}
}

func TestCodexDerivesSummaryFromUserResponseItemWhenNoEvent(t *testing.T) {
	got, ok := CodexDeriveShortTitle([]string{codexUserMsg})
	if !ok || got != "Fix the bug" {
		t.Fatalf("got (%q, %v), want (\"Fix the bug\", true)", got, ok)
	}
}

func TestCodexBuildsAnalysisSequencesGroupedByUserInput(t *testing.T) {
	seqs := CodexToAnalysisSequences([]string{
		codexUserMsg,
		codexAssistantMsg,
		codexFunctionCall,
		codexFunctionOutputOK,
		codexFunctionOutputFl,
	})
	if len(seqs) != 1 {
		t.Fatalf("expected 1 sequence, got %d", len(seqs))
	}
	lines := seqs[0]
	joined := strings.Join(lines, "\n")
	if !strings.HasPrefix(lines[0], "USER REQUEST: Fix the bug") {
		t.Errorf("unexpected first line: %q", lines[0])
	}
	if !strings.Contains(joined, "ASSISTANT: On it.") {
		t.Errorf("missing assistant message: %s", joined)
	}
	if !strings.Contains(joined, "AGENT ACTION: [exec_command] git status") {
		t.Errorf("missing agent action: %s", joined)
	}
	if !strings.Contains(joined, "SYSTEM EVENT/ERROR:") {
		t.Errorf("missing error event: %s", joined)
	}
	if strings.Contains(joined, "clean") {
		t.Errorf("successful output should be omitted: %s", joined)
	}
}

func TestCodexAnalysisLineFallsBackToFullArgsWhenNoKnownField(t *testing.T) {
	fc := `{"type":"response_item","timestamp":"t","payload":{"type":"function_call","name":"grep","arguments":"{\"pattern\":\"abc\"}","call_id":"c"}}`
	seqs := CodexToAnalysisSequences([]string{codexUserMsg, fc})
	if !strings.Contains(seqs[0][1], "pattern") {
		t.Fatalf("expected args fallback to contain 'pattern', got %q", seqs[0][1])
	}
}

func TestCodexAnalysisSequencesSplitOnEachUserInput(t *testing.T) {
	secondUser := `{"type":"response_item","timestamp":"t","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}}`
	seqs := CodexToAnalysisSequences([]string{codexUserMsg, codexAssistantMsg, secondUser})
	if len(seqs) != 2 {
		t.Fatalf("expected 2 sequences, got %d", len(seqs))
	}
}

func TestCodexProducesValidJsonArray(t *testing.T) {
	norm := CodexNormalizer{}
	rawJSON := norm.ToStepsJSON([]string{codexUserMsg, codexFunctionCall})
	var arr []any
	if err := json.Unmarshal([]byte(rawJSON), &arr); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 items, got %d", len(arr))
	}
}

// =============================================================================
// NormalizedStepsTest
// =============================================================================

func TestNormalizedStepsKeepsFinalAssistantMessageAndBudgetsEarlierOnes(t *testing.T) {
	steps := []map[string]any{
		{"type": "USER_INPUT", "source": "USER_EXPLICIT", "content": "go"},
	}
	for i := 0; i < 9; i++ {
		steps = append(steps, map[string]any{
			"type":    "MESSAGE",
			"source":  "MODEL",
			"content": fmt.Sprintf("MSG%d %s", i, strings.Repeat("x", 600)),
		})
	}
	steps = append(steps, map[string]any{
		"type":    "MESSAGE",
		"source":  "MODEL",
		"content": "FINAL outcome message",
	})

	seqs := ToAnalysisSequences(steps)
	var flat []string
	for _, seq := range seqs {
		flat = append(flat, seq...)
	}
	all := strings.Join(flat, "\n")

	if !strings.Contains(all, "FINAL outcome message") {
		t.Errorf("final message must be kept: %s", all)
	}
	if !strings.Contains(all, "MSG0") || !strings.Contains(all, "MSG3") {
		t.Errorf("early messages within budget must be kept: %s", all)
	}
	if strings.Contains(all, "MSG4") || strings.Contains(all, "MSG8") {
		t.Errorf("messages past budget must be dropped: %s", all)
	}
}

func TestNormalizedStepsIncludesTheLoneAssistantMessageAsFinal(t *testing.T) {
	steps := []map[string]any{
		{"type": "USER_INPUT", "source": "USER_EXPLICIT", "content": "hi"},
		{"type": "MESSAGE", "source": "MODEL", "content": "only message"},
	}
	seqs := ToAnalysisSequences(steps)
	var flat []string
	for _, seq := range seqs {
		flat = append(flat, seq...)
	}
	all := strings.Join(flat, "\n")
	if !strings.Contains(all, "ASSISTANT: only message") {
		t.Fatalf("expected ASSISTANT: only message, got %q", all)
	}
}

// =============================================================================
// AnalysisSequencesTest
// =============================================================================

var antigravityTestLines = []string{
	`{"type":"USER_INPUT","content":"fix the parser","created_at":"2026-06-19T10:00:00Z"}`,
	`{"type":"PLANNER_RESPONSE","source":"MODEL","tool_calls":[{"name":"edit_file","arguments":{"toolAction":"edit","TargetFile":"parser.js"}}]}`,
	`{"type":"ERROR_MESSAGE","content":"Build failed: NullPointerException in parser.js"}`,
}

func jsonArrayOf(lines []string) string {
	return "[" + strings.Join(lines, ",") + "]"
}

func TestAntigravityMatchesTheLegacyTranscriptParserExactly(t *testing.T) {
	legacy := ParseSequences(antigravityTestLines)
	viaStore := AnalysisSequencesFromStepsJSON("antigravity-cli", jsonArrayOf(antigravityTestLines))
	if !reflect.DeepEqual(legacy, viaStore) {
		t.Fatalf("got %v, want %v", viaStore, legacy)
	}
}

func TestAntigravityKeepsAgentActionsThatNormalizedStepsWouldDrop(t *testing.T) {
	seqs := AnalysisSequencesFromStepsJSON("antigravity-cli", jsonArrayOf(antigravityTestLines))
	var flat []string
	for _, s := range seqs {
		flat = append(flat, s...)
	}
	joined := strings.Join(flat, "\n")
	if !strings.Contains(joined, "AGENT ACTION") {
		t.Errorf("Antigravity tool calls must survive: %s", joined)
	}
	if !strings.Contains(joined, "SYSTEM EVENT/ERROR") {
		t.Errorf("Antigravity errors must survive: %s", joined)
	}
}

func TestAntigravityIdeAlsoUsesTheAntigravityPath(t *testing.T) {
	ide := AnalysisSequencesFromStepsJSON("antigravity-ide", jsonArrayOf(antigravityTestLines))
	want := ParseSequences(antigravityTestLines)
	if !reflect.DeepEqual(ide, want) {
		t.Fatalf("got %v, want %v", ide, want)
	}
}

func TestNonAntigravitySourcesUseNormalizedSteps(t *testing.T) {
	normalized := `[` +
		`{"type":"USER_INPUT","content":"add a test"},` +
		`{"type":"FUNCTION_CALL","source":"MODEL","tool_calls":[{"name":"bash","args":{"command":"go test"}}]}` +
		`]`
	viaHelper := AnalysisSequencesFromStepsJSON("codex", normalized)
	var flat []string
	for _, s := range viaHelper {
		flat = append(flat, s...)
	}
	joined := strings.Join(flat, "\n")
	if !strings.Contains(joined, "USER REQUEST") {
		t.Errorf("missing USER REQUEST: %s", joined)
	}
	if !strings.Contains(joined, "AGENT ACTION: [bash]") {
		t.Errorf("missing AGENT ACTION: [bash]: %s", joined)
	}
}

func TestMalformedOrEmptyStepsYieldNoSequences(t *testing.T) {
	if len(AnalysisSequencesFromStepsJSON("codex", "not json")) != 0 {
		t.Fatal("expected empty sequences for invalid json")
	}
	if len(AnalysisSequencesFromStepsJSON("antigravity-cli", "[]")) != 0 {
		t.Fatal("expected empty sequences for empty array")
	}
}

// =============================================================================
// In-Memory SQL Driver for IngestorTest
// =============================================================================

type memSessionRow struct {
	source      string
	id          string
	title       string
	stepsJSON   string
	contentHash string
	sourceMtime int64
}

type memSummaryRow struct {
	source      string
	sessionID   string
	summaryJSON string
	shortTitle  string
	contentHash string
}

type memStoreState struct {
	mu        sync.Mutex
	sessions  map[string]memSessionRow
	summaries map[string]memSummaryRow
	files     map[string]string
	failExec  error
}

func newMemStoreState() *memStoreState {
	return &memStoreState{
		sessions:  make(map[string]memSessionRow),
		summaries: make(map[string]memSummaryRow),
		files:     make(map[string]string),
	}
}

var (
	memDriverOnce sync.Once
	memStates     sync.Map // dsn -> *memStoreState
)

type memDriver struct{}

func (memDriver) Open(name string) (driver.Conn, error) {
	v, ok := memStates.Load(name)
	if !ok {
		return nil, errors.New("unknown dsn: " + name)
	}
	return &memConn{state: v.(*memStoreState)}, nil
}

type memConn struct {
	state *memStoreState
}

func (c *memConn) Prepare(query string) (driver.Stmt, error) {
	return &memStmt{conn: c, query: query}, nil
}

func (c *memConn) Close() error { return nil }

func (c *memConn) Begin() (driver.Tx, error) {
	c.state.mu.Lock()
	err := c.state.failExec
	c.state.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &memTx{}, nil
}

type memTx struct{}

func (memTx) Commit() error   { return nil }
func (memTx) Rollback() error { return nil }

type memStmt struct {
	conn  *memConn
	query string
}

func (s *memStmt) Close() error  { return nil }
func (s *memStmt) NumInput() int { return -1 }

func (s *memStmt) Exec(args []driver.Value) (driver.Result, error) {
	nArgs := make([]driver.NamedValue, len(args))
	for i, v := range args {
		nArgs[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.conn.ExecContext(context.Background(), s.query, nArgs)
}

func (s *memStmt) Query(args []driver.Value) (driver.Rows, error) {
	nArgs := make([]driver.NamedValue, len(args))
	for i, v := range args {
		nArgs[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.conn.QueryContext(context.Background(), s.query, nArgs)
}

func (c *memConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	if c.state.failExec != nil {
		return nil, c.state.failExec
	}

	switch {
	case strings.Contains(query, "INSERT INTO sessions"):
		source := args[0].Value.(string)
		id := args[1].Value.(string)
		title := args[2].Value.(string)
		stepsJSON := args[4].Value.(string)
		contentHash := args[5].Value.(string)
		sourceMtime := args[6].Value.(int64)
		if !isValidJSON(stepsJSON) {
			return nil, errors.New("invalid input syntax for type json")
		}
		key := source + "\x00" + id
		if prev, ok := c.state.sessions[key]; ok && prev.contentHash == contentHash {
			return driver.RowsAffected(0), nil
		}
		c.state.sessions[key] = memSessionRow{
			source:      source,
			id:          id,
			title:       title,
			stepsJSON:   stepsJSON,
			contentHash: contentHash,
			sourceMtime: sourceMtime,
		}
		return driver.RowsAffected(1), nil

	case strings.Contains(query, "INSERT INTO summaries"):
		source := args[0].Value.(string)
		sessionID := args[1].Value.(string)
		summaryJSON := args[2].Value.(string)
		var shortTitle string
		if s, ok := args[3].Value.(string); ok {
			shortTitle = s
		}
		contentHash := args[4].Value.(string)
		if !isValidJSON(summaryJSON) {
			return nil, errors.New("invalid input syntax for type json")
		}
		key := source + "\x00" + sessionID
		if prev, ok := c.state.summaries[key]; ok && prev.contentHash == contentHash {
			return driver.RowsAffected(0), nil
		}
		if prev, ok := c.state.summaries[key]; ok && shortTitle == "" {
			shortTitle = prev.shortTitle
		}
		c.state.summaries[key] = memSummaryRow{
			source:      source,
			sessionID:   sessionID,
			summaryJSON: summaryJSON,
			shortTitle:  shortTitle,
			contentHash: contentHash,
		}
		return driver.RowsAffected(1), nil

	case strings.Contains(query, "DELETE FROM session_files"):
		source := args[0].Value.(string)
		sessionID := args[1].Value.(string)
		prefix := source + "\x00" + sessionID + "\x00"
		var deleted int64
		for k := range c.state.files {
			if strings.HasPrefix(k, prefix) {
				delete(c.state.files, k)
				deleted++
			}
		}
		return driver.RowsAffected(deleted), nil

	case strings.Contains(query, "INSERT INTO session_files"):
		source := args[0].Value.(string)
		sessionID := args[1].Value.(string)
		path := args[2].Value.(string)
		content := args[3].Value.(string)
		key := source + "\x00" + sessionID + "\x00" + path
		c.state.files[key] = content
		return driver.RowsAffected(1), nil

	default:
		return nil, fmt.Errorf("unsupported exec query: %s", query)
	}
}

func (c *memConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	if c.state.failExec != nil {
		return nil, c.state.failExec
	}

	switch {
	case strings.Contains(query, "SELECT steps FROM sessions"):
		source := args[0].Value.(string)
		id := args[1].Value.(string)
		key := source + "\x00" + id
		row, ok := c.state.sessions[key]
		if !ok {
			return &memRows{cols: []string{"steps"}}, nil
		}
		return &memRows{cols: []string{"steps"}, vals: [][]driver.Value{{row.stepsJSON}}}, nil

	case strings.Contains(query, "SELECT summary FROM summaries"):
		source := args[0].Value.(string)
		id := args[1].Value.(string)
		key := source + "\x00" + id
		row, ok := c.state.summaries[key]
		if !ok {
			return &memRows{cols: []string{"summary"}}, nil
		}
		return &memRows{cols: []string{"summary"}, vals: [][]driver.Value{{row.summaryJSON}}}, nil

	case strings.Contains(query, "SELECT content FROM session_files"):
		source := args[0].Value.(string)
		sessionID := args[1].Value.(string)
		path := args[2].Value.(string)
		key := source + "\x00" + sessionID + "\x00" + path
		content, ok := c.state.files[key]
		if !ok {
			return &memRows{cols: []string{"content"}}, nil
		}
		return &memRows{cols: []string{"content"}, vals: [][]driver.Value{{content}}}, nil

	default:
		return nil, fmt.Errorf("unsupported query: %s", query)
	}
}

type memRows struct {
	cols []string
	vals [][]driver.Value
	idx  int
}

func (r *memRows) Columns() []string { return r.cols }
func (r *memRows) Close() error      { return nil }
func (r *memRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.idx])
	r.idx++
	return nil
}

// =============================================================================
// IngestorTest
// =============================================================================

const (
	antigravityRaw = "{\"type\":\"USER_INPUT\",\"content\":\"<USER_REQUEST>\\nfix the build\\n</USER_REQUEST>\",\"created_at\":\"2026-06-19T10:00:00Z\"}\n" +
		"{\"type\":\"MESSAGE\",\"source\":\"MODEL\",\"content\":\"done\",\"created_at\":\"2026-06-19T10:01:00Z\"}\n"

	claudeRaw = "{\"type\":\"user\",\"message\":{\"content\":\"add a test\"},\"timestamp\":\"2026-06-19T10:00:00Z\"}\n" +
		"{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]},\"timestamp\":\"2026-06-19T10:01:00Z\"}\n"
)

type ingestorHarness struct {
	state        *memStoreState
	ingestor     *Ingestor
	sessions     *store.SessionRepository
	summaries    *store.SummaryRepository
	sessionFiles *store.SessionFileRepository
}

func newIngestorHarness(t *testing.T) *ingestorHarness {
	t.Helper()
	memDriverOnce.Do(func() {
		sql.Register("transcript_memstore", memDriver{})
	})
	dsn := t.Name()
	state := newMemStoreState()
	memStates.Store(dsn, state)
	t.Cleanup(func() {
		memStates.Delete(dsn)
	})
	db, err := sql.Open("transcript_memstore", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	sessions := store.NewSessionRepository(db)
	summaries := store.NewSummaryRepository(db)
	sessionFiles := store.NewSessionFileRepository(db)
	ingestor := NewIngestor(DefaultNormalizers(), sessions, summaries, sessionFiles)
	return &ingestorHarness{
		state:        state,
		ingestor:     ingestor,
		sessions:     sessions,
		summaries:    summaries,
		sessionFiles: sessionFiles,
	}
}

func pushSession(source, id, raw string) IngestSession {
	return IngestSession{
		Source:      source,
		ID:          id,
		SourceMtime: 1_700_000_000_000,
		Raw:         raw,
	}
}

func TestIngestorStoresAPushedCachedSummaryAlongsideTheSession(t *testing.T) {
	h := newIngestorHarness(t)
	_, err := h.ingestor.Ingest([]IngestSession{
		{
			Source:      "antigravity-cli",
			ID:          "s1",
			SourceMtime: 1,
			Raw:         antigravityRaw,
			Summary:     `{"shortTitle":"t","summary":"cached"}`,
		},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	got, ok, err := h.summaries.Find("antigravity-cli", "s1")
	if err != nil || !ok || !strings.Contains(got, "cached") {
		t.Fatalf("got (%q, %v, %v), expected cached summary", got, ok, err)
	}
}

func TestIngestorAPushWithoutASummaryLeavesNoCachedAnalysis(t *testing.T) {
	h := newIngestorHarness(t)
	if _, err := h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", antigravityRaw)}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, ok, _ := h.summaries.Find("antigravity-cli", "s1"); ok {
		t.Fatal("expected no cached summary")
	}
}

func TestIngestorIngestsASummaryOnItsOwnForAnAlreadyStoredSession(t *testing.T) {
	h := newIngestorHarness(t)
	if _, err := h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", antigravityRaw)}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res, err := h.ingestor.IngestSummaries([]IngestSummary{
		{Source: "antigravity-cli", ID: "s1", Summary: `{"summary":"late"}`},
	})
	if err != nil {
		t.Fatalf("IngestSummaries: %v", err)
	}
	if res != (IngestResult{Ingested: 1, Skipped: 0, Failed: 0}) {
		t.Fatalf("got %+v", res)
	}
	got, ok, _ := h.summaries.Find("antigravity-cli", "s1")
	if !ok || !strings.Contains(got, "late") {
		t.Fatalf("got (%q, %v)", got, ok)
	}
}

func TestIngestorReIngestingAnUnchangedSummaryIsSkipped(t *testing.T) {
	h := newIngestorHarness(t)
	s := IngestSummary{Source: "antigravity-cli", ID: "s1", Summary: `{"summary":"same"}`}
	res1, err := h.ingestor.IngestSummaries([]IngestSummary{s})
	if err != nil || res1 != (IngestResult{Ingested: 1, Skipped: 0, Failed: 0}) {
		t.Fatalf("first push: %+v, %v", res1, err)
	}
	res2, err := h.ingestor.IngestSummaries([]IngestSummary{s})
	if err != nil || res2 != (IngestResult{Ingested: 0, Skipped: 1, Failed: 0}) {
		t.Fatalf("second push: %+v, %v", res2, err)
	}
}

func TestIngestorASummaryForAnUnknownSourceOrWithNoBodyIsCountedAsFailed(t *testing.T) {
	h := newIngestorHarness(t)
	res1, err := h.ingestor.IngestSummaries([]IngestSummary{{Source: "borg", ID: "s1", Summary: "{}"}})
	if err != nil || res1 != (IngestResult{Ingested: 0, Skipped: 0, Failed: 1}) {
		t.Fatalf("unknown source: %+v, %v", res1, err)
	}
	res2, err := h.ingestor.IngestSummaries([]IngestSummary{{Source: "antigravity-cli", ID: "s1", Summary: "  "}})
	if err != nil || res2 != (IngestResult{Ingested: 0, Skipped: 0, Failed: 1}) {
		t.Fatalf("blank summary: %+v, %v", res2, err)
	}
}

func TestIngestorANonJsonSummaryBodyIsFailedWithoutCrashingTheBatch(t *testing.T) {
	h := newIngestorHarness(t)
	res, err := h.ingestor.IngestSummaries([]IngestSummary{
		{Source: "antigravity-cli", ID: "s1", Summary: "not json"},
		{Source: "antigravity-cli", ID: "s2", Summary: `{"summary":"ok"}`},
	})
	if err != nil || res != (IngestResult{Ingested: 1, Skipped: 0, Failed: 1}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, ok, _ := h.summaries.Find("antigravity-cli", "s2"); !ok {
		t.Fatal("expected s2 summary to be stored")
	}
	if _, ok, _ := h.summaries.Find("antigravity-cli", "s1"); ok {
		t.Fatal("expected s1 summary to be rejected")
	}
}

func TestIngestorAMalformedSummaryDoesNotCrashTheBatchAndTheSessionStillIngests(t *testing.T) {
	h := newIngestorHarness(t)
	res, err := h.ingestor.Ingest([]IngestSession{
		{Source: "antigravity-cli", ID: "s1", SourceMtime: 1, Raw: antigravityRaw, Summary: "not json"},
		{Source: "antigravity-cli", ID: "s2", SourceMtime: 1, Raw: antigravityRaw},
	})
	if err != nil || res != (IngestResult{Ingested: 2, Skipped: 0, Failed: 0}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, ok, _ := h.summaries.Find("antigravity-cli", "s1"); ok {
		t.Fatal("expected bad summary to be dropped")
	}
}

func oversizedSummary() string {
	return `{"summary":"` + strings.Repeat("a", MaxSummaryChars) + `"}`
}

func TestIngestorAnOversizedSummaryIsRejectedOnItsOwnEndpoint(t *testing.T) {
	h := newIngestorHarness(t)
	res, err := h.ingestor.IngestSummaries([]IngestSummary{
		{Source: "antigravity-cli", ID: "s1", Summary: oversizedSummary()},
	})
	if err != nil || res != (IngestResult{Ingested: 0, Skipped: 0, Failed: 1}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, ok, _ := h.summaries.Find("antigravity-cli", "s1"); ok {
		t.Fatal("expected oversized summary not to be stored")
	}
}

func TestIngestorAnOversizedSummaryRidingWithATrajectoryIsDroppedButTheSessionStillIngests(t *testing.T) {
	h := newIngestorHarness(t)
	res, err := h.ingestor.Ingest([]IngestSession{
		{Source: "antigravity-cli", ID: "s1", SourceMtime: 1, Raw: antigravityRaw, Summary: oversizedSummary()},
	})
	if err != nil || res != (IngestResult{Ingested: 1, Skipped: 0, Failed: 0}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, ok, _ := h.summaries.Find("antigravity-cli", "s1"); ok {
		t.Fatal("expected oversized summary to be dropped")
	}
}

func TestIngestorASummaryAtTheLimitIsStillAccepted(t *testing.T) {
	h := newIngestorHarness(t)
	body := `{"summary":"` + strings.Repeat("a", MaxSummaryChars-20) + `"}`
	if utf16Len(body) > MaxSummaryChars {
		t.Fatalf("body length %d exceeds MaxSummaryChars %d", utf16Len(body), MaxSummaryChars)
	}
	res, err := h.ingestor.IngestSummaries([]IngestSummary{
		{Source: "antigravity-cli", ID: "s1", Summary: body},
	})
	if err != nil || res != (IngestResult{Ingested: 1, Skipped: 0, Failed: 0}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, ok, _ := h.summaries.Find("antigravity-cli", "s1"); !ok {
		t.Fatal("expected summary at the limit to be stored")
	}
}

func TestIngestorStoresTheFilesATrajectoryAttached(t *testing.T) {
	h := newIngestorHarness(t)
	_, err := h.ingestor.Ingest([]IngestSession{
		{
			Source:      "antigravity-cli",
			ID:          "s1",
			SourceMtime: 1,
			Raw:         antigravityRaw,
			Files: []store.IngestFile{
				{Path: "/home/dev/project/Parser.java", Content: "class Parser {}"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	got, ok, err := h.sessionFiles.Find("antigravity-cli", "s1", "/home/dev/project/Parser.java")
	if err != nil || !ok || got != "class Parser {}" {
		t.Fatalf("got (%q, %v, %v)", got, ok, err)
	}
}

func TestIngestorReIngestingReplacesTheAttachedFilesRatherThanAccumulating(t *testing.T) {
	h := newIngestorHarness(t)
	_, _ = h.ingestor.Ingest([]IngestSession{
		{
			Source: "antigravity-cli", ID: "s1", SourceMtime: 1, Raw: antigravityRaw,
			Files: []store.IngestFile{{Path: "/a.txt", Content: "first"}},
		},
	})
	_, _ = h.ingestor.Ingest([]IngestSession{
		{
			Source: "antigravity-cli", ID: "s1", SourceMtime: 1, Raw: antigravityRaw,
			Files: []store.IngestFile{{Path: "/b.txt", Content: "second"}},
		},
	})
	if _, ok, _ := h.sessionFiles.Find("antigravity-cli", "s1", "/a.txt"); ok {
		t.Fatal("expected /a.txt to be replaced")
	}
	got, ok, _ := h.sessionFiles.Find("antigravity-cli", "s1", "/b.txt")
	if !ok || got != "second" {
		t.Fatalf("expected /b.txt to be 'second', got (%q, %v)", got, ok)
	}
}

func TestIngestorAnOversizedAttachmentIsDroppedButTheRestOfTheSessionSurvives(t *testing.T) {
	h := newIngestorHarness(t)
	_, err := h.ingestor.Ingest([]IngestSession{
		{
			Source: "antigravity-cli", ID: "s1", SourceMtime: 1, Raw: antigravityRaw,
			Files: []store.IngestFile{
				{Path: "/huge.log", Content: strings.Repeat("x", MaxFileChars+1)},
				{Path: "/small.txt", Content: "kept"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, ok, _ := h.sessionFiles.Find("antigravity-cli", "s1", "/huge.log"); ok {
		t.Fatal("expected /huge.log to be dropped")
	}
	got, ok, _ := h.sessionFiles.Find("antigravity-cli", "s1", "/small.txt")
	if !ok || got != "kept" {
		t.Fatalf("expected /small.txt to be 'kept', got (%q, %v)", got, ok)
	}
}

func TestIngestorAPushCarryingNoFilesLeavesNoAttachments(t *testing.T) {
	h := newIngestorHarness(t)
	_, _ = h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", antigravityRaw)})
	if _, ok, _ := h.sessionFiles.Find("antigravity-cli", "s1", "/anything"); ok {
		t.Fatal("expected no attachments")
	}
}

func TestIngestorStoresAntigravityTranscriptAsNormalizedSteps(t *testing.T) {
	h := newIngestorHarness(t)
	res, err := h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", antigravityRaw)})
	if err != nil || res != (IngestResult{Ingested: 1, Skipped: 0, Failed: 0}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	stepsStr, ok, err := h.sessions.Steps("antigravity-cli", "s1")
	if err != nil || !ok {
		t.Fatalf("Steps: %v, %v", ok, err)
	}
	steps := ParseStepsJSON(stepsStr)
	if len(steps) != 2 || steps[0]["type"] != "USER_INPUT" {
		t.Fatalf("unexpected steps: %#v", steps)
	}
}

func TestIngestorConvertsClaudeCodeTranscriptIntoTheSharedStepSchema(t *testing.T) {
	h := newIngestorHarness(t)
	if _, err := h.ingestor.Ingest([]IngestSession{pushSession("claude-code", "c1", claudeRaw)}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	stepsStr, ok, _ := h.sessions.Steps("claude-code", "c1")
	if !ok {
		t.Fatal("expected stored steps")
	}
	steps := ParseStepsJSON(stepsStr)
	if len(steps) != 2 || steps[0]["type"] != "USER_INPUT" || steps[1]["type"] != "MESSAGE" || steps[1]["source"] != "MODEL" {
		t.Fatalf("unexpected steps: %#v", steps)
	}
}

func TestIngestorDerivesATitleFromTheTranscriptWhenTheClientSendsNone(t *testing.T) {
	h := newIngestorHarness(t)
	_, _ = h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", antigravityRaw)})
	row := h.state.sessions["antigravity-cli\x00s1"]
	if row.title != "fix the build" {
		t.Fatalf("got title %q, want %q", row.title, "fix the build")
	}
}

func TestIngestorTheClientsTitleWinsOverTheDerivedOne(t *testing.T) {
	h := newIngestorHarness(t)
	_, _ = h.ingestor.Ingest([]IngestSession{
		{
			Source:      "antigravity-cli",
			ID:          "s1",
			Title:       "  Explicit  ",
			SourceMtime: 1,
			Raw:         antigravityRaw,
		},
	})
	row := h.state.sessions["antigravity-cli\x00s1"]
	if row.title != "Explicit" {
		t.Fatalf("got title %q, want %q", row.title, "Explicit")
	}
}

func TestIngestorFallsBackToAStableTitleWhenTheTranscriptRevealsNone(t *testing.T) {
	h := newIngestorHarness(t)
	_, _ = h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "abcdef123456", "")})
	row := h.state.sessions["antigravity-cli\x00abcdef123456"]
	if row.title != "antigravity-cli session abcdef12" {
		t.Fatalf("got title %q, want %q", row.title, "antigravity-cli session abcdef12")
	}
}

func TestIngestorRePushingTheSameTrajectorySkipsRatherThanDuplicating(t *testing.T) {
	h := newIngestorHarness(t)
	p := pushSession("codex", "x", "{\"type\":\"session_meta\"}\n")
	res1, _ := h.ingestor.Ingest([]IngestSession{p})
	if res1 != (IngestResult{Ingested: 1, Skipped: 0, Failed: 0}) {
		t.Fatalf("first push: %+v", res1)
	}
	res2, _ := h.ingestor.Ingest([]IngestSession{p})
	if res2 != (IngestResult{Ingested: 0, Skipped: 1, Failed: 0}) {
		t.Fatalf("second push: %+v", res2)
	}
}

func TestIngestorAChangedTranscriptIsIngestedAgain(t *testing.T) {
	h := newIngestorHarness(t)
	_, _ = h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", antigravityRaw)})
	longer := antigravityRaw + "{\"type\":\"MESSAGE\",\"source\":\"MODEL\",\"content\":\"more\",\"created_at\":\"2026-06-19T10:02:00Z\"}\n"
	res, _ := h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", longer)})
	if res != (IngestResult{Ingested: 1, Skipped: 0, Failed: 0}) {
		t.Fatalf("changed push: %+v", res)
	}
}

func TestIngestorRejectsUnknownSourcesAndMissingIdsWithoutFailingTheBatch(t *testing.T) {
	h := newIngestorHarness(t)
	res, err := h.ingestor.Ingest([]IngestSession{
		pushSession("borg", "s1", antigravityRaw),
		pushSession("antigravity-cli", "", antigravityRaw),
		{},
		pushSession("antigravity-cli", "good", antigravityRaw),
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res != (IngestResult{Ingested: 1, Skipped: 0, Failed: 3}) {
		t.Fatalf("got %+v, want {1, 0, 3}", res)
	}
}

func TestIngestorAPushedIdCannotForgeALogRecordOfItsOwn(t *testing.T) {
	h := newIngestorHarness(t)
	forged := "s1\n00:00:00.000 [main] WARN  i.g.g.a.Ingestor - nothing to see here"
	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	_, _ = h.ingestor.Ingest([]IngestSession{pushSession("borg", forged, "")})
	logged := buf.String()

	if !strings.Contains(logged, "Rejecting trajectory") {
		t.Fatalf("the rejection must still be logged: %q", logged)
	}
	var forgedLines []string
	for _, line := range strings.Split(strings.TrimSuffix(logged, "\n"), "\n") {
		if strings.Contains(line, "nothing to see here") {
			forgedLines = append(forgedLines, line)
		}
	}
	if len(forgedLines) != 1 {
		t.Fatalf("the pushed id must not span log lines:\n%s", logged)
	}
	if !strings.Contains(forgedLines[0], "Rejecting trajectory") {
		t.Fatalf("the pushed id forged a log record of its own:\n%s", logged)
	}
	if !strings.Contains(logged, `s1\n00:00:00.000`) {
		t.Fatalf("the line break should read as an escape:\n%s", logged)
	}
}

func TestIngestorSkipsMalformedTranscriptLinesRatherThanStoringUnparseableSteps(t *testing.T) {
	h := newIngestorHarness(t)
	raw := "{\"type\":\"USER_INPUT\",\"content\":\"hi\"}\nnot json at all\n"
	_, _ = h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", raw)})
	stepsStr, _, _ := h.sessions.Steps("antigravity-cli", "s1")
	steps := ParseStepsJSON(stepsStr)
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
}

func TestIngestorHashesTheTranscriptTheServerReceivedRatherThanTrustingTheClient(t *testing.T) {
	if Sha256("abc") != Sha256("abc") {
		t.Fatal("same bytes must hash the same")
	}
	if Sha256("abc") == Sha256("abd") {
		t.Fatal("different bytes must not hash the same")
	}
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if Sha256("abc") != want {
		t.Fatalf("got %q, want %q", Sha256("abc"), want)
	}
}

func TestIngestorAZeroMtimeBecomesNowSoTheSessionStillSortsToTheTop(t *testing.T) {
	h := newIngestorHarness(t)
	_, _ = h.ingestor.Ingest([]IngestSession{
		{Source: "antigravity-cli", ID: "s1", SourceMtime: 0, Raw: antigravityRaw},
	})
	row := h.state.sessions["antigravity-cli\x00s1"]
	if row.sourceMtime <= 0 {
		t.Fatalf("a 0 mtime must not sort to the epoch, got %d", row.sourceMtime)
	}
}

func TestIngestorPropagatesStoreUnavailableOnSessionOrSummaryOutage(t *testing.T) {
	h := newIngestorHarness(t)
	h.state.mu.Lock()
	h.state.failExec = errors.New("connection refused")
	h.state.mu.Unlock()

	_, err := h.ingestor.Ingest([]IngestSession{pushSession("antigravity-cli", "s1", antigravityRaw)})
	if _, ok := store.IsStoreUnavailable(err); !ok {
		t.Fatalf("expected StoreUnavailableError from Ingest, got %v", err)
	}

	_, err = h.ingestor.IngestSummaries([]IngestSummary{
		{Source: "antigravity-cli", ID: "s1", Summary: `{"summary":"ok"}`},
	})
	if _, ok := store.IsStoreUnavailable(err); !ok {
		t.Fatalf("expected StoreUnavailableError from IngestSummaries, got %v", err)
	}
}

func TestIngestorCapsAttachedFilesAtMaxFilesPerSession(t *testing.T) {
	h := newIngestorHarness(t)
	files := make([]store.IngestFile, 0, MaxFilesPerSession+5)
	for i := 0; i < MaxFilesPerSession+5; i++ {
		files = append(files, store.IngestFile{
			Path:    fmt.Sprintf("/file-%d.txt", i),
			Content: "ok",
		})
	}
	if _, err := h.ingestor.Ingest([]IngestSession{
		{Source: "antigravity-cli", ID: "s1", SourceMtime: 1, Raw: antigravityRaw, Files: files},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, ok, _ := h.sessionFiles.Find("antigravity-cli", "s1", fmt.Sprintf("/file-%d.txt", MaxFilesPerSession-1)); !ok {
		t.Fatal("expected file at cap index to be stored")
	}
	if _, ok, _ := h.sessionFiles.Find("antigravity-cli", "s1", fmt.Sprintf("/file-%d.txt", MaxFilesPerSession)); ok {
		t.Fatal("expected file beyond MaxFilesPerSession to be dropped")
	}
}

func TestHelperFunctionsAndTruncation(t *testing.T) {
	longPrompt := "<USER_REQUEST>\n" + strings.Repeat("a", 90) + "\n</USER_REQUEST><OTHER>ignored</OTHER>"
	extracted := ExtractUserPromptText(longPrompt)
	if extracted != strings.Repeat("a", 90) {
		t.Fatalf("ExtractUserPromptText got %q", extracted)
	}

	title, ok := DeriveShortTitle([]string{
		`{"type":"USER_INPUT","content":"` + strings.Repeat("a", 90) + `"}`,
	})
	if !ok || title != strings.Repeat("a", 80)+"..." {
		t.Fatalf("DeriveShortTitle got (%q, %v)", title, ok)
	}

	if got := ClaudeCodeCleanUserText("  hello  "); got != "hello" {
		t.Fatalf("ClaudeCodeCleanUserText got %q", got)
	}
	if got := ClaudeCodeCleanTitleLine("  " + strings.Repeat("b", 85) + "  "); got != strings.Repeat("b", 80)+"..." {
		t.Fatalf("ClaudeCodeCleanTitleLine got %q", got)
	}
	if got := CodexExtractUserRequest("  fix it  "); got != "fix it" {
		t.Fatalf("CodexExtractUserRequest got %q", got)
	}

	// Claude tool_result with array of text blocks
	arrayToolResult := `{"type":"user","timestamp":"t","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]}}`
	steps := ClaudeCodeToTimelineSteps([]string{arrayToolResult})
	if len(steps) != 1 || steps[0]["content"] != "line1\nline2" {
		t.Fatalf("unexpected tool_result array content: %#v", steps)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

