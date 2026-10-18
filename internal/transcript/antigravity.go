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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DefaultFlavor is the Antigravity source flavor used when none is supplied.
const DefaultFlavor = "antigravity-cli"

const (
	titleMax     = 80
	requestClose = "</USER_REQUEST>"
)

var userRequestOpenRe = regexp.MustCompile(`(?s)<USER_REQUEST>\s*`)

// GeminiRoot returns the ~/.gemini directory under the current user's home directory.
func GeminiRoot() string {
	return ResolveGeminiDir("")
}

// ResolveGeminiDir returns the .gemini sandbox directory under userHome.
// If userHome is empty, the current user's home directory is used.
func ResolveGeminiDir(userHome string) string {
	if strings.TrimSpace(userHome) == "" {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			userHome = h
		} else if h := os.Getenv("HOME"); h != "" {
			userHome = h
		}
	}
	return filepath.Join(userHome, ".gemini")
}

// IsPreviewDenied reports whether filePath inside geminiDir is off-limits to the inline file preview.
// Any path outside geminiDir, any path with a dot-prefixed segment at any depth, and any .json file
// sitting directly in the geminiDir root are denied.
func IsPreviewDenied(geminiDir, filePath string) bool {
	if strings.TrimSpace(geminiDir) == "" || strings.TrimSpace(filePath) == "" {
		return true
	}
	sandbox := filepath.Clean(geminiDir)
	candidate := filepath.Clean(filePath)
	if filepath.IsAbs(sandbox) != filepath.IsAbs(candidate) {
		return true
	}
	rel, err := filepath.Rel(sandbox, candidate)
	if err != nil {
		return true
	}
	relSlash := filepath.ToSlash(rel)
	parts := strings.Split(relSlash, "/")
	for _, part := range parts {
		if part == "" || strings.HasPrefix(part, ".") {
			return true
		}
	}
	return len(parts) == 1 && strings.HasSuffix(strings.ToLower(parts[0]), ".json")
}

// ParseSequences condenses raw Antigravity JSONL transcript lines into user-initiated sequences
// of human-readable summary lines with consecutive duplicates collapsed.
func ParseSequences(lines []string) [][]string {
	sequences := make([][]string, 0)
	currentSequence := make([]string, 0)

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var node map[string]any
		if err := json.Unmarshal([]byte(line), &node); err != nil || node == nil {
			continue
		}
		stepType := jsonString(node, "type", "")
		source := jsonString(node, "source", "")
		if stepType == "USER_INPUT" || source == "USER_EXPLICIT" {
			if len(currentSequence) > 0 {
				sequences = append(sequences, DeduplicateSequence(currentSequence))
				currentSequence = make([]string, 0)
			}
			content := jsonString(node, "content", "")
			currentSequence = append(currentSequence, "USER REQUEST: "+truncateChars(content, 2000))
		} else if stepType == "PLANNER_RESPONSE" || source == "MODEL" {
			for _, tool := range extractToolCalls(node) {
				name := jsonString(tool, "name", "unknown")
				args, _ := tool["arguments"].(map[string]any)
				action := jsonString(args, "toolAction", "")
				tgt := jsonString(args, "TargetFile", "")
				if tgt == "" {
					tgt = jsonString(args, "CommandLine", "")
				}
				currentSequence = append(currentSequence, "AGENT ACTION: ["+name+"] "+action+" -> "+tgt)
			}
		} else {
			_, hasError := node["error"]
			_, hasContent := node["content"]
			content := jsonString(node, "content", "")
			if hasError || (hasContent && strings.Contains(content, "Exception")) {
				currentSequence = append(currentSequence, "SYSTEM EVENT/ERROR: "+truncateChars(content, 500))
			}
		}
	}
	if len(currentSequence) > 0 {
		sequences = append(sequences, DeduplicateSequence(currentSequence))
	}
	return sequences
}

// DeduplicateSequence collapses runs of identical consecutive lines into a single line annotated
// with the repeat count, e.g. "foo (repeated 3 times)". Non-consecutive duplicates are preserved.
func DeduplicateSequence(seq []string) []string {
	if len(seq) == 0 {
		return []string{}
	}
	deduped := make([]string, 0, len(seq))
	var lastLine string
	hasLast := false
	count := 0
	for _, line := range seq {
		if hasLast && line == lastLine {
			count++
		} else {
			if count > 1 {
				deduped[len(deduped)-1] = fmt.Sprintf("%s (repeated %d times)", lastLine, count)
			}
			deduped = append(deduped, line)
			lastLine = line
			hasLast = true
			count = 1
		}
	}
	if count > 1 {
		deduped[len(deduped)-1] = fmt.Sprintf("%s (repeated %d times)", lastLine, count)
	}
	return deduped
}

