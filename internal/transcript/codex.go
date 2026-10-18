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
	"encoding/json"
	"regexp"
	"strings"
)

// CodexSource is the source identifier for OpenAI Codex rollout transcripts.
const CodexSource = "codex"

var codexExitCodeRe = regexp.MustCompile(`(?m)^Process exited with code (\d+)`)

// CodexNormalizer normalizes pushed OpenAI Codex rollout transcripts into the shared timeline step schema.
type CodexNormalizer struct{}

// Handles reports whether source is "codex".
func (CodexNormalizer) Handles(source string) bool {
	return source == CodexSource
}

// ToStepsJSON adapts Codex rollout JSONL lines into a JSON array string of timeline steps.
func (CodexNormalizer) ToStepsJSON(rawLines []string) string {
	steps := CodexToTimelineSteps(rawLines)
	out, err := json.Marshal(steps)
	if err != nil {
		return "[]"
	}
	return string(out)
}

// DeriveTitle derives a short session title from event_msg/user_message or the first user response_item.
func (CodexNormalizer) DeriveTitle(rawLines []string) (string, bool) {
	return CodexDeriveShortTitle(rawLines)
}

// CodexToTimelineSteps converts OpenAI Codex rollout JSONL lines into timeline step maps.
func CodexToTimelineSteps(lines []string) []map[string]any {
	steps := make([]map[string]any, 0)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var root map[string]any
		if err := json.Unmarshal([]byte(line), &root); err != nil || root == nil {
			continue
		}
		if jsonString(root, "type", "") != "response_item" {
			continue
		}
		ts := jsonString(root, "timestamp", "")
		payload, _ := root["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		if step := codexStepFor(payload, ts); step != nil {
			steps = append(steps, step)
		}
	}
	return steps
}

func codexStepFor(payload map[string]any, ts string) map[string]any {
	switch jsonString(payload, "type", "") {
	case "message":
		return codexMessageStep(payload, ts)
	case "function_call":
		return codexFunctionCallStep(payload, ts)
	case "function_call_output":
		return codexFunctionOutputStep(payload, ts)
	case "reasoning":
		return codexReasoningStep(payload, ts)
	default:
		return nil
	}
}

func codexMessageStep(payload map[string]any, ts string) map[string]any {
	role := jsonString(payload, "role", "")
	text := codexJoinTextParts(payload["content"])
	if strings.TrimSpace(text) == "" {
		return nil
	}
	step := newTimelineStep(ts)
	if role == "user" {
		step["type"] = "USER_INPUT"
		step["source"] = "USER_EXPLICIT"
	} else {
		step["type"] = "MESSAGE"
		step["source"] = "MODEL"
	}
	step["content"] = text
	return step
}

func codexFunctionCallStep(payload map[string]any, ts string) map[string]any {
	step := newTimelineStep(ts)
	step["type"] = "FUNCTION_CALL"
	step["source"] = "MODEL"
	name := jsonString(payload, "name", "unknown")
	args := codexParseArguments(jsonString(payload, "arguments", ""))
	step["tool_calls"] = []any{
		map[string]any{
			"name": name,
			"args": args,
		},
	}
	return step
}

func codexFunctionOutputStep(payload map[string]any, ts string) map[string]any {
	output := codexOutputText(payload["output"])
	if strings.TrimSpace(output) == "" {
		return nil
	}
	step := newTimelineStep(ts)
	step["type"] = "FUNCTION_OUTPUT"
	step["source"] = "TOOL"
	step["content"] = output
	if isCodexFailure(output) {
		step["status"] = "ERROR"
	}
	return step
}

func codexReasoningStep(payload map[string]any, ts string) map[string]any {
	text := codexJoinTextParts(payload["summary"])
	if strings.TrimSpace(text) == "" {
		return nil
	}
	step := newTimelineStep(ts)
	step["type"] = "REASONING"
	step["source"] = "MODEL"
	step["thinking"] = text
	return step
}

// CodexToAnalysisSequences converts Codex rollout lines into condensed LLM-analysis sequences.
func CodexToAnalysisSequences(lines []string) [][]string {
	return ToAnalysisSequences(CodexToTimelineSteps(lines))
}

// CodexDeriveShortTitle derives a short session title from the first event_msg/user_message,
// falling back to the first user response_item message.
func CodexDeriveShortTitle(lines []string) (string, bool) {
	var fallback string
	hasFallback := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var root map[string]any
		if err := json.Unmarshal([]byte(line), &root); err != nil || root == nil {
			continue
		}
		payload, _ := root["payload"].(map[string]any)
		outer := jsonString(root, "type", "")
		if outer == "event_msg" && jsonString(payload, "type", "") == "user_message" {
			msg := CodexExtractUserRequest(jsonString(payload, "message", ""))
			if msg != "" {
				return truncateTitle(msg), true
			}
		} else if !hasFallback &&
			outer == "response_item" &&
			jsonString(payload, "type", "") == "message" &&
			jsonString(payload, "role", "") == "user" {
			text := CodexExtractUserRequest(codexJoinTextParts(payload["content"]))
			if text != "" {
				fallback = truncateTitle(text)
				hasFallback = true
			}
		}
	}
	if hasFallback {
		return fallback, true
	}
	return "", false
}

// CodexExtractUserRequest trims surrounding whitespace from a Codex user prompt string.
func CodexExtractUserRequest(text string) string {
	return strings.TrimSpace(text)
}

func codexJoinTextParts(parts any) string {
	arr, ok := parts.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, rawPart := range arr {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(text)
		}
	}
	return sb.String()
}

func codexParseArguments(arguments string) any {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil && parsed != nil {
		return parsed
	}
	return arguments
}

func codexOutputText(output any) string {
	if output == nil {
		return ""
	}
	if s, ok := output.(string); ok {
		return s
	}
	b, err := json.Marshal(output)
	if err != nil {
		return ""
	}
	return string(b)
}

func isCodexFailure(output string) bool {
	m := codexExitCodeRe.FindStringSubmatch(output)
	return len(m) >= 2 && m[1] != "0"
}
