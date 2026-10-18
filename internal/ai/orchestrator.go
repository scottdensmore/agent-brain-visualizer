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
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
	"github.com/scottdensmore/agent-brain-visualizer/internal/transcript"
)

const (
	// MaxTokensPerChunk is the conservative token budget per chunk sent to the LLM.
	MaxTokensPerChunk = 100000
	// MaxConcurrentChunkAnalyses bounds the number of concurrent chunk analysis calls.
	MaxConcurrentChunkAnalyses = 20
	// MaxConsolidationTokens is the max token budget for a single consolidation prompt.
	MaxConsolidationTokens = 500000

	// MaxTitleChars bounds the normalized shortTitle length.
	MaxTitleChars = 120
	// MaxSummaryChars bounds the normalized summary length.
	MaxSummaryChars = 8000
	// MaxItemChars bounds individual flow, action, issue, and recommendation string lengths.
	MaxItemChars = 600
	// MaxItems bounds the number of flow steps, agentActions, and issues retained after normalization.
	MaxItems = 40
	// MaxRecommendations bounds the number of recommendations retained after normalization.
	MaxRecommendations = 30
)

// ProgressState reports the live phase and completion percentage of an in-flight analysis.
type ProgressState struct {
	Phase    string `json:"phase"`
	Progress int    `json:"progress"`
}

// ProgressStatus is an alias for ProgressState.
type ProgressStatus = ProgressState

// AnalysisOrchestrator coordinates transcript condensation, chunking, parallel LLM analysis,
// map-reduce consolidation, and summary caching.
type AnalysisOrchestrator struct {
	Config       *config.AiConfig
	Analyzer     *AnalyzerService
	TokenCounter *TokenCounter
	SessionRepo  *store.SessionRepository
	SummaryRepo  *store.SummaryRepository

	mu          sync.Mutex
	activeTasks map[string]bool
	progressMap map[string]ProgressState
}

// NewAnalysisOrchestrator constructs an AnalysisOrchestrator wired to cfg and the store repositories.
func NewAnalysisOrchestrator(
	cfg *config.AiConfig,
	sessionRepo *store.SessionRepository,
	summaryRepo *store.SummaryRepository,
) *AnalysisOrchestrator {
	var analyzer *AnalyzerService
	if cfg != nil && cfg.IsConfigured() {
		analyzer = NewAnalyzerService(NewChatModel(cfg))
	}
	return &AnalysisOrchestrator{
		Config:       cfg,
		Analyzer:     analyzer,
		TokenCounter: NewTokenCounter(cfg),
		SessionRepo:  sessionRepo,
		SummaryRepo:  summaryRepo,
		activeTasks:  make(map[string]bool),
		progressMap:  make(map[string]ProgressState),
	}
}

func taskKey(flavorName, id string) string {
	f := strings.TrimSpace(flavorName)
	if f == "" {
		f = transcript.DefaultFlavor
	}
	return f + "/" + id
}

// Progress returns the current in-flight ProgressState for (flavorName, id), or nil if none is running.
func (o *AnalysisOrchestrator) Progress(flavorName, id string) *ProgressState {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progressMap == nil {
		return nil
	}
	st, ok := o.progressMap[taskKey(flavorName, id)]
	if !ok {
		return nil
	}
	copySt := st
	return &copySt
}

func (o *AnalysisOrchestrator) setProgress(key, phase string, progress int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progressMap == nil {
		o.progressMap = make(map[string]ProgressState)
	}
	o.progressMap[key] = ProgressState{Phase: phase, Progress: progress}
}

func (o *AnalysisOrchestrator) tryClaimTask(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.activeTasks == nil {
		o.activeTasks = make(map[string]bool)
	}
	if o.activeTasks[key] {
		return false
	}
	o.activeTasks[key] = true
	return true
}

func (o *AnalysisOrchestrator) releaseTask(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.activeTasks, key)
	delete(o.progressMap, key)
}

func (o *AnalysisOrchestrator) estimateTokens(text string) int {
	if o != nil && o.TokenCounter != nil {
		return o.TokenCounter.Estimate(text)
	}
	return EstimateTokens(text)
}

