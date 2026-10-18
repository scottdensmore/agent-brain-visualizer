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

// Package store provides PostgreSQL persistence for sessions, summaries,
// attached session files, and evaluation run history.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

//go:embed schema.sql
var embeddedSchemaSQL string

// SchemaSQL returns the bundled DDL script for initializing the trajectory store.
func SchemaSQL() string {
	return embeddedSchemaSQL
}

// StoreUnavailableError signals that the trajectory database could not be reached
// or returned a SQL error. The HTTP layer maps this to 503 Service Unavailable.
type StoreUnavailableError struct {
	Message string
	Cause   error
}

func (e *StoreUnavailableError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *StoreUnavailableError) Unwrap() error {
	return e.Cause
}

// IsStoreUnavailable reports whether err wraps a *StoreUnavailableError.
func IsStoreUnavailable(err error) (*StoreUnavailableError, bool) {
	var target *StoreUnavailableError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// Sha256Hex returns the lowercase hex SHA-256 of content's UTF-8 bytes.
func Sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// ApplySchema executes schemaSQL on db.
func ApplySchema(ctx context.Context, db *sql.DB, schemaSQL string) error {
	if db == nil {
		return &StoreUnavailableError{Message: "database is nil"}
	}
	_, err := db.ExecContext(ctx, schemaSQL)
	return err
}

// BootstrapSchema applies the embedded schema.sql on startup. It logs and never fails
// startup if the database is unreachable.
func BootstrapSchema(ctx context.Context, db *sql.DB) {
	if strings.TrimSpace(embeddedSchemaSQL) == "" {
		log.Printf("ERROR SchemaBootstrap - Could not read /db/schema.sql — the store schema was not applied.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ApplySchema(cctx, db, embeddedSchemaSQL); err != nil {
		log.Printf("WARN  SchemaBootstrap - Could not apply the store schema (is the database running? `docker compose up -d`): %v", err)
		return
	}
	log.Printf("INFO  SchemaBootstrap - Trajectory store schema is up to date.")
}

// Session represents one normalized trajectory stored in the sessions table.
type Session struct {
	Source      string
	ID          string
	Title       string
	SourceMtime int64
	StepsJSON   string
	ContentHash string
}

// CollectedSession holds a session's ID, normalized steps JSON, and optional cached summary JSON.
type CollectedSession struct {
	ID          string
	StepsJSON   string
	SummaryJSON string // empty string when no summary is stored (NULL)
	HasSummary  bool
}

// SessionRepository reads and writes ingested trajectories in the sessions table.
type SessionRepository struct {
	db *sql.DB
}

// NewSessionRepository creates a SessionRepository backed by db.
func NewSessionRepository(db *sql.DB) *SessionRepository {
	return &SessionRepository{db: db}
}

const (
	sessionUpsertSQL = `
INSERT INTO sessions (source, id, title, updated_at, steps, content_hash, source_mtime)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
ON CONFLICT (source, id) DO UPDATE
   SET title = excluded.title,
       updated_at = excluded.updated_at,
       steps = excluded.steps,
       content_hash = excluded.content_hash,
       source_mtime = excluded.source_mtime,
       ingested_at = now()
 WHERE sessions.content_hash <> excluded.content_hash`

	sessionManifestSQL = `SELECT id, content_hash FROM sessions WHERE source = $1`

	sessionCountSQL = `SELECT count(*) FROM sessions WHERE source = $1`

	sessionListSQL = `
SELECT s.id, COALESCE(NULLIF(m.short_title, ''), s.title) AS title, s.source_mtime
FROM sessions s
LEFT JOIN summaries m ON m.source = s.source AND m.session_id = s.id
WHERE s.source = $1
ORDER BY s.updated_at DESC, s.id
LIMIT $2 OFFSET $3`

	sessionStepsSQL = `SELECT steps FROM sessions WHERE source = $1 AND id = $2`

	sessionExistsSQL = `SELECT 1 FROM sessions WHERE source = $1 AND id = $2`

	sessionCollectSQL = `
SELECT s.id, s.steps, m.summary
FROM sessions s
LEFT JOIN summaries m ON m.source = s.source AND m.session_id = s.id
WHERE s.source = $1
ORDER BY s.updated_at DESC, s.id
LIMIT $2`
)

func sanitizePostgresText(s string) string {
	if !strings.Contains(s, "\x00") {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// sanitizePostgresJSONB strips literal NUL bytes and unescaped \u0000 JSON sequences,
// which PostgreSQL jsonb rejects with SQLSTATE 22P05 ("unsupported Unicode escape sequence").
func sanitizePostgresJSONB(s string) string {
	s = sanitizePostgresText(s)
	if !strings.Contains(s, `\u0000`) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		if i+6 <= len(s) && s[i:i+6] == `\u0000` {
			backslashes := 0
			for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 == 0 {
				i += 6
				continue
			}
		}
		sb.WriteByte(s[i])
		i++
	}
	return sb.String()
}

// Upsert inserts the session or updates it when its content_hash changed.
// Returns true if a row was inserted or updated, or false if an identical hash was already stored.
func (r *SessionRepository) Upsert(s Session) (bool, error) {
	if r == nil || r.db == nil {
		return false, &StoreUnavailableError{Message: "Could not store session " + s.ID}
	}
	updatedAt := time.UnixMilli(s.SourceMtime).UTC()
	title := sanitizePostgresText(s.Title)
	stepsJSON := sanitizePostgresJSONB(s.StepsJSON)
	res, err := r.db.Exec(sessionUpsertSQL, s.Source, s.ID, title, updatedAt, stepsJSON, s.ContentHash, s.SourceMtime)
	if err != nil {
		return false, &StoreUnavailableError{Message: "Could not store session " + s.ID, Cause: err}
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, &StoreUnavailableError{Message: "Could not store session " + s.ID, Cause: err}
	}
	return n > 0, nil
}

// Manifest returns every stored id -> contentHash for a source.
func (r *SessionRepository) Manifest(source string) (map[string]string, error) {
	if r == nil || r.db == nil {
		return nil, &StoreUnavailableError{Message: "Could not read the manifest for " + source}
	}
	rows, err := r.db.Query(sessionManifestSQL, source)
	if err != nil {
		return nil, &StoreUnavailableError{Message: "Could not read the manifest for " + source, Cause: err}
	}
	defer rows.Close()

	manifest := make(map[string]string)
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, &StoreUnavailableError{Message: "Could not read the manifest for " + source, Cause: err}
		}
		manifest[id] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, &StoreUnavailableError{Message: "Could not read the manifest for " + source, Cause: err}
	}
	return manifest, nil
}

// ListConversations returns up to limit sessions (id, summary, updatedAt) for a source, newest first.
func (r *SessionRepository) ListConversations(source string, limit, offset int) ([]map[string]string, error) {
	if r == nil || r.db == nil {
		return nil, &StoreUnavailableError{Message: "Could not list sessions for " + source}
	}
	rows, err := r.db.Query(sessionListSQL, source, limit, offset)
	if err != nil {
		return nil, &StoreUnavailableError{Message: "Could not list sessions for " + source, Cause: err}
	}
	defer rows.Close()

	out := make([]map[string]string, 0)
	for rows.Next() {
		var id, title string
		var mtime int64
		if err := rows.Scan(&id, &title, &mtime); err != nil {
			return nil, &StoreUnavailableError{Message: "Could not list sessions for " + source, Cause: err}
		}
		out = append(out, map[string]string{
			"id":        id,
			"summary":   title,
			"updatedAt": strconv.FormatInt(mtime, 10),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, &StoreUnavailableError{Message: "Could not list sessions for " + source, Cause: err}
	}
	return out, nil
}

// Steps returns the session's normalized steps JSON array string, or ("", false, nil) if not found.
func (r *SessionRepository) Steps(source, id string) (string, bool, error) {
	if r == nil || r.db == nil {
		return "", false, &StoreUnavailableError{Message: "Could not read the transcript for " + id}
	}
	var steps string
	err := r.db.QueryRow(sessionStepsSQL, source, id).Scan(&steps)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, &StoreUnavailableError{Message: "Could not read the transcript for " + id, Cause: err}
	}
	return steps, true, nil
}

// Exists reports whether a session with this id is stored under source.
func (r *SessionRepository) Exists(source, id string) (bool, error) {
	if r == nil || r.db == nil {
		return false, &StoreUnavailableError{Message: "Could not check for session " + id}
	}
	var one int
	err := r.db.QueryRow(sessionExistsSQL, source, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, &StoreUnavailableError{Message: "Could not check for session " + id, Cause: err}
	}
	return true, nil
}

// Collect returns up to limit sessions for a source, newest first, each with its cached summary if present.
func (r *SessionRepository) Collect(source string, limit int) ([]CollectedSession, error) {
	if r == nil || r.db == nil {
		return nil, &StoreUnavailableError{Message: "Could not collect sessions for " + source}
	}
	rows, err := r.db.Query(sessionCollectSQL, source, limit)
	if err != nil {
		return nil, &StoreUnavailableError{Message: "Could not collect sessions for " + source, Cause: err}
	}
	defer rows.Close()

	out := make([]CollectedSession, 0)
	for rows.Next() {
		var id, steps string
		var summary sql.NullString
		if err := rows.Scan(&id, &steps, &summary); err != nil {
			return nil, &StoreUnavailableError{Message: "Could not collect sessions for " + source, Cause: err}
		}
		out = append(out, CollectedSession{
			ID:          id,
			StepsJSON:   steps,
			SummaryJSON: summary.String,
			HasSummary:  summary.Valid,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, &StoreUnavailableError{Message: "Could not collect sessions for " + source, Cause: err}
	}
	return out, nil
}

// CountBySource returns how many trajectories are stored for source.
func (r *SessionRepository) CountBySource(source string) (int, error) {
	if r == nil || r.db == nil {
		return 0, &StoreUnavailableError{Message: "Could not count sessions for " + source}
	}
	var count int
	err := r.db.QueryRow(sessionCountSQL, source).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, &StoreUnavailableError{Message: "Could not count sessions for " + source, Cause: err}
	}
	return count, nil
}

// SummaryRepository reads and writes AI analysis summaries in the summaries table.
type SummaryRepository struct {
	db *sql.DB
}

// NewSummaryRepository creates a SummaryRepository backed by db.
func NewSummaryRepository(db *sql.DB) *SummaryRepository {
	return &SummaryRepository{db: db}
}

const (
	summaryUpsertSQL = `
INSERT INTO summaries (source, session_id, summary, short_title, content_hash, updated_at)
VALUES ($1, $2, $3::jsonb, $4, $5, now())
ON CONFLICT (source, session_id) DO UPDATE
   SET summary = excluded.summary,
       short_title = COALESCE(excluded.short_title, summaries.short_title),
       content_hash = excluded.content_hash,
       updated_at = now()
   WHERE summaries.content_hash IS DISTINCT FROM excluded.content_hash`

	summaryFindSQL = `SELECT summary FROM summaries WHERE source = $1 AND session_id = $2`

	summaryManifestSQL = `SELECT session_id, content_hash FROM summaries WHERE source = $1 AND content_hash IS NOT NULL`

	summaryDeleteSQL = `DELETE FROM summaries WHERE source = $1 AND session_id = $2`
)

// Find returns the cached analysis JSON for a session, or ("", false, nil) when none is stored.
func (r *SummaryRepository) Find(source, id string) (string, bool, error) {
	if r == nil || r.db == nil {
		return "", false, &StoreUnavailableError{Message: "Could not read the cached summary for " + id}
	}
	var summary string
	err := r.db.QueryRow(summaryFindSQL, source, id).Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, &StoreUnavailableError{Message: "Could not read the cached summary for " + id, Cause: err}
	}
	return summary, true, nil
}

// Upsert stores or replaces the analysis JSON and short title for a session.
// Returns true if a row was written, false if the content_hash was already stored.
func (r *SummaryRepository) Upsert(source, id, summaryJSON, title string) (bool, error) {
	if r == nil || r.db == nil {
		return false, &StoreUnavailableError{Message: "Could not store the summary for " + id}
	}
	var titleArg any
	trimmedTitle := strings.TrimSpace(sanitizePostgresText(title))
	if trimmedTitle != "" {
		titleArg = trimmedTitle
	}
	hash := Sha256Hex(summaryJSON)
	res, err := r.db.Exec(summaryUpsertSQL, source, id, sanitizePostgresJSONB(summaryJSON), titleArg, hash)
	if err != nil {
		return false, &StoreUnavailableError{Message: "Could not store the summary for " + id, Cause: err}
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, &StoreUnavailableError{Message: "Could not store the summary for " + id, Cause: err}
	}
	return n > 0, nil
}

// Manifest returns every stored session_id -> content_hash for a source where content_hash IS NOT NULL.
func (r *SummaryRepository) Manifest(source string) (map[string]string, error) {
	if r == nil || r.db == nil {
		return nil, &StoreUnavailableError{Message: "Could not read the summary manifest for " + source}
	}
	rows, err := r.db.Query(summaryManifestSQL, source)
	if err != nil {
		return nil, &StoreUnavailableError{Message: "Could not read the summary manifest for " + source, Cause: err}
	}
	defer rows.Close()

	manifest := make(map[string]string)
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, &StoreUnavailableError{Message: "Could not read the summary manifest for " + source, Cause: err}
		}
		manifest[id] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, &StoreUnavailableError{Message: "Could not read the summary manifest for " + source, Cause: err}
	}
	return manifest, nil
}

