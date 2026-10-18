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
	"log"
	"strings"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
)

const (
	// MaxSummaryChars is the largest cached analysis the server will store for one session (256 KiB).
	MaxSummaryChars = 256 * 1024

	// MaxFileChars is the largest attached file the server will store for a session (512 KiB).
	MaxFileChars = 512 * 1024

	// MaxFilesPerSession is the maximum number of attached files stored for a single session.
	MaxFilesPerSession = 50
)

type ingestOutcome int

const (
	outcomeIngested ingestOutcome = iota
	outcomeSkipped
	outcomeFailed
)

// Ingestor normalizes pushed trajectories and summaries and stores them in the trajectory repositories.
type Ingestor struct {
	normalizers  []SourceNormalizer
	sessions     *store.SessionRepository
	summaries    *store.SummaryRepository
	sessionFiles *store.SessionFileRepository
}

// NewIngestor creates an Ingestor backed by the provided SourceNormalizers and store repositories.
func NewIngestor(
	normalizers []SourceNormalizer,
	sessions *store.SessionRepository,
	summaries *store.SummaryRepository,
	sessionFiles *store.SessionFileRepository,
) *Ingestor {
	if normalizers == nil {
		normalizers = DefaultNormalizers()
	}
	return &Ingestor{
		normalizers:  normalizers,
		sessions:     sessions,
		summaries:    summaries,
		sessionFiles: sessionFiles,
	}
}

// Ingest normalizes and stores a batch of pushed trajectories, counting each as ingested, skipped, or failed.
// A database error when storing a session returns *store.StoreUnavailableError.
func (ing *Ingestor) Ingest(batch []IngestSession) (IngestResult, error) {
	var res IngestResult
	for _, pushed := range batch {
		outcome, err := ing.storeSession(pushed)
		if err != nil {
			return IngestResult{}, err
		}
		switch outcome {
		case outcomeIngested:
			res.Ingested++
		case outcomeSkipped:
			res.Skipped++
		case outcomeFailed:
			res.Failed++
		}
	}
	return res, nil
}

// IngestSummaries stores a batch of standalone cached analyses, counting each as ingested, skipped, or failed.
// Non-JSON or oversized bodies are counted as Failed without calling the store; a store error propagates out.
func (ing *Ingestor) IngestSummaries(batch []IngestSummary) (IngestResult, error) {
	var res IngestResult
	for _, pushed := range batch {
		outcome, err := ing.storeSummary(pushed)
		if err != nil {
			return IngestResult{}, err
		}
		switch outcome {
		case outcomeIngested:
			res.Ingested++
		case outcomeSkipped:
			res.Skipped++
		case outcomeFailed:
			res.Failed++
		}
	}
	return res, nil
}

func (ing *Ingestor) storeSummary(pushed IngestSummary) (ingestOutcome, error) {
	if strings.TrimSpace(pushed.ID) == "" ||
		strings.TrimSpace(pushed.Source) == "" ||
		strings.TrimSpace(pushed.Summary) == "" {
		log.Printf("WARN  Ingestor - Rejecting a pushed summary with no source, id, or body")
		return outcomeFailed, nil
	}
	if _, ok := ing.normalizerFor(pushed.Source); !ok {
		log.Printf(
			"WARN  Ingestor - Rejecting summary for %s: unknown source '%s'",
			config.EscapeLog(pushed.ID),
			config.EscapeLog(pushed.Source),
		)
		return outcomeFailed, nil
	}
	summaryLen := utf16Len(pushed.Summary)
	if summaryLen > MaxSummaryChars {
		log.Printf(
			"WARN  Ingestor - Rejecting summary for %s: body is %d chars, over the %d limit",
			config.EscapeLog(pushed.ID),
			summaryLen,
			MaxSummaryChars,
		)
		return outcomeFailed, nil
	}
	if !isValidJSON(pushed.Summary) {
		log.Printf(
			"WARN  Ingestor - Rejecting summary for %s: body is not valid JSON",
			config.EscapeLog(pushed.ID),
		)
		return outcomeFailed, nil
	}
	written, err := ing.summaries.Upsert(pushed.Source, pushed.ID, pushed.Summary, "")
	if err != nil {
		return outcomeFailed, err
	}
	if written {
		return outcomeIngested, nil
	}
	return outcomeSkipped, nil
}