func (o *AnalysisOrchestrator) getAnalyzer() *AnalyzerService {
	if o.Analyzer != nil {
		return o.Analyzer
	}
	if o.Config != nil && o.Config.IsConfigured() {
		return NewAnalyzerService(NewChatModel(o.Config))
	}
	return nil
}

func summaryOnlyJSON(msg string) string {
	b, err := json.Marshal(map[string]string{"summary": msg})
	if err != nil {
		return `{"summary":""}`
	}
	return string(b)
}

// Summarize condenses and analyzes a session transcript, returning the cached or newly generated
// JSON string (or a `{"summary": ...}` message when AI is unconfigured, already running, missing, or fails).
// Returns a non-nil error only when the backing database is unavailable (*store.StoreUnavailableError).
func (o *AnalysisOrchestrator) Summarize(ctx context.Context, flavorName, id string, forceRecompute bool) (string, error) {
	effectiveFlavor := strings.TrimSpace(flavorName)
	if effectiveFlavor == "" {
		effectiveFlavor = transcript.DefaultFlavor
	}

	analyzer := o.getAnalyzer()
	if analyzer == nil || (o.Config != nil && !o.Config.IsConfigured() && o.Analyzer == nil) {
		msg := "AI summarization is not configured."
		if o.Config != nil {
			msg = o.Config.NotConfiguredMessage()
		}
		return summaryOnlyJSON(msg), nil
	}

	key := taskKey(effectiveFlavor, id)
	if !o.tryClaimTask(key) {
		return `{"summary": "Analysis is already running in the background for this conversation. Please wait a moment and refresh."}`, nil
	}
	defer o.releaseTask(key)

	o.setProgress(key, "Checking cache & transcript...", 5)

	if o.SessionRepo == nil {
		return `{"summary": "No transcript found."}`, nil
	}
	exists, err := o.SessionRepo.Exists(effectiveFlavor, id)
	if err != nil {
		return "", err
	}
	if !exists {
		return `{"summary": "No transcript found."}`, nil
	}

	if o.SummaryRepo != nil {
		if forceRecompute {
			if err := o.SummaryRepo.Delete(effectiveFlavor, id); err != nil {
				return "", err
			}
		} else {
			cached, ok, err := o.SummaryRepo.Find(effectiveFlavor, id)
			if err != nil {
				return "", err
			}
			if ok && strings.TrimSpace(cached) != "" {
				return cached, nil
			}
		}
	}

	o.setProgress(key, "Parsing sequences...", 10)
	stepsJSON, ok, err := o.SessionRepo.Steps(effectiveFlavor, id)
	if err != nil {
		return "", err
	}
	var sequences [][]string
	if ok && strings.TrimSpace(stepsJSON) != "" {
		sequences = transcript.AnalysisSequencesFromStepsJSON(effectiveFlavor, stepsJSON)
	}

	o.setProgress(key, "Calculating optimal token chunks...", 15)
	optimalChunks := o.buildChunks(sequences)

	if len(optimalChunks) == 0 {
		return `{"summary": "No transcript lines found."}`, nil
	}

	seqResponses := o.analyzeChunksInParallel(ctx, analyzer, key, optimalChunks)
	if len(seqResponses) == 0 {
		return `{"summary": "Analysis could not be generated: the model did not return a result for any part of this conversation. Please try again."}`, nil
	}

	var finalResponse AnalysisResponse
	cacheable := false

	if len(seqResponses) == 1 {
		finalResponse = Normalize(seqResponses[0])
		cacheable = true
	} else {
		consolidated, ok := o.consolidateInTiers(ctx, analyzer, key, seqResponses)
		if ok {
			finalResponse = Normalize(consolidated)
			cacheable = true
		} else {
			log.Printf("WARN  AnalysisOrchestrator - Consolidation failed for %s; returning local merge of %d partial analyses", config.EscapeLog(key), len(seqResponses))
			finalResponse = LocalMerge(seqResponses)
			cacheable = false
		}
	}

	jsonBytes, err := json.MarshalIndent(finalResponse, "", "  ")
	if err != nil {
		return summaryOnlyJSON("Error generating summary: " + err.Error()), nil
	}
	jsonResult := string(jsonBytes)

	if cacheable && o.SummaryRepo != nil {
		o.setProgress(key, "Saving summary to cache...", 95)
		if _, err := o.SummaryRepo.Upsert(effectiveFlavor, id, jsonResult, finalResponse.ShortTitle); err != nil {
			if _, isUnavail := store.IsStoreUnavailable(err); isUnavail {
				return "", err
			}
			log.Printf("WARN  AnalysisOrchestrator - Failed to save summary for %s: %v", config.EscapeLog(key), err)
		}
	}

	return jsonResult, nil
}