// Delete removes any cached summary for a session.
func (r *SummaryRepository) Delete(source, id string) error {
	if r == nil || r.db == nil {
		return &StoreUnavailableError{Message: "Could not delete the summary for " + id}
	}
	if _, err := r.db.Exec(summaryDeleteSQL, source, id); err != nil {
		return &StoreUnavailableError{Message: "Could not delete the summary for " + id, Cause: err}
	}
	return nil
}

// IngestFile is one file attached to a pushed session.
type IngestFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SessionFileRepository stores and retrieves files attached to a session in the session_files table.
type SessionFileRepository struct {
	db *sql.DB
}

// NewSessionFileRepository creates a SessionFileRepository backed by db.
func NewSessionFileRepository(db *sql.DB) *SessionFileRepository {
	return &SessionFileRepository{db: db}
}

const (
	sessionFileUpsertSQL = `
INSERT INTO session_files (source, session_id, path, content, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (source, session_id, path) DO UPDATE
   SET content = excluded.content,
       updated_at = now()
   WHERE session_files.content IS DISTINCT FROM excluded.content`

	sessionFileFindSQL = `SELECT content FROM session_files WHERE source = $1 AND session_id = $2 AND path = $3`

	sessionFileDeleteForSessionSQL = `DELETE FROM session_files WHERE source = $1 AND session_id = $2`
)

