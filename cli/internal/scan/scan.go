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

// Package scan locates an agent tool's local transcripts and turns each into a
// stable, hashed Session ready to push. It deliberately does not parse the
// transcript formats — the server normalizes them — so the CLI never has to
// track how Antigravity, Codex, or Claude Code lay out their steps.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// warnWriter receives scan warnings (stderr in the binary); it is a package
// variable only so tests can capture the output.
var warnWriter io.Writer = os.Stderr

// Source names, which are also the `source` the server stores each session under.
const (
	SourceAntigravityCLI = "antigravity-cli"
	SourceAntigravityIDE = "antigravity-ide"
	SourceAntigravity    = "antigravity"
	SourceCodex          = "codex"
	SourceClaudeCode     = "claude-code"
)

// AllSources is every source the CLI knows how to scan, in listing order.
var AllSources = []string{SourceAntigravityCLI, SourceAntigravityIDE, SourceAntigravity, SourceCodex, SourceClaudeCode}

// Tool-owned cache directory this CLI's server writes next to a source's
// sessions; it holds summaries, never transcripts, so never descend into it.
const cacheDir = ".agybrainviz"

// Session is one local trajectory, ready to push. Title is intentionally left
// empty: the server derives a consistent title from the transcript.
type Session struct {
	Source  string
	ID      string
	Path    string // transcript file the session was scanned from
	Mtime   int64  // file modification time, epoch milliseconds
	Raw     string // sanitized transcript content; empty on a cache hit until LoadRaw
	Hash    string // hex SHA-256 of the sanitized content, matching the server's content hash
	Summary string // the tool's own AI analysis JSON, or "" when there is none

	// rawLoaded distinguishes "content not read yet" (a cache hit) from an
	// actually loaded Raw, so LoadRaw knows whether the file must be read.
	rawLoaded bool
}

// LoadRaw ensures Raw holds the transcript content, sanitized exactly as a
// full scan would have (invalid UTF-8 coerced to U+FFFD). A session read
// fully at scan time is untouched; one whose read was skipped by the hash
// cache is read now. Call it before uploading a session's content.
func (s *Session) LoadRaw() error {
	if s.rawLoaded {
		return nil
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return err
	}
	s.Raw = strings.ToValidUTF8(string(raw), "�")
	s.rawLoaded = true
	return nil
}

// Sha256Hex returns the lowercase hex SHA-256 of s, byte-for-byte identical to
// the server's hash of the same raw transcript.
func Sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Scan collects the sessions for the requested sources under home. A source
// whose directory is absent contributes nothing; an unknown source is an error.
func Scan(home string, sources []string) ([]Session, error) {
	return ScanWithCache(home, sources, nil)
}

// ScanWithCache is Scan with an optional hash cache: a file whose path, size,
// and mtime match its cache entry reuses the cached hash instead of being read
// and re-hashed (its Raw stays unloaded until LoadRaw). A nil cache forces
// full hashing.
func ScanWithCache(home string, sources []string, cache *Cache) ([]Session, error) {
	var out []Session
	for _, source := range sources {
		var (
			sessions []Session
			err      error
		)
		switch source {
		case SourceClaudeCode:
			sessions, err = scanFlat(source, filepath.Join(home, ".claude", "projects"), cache)
		case SourceCodex:
			sessions, err = scanFlat(source, filepath.Join(home, ".codex", "sessions"), cache)
		case SourceAntigravityCLI, SourceAntigravityIDE, SourceAntigravity:
			sessions, err = scanAntigravity(source, filepath.Join(home, ".gemini", source, "brain"), cache)
		default:
			return nil, fmt.Errorf("unknown source %q (known: %s)", source, strings.Join(AllSources, ", "))
		}
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", source, err)
		}
		out = append(out, sessions...)
	}
	return out, nil
}

