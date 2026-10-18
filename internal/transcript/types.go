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

// Package transcript normalizes Antigravity, Claude Code, and OpenAI Codex
// session transcripts and ingests them into the trajectory store.
package transcript

import (
	"strings"

	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
)

// AntigravityFlavorPrefix is the prefix shared by all Antigravity source names
// ("antigravity-cli", "antigravity-ide", etc.).
const AntigravityFlavorPrefix = "antigravity"

// IsAntigravitySource reports whether source uses the native Antigravity transcript schema.
func IsAntigravitySource(source string) bool {
	return strings.HasPrefix(source, AntigravityFlavorPrefix)
}

// AgentAction represents a condensed step action in an AI analysis response.
type AgentAction struct {
	Action      string `json:"action"`
	Description string `json:"description"`
}

// SessionRef points to a session (ID + short human title) in drill-down lists and mining reports.
type SessionRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// IngestSession is one trajectory pushed to POST /api/ingest/sessions.
type IngestSession struct {
	Source      string             `json:"source"`
	ID          string             `json:"id"`
	Title       string             `json:"title,omitempty"`
	SourceMtime int64              `json:"sourceMtime"`
	Raw         string             `json:"raw"`
	Summary     string             `json:"summary,omitempty"`
	Files       []store.IngestFile `json:"files,omitempty"`
}

// IngestSummary is one standalone cached analysis pushed to POST /api/ingest/summaries.
type IngestSummary struct {
	Source  string `json:"source"`
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// IngestResult is the tally returned by POST /api/ingest/sessions and POST /api/ingest/summaries.
type IngestResult struct {
	Ingested int `json:"ingested"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}

// SourceNormalizer normalizes raw JSONL transcript lines for a specific agent source.
type SourceNormalizer interface {
	Handles(source string) bool
	ToStepsJSON(rawLines []string) string
	DeriveTitle(rawLines []string) (string, bool)
}
