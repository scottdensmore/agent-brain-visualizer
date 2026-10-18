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

// Package server implements the HTTP API, authentication filters, store-unavailable
// error handling, and embedded static UI serving for Agent Brain Visualizer.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/ai"
	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/insights"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
	"github.com/scottdensmore/agent-brain-visualizer/internal/transcript"
	"github.com/scottdensmore/agent-brain-visualizer/web"
)

const (
	// DefaultLimit is the default page size for GET /api/brain/conversations.
	DefaultLimit = 200
	// MaxLimit is the hard cap on page size for GET /api/brain/conversations.
	MaxLimit = 1000
	// ReadinessTimeout is the timeout for the database readiness probe.
	ReadinessTimeout = 2 * time.Second
)

// DefaultVersion is the build version reported by GET /health and --version unless overridden.
var DefaultVersion = func() string {
	if v := strings.TrimSpace(os.Getenv("GITHUB_REF_NAME")); v != "" {
		return v
	}
	return "0.1-SNAPSHOT"
}()

// ConversationPage is one page of a source's conversation list (ported from ConversationPage.java).
type ConversationPage struct {
	Items  []map[string]string `json:"items"`
	Total  int                 `json:"total"`
	Limit  int                 `json:"limit"`
	Offset int                 `json:"offset"`
}

// MarshalJSON ensures Items serializes as [] rather than null.
func (p ConversationPage) MarshalJSON() ([]byte, error) {
	type wire struct {
		Items  []map[string]string `json:"items"`
		Total  int                 `json:"total"`
		Limit  int                 `json:"limit"`
		Offset int                 `json:"offset"`
	}
	items := p.Items
	if items == nil {
		items = []map[string]string{}
	}
	return json.Marshal(wire{
		Items:  items,
		Total:  p.Total,
		Limit:  p.Limit,
		Offset: p.Offset,
	})
}

// DeleteResult reports how many records a delete removed (ported from DeleteResult.java).
type DeleteResult struct {
	Deleted int `json:"deleted"`
}

// ProgressResponse is the JSON payload returned by GET /api/analysis/conversations/{id}/progress.
type ProgressResponse struct {
	Phase    string `json:"phase"`
	Progress int    `json:"progress"`
}

// DefaultInstructionResponse is the JSON payload returned by GET /api/optimize.
type DefaultInstructionResponse struct {
	Instruction string `json:"instruction"`
	MaxSample   int    `json:"maxSample"`
}

// Server wires all repositories, domain services, auth filters, and HTTP routes.
type Server struct {
	Version      string
	UserHome     string
	DB           *sql.DB
	DBPing       func(ctx context.Context) error
	ApiConfig    *config.ApiConfig
	IngestConfig *config.IngestConfig
	AiConfig     *config.AiConfig

	Sessions     *store.SessionRepository
	Summaries    *store.SummaryRepository
	SessionFiles *store.SessionFileRepository
	EvalRuns     *store.EvalRunRepository

	Ingestor     *transcript.Ingestor
	Orchestrator *ai.AnalysisOrchestrator
	Collector    *insights.SessionCollector
	Insights     *insights.InsightsService
	Miner        *insights.MinerService
	Eval         *insights.EvalService
	Optimize     *insights.OptimizeService

	StaticFS fs.FS
	mux      *http.ServeMux
}

// Dependencies holds optional overrides when constructing a Server.
type Dependencies struct {
	Version      string
	UserHome     string
	DB           *sql.DB
	DBPing       func(ctx context.Context) error
	ApiConfig    *config.ApiConfig
	IngestConfig *config.IngestConfig
	AiConfig     *config.AiConfig
	StaticFS     fs.FS
}

