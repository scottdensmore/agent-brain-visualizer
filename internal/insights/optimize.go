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

package insights

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/scottdensmore/agent-brain-visualizer/internal/ai"
	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
)

const (
	// DefaultInstruction is the baseline analysis instruction used for variant A by default (ported from OptimizeService.DEFAULT_INSTRUCTION).
	DefaultInstruction = `Analyze the following coding-agent session transcript. Extract, into the structured format:
a very short title; the key flow of what happened; the agent's notable actions; the issues or
errors it hit and how it worked around them; concrete, actionable recommendations (e.g. missing
CLI tools, skills to create, or AGENTS.md advice); and a short (max 3 paragraph) summary.`

	// MaxSample is the maximum number of sessions evaluated in an A/B prompt comparison.
	MaxSample = 5
	// MaxOptimizeSample is an alias for MaxSample.
	MaxOptimizeSample = MaxSample

	optimizeConcurrency        = 6
	optimizeMaxTranscriptChars = 12000
)

// OptimizeRequest is the request payload for POST /api/optimize (ported from OptimizeRequest.java).
type OptimizeRequest struct {
	Flavor       string `json:"flavor"`
	SampleSize   int    `json:"sampleSize"`
	InstructionA string `json:"instructionA"`
	InstructionB string `json:"instructionB"`
}

// VariantResult holds one prompt variant's eval outcome over the sampled sessions (ported from VariantResult.java).
type VariantResult struct {
	AvgScore       float64     `json:"avgScore"`
	Scored         int         `json:"scored"`
	CheckPassRates []NameCount `json:"checkPassRates"`
}

// EmptyVariantResult returns an empty VariantResult (0.0 avgScore, 0 scored, [] checkPassRates).
func EmptyVariantResult() VariantResult {
	return VariantResult{
		AvgScore:       0.0,
		Scored:         0,
		CheckPassRates: []NameCount{},
	}
}

// MarshalJSON ensures CheckPassRates serializes as [] rather than null.
func (v VariantResult) MarshalJSON() ([]byte, error) {
	type wire struct {
		AvgScore       float64     `json:"avgScore"`
		Scored         int         `json:"scored"`
		CheckPassRates []NameCount `json:"checkPassRates"`
	}
	return json.Marshal(wire{
		AvgScore:       v.AvgScore,
		Scored:         v.Scored,
		CheckPassRates: nonNilNameCounts(v.CheckPassRates),
	})
}

// OptimizeReport is the result of a prompt-lab comparison returned by POST /api/optimize (ported from OptimizeReport.java).
type OptimizeReport struct {
	SampleSize int           `json:"sampleSize"`
	Note       string        `json:"note"`
	A          VariantResult `json:"a"`
	B          VariantResult `json:"b"`
}

// UnavailableOptimizeReport returns an OptimizeReport with sampleSize=0 and an explanatory note.
func UnavailableOptimizeReport(note string) OptimizeReport {
	return OptimizeReport{
		SampleSize: 0,
		Note:       note,
		A:          EmptyVariantResult(),
		B:          EmptyVariantResult(),
	}
}

// OptimizeService runs eval-driven A/B prompt optimization across a sample of sessions (ported from OptimizeService.java).
type OptimizeService struct {
	Collector *SessionCollector
	Analyzer  *ai.VariantAnalyzerService
	Config    *config.AiConfig
}

// NewOptimizeService constructs an OptimizeService wired to collector, analyzer, and cfg.
func NewOptimizeService(collector *SessionCollector, analyzer *ai.VariantAnalyzerService, cfg *config.AiConfig) *OptimizeService {
	if analyzer == nil && cfg != nil && cfg.IsConfigured() {
		analyzer = ai.NewVariantAnalyzerService(ai.NewChatModel(cfg))
	}
	return &OptimizeService{
		Collector: collector,
		Analyzer:  analyzer,
		Config:    cfg,
	}
}

func (s *OptimizeService) getAnalyzer() *ai.VariantAnalyzerService {
	if s.Analyzer != nil {
		return s.Analyzer
	}
	if s.Config != nil && s.Config.IsConfigured() {
		return ai.NewVariantAnalyzerService(ai.NewChatModel(s.Config))
	}
	return nil
}

// Defaults returns the baseline instruction and max sample size for GET /api/optimize.
func (s *OptimizeService) Defaults() map[string]any {
	return map[string]any{
		"instruction": DefaultInstruction,
		"maxSample":   MaxSample,
	}
}

