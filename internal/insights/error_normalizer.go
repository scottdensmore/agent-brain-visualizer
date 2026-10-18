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

// Package insights provides cross-session fleet analytics, pattern mining,
// evaluation scoring, and prompt optimization.
package insights

import (
	"regexp"
	"strings"
)

const (
	// MaxErrorLength is the maximum length of a normalized error key (100 chars).
	MaxErrorLength = 100
	maxErrorScan   = 500
	stepErrPrefix  = "Encountered error in step execution: "
)

var (
	strongErrorSignals = []string{
		"error",
		"exception",
		"cannot",
		"no such file",
		"not found",
		"permission denied",
		"traceback",
		"panic",
		"fatal",
		"segmentation fault",
		"npm err",
	}
	weakErrorSignals = []string{
		"failed",
		"exited with code",
		"non-zero exit",
	}

	errHexRe          = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	errUUIDRe         = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	errSingleQuotedRe = regexp.MustCompile(`'[^']*'`)
	errDoubleQuotedRe = regexp.MustCompile(`"[^"]*"`)
	errPathRe         = regexp.MustCompile(`(?:[A-Za-z]:)?(?:[\w.+\-]*[/\\])+[\w.+\-]+`)
	errNumberRe       = regexp.MustCompile(`\b\d+\b`)
	whitespaceRe      = regexp.MustCompile(`\s+`)
)

// NormalizeError collapses a raw error blob into a stable, human-readable grouping key
// (or "Unknown error" when there is nothing meaningful to key on), matching ErrorNormalizer.java.
func NormalizeError(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "Unknown error"
	}
	line := salientErrorLine(raw)
	if line == "" {
		return "Unknown error"
	}
	if strings.HasPrefix(line, stepErrPrefix) {
		line = strings.TrimSpace(line[len(stepErrPrefix):])
	}
	if len(line) > maxErrorScan {
		line = line[:maxErrorScan]
	}
	key := strings.TrimSpace(canonicalizeErrorLine(line))
	if key == "" {
		return "Unknown error"
	}
	if len(key) > MaxErrorLength {
		return key[:MaxErrorLength]
	}
	return key
}

func salientErrorLine(raw string) string {
	firstMeaningful := ""
	weak := ""
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || isErrorMetadata(line) {
			continue
		}
		if firstMeaningful == "" {
			firstMeaningful = line
		}
		lower := strings.ToLower(line)
		if containsAnySubstring(lower, strongErrorSignals) {
			return line
		}
		if weak == "" && containsAnySubstring(lower, weakErrorSignals) {
			weak = line
		}
	}
	if weak != "" {
		return weak
	}
	return firstMeaningful
}

func isErrorMetadata(line string) bool {
	return strings.HasPrefix(line, "Created At:") || strings.HasPrefix(line, "Completed At:")
}

func containsAnySubstring(lower string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func canonicalizeErrorLine(s string) string {
	s = errHexRe.ReplaceAllString(s, "<hex>")
	s = errUUIDRe.ReplaceAllString(s, "<id>")
	s = errSingleQuotedRe.ReplaceAllString(s, "'<v>'")
	s = errDoubleQuotedRe.ReplaceAllString(s, "\"<v>\"")
	s = errPathRe.ReplaceAllString(s, "<path>")
	s = errNumberRe.ReplaceAllString(s, "<n>")
	s = whitespaceRe.ReplaceAllString(s, " ")
	return s
}