// New constructs a Server wired to the given database and configuration.
func New(deps Dependencies) *Server {
	version := deps.Version
	if strings.TrimSpace(version) == "" {
		version = DefaultVersion
	}
	apiCfg := deps.ApiConfig
	if apiCfg == nil {
		apiCfg = config.NewApiConfig("", false)
	}
	ingestCfg := deps.IngestConfig
	if ingestCfg == nil {
		ingestCfg = config.NewIngestConfig("", false)
	}
	aiCfg := deps.AiConfig
	if aiCfg == nil {
		aiCfg = config.NewAiConfig("gemini", "", config.DefaultGeminiModel, config.DefaultOllamaBaseURL, config.DefaultOllamaModel)
	}
	staticFS := deps.StaticFS
	if staticFS == nil {
		staticFS = web.PublicFS()
	}

	sessions := store.NewSessionRepository(deps.DB)
	summaries := store.NewSummaryRepository(deps.DB)
	sessionFiles := store.NewSessionFileRepository(deps.DB)
	evalRuns := store.NewEvalRunRepository(deps.DB)

	ingestor := transcript.NewIngestor(transcript.DefaultNormalizers(), sessions, summaries, sessionFiles)
	orchestrator := ai.NewAnalysisOrchestrator(aiCfg, sessions, summaries)
	collector := insights.NewSessionCollector(sessions)
	insightsSvc := insights.NewInsightsService(collector)
	minerSvc := insights.NewMinerService(collector, nil, aiCfg)
	evalSvc := insights.NewEvalService(collector, aiCfg, nil)
	optimizeSvc := insights.NewOptimizeService(collector, nil, aiCfg)

	s := &Server{
		Version:      version,
		UserHome:     deps.UserHome,
		DB:           deps.DB,
		DBPing:       deps.DBPing,
		ApiConfig:    apiCfg,
		IngestConfig: ingestCfg,
		AiConfig:     aiCfg,
		Sessions:     sessions,
		Summaries:    summaries,
		SessionFiles: sessionFiles,
		EvalRuns:     evalRuns,
		Ingestor:     ingestor,
		Orchestrator: orchestrator,
		Collector:    collector,
		Insights:     insightsSvc,
		Miner:        minerSvc,
		Eval:         evalSvc,
		Optimize:     optimizeSvc,
		StaticFS:     staticFS,
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler, applying IngestAuthFilter and ApiAuthFilter before dispatching.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if isIngestPath(path) {
		if status, body := GuardIngest(s.IngestConfig, r.Header.Get("Authorization")); status != 0 {
			writeJSONStatus(w, status, body)
			return
		}
	} else if isApiPath(path) {
		if status, body := GuardAPI(s.ApiConfig, r.Header.Get("Authorization")); status != 0 {
			writeJSONStatus(w, status, body)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func isIngestPath(path string) bool {
	return path == "/api/ingest" || strings.HasPrefix(path, "/api/ingest/")
}

func isApiPath(path string) bool {
	if isIngestPath(path) {
		return false
	}
	for _, prefix := range []string{
		"/api/analysis",
		"/api/brain",
		"/api/eval",
		"/api/insights",
		"/api/mine",
		"/api/optimize",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// GuardAPI implements ApiAuthFilter.guard, returning (0, nil) when allowed, or (statusCode, errorBody) when blocked.
func GuardAPI(cfg *config.ApiConfig, authHeader string) (int, map[string]string) {
	expected, hasToken := cfg.Token()
	if !hasToken {
		if cfg != nil && cfg.RequireAuth() {
			return http.StatusServiceUnavailable, map[string]string{
				"error": "The API is unavailable.",
			}
		}
		return 0, nil
	}
	if !config.MatchesBearerToken(expected, authHeader) {
		return http.StatusUnauthorized, map[string]string{
			"error": "This API requires a valid Authorization: Bearer <API_TOKEN>.",
		}
	}
	return 0, nil
}

// GuardIngest implements IngestAuthFilter.guard, returning (0, nil) when allowed, or (statusCode, errorBody) when blocked.
func GuardIngest(cfg *config.IngestConfig, authHeader string) (int, map[string]string) {
	expected, hasToken := cfg.Token()
	if !hasToken {
		if cfg != nil && cfg.RequireAuth() {
			return http.StatusServiceUnavailable, map[string]string{
				"error": "Ingest is unavailable.",
			}
		}
		return 0, nil
	}
	if !config.MatchesBearerToken(expected, authHeader) {
		return http.StatusUnauthorized, map[string]string{
			"error": "Ingest requires a valid Authorization: Bearer <INGEST_TOKEN>.",
		}
	}
	return 0, nil
}

func (s *Server) registerRoutes() {
	mux := http.NewServeMux()

	// Health probes
	mux.HandleFunc("GET /health", s.handleLiveness)
	mux.HandleFunc("GET /health/ready", s.handleReadiness)

	// Brain endpoints
	mux.HandleFunc("GET /api/brain/flavors", s.handleFlavors)
	mux.HandleFunc("GET /api/brain/conversations", s.handleListConversations)
	mux.HandleFunc("GET /api/brain/conversations/{id}/transcript", s.handleGetTranscript)
	mux.HandleFunc("GET /api/brain/file", s.handleGetFileContent)
	mux.HandleFunc("GET /api/brain/files", s.handleGetFileContent)

	// Ingest endpoints
	mux.HandleFunc("GET /api/ingest/manifest", s.handleIngestManifest)
	mux.HandleFunc("POST /api/ingest/sessions", s.handleIngestSessions)
	mux.HandleFunc("GET /api/ingest/summaries/manifest", s.handleIngestSummaryManifest)
	mux.HandleFunc("POST /api/ingest/summaries", s.handleIngestSummaries)

	// Analysis endpoints
	mux.HandleFunc("GET /api/analysis/conversations/{id}/progress", s.handleAnalysisProgress)
	mux.HandleFunc("POST /api/analysis/conversations/{id}/summarize", s.handleSummarizeConversation)

	// Eval endpoints
	mux.HandleFunc("GET /api/eval", s.handleEval)
	mux.HandleFunc("POST /api/eval/runs", s.handleSaveEvalRun)
	mux.HandleFunc("GET /api/eval/runs", s.handleListEvalRuns)
	mux.HandleFunc("DELETE /api/eval/runs", s.handleDeleteEvalRun)

	// Insights endpoints
	mux.HandleFunc("GET /api/insights", s.handleInsights)
	mux.HandleFunc("GET /api/insights/sessions", s.handleInsightsDrilldown)
	mux.HandleFunc("GET /api/insights/drilldown", s.handleInsightsDrilldown)

	// Mine endpoints
	mux.HandleFunc("GET /api/mine", s.handleMine)

	// Optimize endpoints
	mux.HandleFunc("GET /api/optimize", s.handleOptimizeDefaults)
	mux.HandleFunc("POST /api/optimize", s.handleOptimizeCompare)

	// Static files
	if s.StaticFS != nil {
		mux.Handle("/", http.FileServer(http.FS(s.StaticFS)))
	}

	s.mux = mux
}

func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	writeJSONStatus(w, http.StatusOK, map[string]string{
		"status":  "UP",
		"version": s.Version,
	})
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), ReadinessTimeout)
	defer cancel()

	var err error
	if s.DBPing != nil {
		err = s.DBPing(ctx)
	} else if s.DB == nil {
		err = errors.New("database not configured")
	} else {
		var one int
		err = s.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	}

	if err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
			"status": "DOWN",
			"db":     "DOWN",
		})
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]string{
		"status": "UP",
		"db":     "UP",
	})
}