// Compare re-analyzes up to sampleSize sessions with instructionA and instructionB and scores both variants.
func (s *OptimizeService) Compare(
	ctx context.Context,
	flavor string,
	sampleSize int,
	instructionA string,
	instructionB string,
) (OptimizeReport, error) {
	if s.Config == nil || !s.Config.IsConfigured() {
		return UnavailableOptimizeReport("Configure an AI provider to run the prompt lab."), nil
	}
	analyzer := s.getAnalyzer()
	if analyzer == nil {
		return UnavailableOptimizeReport("Configure an AI provider to run the prompt lab."), nil
	}

	a := instructionA
	if strings.TrimSpace(a) == "" {
		a = DefaultInstruction
	}
	b := instructionB
	if strings.TrimSpace(b) == "" {
		b = DefaultInstruction
	}
	n := max(1, min(MaxSample, sampleSize))

	var collected Collected
	if s.Collector != nil {
		var err error
		collected, err = s.Collector.Collect(flavor)
		if err != nil {
			return OptimizeReport{}, err
		}
	}

	sample := make([]Session, 0, n)
	for _, sess := range collected.Sessions {
		if len(sess.Steps) > 0 {
			sample = append(sample, sess)
			if len(sample) >= n {
				break
			}
		}
	}
	if len(sample) == 0 {
		return UnavailableOptimizeReport("No sessions with a transcript to analyze here."), nil
	}

	type outcome struct {
		res EvalCaseResult
		ok  bool
	}
	outcomesA := make([]outcome, len(sample))
	outcomesB := make([]outcome, len(sample))

	sem := make(chan struct{}, optimizeConcurrency)
	var wg sync.WaitGroup
	for i, sess := range sample {
		idx := i
		session := sess
		txText := optimizeTranscriptText(session.Steps)

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, ok := s.runVariant(ctx, analyzer, session, a, txText)
			outcomesA[idx] = outcome{res: r, ok: ok}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, ok := s.runVariant(ctx, analyzer, session, b, txText)
			outcomesB[idx] = outcome{res: r, ok: ok}
		}()
	}
	wg.Wait()

	resultsA := make([]EvalCaseResult, 0, len(sample))
	for _, o := range outcomesA {
		if o.ok {
			resultsA = append(resultsA, o.res)
		}
	}
	resultsB := make([]EvalCaseResult, 0, len(sample))
	for _, o := range outcomesB {
		if o.ok {
			resultsB = append(resultsB, o.res)
		}
	}

	return OptimizeReport{
		SampleSize: len(sample),
		Note:       "",
		A:          aggregateVariantResults(resultsA),
		B:          aggregateVariantResults(resultsB),
	}, nil
}

func (s *OptimizeService) runVariant(
	ctx context.Context,
	analyzer *ai.VariantAnalyzerService,
	session Session,
	instruction string,
	transcript string,
) (EvalCaseResult, bool) {
	resp, err := analyzer.AnalyzeWithInstruction(ctx, instruction, ai.FenceTranscript(transcript))
	if err != nil {
		log.Printf("WARN  OptimizeService - Prompt-lab analysis failed: %s", config.EscapeLog(err.Error()))
		return EvalCaseResult{}, false
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return EvalCaseResult{}, false
	}
	var analysisMap map[string]any
	if err := json.Unmarshal(b, &analysisMap); err != nil {
		return EvalCaseResult{}, false
	}
	return ScoreSession(sessionIDOf(session), session.Steps, analysisMap), true
}

func aggregateVariantResults(results []EvalCaseResult) VariantResult {
	if len(results) == 0 {
		return EmptyVariantResult()
	}
	sum := 0
	for _, r := range results {
		sum += r.Score
	}
	avg := Round1(float64(sum) / float64(len(results)))

	passRates := make([]NameCount, 0, len(CheckNames()))
	for _, check := range CheckNames() {
		count := 0
		for _, r := range results {
			for _, p := range r.Passed {
				if p == check {
					count++
					break
				}
			}
		}
		passRates = append(passRates, NameCount{Name: check, Count: count})
	}
	return VariantResult{
		AvgScore:       avg,
		Scored:         len(results),
		CheckPassRates: passRates,
	}
}

func optimizeTranscriptText(steps []map[string]any) string {
	var sb strings.Builder
	for _, step := range steps {
		if sb.Len() >= optimizeMaxTranscriptChars {
			sb.WriteString("...(truncated)\n")
			break
		}
		stepType := stepString(step, "type")
		toolCalls := stepToolCalls(step)
		if len(toolCalls) > 0 {
			names := make([]string, 0, len(toolCalls))
			for _, t := range toolCalls {
				name := stepString(t, "name")
				if name == "" {
					name = "unknown"
				}
				names = append(names, name)
			}
			sb.WriteString("TOOL_CALL: ")
			sb.WriteString(strings.Join(names, ", "))
			sb.WriteString("\n")
		} else if IsStepError(step) {
			var errStr string
			if _, hasErr := step["error"]; hasErr {
				errStr = stepString(step, "error")
			} else {
				errStr = stepString(step, "content")
			}
			sb.WriteString("ERROR: ")
			sb.WriteString(truncateStr(strings.TrimSpace(errStr), 400))
			sb.WriteString("\n")
		} else if stepType == "USER_INPUT" {
			content := strings.TrimSpace(stepString(step, "content"))
			if content != "" {
				sb.WriteString("USER: ")
				sb.WriteString(truncateStr(content, 500))
				sb.WriteString("\n")
			}
		} else {
			content := strings.TrimSpace(stepString(step, "content"))
			if content != "" {
				sb.WriteString("AGENT: ")
				sb.WriteString(truncateStr(content, 500))
				sb.WriteString("\n")
			}
		}
	}
	if sb.Len() == 0 {
		return "(empty transcript)"
	}
	return sb.String()
}