// ReplaceForSession replaces all stored attachments for (source, sessionID) in a transaction.
func (r *SessionFileRepository) ReplaceForSession(source, sessionID string, files []IngestFile) error {
	if r == nil || r.db == nil {
		return &StoreUnavailableError{Message: "Could not store the attached files for " + sessionID}
	}
	tx, err := r.db.Begin()
	if err != nil {
		return &StoreUnavailableError{Message: "Could not store the attached files for " + sessionID, Cause: err}
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(sessionFileDeleteForSessionSQL, source, sessionID); err != nil {
		return &StoreUnavailableError{Message: "Could not store the attached files for " + sessionID, Cause: err}
	}
	for _, f := range files {
		if _, err := tx.Exec(sessionFileUpsertSQL, source, sessionID, sanitizePostgresText(f.Path), sanitizePostgresText(f.Content)); err != nil {
			return &StoreUnavailableError{Message: "Could not store the attached files for " + sessionID, Cause: err}
		}
	}
	if err := tx.Commit(); err != nil {
		return &StoreUnavailableError{Message: "Could not store the attached files for " + sessionID, Cause: err}
	}
	return nil
}

// Find returns the stored content of one attached file, or ("", false, nil) if not found.
func (r *SessionFileRepository) Find(source, sessionID, path string) (string, bool, error) {
	if r == nil || r.db == nil {
		return "", false, &StoreUnavailableError{Message: "Could not read the attached files for " + sessionID}
	}
	var content sql.NullString
	err := r.db.QueryRow(sessionFileFindSQL, source, sessionID, path).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) || !content.Valid {
		return "", false, nil
	}
	if err != nil {
		return "", false, &StoreUnavailableError{Message: "Could not read the attached files for " + sessionID, Cause: err}
	}
	return content.String, true, nil
}

