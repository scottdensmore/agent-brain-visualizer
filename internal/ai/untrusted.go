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

package ai

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"
)

const (
	// TranscriptTag is the base XML tag name used for untrusted transcript blocks.
	TranscriptTag = "untrusted_transcript"
	// AnalysisTag is the base XML tag name used for untrusted prior analysis blocks.
	AnalysisTag = "untrusted_analysis"
	// EvidenceTag is the base XML tag name used for untrusted mined evidence blocks.
	EvidenceTag = "untrusted_evidence"
	// MarkerPlaceholder replaces any "untrusted..." marker sequences inside untrusted payloads.
	MarkerPlaceholder = "[marker removed]"
)

var (
	// Matches any attempt to spoof or reference an "untrusted..." fence tag or marker, even
	// when split with punctuation, symbols, or whitespace (e.g. "</untrusted_transcript_deadbeef>",
	// "untrusted-analysis", "UNTRUSTED_EVIDENCE").
	markerPattern     = regexp.MustCompile(`(?i)untrusted[\p{P}\p{S}\s]*\w*`)
	multiSpacePattern = regexp.MustCompile(`\s{2,}`)
)

// FenceTranscript wraps an untrusted session transcript in a per-call random-nonce fence.
func FenceTranscript(body string) string {
	return Fenced(TranscriptTag, body)
}

// FenceAnalysis wraps a previously-stored (or partial) analysis payload in a random-nonce fence.
func FenceAnalysis(body string) string {
	return Fenced(AnalysisTag, body)
}

// FenceEvidence wraps mined fleet evidence (issue strings, command errors, prompt snippets) in a random-nonce fence.
func FenceEvidence(body string) string {
	return Fenced(EvidenceTag, body)
}

// Fenced wraps body in a per-call random-nonce XML-like tag (<tag_nonce>...</tag_nonce>) after
// scrubbing control/format characters and neutralizing any "untrusted..." marker substrings.
func Fenced(tag, body string) string {
	nonce := randomHex64()
	open := "<" + tag + "_" + nonce + ">"
	closeTag := "</" + tag + "_" + nonce + ">"
	cleaned := StripControlAndFormat(body)
	return open + "\n" + cleaned + "\n" + closeTag
}

// StripControlAndFormat strips control characters, Unicode format characters (bidi overrides,
// zero-width joiners, invisible separators, language tags), and neutralizes any "untrusted..."
// substrings so attacker content cannot close or impersonate a fence tag. Preserves \n and \t.
func StripControlAndFormat(text string) string {
	if text == "" {
		return ""
	}
	scrubbed := scrub(text, true)
	return markerPattern.ReplaceAllString(scrubbed, MarkerPlaceholder)
}

// Inert is an alias for StripControlAndFormat.
func Inert(text string) string {
	return StripControlAndFormat(text)
}

// BoundedLine collapses text to a single line (newlines/tabs become spaces, runs of whitespace
// collapsed), strips control/format chars, and truncates to maxChars runes with an ellipsis.
func BoundedLine(text string, maxChars int) string {
	if text == "" {
		return ""
	}
	single := multiSpacePattern.ReplaceAllString(scrub(text, false), " ")
	single = strings.TrimSpace(single)
	runes := []rune(single)
	if len(runes) <= maxChars {
		return single
	}
	return string(runes[:maxChars]) + "..."
}

// BoundedText strips control/format chars while preserving line breaks, trims outer whitespace,
// and truncates to maxChars runes with an ellipsis.
func BoundedText(text string, maxChars int) string {
	if text == "" {
		return ""
	}
	cleaned := strings.TrimSpace(scrub(text, true))
	runes := []rune(cleaned)
	if len(runes) <= maxChars {
		return cleaned
	}
	return string(runes[:maxChars]) + "..."
}

func scrub(text string, keepLineBreaks bool) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, cp := range text {
		if keepLineBreaks && (cp == '\n' || cp == '\t') {
			b.WriteRune(cp)
			continue
		}
		// Drop Unicode FORMAT (Cf) characters: zero-width space/joiners (U+200B..U+200F),
		// bidi overrides/isolates (U+202A..U+202E, U+2066..U+2069), BOM (U+FEFF),
		// soft hyphen (U+00AD), and tag characters (U+E0000..U+E007F).
		if unicode.Is(unicode.Cf, cp) || (cp >= 0xE0000 && cp <= 0xE007F) {
			continue
		}
		// Replace C0/C1 controls (including \r, \n, \t when not kept) and line/paragraph
		// separators (U+2028, U+2029) with a single space.
		if cp <= 0x001F || (cp >= 0x007F && cp <= 0x009F) ||
			unicode.Is(unicode.Cc, cp) || unicode.Is(unicode.Zl, cp) || unicode.Is(unicode.Zp, cp) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(cp)
	}
	return b.String()
}

func randomHex64() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
