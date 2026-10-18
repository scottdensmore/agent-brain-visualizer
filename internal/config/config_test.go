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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDotEnvParsesKeyValuePairs(t *testing.T) {
	values := ParseDotEnv([]string{"AI_PROVIDER=ollama", "OLLAMA_MODEL=gemma4"})
	if values["AI_PROVIDER"] != "ollama" {
		t.Errorf("AI_PROVIDER = %q, want ollama", values["AI_PROVIDER"])
	}
	if values["OLLAMA_MODEL"] != "gemma4" {
		t.Errorf("OLLAMA_MODEL = %q, want gemma4", values["OLLAMA_MODEL"])
	}
}

func TestDotEnvIgnoresBlankLinesAndComments(t *testing.T) {
	values := ParseDotEnv([]string{"", "   ", "# a comment", "#KEY=ignored", "REAL=1"})
	if len(values) != 1 || values["REAL"] != "1" {
		t.Errorf("values = %v, want REAL=1", values)
	}
}

func TestDotEnvToleratesExportPrefixAndSurroundingWhitespace(t *testing.T) {
	values := ParseDotEnv([]string{"export GEMINI_API_KEY=abc123", "  SPACED  =  value  "})
	if values["GEMINI_API_KEY"] != "abc123" || values["SPACED"] != "value" {
		t.Errorf("values = %v", values)
	}
}

func TestDotEnvStripsQuotesAndKeepsQuotedSpecialCharacters(t *testing.T) {
	values := ParseDotEnv([]string{
		`A="a value # not a comment"`,
		`B='single'`,
		`C=plain`,
	})
	if values["A"] != "a value # not a comment" || values["B"] != "single" || values["C"] != "plain" {
		t.Errorf("values = %v", values)
	}
}

func TestDotEnvDropsTrailingInlineCommentsOnUnquotedValues(t *testing.T) {
	values := ParseDotEnv([]string{"PORT=9090 # the server port"})
	if values["PORT"] != "9090" {
		t.Errorf("PORT = %q, want 9090", values["PORT"])
	}
}

func TestDotEnvDropsTrailingCommentsAfterQuotedValueWithoutKeepingQuotes(t *testing.T) {
	values := ParseDotEnv([]string{
		`GEMINI_API_KEY="sk-secret" # my key`,
		`OTHER='single' # note`,
		`EMPTY=""`,
	})
	if values["GEMINI_API_KEY"] != "sk-secret" || values["OTHER"] != "single" || values["EMPTY"] != "" {
		t.Errorf("values = %v", values)
	}
}

func TestDotEnvToleratesTabAfterExportAndLeadingBOM(t *testing.T) {
	values := ParseDotEnv([]string{
		"\uFEFFAI_PROVIDER=ollama",
		"export\tGEMINI_MODEL=fast",
	})
	if values["AI_PROVIDER"] != "ollama" || values["GEMINI_MODEL"] != "fast" {
		t.Errorf("values = %v", values)
	}
}

func TestDotEnvKeepsValuesContainingEqualsSigns(t *testing.T) {
	values := ParseDotEnv([]string{"TOKEN=abc=def=="})
	if values["TOKEN"] != "abc=def==" {
		t.Errorf("TOKEN = %q", values["TOKEN"])
	}
}

func TestDotEnvSkipsMalformedLines(t *testing.T) {
	values := ParseDotEnv([]string{"no-equals-sign", "=novalue", "OK=1"})
	if len(values) != 1 || values["OK"] != "1" {
		t.Errorf("values = %v", values)
	}
}

func TestDotEnvHandlesNilAndEmptyInput(t *testing.T) {
	if len(ParseDotEnv(nil)) != 0 || len(ParseDotEnv([]string{})) != 0 {
		t.Error("expected empty map")
	}
}

func TestDotEnvLoadsFromFileAndYieldsNothingWhenMissing(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".env")
	if err := os.WriteFile(file, []byte("AI_PROVIDER=ollama\n# comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadDotEnv(file)["AI_PROVIDER"]; got != "ollama" {
		t.Errorf("got %q, want ollama", got)
	}
	if len(LoadDotEnv(filepath.Join(dir, "absent.env"))) != 0 {
		t.Error("expected empty map for absent file")
	}
	if len(LoadDotEnv(dir)) != 0 {
		t.Error("expected empty map for directory")
	}
}