// DeleteForSession removes all attachments for a session.
func (r *SessionFileRepository) DeleteForSession(source, sessionID string) error {
	if r == nil || r.db == nil {
		return &StoreUnavailableError{Message: "Could not delete the attached files for " + sessionID}
	}
	if _, err := r.db.Exec(sessionFileDeleteForSessionSQL, source, sessionID); err != nil {
		return &StoreUnavailableError{Message: "Could not delete the attached files for " + sessionID, Cause: err}
	}
	return nil
}

// NameCount is a generic name + count pair used in checkPassRates and fleet insights.
type NameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// EvalCaseResult is the graded evaluation result for a single session's analysis.
type EvalCaseResult struct {
	SessionID string   `json:"sessionId"`
	Title     string   `json:"title"`
	Score     int      `json:"score"`
	Passed    []string `json:"passed"`
	Failed    []string `json:"failed"`
}

// MarshalJSON ensures Passed and Failed slices serialize as [] rather than null.
func (e EvalCaseResult) MarshalJSON() ([]byte, error) {
	type wire struct {
		SessionID string   `json:"sessionId"`
		Title     string   `json:"title"`
		Score     int      `json:"score"`
		Passed    []string `json:"passed"`
		Failed    []string `json:"failed"`
	}
	passed := e.Passed
	if passed == nil {
		passed = []string{}
	}
	failed := e.Failed
	if failed == nil {
		failed = []string{}
	}
	return json.Marshal(wire{
		SessionID: e.SessionID,
		Title:     e.Title,
		Score:     e.Score,
		Passed:    passed,
		Failed:    failed,
	})
}

