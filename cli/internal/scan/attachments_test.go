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
	"strings"
	"testing"
)

// mention builds the metadata block shape Antigravity writes, which is the only
// thing ReferencedFiles collects.
func mention(name, path string) string {
	return "@[" + name + "] is a [File]:\n" + path + "\n"
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReferencedFilesCollectsAnAttachedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "notes.md", "hello notes")

	files, skipped := ReferencedFiles(mention("notes.md", path))

	if len(files) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(files))
	}
	if files[0].Path != path || files[0].Content != "hello notes" {
		t.Fatalf("unexpected attachment: %+v", files[0])
	}
	if len(skipped) != 0 {
		t.Fatalf("want no skips, got %v", skipped)
	}
}

func TestReferencedFilesRefusesCredentialMaterial(t *testing.T) {
	dir := t.TempDir()
	// Each of these is a real file, so only the name-based refusal can keep it out.
	for _, name := range []string{
		".env",
		".env.production",
		"id_rsa",
		"server.pem",
		"private.key",
		"oauth_creds.json",
		filepath.Join(".ssh", "config"),
	} {
		path := writeFile(t, dir, name, "SECRET=do-not-upload")
		files, skipped := ReferencedFiles(mention(filepath.Base(name), path))
		if len(files) != 0 {
			t.Errorf("%s was attached but must not be", name)
		}
		if len(skipped) != 1 || !strings.Contains(skipped[0], "credential") {
			t.Errorf("%s should be reported as a credential skip, got %v", name, skipped)
		}
	}
}

func TestReferencedFilesRefusesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "huge.log", strings.Repeat("x", MaxAttachmentBytes+1))

	files, skipped := ReferencedFiles(mention("huge.log", path))

	if len(files) != 0 {
		t.Fatalf("an oversized file must not be attached, got %d", len(files))
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "size limit") {
		t.Fatalf("want a size-limit skip, got %v", skipped)
	}
}

func TestReferencedFilesBoundsHowManyItAttaches(t *testing.T) {
	dir := t.TempDir()
	var raw strings.Builder
	for i := 0; i < MaxAttachmentsPerSession+5; i++ {
		name := filepath.Join("many", string(rune('a'+i%26))+string(rune('a'+i/26))+".txt")
		raw.WriteString(mention(filepath.Base(name), writeFile(t, dir, name, "x")))
	}

	files, skipped := ReferencedFiles(raw.String())

	if len(files) != MaxAttachmentsPerSession {
		t.Fatalf("want %d attachments, got %d", MaxAttachmentsPerSession, len(files))
	}
	if len(skipped) != 5 {
		t.Fatalf("the 5 over the limit should be reported, got %v", skipped)
	}
}

func TestReferencedFilesIgnoresWhatItShould(t *testing.T) {
	dir := t.TempDir()
	real := writeFile(t, dir, "real.txt", "kept")

	raw := strings.Join([]string{
		mention("real.txt", real),
		mention("real.txt", real),                               // duplicate: attached once
		mention("gone.txt", filepath.Join(dir, "missing.txt")),  // never existed
		mention("dir", dir),                                     // a directory
		mention("rel.txt", "relative/path.txt"),                 // not absolute
		"a bare /etc/passwd mentioned in prose, not attached\n", // not the metadata shape
	}, "")

	files, _ := ReferencedFiles(raw)

	if len(files) != 1 || files[0].Path != real {
		t.Fatalf("want only the one real absolute file, got %+v", files)
	}
}

func TestReferencedFilesSanitizesInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary.txt")
	if err := os.WriteFile(path, []byte{0xff, 0xfe, 'o', 'k'}, 0o600); err != nil {
		t.Fatal(err)
	}

	files, _ := ReferencedFiles(mention("binary.txt", path))

	if len(files) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(files))
	}
	// Same coercion transcripts get, so an attachment can't carry bytes the store rejects.
	if !strings.HasSuffix(files[0].Content, "ok") || strings.ContainsRune(files[0].Content, 0xff) {
		t.Fatalf("content was not sanitized: %q", files[0].Content)
	}
}
