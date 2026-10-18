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
	"strings"
)

// ClaudeCodeSource is the source identifier for Claude Code transcripts.
const ClaudeCodeSource = "claude-code"

// ClaudeCodeNormalizer normalizes pushed Claude Code transcripts into the shared timeline step schema.
type ClaudeCodeNormalizer struct{}

// Handles reports whether source is "claude-code".
func (ClaudeCodeNormalizer) Handles(source string) bool {
	return source == ClaudeCodeSource
}

// ToStepsJSON adapts Claude Code JSONL lines into a JSON array string of timeline steps.
func (ClaudeCodeNormalizer) ToStepsJSON(rawLines []string) string {
	steps := ClaudeCodeToTimelineSteps(rawLines)
	out, err := json.Marshal(steps)
	if err != nil {
		return "[]"
	}
	return string(out)
}

// DeriveTitle derives a short session title from a summary line or the first user prompt.
func (ClaudeCodeNormalizer) DeriveTitle(rawLines []string) (string, bool) {
	return ClaudeCodeDeriveShortTitle(rawLines)
}

// ClaudeCodeToTimelineSteps converts Claude Code JSONL transcript lines into timeline step maps.
func ClaudeCodeToTimelineSteps(lines []string) []map[string]any {
	steps := make([]map[string]any, 0)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var root map[string]any
		if err := json.Unmarshal([]byte(line), &root); err != nil || root == nil {
			continue
		}
		stepType := jsonString(root, "type", "")
		ts := jsonString(root, "timestamp", "")
		msg, _ := root["message"].(map[string]any)
		if msg == nil {
			continue
		}
		content := msg["content"]
		switch stepType {
		case "user":
			steps = addClaudeUserSteps(steps, content, ts)
		case "assistant":
			steps = addClaudeAssistantSteps(steps, content, ts)
		}
	}
	return steps
}

func addClaudeUserSteps(steps []map[string]any, content any, ts string) []map[string]any {
	if text, ok := content.(string); ok {
		if strings.TrimSpace(text) == "" {
			return steps
		}
		step := newTimelineStep(ts)
		step["type"] = "USER_INPUT"
		step["source"] = "USER_EXPLICIT"
		step["content"] = text
		return append(steps, step)
	}
	if arr, ok := content.([]any); ok {
		for _, rawBlock := range arr {
			block, ok := rawBlock.(map[string]any)
			if !ok || jsonString(block, "type", "") != "tool_result" {
				continue
			}
			text := claudeTextOf(block["content"])
			if strings.TrimSpace(text) == "" {
				continue
			}
			step := newTimelineStep(ts)
			step["type"] = "FUNCTION_OUTPUT"
			step["source"] = "TOOL"
			step["content"] = text
			if jsonBool(block, "is_error", false) {
				step["status"] = "ERROR"
			}
			steps = append(steps, step)
		}
	}
	return steps
}

func addClaudeAssistantSteps(steps []map[string]any, content any, ts string) []map[string]any {
	arr, ok := content.([]any)
	if !ok {
		return steps
	}
	for _, rawBlock := range arr {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		switch jsonString(block, "type", "") {
		case "text":
			text := jsonString(block, "text", "")
			if strings.TrimSpace(text) == "" {
				continue
			}
			step := newTimelineStep(ts)
			step["type"] = "MESSAGE"
			step["source"] = "MODEL"
			step["content"] = text
			steps = append(steps, step)
		case "thinking":
			text := jsonString(block, "thinking", "")
			if strings.TrimSpace(text) == "" {
				continue
			}
			step := newTimelineStep(ts)
			step["type"] = "REASONING"
			step["source"] = "MODEL"
			step["thinking"] = text
			steps = append(steps, step)
		case "tool_use":
			step := newTimelineStep(ts)
			step["type"] = "FUNCTION_CALL"
			step["source"] = "MODEL"
			name := jsonString(block, "name", "unknown")
			var args any = map[string]any{}
			if input, hasInput := block["input"]; hasInput && input != nil {
				args = input
			}
			step["tool_calls"] = []any{
				map[string]any{
					"name": name,
					"args": args,
				},
			}
			steps = append(steps, step)
		}
	}
	return steps
}

// ClaudeCodeToAnalysisSequences converts Claude Code JSONL lines into condensed LLM-analysis sequences.
func ClaudeCodeToAnalysisSequences(lines []string) [][]string {
	return ToAnalysisSequences(ClaudeCodeToTimelineSteps(lines))
}

// ClaudeCodeDeriveShortTitle derives a short session title from a summary line if present,
// otherwise from the first user prompt with string content.
func ClaudeCodeDeriveShortTitle(lines []string) (string, bool) {
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
		stepType := jsonString(root, "type", "")
		if stepType == "summary" {
			s := jsonString(root, "summary", "")
			if strings.TrimSpace(s) != "" {
				return ClaudeCodeCleanTitleLine(s), true
			}
		} else if !hasFallback && stepType == "user" {
			msg, _ := root["message"].(map[string]any)
			if msg != nil {
				if text, ok := msg["content"].(string); ok {
					cleaned := ClaudeCodeCleanUserText(text)
					if cleaned != "" {
						fallback = ClaudeCodeCleanTitleLine(cleaned)
						hasFallback = fallback != ""
					}
				}
			}
		}
	}
	if hasFallback {
		return fallback, true
	}
	return "", false
}

// ClaudeCodeCleanUserText trims surrounding whitespace from a user prompt string.
func ClaudeCodeCleanUserText(raw string) string {
	return strings.TrimSpace(raw)
}

// ClaudeCodeCleanTitleLine trims surrounding whitespace and truncates a title line to 80 chars.
func ClaudeCodeCleanTitleLine(text string) string {
	return truncateTitle(strings.TrimSpace(text))
}

func newTimelineStep(ts string) map[string]any {
	step := make(map[string]any)
	if strings.TrimSpace(ts) != "" {
		step["created_at"] = ts
	}
	return step
}

func claudeTextOf(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
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
	b, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(b)
}