func (s *Server) handleFlavors(w http.ResponseWriter, r *http.Request) {
	writeJSONStatus(w, http.StatusOK, []string{
		"antigravity-cli",
		"antigravity-ide",
		"antigravity",
		"codex",
		"claude-code",
	})
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source := queryOrDefault(q.Get("flavor"), transcript.DefaultFlavor)
	limit := parseIntOrDefault(q.Get("limit"), DefaultLimit)
	pageLimit := clamp(limit, 1, MaxLimit)
	offset := parseIntOrDefault(q.Get("offset"), 0)
	pageOffset := max(0, offset)

	items, err := s.Sessions.ListConversations(source, pageLimit, pageOffset)
	if s.checkStoreError(w, r, err) {
		return
	}
	total, err := s.Sessions.CountBySource(source)
	if s.checkStoreError(w, r, err) {
		return
	}

	writeJSONStatus(w, http.StatusOK, ConversationPage{
		Items:  items,
		Total:  total,
		Limit:  pageLimit,
		Offset: pageOffset,
	})
}

func (s *Server) handleGetTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	source := queryOrDefault(r.URL.Query().Get("flavor"), transcript.DefaultFlavor)
	steps, ok, err := s.Sessions.Steps(source, id)
	if s.checkStoreError(w, r, err) {
		return
	}
	if !ok || strings.TrimSpace(steps) == "" {
		steps = "[]"
	}
	writeRawJSON(w, http.StatusOK, steps)
}

