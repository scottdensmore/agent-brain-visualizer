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

const (
	assistantFinalMax     = 2000
	assistantOtherMaxEach = 500
	assistantOtherBudget  = 2000
)

// DefaultNormalizers returns the built-in SourceNormalizer implementations in standard order.
func DefaultNormalizers() []SourceNormalizer {
	return []SourceNormalizer{
		AntigravityNormalizer{},
		CodexNormalizer{},
		ClaudeCodeNormalizer{},
	}
}

// AnalysisSequencesFromStepsJSON parses stored steps JSON and derives LLM-analysis sequences
// using the schema appropriate for source.
func AnalysisSequencesFromStepsJSON(source, stepsJSON string) [][]string {
	return AnalysisSequencesFromSteps(source, ParseStepsJSON(stepsJSON))
}

// AnalysisSequencesFromSteps derives LLM-analysis sequences from parsed step objects, routing
// Antigravity flavors through ParseSequences and normalized sources through ToAnalysisSequences.
func AnalysisSequencesFromSteps(source string, steps []map[string]any) [][]string {
	if IsAntigravitySource(source) {
		lines := make([]string, 0, len(steps))
		for _, step := range steps {
			if step == nil {
				continue
			}
			if b, err := json.Marshal(step); err == nil {
				lines = append(lines, string(b))
			}
		}
		return ParseSequences(lines)
	}
	return ToAnalysisSequences(steps)
}

// ParseStepsJSON parses a JSON array string of step objects, returning an empty slice on error.
func ParseStepsJSON(stepsJSON string) []map[string]any {
	steps := make([]map[string]any, 0)
	if strings.TrimSpace(stepsJSON) == "" {
		return steps
	}
	var rawArr []any
	if err := json.Unmarshal([]byte(stepsJSON), &rawArr); err != nil {
		return steps
	}
	for _, item := range rawArr {
		if m, ok := item.(map[string]any); ok && m != nil {
			steps = append(steps, m)
		}
	}
	return steps
}

// ToAnalysisSequences turns normalized timeline steps (Codex / Claude Code schema) into
// condensed LLM-analysis sequences, keeping the final assistant message and a bounded budget
// of earlier assistant messages.
func ToAnalysisSequences(steps []map[string]any) [][]string {
	lastMsgIdx := lastMessageIndex(steps)
	budget := assistantOtherBudget

	sequences := make([][]string, 0)
	current := make([]string, 0)
	for i, step := range steps {
		stepType := jsonString(step, "type", "")

		var line string
		if stepType == "MESSAGE" {
			content := jsonString(step, "content", "")
			if strings.TrimSpace(content) == "" {
				continue
			}
			if i == lastMsgIdx {
				line = "ASSISTANT: " + truncateChars(content, assistantFinalMax)
			} else if budget > 0 {
				maxEach := assistantOtherMaxEach
				if budget < maxEach {
					maxEach = budget
				}
				text := truncateChars(content, maxEach)
				budget -= utf16Len(text)
				line = "ASSISTANT: " + text
			} else {
				continue
			}
		} else {
			var ok bool
			line, ok = analysisLine(step, stepType)
			if !ok {
				continue
			}
		}

		if stepType == "USER_INPUT" && len(current) > 0 {
			sequences = append(sequences, DeduplicateSequence(current))
			current = make([]string, 0)
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		sequences = append(sequences, DeduplicateSequence(current))
	}
	return sequences
}

func lastMessageIndex(steps []map[string]any) int {
	idx := -1
	for i, step := range steps {
		if jsonString(step, "type", "") == "MESSAGE" &&
			strings.TrimSpace(jsonString(step, "content", "")) != "" {
			idx = i
		}
	}
	return idx
}

func analysisLine(step map[string]any, stepType string) (string, bool) {
	switch stepType {
	case "USER_INPUT":
		return "USER REQUEST: " + truncateChars(jsonString(step, "content", ""), 2000), true
	case "FUNCTION_CALL":
		var tool map[string]any
		if tools := extractToolCalls(step); len(tools) > 0 {
			tool = tools[0]
		}
		name := jsonString(tool, "name", "unknown")
		var args any
		if tool != nil {
			args = tool["args"]
		}
		return "AGENT ACTION: [" + name + "] " + argsSummary(args), true
	case "FUNCTION_OUTPUT":
		if jsonString(step, "status", "") == "ERROR" {
			return "SYSTEM EVENT/ERROR: " + truncateChars(jsonString(step, "content", ""), 500), true
		}
		return "", false
	default:
		return "", false
	}
}

func argsSummary(args any) string {
	if args == nil {
		return ""
	}
	if m, ok := args.(map[string]any); ok {
		for _, key := range []string{"cmd", "command", "file_path"} {
			if val, ok := m[key].(string); ok {
				return truncateChars(val, 200)
			}
		}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return truncateChars(string(b), 200)
}

func extractToolCalls(step map[string]any) []map[string]any {
	if step == nil {
		return nil
	}
	switch tc := step["tool_calls"].(type) {
	case []map[string]any:
		return tc
	case []any:
		out := make([]map[string]any, 0, len(tc))
		for _, raw := range tc {
			if m, ok := raw.(map[string]any); ok && m != nil {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}