func TestDotEnvCanBeDisabled(t *testing.T) {
	SetDotEnvEnabled(false)
	defer SetDotEnvEnabled(true)
	if DotEnvEnabled() {
		t.Error("expected DotEnvEnabled() == false")
	}
	if len(DotEnvValues()) != 0 {
		t.Error("expected empty DotEnvValues()")
	}
	if _, ok := DotEnvGet("GEMINI_API_KEY"); ok {
		t.Error("expected absent key")
	}
}

func TestAiConfigBehavior(t *testing.T) {
	cfg := func(provider, key string) *AiConfig {
		return NewAiConfig(provider, key, "", "", "")
	}
	if cfg("", "k").Provider() != ProviderGemini {
		t.Error("expected GEMINI default")
	}
	for _, p := range []string{"ollama", "OLLAMA", " Ollama "} {
		if cfg(p, "").Provider() != ProviderOllama {
			t.Errorf("expected OLLAMA for %q", p)
		}
	}
	if cfg("gemini", "").IsConfigured() || cfg("gemini", "   ").IsConfigured() {
		t.Error("gemini without key must not be configured")
	}
	if !cfg("gemini", "secret").IsConfigured() {
		t.Error("gemini with key must be configured")
	}
	if !cfg("ollama", "").IsConfigured() {
		t.Error("ollama without key must be configured")
	}
	if !strings.Contains(cfg("gemini", "").NotConfiguredMessage(), "GEMINI_API_KEY") {
		t.Error("NotConfiguredMessage should mention GEMINI_API_KEY")
	}

	defaults := NewAiConfig("ollama", "", "", "", "")
	if defaults.GeminiModel() != "gemini-3.5-flash" ||
		defaults.OllamaBaseURL() != "http://localhost:11434" ||
		defaults.OllamaModel() != "gemma4" {
		t.Errorf("unexpected defaults: %+v", defaults)
	}

	custom := NewAiConfig("ollama", "", "gemini-custom", "http://remote:9999", "gemma4:31b")
	if custom.GeminiModel() != "gemini-custom" ||
		custom.OllamaBaseURL() != "http://remote:9999" ||
		custom.OllamaModel() != "gemma4:31b" {
		t.Errorf("unexpected custom values: %+v", custom)
	}
}