func (s *Server) handleGetFileContent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if strings.TrimSpace(path) == "" {
		writePlainText(w, http.StatusBadRequest, "Missing path")
		return
	}

	id := q.Get("id")
	if id == "" {
		id = q.Get("sessionId")
	}
	if id != "" && s.SessionFiles != nil {
		source := queryOrDefault(q.Get("flavor"), transcript.DefaultFlavor)
		stored, ok, err := s.SessionFiles.Find(source, id, path)
		if s.checkStoreError(w, r, err) {
			return
		}
		if ok {
			writePlainText(w, http.StatusOK, stored)
			return
		}
	}

	geminiDir := filepath.Clean(transcript.ResolveGeminiDir(s.effectiveUserHome()))
	filePath := filepath.Clean(path)

	if !isWithinDir(filePath, geminiDir) {
		writePlainText(w, http.StatusUnauthorized, "")
		return
	}
	if transcript.IsPreviewDenied(geminiDir, filePath) {
		writePlainText(w, http.StatusForbidden, "This file is not available for preview.")
		return
	}

	info, err := os.Stat(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writePlainText(w, http.StatusNotFound, "File not available on this machine.")
			return
		}
		writePlainText(w, http.StatusInternalServerError, "Error reading file: "+err.Error())
		return
	}
	if info.IsDir() {
		writePlainText(w, http.StatusNotFound, "File not available on this machine.")
		return
	}

	realFile, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writePlainText(w, http.StatusNotFound, "File not available on this machine.")
			return
		}
		writePlainText(w, http.StatusInternalServerError, "Error reading file: "+err.Error())
		return
	}
	realGeminiDir, err := filepath.EvalSymlinks(geminiDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writePlainText(w, http.StatusNotFound, "File not available on this machine.")
			return
		}
		writePlainText(w, http.StatusInternalServerError, "Error reading file: "+err.Error())
		return
	}

	if !isWithinDir(realFile, realGeminiDir) {
		writePlainText(w, http.StatusUnauthorized, "")
		return
	}
	if transcript.IsPreviewDenied(realGeminiDir, realFile) {
		writePlainText(w, http.StatusForbidden, "This file is not available for preview.")
		return
	}

	data, err := os.ReadFile(realFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writePlainText(w, http.StatusNotFound, "File not available on this machine.")
			return
		}
		writePlainText(w, http.StatusInternalServerError, "Error reading file: "+err.Error())
		return
	}
	writePlainText(w, http.StatusOK, string(data))
}

func (s *Server) effectiveUserHome() string {
	if strings.TrimSpace(s.UserHome) != "" {
		return s.UserHome
	}
	if h := strings.TrimSpace(os.Getenv("USER_HOME")); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

func isWithinDir(targetPath, dirPath string) bool {
	t := filepath.Clean(targetPath)
	d := filepath.Clean(dirPath)
	if t == d {
		return true
	}
	prefix := d
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(t, prefix)
}

func (s *Server) handleIngestManifest(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		writeJSONStatus(w, http.StatusOK, map[string]string{})
		return
	}
	m, err := s.Sessions.Manifest(source)
	if s.checkStoreError(w, r, err) {
		return
	}
	if m == nil {
		m = map[string]string{}
	}
	writeJSONStatus(w, http.StatusOK, m)
}

func (s *Server) handleIngestSessions(w http.ResponseWriter, r *http.Request) {
	var batch []transcript.IngestSession
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&batch); err != nil && !errors.Is(err, io.EOF) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON body."})
		return
	}
	res, err := s.Ingestor.Ingest(batch)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, res)
}

func (s *Server) handleIngestSummaryManifest(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		writeJSONStatus(w, http.StatusOK, map[string]string{})
		return
	}
	m, err := s.Summaries.Manifest(source)
	if s.checkStoreError(w, r, err) {
		return
	}
	if m == nil {
		m = map[string]string{}
	}
	writeJSONStatus(w, http.StatusOK, m)
}

func (s *Server) handleIngestSummaries(w http.ResponseWriter, r *http.Request) {
	var batch []transcript.IngestSummary
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&batch); err != nil && !errors.Is(err, io.EOF) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON body."})
		return
	}
	res, err := s.Ingestor.IngestSummaries(batch)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, res)
}

func (s *Server) handleAnalysisProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flavor := queryOrDefault(r.URL.Query().Get("flavor"), transcript.DefaultFlavor)
	state := s.Orchestrator.Progress(flavor, id)
	if state == nil {
		writeJSONStatus(w, http.StatusOK, ProgressResponse{Phase: "", Progress: -1})
		return
	}
	writeJSONStatus(w, http.StatusOK, ProgressResponse{Phase: state.Phase, Progress: state.Progress})
}

func (s *Server) handleSummarizeConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	flavor := queryOrDefault(q.Get("flavor"), transcript.DefaultFlavor)
	force := parseBoolOrDefault(q.Get("force"), false)

	if s.AiConfig != nil && !s.AiConfig.IsConfigured() {
		writeJSONStatus(w, http.StatusOK, map[string]string{
			"summary": s.AiConfig.NotConfiguredMessage(),
		})
		return
	}

	out, err := s.Orchestrator.Summarize(r.Context(), flavor, id, force)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeRawJSON(w, http.StatusOK, out)
}