// JudgedCase is the ensembled LLM-as-a-judge panel result for a single session.
type JudgedCase struct {
	SessionID     string  `json:"sessionId"`
	Title         string  `json:"title"`
	Faithfulness  float64 `json:"faithfulness"`
	Actionability float64 `json:"actionability"`
	Clarity       float64 `json:"clarity"`
	Samples       int     `json:"samples"`
	PanelMin      float64 `json:"panelMin"`
	PanelMax      float64 `json:"panelMax"`
	Comment       string  `json:"comment"`
}

// JudgeSummary summarizes the optional LLM-as-a-judge pass across sessions.
type JudgeSummary struct {
	Ran              bool         `json:"ran"`
	Note             string       `json:"note"`
	JudgedSessions   int          `json:"judgedSessions"`
	AvgFaithfulness  float64      `json:"avgFaithfulness"`
	AvgActionability float64      `json:"avgActionability"`
	AvgClarity       float64      `json:"avgClarity"`
	Cases            []JudgedCase `json:"cases"`
}

// MarshalJSON ensures Cases serializes as [] rather than null.
func (j JudgeSummary) MarshalJSON() ([]byte, error) {
	type wire struct {
		Ran              bool         `json:"ran"`
		Note             string       `json:"note"`
		JudgedSessions   int          `json:"judgedSessions"`
		AvgFaithfulness  float64      `json:"avgFaithfulness"`
		AvgActionability float64      `json:"avgActionability"`
		AvgClarity       float64      `json:"avgClarity"`
		Cases            []JudgedCase `json:"cases"`
	}
	cases := j.Cases
	if cases == nil {
		cases = []JudgedCase{}
	}
	return json.Marshal(wire{
		Ran:              j.Ran,
		Note:             j.Note,
		JudgedSessions:   j.JudgedSessions,
		AvgFaithfulness:  j.AvgFaithfulness,
		AvgActionability: j.AvgActionability,
		AvgClarity:       j.AvgClarity,
		Cases:            cases,
	})
}

// JudgeNotRun constructs a JudgeSummary indicating the LLM judge did not run, with an explanatory note.
func JudgeNotRun(note string) *JudgeSummary {
	return &JudgeSummary{
		Ran:   false,
		Note:  note,
		Cases: []JudgedCase{},
	}
}

// EvalReport is the full evaluation report returned by GET /api/eval and accepted by POST /api/eval/runs.
type EvalReport struct {
	Flavor            string           `json:"flavor"`
	SessionCount      int              `json:"sessionCount"`
	SampledSessions   int              `json:"sampledSessions"`
	EvaluatedSessions int              `json:"evaluatedSessions"`
	AvgScore          float64          `json:"avgScore"`
	ModelLabel        string           `json:"modelLabel"`
	CheckPassRates    []NameCount      `json:"checkPassRates"`
	WorstCases        []EvalCaseResult `json:"worstCases"`
	Judge             *JudgeSummary    `json:"judge"`
}