func (ing *Ingestor) storeSession(pushed IngestSession) (ingestOutcome, error) {
	if strings.TrimSpace(pushed.ID) == "" || strings.TrimSpace(pushed.Source) == "" {
		log.Printf("WARN  Ingestor - Rejecting a pushed trajectory with no source or id")
		return outcomeFailed, nil
	}
	normalizer, ok := ing.normalizerFor(pushed.Source)
	if !ok {
		log.Printf(
			"WARN  Ingestor - Rejecting trajectory %s: unknown source '%s'",
			config.EscapeLog(pushed.ID),
			config.EscapeLog(pushed.Source),
		)
		return outcomeFailed, nil
	}

	raw := pushed.Raw
	lines := splitLines(raw)
	stepsJSON := normalizer.ToStepsJSON(lines)
	title := ing.titleFor(pushed, normalizer, lines)

	mtime := pushed.SourceMtime
	if mtime <= 0 {
		mtime = time.Now().UnixMilli()
	}

	written, err := ing.sessions.Upsert(store.Session{
		Source:      pushed.Source,
		ID:          pushed.ID,
		Title:       title,
		SourceMtime: mtime,
		StepsJSON:   stepsJSON,
		ContentHash: Sha256(raw),
	})
	if err != nil {
		return outcomeFailed, err
	}

	accepted := ing.acceptableFiles(pushed)
	if len(accepted) > 0 || len(pushed.Files) > 0 {
		if err := ing.sessionFiles.ReplaceForSession(pushed.Source, pushed.ID, accepted); err != nil {
			log.Printf(
				"WARN  Ingestor - Stored trajectory %s but could not store its attached files: %s",
				config.EscapeLog(pushed.ID),
				config.EscapeLog(err.Error()),
			)
		}
	}

	if strings.TrimSpace(pushed.Summary) != "" {
		summaryLen := utf16Len(pushed.Summary)
		if summaryLen > MaxSummaryChars {
			log.Printf(
				"WARN  Ingestor - Stored trajectory %s but dropped its pushed summary: %d chars, over the %d limit",
				config.EscapeLog(pushed.ID),
				summaryLen,
				MaxSummaryChars,
			)
		} else if !isValidJSON(pushed.Summary) {
			log.Printf(
				"WARN  Ingestor - Stored trajectory %s but could not store its pushed summary: body is not valid JSON",
				config.EscapeLog(pushed.ID),
			)
		} else if _, err := ing.summaries.Upsert(pushed.Source, pushed.ID, pushed.Summary, title); err != nil {
			log.Printf(
				"WARN  Ingestor - Stored trajectory %s but could not store its pushed summary: %s",
				config.EscapeLog(pushed.ID),
				config.EscapeLog(err.Error()),
			)
		}
	}

	if written {
		return outcomeIngested, nil
	}
	return outcomeSkipped, nil
}

func (ing *Ingestor) acceptableFiles(pushed IngestSession) []store.IngestFile {
	accepted := make([]store.IngestFile, 0, len(pushed.Files))
	for _, file := range pushed.Files {
		if strings.TrimSpace(file.Path) == "" {
			continue
		}
		if len(accepted) >= MaxFilesPerSession {
			log.Printf(
				"WARN  Ingestor - Trajectory %s attached more than %d files; the rest were dropped",
				config.EscapeLog(pushed.ID),
				MaxFilesPerSession,
			)
			break
		}
		fileLen := utf16Len(file.Content)
		if fileLen > MaxFileChars {
			log.Printf(
				"WARN  Ingestor - Dropping attached file %s for %s: %d chars, over the %d limit",
				config.EscapeLog(file.Path),
				config.EscapeLog(pushed.ID),
				fileLen,
				MaxFileChars,
			)
			continue
		}
		accepted = append(accepted, file)
	}
	return accepted
}

func (ing *Ingestor) normalizerFor(source string) (SourceNormalizer, bool) {
	if ing == nil {
		return nil, false
	}
	for _, n := range ing.normalizers {
		if n != nil && n.Handles(source) {
			return n, true
		}
	}
	return nil, false
}

func (ing *Ingestor) titleFor(pushed IngestSession, normalizer SourceNormalizer, lines []string) string {
	if strings.TrimSpace(pushed.Title) != "" {
		return strings.TrimSpace(pushed.Title)
	}
	if derived, ok := normalizer.DeriveTitle(lines); ok && strings.TrimSpace(derived) != "" {
		return derived
	}
	return pushed.Source + " session " + shortID(pushed.ID)
}

func shortID(id string) string {
	return truncateChars(id, 8)
}

// Sha256 returns the lowercase hex SHA-256 of raw's UTF-8 bytes.
func Sha256(raw string) string {
	return store.Sha256Hex(raw)
}

func isValidJSON(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	var raw json.RawMessage
	return json.Unmarshal([]byte(value), &raw) == nil
}

func splitLines(raw string) []string {
	if raw == "" {
		return []string{}
	}
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}