func (s *Server) handleEval(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	flavor := queryOrDefault(q.Get("flavor"), transcript.DefaultFlavor)
	judge := parseBoolOrDefault(q.Get("judge"), false)

	report, err := s.Eval.ForFlavor(r.Context(), flavor, judge)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, report)
}

func (s *Server) handleSaveEvalRun(w http.ResponseWriter, r *http.Request) {
	var report store.EvalReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON body."})
		return
	}
	snap, err := s.EvalRuns.Save(report)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, snap)
}

func (s *Server) handleListEvalRuns(w http.ResponseWriter, r *http.Request) {
	flavor := queryOrDefault(r.URL.Query().Get("flavor"), transcript.DefaultFlavor)
	runs, err := s.EvalRuns.List(flavor)
	if s.checkStoreError(w, r, err) {
		return
	}
	if runs == nil {
		runs = []store.EvalRunSnapshot{}
	}
	writeJSONStatus(w, http.StatusOK, runs)
}

func (s *Server) handleDeleteEvalRun(w http.ResponseWriter, r *http.Request) {
	savedAt := r.URL.Query().Get("savedAt")
	deleted, err := s.EvalRuns.Delete(savedAt)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, DeleteResult{Deleted: deleted})
}

func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	flavor := queryOrDefault(r.URL.Query().Get("flavor"), transcript.DefaultFlavor)
	report, err := s.Insights.ForFlavor(flavor)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, report)
}

func (s *Server) handleInsightsDrilldown(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	flavor := queryOrDefault(q.Get("flavor"), transcript.DefaultFlavor)
	category := q.Get("category")
	key := q.Get("key")

	res, err := s.Insights.Drilldown(flavor, category, key)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, res)
}

func (s *Server) handleMine(w http.ResponseWriter, r *http.Request) {
	flavor := queryOrDefault(r.URL.Query().Get("flavor"), transcript.DefaultFlavor)
	report, err := s.Miner.ForFlavor(r.Context(), flavor)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, report)
}

func (s *Server) handleOptimizeDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSONStatus(w, http.StatusOK, DefaultInstructionResponse{
		Instruction: insights.DefaultInstruction,
		MaxSample:   insights.MaxSample,
	})
}

func (s *Server) handleOptimizeCompare(w http.ResponseWriter, r *http.Request) {
	var req insights.OptimizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON body."})
		return
	}
	flavor := req.Flavor
	if strings.TrimSpace(flavor) == "" {
		flavor = transcript.DefaultFlavor
	}
	report, err := s.Optimize.Compare(r.Context(), flavor, req.SampleSize, req.InstructionA, req.InstructionB)
	if s.checkStoreError(w, r, err) {
		return
	}
	writeJSONStatus(w, http.StatusOK, report)
}

func (s *Server) checkStoreError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if _, ok := store.IsStoreUnavailable(err); ok {
		log.Printf(
			"WARN  StoreUnavailableHandler - Store unavailable serving %s: %s",
			config.EscapeLog(r.URL.Path),
			config.EscapeLog(err.Error()),
		)
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
			"error": "The trajectory store is unavailable. Is the database running? Try `docker compose up -d`.",
		})
		return true
	}
	log.Printf(
		"ERROR Server - Unexpected error serving %s: %s",
		config.EscapeLog(r.URL.Path),
		config.EscapeLog(err.Error()),
	)
	writeJSONStatus(w, http.StatusInternalServerError, map[string]string{
		"error": err.Error(),
	})
	return true
}

func queryOrDefault(val, def string) string {
	if strings.TrimSpace(val) == "" {
		return def
	}
	return val
}

func parseIntOrDefault(val string, def int) int {
	if strings.TrimSpace(val) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil {
		return def
	}
	return n
}

func parseBoolOrDefault(val string, def bool) bool {
	if strings.TrimSpace(val) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(val))
	if err != nil {
		return def
	}
	return b
}

func clamp(val, low, high int) int {
	return max(low, min(high, val))
}

func writeJSONStatus(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, err := json.Marshal(payload)
	if err != nil {
		_, _ = w.Write([]byte(`{"error":"serialization error"}`))
		return
	}
	_, _ = w.Write(b)
}

func writeRawJSON(w http.ResponseWriter, status int, rawJSON string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, rawJSON)
}

func writePlainText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}