// SplitIntoSafeChunks recursively splits lines into chunks whose joined text stays within maxTokens.
// A single line is never split further even if it exceeds maxTokens. If tokenCounter is nil, panics,
// or returns a negative value, a character-length heuristic (utf16Len(text)/2) is used.
func SplitIntoSafeChunks(lines []string, tokenCounter func(string) int, maxTokens int) [][]string {
	chunks := make([][]string, 0)
	splitIntoSafeChunks(lines, tokenCounter, maxTokens, &chunks)
	return chunks
}

func splitIntoSafeChunks(lines []string, tokenCounter func(string) int, maxTokens int, safeChunks *[][]string) {
	if len(lines) == 0 {
		return
	}
	text := strings.Join(lines, "\n")
	tokens, ok := safeCountTokens(text, tokenCounter)
	if !ok {
		tokens = utf16Len(text) / 2
	}
	if tokens <= maxTokens || len(lines) == 1 {
		chunk := make([]string, len(lines))
		copy(chunk, lines)
		*safeChunks = append(*safeChunks, chunk)
		return
	}
	mid := len(lines) / 2
	splitIntoSafeChunks(lines[:mid], tokenCounter, maxTokens, safeChunks)
	splitIntoSafeChunks(lines[mid:], tokenCounter, maxTokens, safeChunks)
}

func safeCountTokens(text string, tokenCounter func(string) int) (tokens int, ok bool) {
	if tokenCounter == nil {
		return 0, false
	}
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	t := tokenCounter(text)
	if t < 0 {
		return 0, false
	}
	return t, true
}

// ExtractUserPromptText unwraps Antigravity's <USER_REQUEST>...</USER_REQUEST> wrapper and trims whitespace.
func ExtractUserPromptText(raw string) string {
	text := userRequestOpenRe.ReplaceAllString(raw, "")
	if end := strings.Index(text, requestClose); end != -1 {
		text = text[:end]
	}
	return strings.TrimSpace(text)
}

// DeriveShortTitle derives a short session title from the first USER_INPUT step in rawLines.
func DeriveShortTitle(lines []string) (string, bool) {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var step map[string]any
		if err := json.Unmarshal([]byte(line), &step); err != nil || step == nil {
			continue
		}
		if jsonString(step, "type", "") != "USER_INPUT" {
			continue
		}
		content := ExtractUserPromptText(jsonString(step, "content", ""))
		if content != "" {
			return truncateTitle(content), true
		}
	}
	return "", false
}

// AntigravityNormalizer normalizes pushed Antigravity transcripts (antigravity-cli, antigravity-ide, etc.).
type AntigravityNormalizer struct{}

// Handles reports whether source begins with the "antigravity" flavor prefix.
func (AntigravityNormalizer) Handles(source string) bool {
	return IsAntigravitySource(source)
}

// ToStepsJSON parses non-blank JSON lines and returns them as a JSON array string, skipping malformed lines.
func (AntigravityNormalizer) ToStepsJSON(rawLines []string) string {
	steps := make([]json.RawMessage, 0)
	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			continue
		}
		steps = append(steps, raw)
	}
	out, err := json.Marshal(steps)
	if err != nil {
		return "[]"
	}
	return string(out)
}

// DeriveTitle returns the first user prompt with <USER_REQUEST> stripped and truncated to 80 chars.
func (AntigravityNormalizer) DeriveTitle(rawLines []string) (string, bool) {
	return DeriveShortTitle(rawLines)
}

func jsonString(m map[string]any, key, def string) string {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return def
	}
}

func jsonBool(m map[string]any, key string, def bool) bool {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		if strings.EqualFold(val, "true") {
			return true
		}
		if strings.EqualFold(val, "false") {
			return false
		}
		return def
	default:
		return def
	}
}

// utf16Len returns the number of UTF-16 code units in s (matching Java's String.length()).
func utf16Len(s string) int {
	isASCII := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			isASCII = false
			break
		}
	}
	if isASCII {
		return len(s)
	}
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// truncateChars truncates s to at most max UTF-16 code units without splitting a rune.
func truncateChars(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	n := 0
	for i, r := range s {
		step := 1
		if r >= 0x10000 {
			step = 2
		}
		if n+step > max {
			return s[:i]
		}
		n += step
	}
	return s
}

func truncateTitle(s string) string {
	if utf16Len(s) > titleMax {
		return truncateChars(s, titleMax) + "..."
	}
	return s
}
