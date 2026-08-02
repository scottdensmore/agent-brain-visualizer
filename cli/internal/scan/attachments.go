// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scan

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Attachment is one file a transcript referenced, carried alongside the session
// so the viewer can preview it from any machine.
type Attachment struct {
	Path    string `json:"path"`    // absolute path as the transcript spelled it
	Content string `json:"content"` // UTF-8 sanitized file content
}

const (
	// MaxAttachmentBytes bounds one attached file. Mentions are things a person
	// attached to a prompt — source files, configs, notes — so this is generous
	// for that and still refuses a log or a dump that happened to be mentioned.
	MaxAttachmentBytes = 256 * 1024
	// MaxAttachmentsPerSession bounds how many are carried, so a transcript that
	// mentions hundreds of files cannot turn one push into a huge request.
	MaxAttachmentsPerSession = 25
)

// mentionRe matches Antigravity's file-mention metadata, the same shape the web
// UI turns into a clickable preview link:
//
//	@[name.txt] is a [File]:
//	/absolute/path/to/name.txt
//
// Only these explicit references are collected. Paths that merely appear in tool
// output are not: a mention is something the user deliberately attached, which is
// what makes uploading it reasonable.
// Unbounded repeats are safe here: Go's regexp is RE2, which matches in linear
// time and cannot backtrack, so a hostile transcript cannot make this expensive.
var mentionRe = regexp.MustCompile(`@\[[^\]\n]*\] is a \[File\]:\n([^\n]+)`)

// secretNames are refused whatever a transcript says, because a file attached to
// a prompt is not necessarily one the person wanted copied to a shared server.
// Matched case-insensitively against the base name.
var secretNames = map[string]bool{
	".env":                 true,
	".netrc":               true,
	".pgpass":              true,
	"credentials":          true,
	"credentials.json":     true,
	"oauth_creds.json":     true,
	"google_accounts.json": true,
	"id_rsa":               true,
	"id_ed25519":           true,
	"id_ecdsa":             true,
	"id_dsa":               true,
}

// secretSuffixes catch the shapes a name-by-name list cannot.
var secretSuffixes = []string{
	".pem",
	".key",
	".p12",
	".pfx",
	".keystore",
	".jks",
}

// IsSecretPath reports whether a path looks like credential material. It is a
// heuristic and deliberately errs toward refusing: a file wrongly skipped costs a
// preview, while one wrongly uploaded copies a secret to a shared store.
func IsSecretPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if secretNames[base] {
		return true
	}
	// Any dotfile whose name starts with .env (.env.local, .env.production).
	if strings.HasPrefix(base, ".env") {
		return true
	}
	for _, suffix := range secretSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	// A path component that is a well-known secret directory.
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		switch strings.ToLower(part) {
		case ".ssh", ".gnupg", ".aws", ".config/gcloud", ".credentials":
			return true
		}
	}
	return false
}

// ReferencedFiles returns the files a transcript explicitly attached, read from
// disk and sanitized the same way transcript content is.
//
// Skips are silent by design and each is reported to the caller: a file that has
// moved, grown too large, or looks like a secret must not fail the session it was
// mentioned in — the transcript is what matters, the attachment is a convenience.
func ReferencedFiles(raw string) ([]Attachment, []string) {
	var out []Attachment
	var skipped []string
	seen := map[string]bool{}

	for _, m := range mentionRe.FindAllStringSubmatch(raw, -1) {
		path := strings.TrimSpace(m[1])
		if path == "" || !filepath.IsAbs(path) || seen[path] {
			continue
		}
		seen[path] = true

		if IsSecretPath(path) {
			skipped = append(skipped, path+" (looks like credential material)")
			continue
		}
		if len(out) >= MaxAttachmentsPerSession {
			skipped = append(skipped, path+" (per-session attachment limit reached)")
			continue
		}

		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue // moved, unreadable, or a directory: nothing to attach
		}
		if info.Size() > MaxAttachmentBytes {
			skipped = append(skipped, path+" (larger than the attachment size limit)")
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Sanitized exactly as transcript content is, so an attachment can never
		// carry bytes the store would reject.
		out = append(out, Attachment{
			Path:    path,
			Content: strings.ToValidUTF8(string(content), "�"),
		})
	}
	return out, skipped
}