// scanFlat walks a tree of `<id>.jsonl` files (Codex, Claude Code), taking the
// id from the filename stem — never from the path, so the same session
// de-duplicates no matter which machine or project directory produced it.
func scanFlat(source, root string, cache *Cache) ([]Session, error) {
	if !isDir(root) {
		return nil, nil
	}
	var sessions []Session
	seen := map[string]string{} // id -> path already kept, to catch filename-stem collisions
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory or entry must not abort syncing the machine;
			// skip it and carry on with what is readable.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == cacheDir {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		id := strings.TrimSuffix(d.Name(), ".jsonl")
		if prev, dup := seen[id]; dup {
			// Two different files with the same stem would collide on (source, id)
			// and silently overwrite each other on the server (last writer wins).
			// Keep the first — its id may already be stored — and warn about the
			// one skipped rather than qualify the id, which would re-ingest
			// already-stored sessions under new ids.
			fmt.Fprintf(warnWriter, "agent-ingest: warning: duplicate %s session id %q: keeping %s, skipping %s\n", source, id, prev, path)
			return nil
		}
		session, ok, err := readSession(source, id, path, cache)
		if err != nil {
			return nil // skip an unreadable transcript rather than failing the source
		}
		if ok {
			seen[id] = path
			sessions = append(sessions, session)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// scanAntigravity lists the session directories under a flavor's brain and reads
// each one's transcript, preferring transcript_full.jsonl when present (the
// richer capture) exactly as the server does. The id is the directory name.
func scanAntigravity(source, brain string, cache *Cache) ([]Session, error) {
	if !isDir(brain) {
		return nil, nil
	}
	entries, err := os.ReadDir(brain)
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		logs := filepath.Join(brain, entry.Name(), ".system_generated", "logs")
		transcript := firstExisting(
			filepath.Join(logs, "transcript_full.jsonl"),
			filepath.Join(logs, "transcript.jsonl"),
		)
		if transcript == "" {
			continue
		}
		session, ok, err := readSession(source, entry.Name(), transcript, cache)
		if err != nil {
			continue // skip an unreadable transcript rather than failing the source
		}
		if ok {
			// Antigravity caches its own AI analysis beside the transcript; carry it so a
			// summary generated on this machine reaches the store without a recompute.
			session.Summary = readSummary(filepath.Join(logs, "summary.json"))
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

// readSummary returns the tool's on-disk summary JSON, or "" when it is absent,
// empty, or not valid JSON. The server stores it in a jsonb column, so sending
// anything but valid JSON would fail the whole push — drop it rather than risk that.
func readSummary(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return ""
	}
	content := strings.ToValidUTF8(string(raw), "�")
	if !json.Valid([]byte(content)) {
		return ""
	}
	return content
}

// readSession reads one transcript into a hashed Session, reporting ok=false for
// an empty file (a session that never really started, which the server ignores).
// When the cache proves the file unchanged since it was last hashed, the read is
// skipped entirely and the Session carries only the cached hash — its content is
// loaded on demand (LoadRaw) in the rare case the server still wants it.
func readSession(source, id, path string, cache *Cache) (Session, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Session{}, false, err
	}
	if info.Size() == 0 {
		return Session{}, false, nil
	}
	if hash, ok := cache.lookup(source, id, path, info); ok {
		return Session{
			Source: source,
			ID:     id,
			Path:   path,
			Mtime:  info.ModTime().UnixMilli(),
			Hash:   hash,
		}, true, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Session{}, false, err
	}
	if len(raw) == 0 {
		return Session{}, false, nil
	}
	// Send and hash the UTF-8-coerced content the server will actually store. JSON
	// transport replaces invalid bytes with U+FFFD, so hashing the raw bytes would
	// diverge from the server's hash and re-upload the session on every run.
	content := strings.ToValidUTF8(string(raw), "�")
	hash := Sha256Hex(content)
	// Cache under the pre-read stat: if the file changed between stat and read,
	// the next run's stat won't match and the entry self-invalidates.
	cache.store(source, id, path, info, hash)
	return Session{
		Source:    source,
		ID:        id,
		Path:      path,
		Mtime:     info.ModTime().UnixMilli(),
		Raw:       content,
		Hash:      hash,
		rawLoaded: true,
	}, true, nil
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Validate reports the first unknown source name, if any.
func Validate(sources []string) error {
	for _, s := range sources {
		if !slices.Contains(AllSources, s) {
			return fmt.Errorf("unknown source %q (known: %s)", s, strings.Join(AllSources, ", "))
		}
	}
	return nil
}

// SortedByKey returns sessions ordered by source then id, for stable output.
func SortedByKey(sessions []Session) []Session {
	out := slices.Clone(sessions)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].ID < out[j].ID
	})
	return out
}