func (o *AnalysisOrchestrator) buildChunks(sequences [][]string) []string {
	combinedLines := make([]string, 0)
	for _, seq := range sequences {
		combinedLines = append(combinedLines, seq...)
	}
	if len(combinedLines) == 0 {
		return nil
	}
	rawChunks := transcript.SplitIntoSafeChunks(combinedLines, o.estimateTokens, MaxTokensPerChunk)
	chunks := make([]string, 0, len(rawChunks))
	for _, c := range rawChunks {
		if len(c) > 0 {
			chunks = append(chunks, strings.Join(c, "\n"))
		}
	}
	return chunks
}

func (o *AnalysisOrchestrator) analyzeChunksInParallel(
	ctx context.Context,
	analyzer *AnalyzerService,
	key string,
	chunks []string,
) []AnalysisResponse {
	total := len(chunks)
	o.setProgress(key, "Starting chunk processing...", 0)

	type indexedResult struct {
		resp AnalysisResponse
		ok   bool
	}
	results := make([]indexedResult, total)
	var cursor atomic.Int32
	var completed atomic.Int32
	workerCount := min(MaxConcurrentChunkAnalyses, total)
	var wg sync.WaitGroup

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				idx := int(cursor.Add(1) - 1)
				if idx >= total {
					return
				}
				chunk := chunks[idx]
				res, err := analyzer.Analyze(ctx, FenceTranscript(chunk))
				done := int(completed.Add(1))
				pct := int(float64(done)*90.0/float64(total) + 0.5)
				o.advanceProgress(key, fmt.Sprintf("Processing chunk %d of %d...", done, total), pct)

				if err != nil {
					log.Printf("WARN  AnalysisOrchestrator - Chunk %d/%d analysis failed: %s", idx+1, total, config.EscapeLog(err.Error()))
					continue
				}
				results[idx] = indexedResult{resp: res, ok: true}
			}
		}()
	}
	wg.Wait()

	out := make([]AnalysisResponse, 0, total)
	for _, r := range results {
		if r.ok {
			out = append(out, r.resp)
		}
	}
	return out
}

func (o *AnalysisOrchestrator) advanceProgress(key, phase string, progress int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progressMap == nil {
		o.progressMap = make(map[string]ProgressState)
	}
	if cur, exists := o.progressMap[key]; !exists || progress > cur.Progress {
		o.progressMap[key] = ProgressState{Phase: phase, Progress: progress}
	}
}

func (o *AnalysisOrchestrator) consolidateInTiers(
	ctx context.Context,
	analyzer *AnalyzerService,
	key string,
	partials []AnalysisResponse,
) (AnalysisResponse, bool) {
	o.setProgress(key, "Consolidating final analysis...", 90)
	res, err := o.recursivelyConsolidate(ctx, analyzer, partials, MaxConsolidationTokens)
	if err != nil {
		log.Printf("WARN  AnalysisOrchestrator - Consolidation failed for %s: %s", config.EscapeLog(key), config.EscapeLog(err.Error()))
		return AnalysisResponse{}, false
	}
	return res, true
}

func (o *AnalysisOrchestrator) recursivelyConsolidate(
	ctx context.Context,
	analyzer *AnalyzerService,
	responses []AnalysisResponse,
	maxTokens int,
) (AnalysisResponse, error) {
	b, err := json.Marshal(responses)
	if err != nil {
		return AnalysisResponse{}, err
	}
	jsonStr := string(b)
	withinBudget := o.estimateTokens(jsonStr) <= maxTokens
	if withinBudget || len(responses) <= 1 {
		return analyzer.ConsolidateAnalysis(ctx, FenceAnalysis(jsonStr))
	}

	mid := len(responses) / 2
	r1, err := o.recursivelyConsolidate(ctx, analyzer, responses[:mid], maxTokens)
	if err != nil {
		return AnalysisResponse{}, err
	}
	r2, err := o.recursivelyConsolidate(ctx, analyzer, responses[mid:], maxTokens)
	if err != nil {
		return AnalysisResponse{}, err
	}
	pairBytes, err := json.Marshal([]AnalysisResponse{r1, r2})
	if err != nil {
		return AnalysisResponse{}, err
	}
	return analyzer.ConsolidateAnalysis(ctx, FenceAnalysis(string(pairBytes)))
}