func TestApiSecurityAdvisoryClassification(t *testing.T) {
	const loopback = "127.0.0.1"
	if ClassifyApiPosture(NewApiConfig("t", false), loopback) != ApiPostureAuthenticated {
		t.Error("expected AUTHENTICATED")
	}
	if ClassifyApiPosture(NewApiConfig("t", false), "0.0.0.0") != ApiPostureAuthenticated {
		t.Error("expected AUTHENTICATED")
	}
	if ClassifyApiPosture(NewApiConfig("t", true), "") != ApiPostureAuthenticated {
		t.Error("expected AUTHENTICATED")
	}
	if ClassifyApiPosture(NewApiConfig("", false), loopback) != ApiPostureLoopbackOnly {
		t.Error("expected LOOPBACK_ONLY")
	}
	for _, host := range []string{"0.0.0.0", "192.168.1.10", "::", ""} {
		if ClassifyApiPosture(NewApiConfig("", false), host) != ApiPostureExposed {
			t.Errorf("expected EXPOSED for %q", host)
		}
	}
	if ClassifyApiPosture(NewApiConfig("", true), loopback) != ApiPostureMisconfigured {
		t.Error("expected MISCONFIGURED")
	}
	if ClassifyApiPosture(NewApiConfig("   ", true), "0.0.0.0") != ApiPostureMisconfigured {
		t.Error("expected MISCONFIGURED")
	}
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "[::1]", " 127.0.0.1 "} {
		if !IsLoopback(host) {
			t.Errorf("%q should be loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "::", "10.0.0.5", "example.com", "", "127.0.0.1.evil.com"} {
		if IsLoopback(host) {
			t.Errorf("%q should not be loopback", host)
		}
	}
	if !strings.Contains(DescribeBind(""), "every interface") {
		t.Error("DescribeBind('') should mention every interface")
	}
	if !strings.Contains(DescribeBind(loopback), "this machine only") {
		t.Error("DescribeBind(loopback) should mention this machine only")
	}
	if !strings.Contains(DescribeBind("0.0.0.0"), "reachable off this machine") {
		t.Error("DescribeBind('0.0.0.0') should mention reachable off this machine")
	}
}

func TestIngestSecurityAdvisoryClassification(t *testing.T) {
	if ClassifyIngestPosture(NewIngestConfig("t", false)) != IngestPostureAuthenticated {
		t.Error("expected AUTHENTICATED")
	}
	if ClassifyIngestPosture(NewIngestConfig("t", true)) != IngestPostureAuthenticated {
		t.Error("expected AUTHENTICATED")
	}
	if ClassifyIngestPosture(NewIngestConfig("", false)) != IngestPostureOpen {
		t.Error("expected OPEN")
	}
	if ClassifyIngestPosture(NewIngestConfig("", true)) != IngestPostureMisconfigured {
		t.Error("expected MISCONFIGURED")
	}
	if ClassifyIngestPosture(NewIngestConfig("   ", true)) != IngestPostureMisconfigured {
		t.Error("expected MISCONFIGURED")
	}
}

func TestLogSafeEscaping(t *testing.T) {
	recordBoundaries := []rune{
		'\n', '\r', 0x0b, '\f', 0x00, 0x1b, 0x85, 0x7f, 0x9b, 0x2028, 0x2029,
	}
	if EscapeLog("antigravity-cli") != "antigravity-cli" {
		t.Error("ordinary value modified")
	}
	if EscapeLog("héllo — 世界") != "héllo — 世界" {
		t.Error("unicode value modified")
	}
	if EscapeLog("rocket 🚀 id") != "rocket 🚀 id" {
		t.Error("emoji modified")
	}
	if EscapeLog("s1\nWARN forged") != "s1\\nWARN forged" {
		t.Errorf("got %q", EscapeLog("s1\nWARN forged"))
	}
	if EscapeLog("s1\r\nWARN forged") != "s1\\r\\nWARN forged" {
		t.Errorf("got %q", EscapeLog("s1\r\nWARN forged"))
	}
	if EscapeLog("s1\tcol") != "s1\\tcol" {
		t.Errorf("got %q", EscapeLog("s1\tcol"))
	}
	for _, b := range recordBoundaries {
		escaped := EscapeLog("s1" + string(b) + "forged")
		if strings.ContainsAny(escaped, "\r\n") {
			t.Errorf("boundary %U left newline in %q", b, escaped)
		}
		if !strings.HasPrefix(escaped, "s1") || !strings.HasSuffix(escaped, "forged") {
			t.Errorf("unexpected escaped %q", escaped)
		}
	}
	if EscapeLog("s1"+"\x1b"+"forged") != "s1\\u001Bforged" {
		t.Errorf("got %q", EscapeLog("s1"+"\x1b"+"forged"))
	}
	if EscapeLog("s1"+"\u2028"+"forged") != "s1\\u2028forged" {
		t.Errorf("got %q", EscapeLog("s1"+"\u2028"+"forged"))
	}
	if got := EscapeLog(strings.Repeat("a", 5000)); got != strings.Repeat("a", 512)+"...[truncated]" {
		t.Errorf("unexpected truncation: len=%d", len(got))
	}
	if got := EscapeLog(strings.Repeat("a", 512)); got != strings.Repeat("a", 512) {
		t.Errorf("512-char string should not be truncated")
	}
	if got := EscapeLog(strings.Repeat("\n", 600)); got != strings.Repeat("\\n", 512)+"...[truncated]" {
		t.Errorf("unexpected newline truncation")
	}
	// Surrogate pair straddling 512 UTF-16 code units:
	if got := EscapeLog(strings.Repeat("a", 511) + "🚀tail"); got != strings.Repeat("a", 511)+"...[truncated]" {
		t.Errorf("surrogate pair straddling cap should be dropped cleanly, got %q", got)
	}
	if got := EscapeLog(strings.Repeat("a", 510) + "🚀tail"); got != strings.Repeat("a", 510)+"🚀...[truncated]" {
		t.Errorf("surrogate pair fitting inside cap should survive whole, got %q", got)
	}
}