// MarshalJSON ensures slices on EvalReport serialize as [] rather than null.
func (r EvalReport) MarshalJSON() ([]byte, error) {
	type wire struct {
		Flavor            string           `json:"flavor"`
		SessionCount      int              `json:"sessionCount"`
		SampledSessions   int              `json:"sampledSessions"`
		EvaluatedSessions int              `json:"evaluatedSessions"`
		AvgScore          float64          `json:"avgScore"`
		ModelLabel        string           `json:"modelLabel"`
		CheckPassRates    []NameCount      `json:"checkPassRates"`
		WorstCases        []EvalCaseResult `json:"worstCases"`
		Judge             *JudgeSummary    `json:"judge"`
	}
	rates := r.CheckPassRates
	if rates == nil {
		rates = []NameCount{}
	}
	worst := r.WorstCases
	if worst == nil {
		worst = []EvalCaseResult{}
	}
	return json.Marshal(wire{
		Flavor:            r.Flavor,
		SessionCount:      r.SessionCount,
		SampledSessions:   r.SampledSessions,
		EvaluatedSessions: r.EvaluatedSessions,
		AvgScore:          r.AvgScore,
		ModelLabel:        r.ModelLabel,
		CheckPassRates:    rates,
		WorstCases:        worst,
		Judge:             r.Judge,
	})
}

// EvalRunSnapshot is a persisted evaluation run summary stored in eval_runs.
type EvalRunSnapshot struct {
	SavedAt           string      `json:"savedAt"`
	Flavor            string      `json:"flavor"`
	ModelLabel        string      `json:"modelLabel"`
	SessionCount      int         `json:"sessionCount"`
	EvaluatedSessions int         `json:"evaluatedSessions"`
	AvgScore          float64     `json:"avgScore"`
	CheckPassRates    []NameCount `json:"checkPassRates"`
	Judged            bool        `json:"judged"`
	JudgedSessions    int         `json:"judgedSessions"`
	AvgFaithfulness   float64     `json:"avgFaithfulness"`
	AvgActionability  float64     `json:"avgActionability"`
	AvgClarity        float64     `json:"avgClarity"`
}

// EvalRunMaxRuns is the global cap on stored evaluation runs.
const EvalRunMaxRuns = 200

// EvalRunRepository persists EvalRunSnapshots in the eval_runs table.
type EvalRunRepository struct {
	db *sql.DB
}

// NewEvalRunRepository creates an EvalRunRepository backed by db.
func NewEvalRunRepository(db *sql.DB) *EvalRunRepository {
	return &EvalRunRepository{db: db}
}

const (
	evalRunInsertSQL = `
INSERT INTO eval_runs (saved_at, flavor, model_label, session_count, evaluated_sessions,
                       avg_score, check_pass_rates, judged, judged_sessions,
                       avg_faithfulness, avg_actionability, avg_clarity)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12)`

	evalRunSelectSQL = `
SELECT saved_at, flavor, model_label, session_count, evaluated_sessions, avg_score,
       check_pass_rates, judged, judged_sessions, avg_faithfulness, avg_actionability,
       avg_clarity
FROM eval_runs
WHERE flavor = $1
ORDER BY saved_at DESC, run_id DESC`

	evalRunTrimSQL = `
DELETE FROM eval_runs
WHERE run_id NOT IN (
    SELECT run_id FROM eval_runs ORDER BY saved_at DESC, run_id DESC LIMIT $1
)`

	evalRunDeleteSQL = `DELETE FROM eval_runs WHERE saved_at = $1`
)

// Save records a snapshot of report stamped with the current UTC ISO-8601 instant.
func (r *EvalRunRepository) Save(report EvalReport) (EvalRunSnapshot, error) {
	return r.SaveAt(report, time.Now().UTC().Format(time.RFC3339Nano))
}

// SaveAt records a snapshot of report stamped with an explicit savedAt instant (test seam).
func (r *EvalRunRepository) SaveAt(report EvalReport, savedAt string) (EvalRunSnapshot, error) {
	snapshot := snapshotOf(report, savedAt)
	if r == nil || r.db == nil {
		return EvalRunSnapshot{}, &StoreUnavailableError{Message: "Could not save the eval run"}
	}
	ratesJSON, err := json.Marshal(snapshot.CheckPassRates)
	if err != nil {
		ratesJSON = []byte("[]")
	}
	_, err = r.db.Exec(
		evalRunInsertSQL,
		snapshot.SavedAt,
		snapshot.Flavor,
		snapshot.ModelLabel,
		snapshot.SessionCount,
		snapshot.EvaluatedSessions,
		snapshot.AvgScore,
		string(ratesJSON),
		snapshot.Judged,
		snapshot.JudgedSessions,
		snapshot.AvgFaithfulness,
		snapshot.AvgActionability,
		snapshot.AvgClarity,
	)
	if err != nil {
		return EvalRunSnapshot{}, &StoreUnavailableError{Message: "Could not save the eval run", Cause: err}
	}
	if _, err := r.db.Exec(evalRunTrimSQL, EvalRunMaxRuns); err != nil {
		return EvalRunSnapshot{}, &StoreUnavailableError{Message: "Could not save the eval run", Cause: err}
	}
	return snapshot, nil
}