// Normalize sanitizes and bounds all fields of an LLM-produced AnalysisResponse before storage or UI rendering.
func Normalize(r AnalysisResponse) AnalysisResponse {
	title := strings.TrimSpace(BoundedLine(r.ShortTitle, MaxTitleChars))
	if title == "" {
		title = "Session Summary"
	}

	flow := make([]string, 0, min(len(r.Flow), MaxItems))
	for _, s := range r.Flow {
		if len(flow) >= MaxItems {
			break
		}
		cleaned := BoundedLine(s, MaxItemChars)
		if cleaned != "" {
			flow = append(flow, cleaned)
		}
	}

	actions := make([]transcript.AgentAction, 0, min(len(r.AgentActions), MaxItems))
	for _, a := range r.AgentActions {
		if len(actions) >= MaxItems {
			break
		}
		act := BoundedLine(a.Action, MaxItemChars)
		desc := BoundedLine(a.Description, MaxItemChars)
		if act != "" || desc != "" {
			actions = append(actions, transcript.AgentAction{
				Action:      act,
				Description: desc,
			})
		}
	}

	issues := make([]Issue, 0, min(len(r.Issues), MaxItems))
	for _, iss := range r.Issues {
		if len(issues) >= MaxItems {
			break
		}
		errStr := BoundedLine(iss.Error, MaxItemChars)
		circ := BoundedLine(iss.Circumvention, MaxItemChars)
		if errStr != "" || circ != "" {
			issues = append(issues, Issue{
				Error:         errStr,
				Circumvention: circ,
			})
		}
	}

	recs := make([]string, 0, min(len(r.Recommendations), MaxRecommendations))
	for _, rec := range r.Recommendations {
		if len(recs) >= MaxRecommendations {
			break
		}
		cleaned := BoundedLine(rec, MaxItemChars)
		if cleaned != "" {
			recs = append(recs, cleaned)
		}
	}

	summary := BoundedText(r.Summary, MaxSummaryChars)

	return AnalysisResponse{
		ShortTitle:      title,
		Flow:            flow,
		AgentActions:    actions,
		Issues:          issues,
		Recommendations: recs,
		Summary:         summary,
	}
}

// LocalMerge performs a deterministic, non-LLM fallback merge of partial analyses when consolidation fails.
func LocalMerge(partials []AnalysisResponse) AnalysisResponse {
	title := "Session Summary"
	for _, p := range partials {
		if strings.TrimSpace(p.ShortTitle) != "" {
			title = p.ShortTitle
			break
		}
	}

	flowSeen := make(map[string]struct{})
	flow := make([]string, 0)
	actions := make([]transcript.AgentAction, 0)
	issues := make([]Issue, 0)
	recsSeen := make(map[string]struct{})
	recs := make([]string, 0)
	summaries := make([]string, 0, len(partials))

	for _, p := range partials {
		for _, f := range p.Flow {
			if strings.TrimSpace(f) != "" {
				if _, ok := flowSeen[f]; !ok {
					flowSeen[f] = struct{}{}
					flow = append(flow, f)
				}
			}
		}
		actions = append(actions, p.AgentActions...)
		issues = append(issues, p.Issues...)
		for _, r := range p.Recommendations {
			if strings.TrimSpace(r) != "" {
				if _, ok := recsSeen[r]; !ok {
					recsSeen[r] = struct{}{}
					recs = append(recs, r)
				}
			}
		}
		if strings.TrimSpace(p.Summary) != "" {
			summaries = append(summaries, strings.TrimSpace(p.Summary))
		}
	}

	summaryText := fmt.Sprintf("(Combined from %d partial analyses; full consolidation was unavailable.) %s", len(partials), strings.Join(summaries, "\n\n"))

	return Normalize(AnalysisResponse{
		ShortTitle:      title,
		Flow:            flow,
		AgentActions:    actions,
		Issues:          issues,
		Recommendations: recs,
		Summary:         summaryText,
	})
}