// List returns all saved runs for flavor, newest first.
func (r *EvalRunRepository) List(flavor string) ([]EvalRunSnapshot, error) {
	if r == nil || r.db == nil {
		return nil, &StoreUnavailableError{Message: "Could not list the eval runs"}
	}
	rows, err := r.db.Query(evalRunSelectSQL, flavor)
	if err != nil {
		return nil, &StoreUnavailableError{Message: "Could not list the eval runs", Cause: err}
	}
	defer rows.Close()

	runs := make([]EvalRunSnapshot, 0)
	for rows.Next() {
		var s EvalRunSnapshot
		var ratesRaw sql.NullString
		if err := rows.Scan(
			&s.SavedAt,
			&s.Flavor,
			&s.ModelLabel,
			&s.SessionCount,
			&s.EvaluatedSessions,
			&s.AvgScore,
			&ratesRaw,
			&s.Judged,
			&s.JudgedSessions,
			&s.AvgFaithfulness,
			&s.AvgActionability,
			&s.AvgClarity,
		); err != nil {
			return nil, &StoreUnavailableError{Message: "Could not list the eval runs", Cause: err}
		}
		s.CheckPassRates = readCheckPassRates(ratesRaw.String)
		runs = append(runs, s)
	}
	if err := rows.Err(); err != nil {
		return nil, &StoreUnavailableError{Message: "Could not list the eval runs", Cause: err}
	}
	return runs, nil
}

// Delete removes saved run(s) matching savedAt and returns how many rows were deleted.
func (r *EvalRunRepository) Delete(savedAt string) (int, error) {
	if strings.TrimSpace(savedAt) == "" {
		return 0, nil
	}
	if r == nil || r.db == nil {
		return 0, &StoreUnavailableError{Message: "Could not delete the eval run"}
	}
	res, err := r.db.Exec(evalRunDeleteSQL, savedAt)
	if err != nil {
		return 0, &StoreUnavailableError{Message: "Could not delete the eval run", Cause: err}
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, &StoreUnavailableError{Message: "Could not delete the eval run", Cause: err}
	}
	return int(n), nil
}

func readCheckPassRates(raw string) []NameCount {
	if strings.TrimSpace(raw) == "" {
		return []NameCount{}
	}
	var out []NameCount
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []NameCount{}
	}
	return out
}

func snapshotOf(report EvalReport, savedAt string) EvalRunSnapshot {
	flavor := strings.TrimSpace(report.Flavor)
	if flavor == "" {
		flavor = "unknown"
	} else {
		flavor = report.Flavor
	}
	modelLabel := strings.TrimSpace(report.ModelLabel)
	if modelLabel == "" {
		modelLabel = "unknown"
	} else {
		modelLabel = report.ModelLabel
	}
	rates := report.CheckPassRates
	if rates == nil {
		rates = []NameCount{}
	}
	judged := report.Judge != nil && report.Judge.Ran
	var judgedSessions int
	var avgFaith, avgAction, avgClarity float64
	if judged {
		judgedSessions = report.Judge.JudgedSessions
		avgFaith = report.Judge.AvgFaithfulness
		avgAction = report.Judge.AvgActionability
		avgClarity = report.Judge.AvgClarity
	}
	return EvalRunSnapshot{
		SavedAt:           savedAt,
		Flavor:            flavor,
		ModelLabel:        modelLabel,
		SessionCount:      report.SessionCount,
		EvaluatedSessions: report.EvaluatedSessions,
		AvgScore:          report.AvgScore,
		CheckPassRates:    rates,
		Judged:            judged,
		JudgedSessions:    judgedSessions,
		AvgFaithfulness:   avgFaith,
		AvgActionability:  avgAction,
		AvgClarity:        avgClarity,
	}
}
