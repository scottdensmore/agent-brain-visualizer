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
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/scottdensmore/agent-brain-visualizer/internal/ai"
	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
)

const (
	evalMaxSummaryChars           = 2000
	evalMaxTokenChars             = 80
	evalMaxConsecutiveOccurrences = 6

	evalWorstCases       = 10
	JudgeMaxSessions     = 12
	judgeConcurrency     = 8
	evalMaxDigestChars   = 6000
	evalMaxAnalysisChars = 6000
)

// JudgeLenses are the 3 reviewer personas averaged by the LLM judge panel (ported from EvalService.JUDGE_LENSES).
var JudgeLenses = []string{
	"a strict, skeptical reviewer who demands explicit evidence for every claim",
	"a fair, balanced reviewer weighing the analysis's strengths and weaknesses",
	"a pragmatic engineer judging whether the analysis would actually help a teammate",
}

// EvalCaseResult is an alias for store.EvalCaseResult.
type EvalCaseResult = store.EvalCaseResult

// JudgedCase is an alias for store.JudgedCase.
type JudgedCase = store.JudgedCase

// JudgeSummary is an alias for store.JudgeSummary.
type JudgeSummary = store.JudgeSummary

// EvalReport is an alias for store.EvalReport.
type EvalReport = store.EvalReport

// CheckNames returns the 6 named deterministic quality checks in canonical order (ported from EvalScorer.checkNames).
func CheckNames() []string {
	return []string{
		"schema-complete",
		"has-recommendations",
		"issues-have-fixes",
		"concise-summary",
		"not-degenerate",
		"error-coverage",
	}
}

// ScoreSession deterministically grades one session's AI analysis against its transcript steps (ported from EvalScorer.score).
func ScoreSession(sessionID string, steps []map[string]any, analysis map[string]any) EvalCaseResult {
	passed := make([]string, 0, 6)
	failed := make([]string, 0, 6)

	summary := stepString(analysis, "summary")
	shortTitle := stepString(analysis, "shortTitle")
	flow, hasFlowArray := summaryFlow(analysis)
	recs := summaryRecommendations(analysis)
	issues := summaryIssues(analysis)

	schemaComplete := strings.TrimSpace(shortTitle) != "" &&
		strings.TrimSpace(summary) != "" &&
		hasFlowArray &&
		len(flow) > 0
	recordEvalCheck(&passed, &failed, "schema-complete", schemaComplete)

	recordEvalCheck(&passed, &failed, "has-recommendations", len(recs) > 0)

	issuesHaveFixes := true
	for _, iss := range issues {
		if strings.TrimSpace(stepString(iss, "circumvention")) == "" {
			issuesHaveFixes = false
			break
		}
	}
	recordEvalCheck(&passed, &failed, "issues-have-fixes", issuesHaveFixes)

	recordEvalCheck(&passed, &failed, "concise-summary", summary != "" && len(summary) <= evalMaxSummaryChars)

	recordEvalCheck(&passed, &failed, "not-degenerate", !isDegenerateText(summary))

	transcriptHadError := false
	for _, step := range steps {
		if IsStepError(step) {
			transcriptHadError = true
			break
		}
	}
	analysisHasIssue := len(issues) > 0
	recordEvalCheck(&passed, &failed, "error-coverage", !transcriptHadError || analysisHasIssue)

	total := len(passed) + len(failed)
	score := 0
	if total > 0 {
		score = int(math.Round(100.0 * float64(len(passed)) / float64(total)))
	}

	title := sessionID
	if strings.TrimSpace(shortTitle) != "" {
		title = shortTitle
	}

	return EvalCaseResult{
		SessionID: sessionID,
		Title:     title,
		Score:     score,
		Passed:    passed,
		Failed:    failed,
	}
}

func recordEvalCheck(passed, failed *[]string, name string, ok bool) {
	if ok {
		*passed = append(*passed, name)
	} else {
		*failed = append(*failed, name)
	}
}

func isDegenerateText(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	tokens := strings.Fields(text)
	run := 1
	for i := 0; i < len(tokens); i++ {
		if len(tokens[i]) > evalMaxTokenChars {
			return true
		}
		if i > 0 && strings.EqualFold(tokens[i], tokens[i-1]) {
			run++
			if run >= evalMaxConsecutiveOccurrences {
				return true
			}
		} else {
			run = 1
		}
	}
	return false
}

// EvalService runs deterministic quality checks and optional LLM-as-a-judge grading across sessions.
type EvalService struct {
	Collector *SessionCollector
	Config    *config.AiConfig
	Judge     *ai.AnalysisJudgeService
}

// NewEvalService constructs an EvalService wired to collector, cfg, and judge.
func NewEvalService(collector *SessionCollector, cfg *config.AiConfig, judge *ai.AnalysisJudgeService) *EvalService {
	if judge == nil && cfg != nil && cfg.IsConfigured() {
		judge = ai.NewAnalysisJudgeService(ai.NewChatModel(cfg))
	}
	return &EvalService{
		Collector: collector,
		Config:    cfg,
		Judge:     judge,
	}
}

// ModelLabel returns the active AI provider and model label ("ollama · <model>" or "gemini · <model>").
func (s *EvalService) ModelLabel() string {
	if s == nil || s.Config == nil {
		return "gemini · " + config.DefaultGeminiModel
	}
	if s.Config.Provider() == config.ProviderOllama {
		return "ollama · " + s.Config.OllamaModel()
	}
	return "gemini · " + s.Config.GeminiModel()
}

func (s *EvalService) getJudge() *ai.AnalysisJudgeService {
	if s.Judge != nil {
		return s.Judge
	}
	if s.Config != nil && s.Config.IsConfigured() {
		return ai.NewAnalysisJudgeService(ai.NewChatModel(s.Config))
	}
	return nil
}

type judgeableSession struct {
	id       string
	title    string
	steps    []map[string]any
	analysis map[string]any
}

// ForFlavor grades cached analyses in flavor and optionally runs the LLM judge panel (ported from EvalService.forFlavor).
func (s *EvalService) ForFlavor(ctx context.Context, flavor string, judge bool) (EvalReport, error) {
	var collected Collected
	if s != nil && s.Collector != nil {
		var err error
		collected, err = s.Collector.Collect(flavor)
		if err != nil {
			return EvalReport{}, err
		}
	}

	results := make([]EvalCaseResult, 0)
	judgeables := make([]judgeableSession, 0)

	for _, session := range collected.Sessions {
		if session.CachedSummary == nil {
			continue
		}
		id := sessionIDOf(session)
		res := ScoreSession(id, session.Steps, session.CachedSummary)
		results = append(results, res)
		judgeables = append(judgeables, judgeableSession{
			id:       id,
			title:    res.Title,
			steps:    session.Steps,
			analysis: session.CachedSummary,
		})
	}

	avgScore := 0.0
	if len(results) > 0 {
		sum := 0
		for _, r := range results {
			sum += r.Score
		}
		avgScore = Round1(float64(sum) / float64(len(results)))
	}

	passRates := make([]NameCount, 0, len(CheckNames()))
	for _, check := range CheckNames() {
		passing := 0
		for _, r := range results {
			for _, p := range r.Passed {
				if p == check {
					passing++
					break
				}
			}
		}
		passRates = append(passRates, NameCount{Name: check, Count: passing})
	}

	worst := make([]EvalCaseResult, len(results))
	copy(worst, results)
	sort.SliceStable(worst, func(i, j int) bool {
		return worst[i].Score < worst[j].Score
	})
	if len(worst) > evalWorstCases {
		worst = worst[:evalWorstCases]
	}

	return EvalReport{
		Flavor:            flavor,
		SessionCount:      collected.TotalSessionCount,
		SampledSessions:   len(collected.Sessions),
		EvaluatedSessions: len(results),
		AvgScore:          avgScore,
		ModelLabel:        s.ModelLabel(),
		CheckPassRates:    passRates,
		WorstCases:        worst,
		Judge:             s.runJudge(ctx, judge, judgeables),
	}, nil
}

func (s *EvalService) runJudge(ctx context.Context, requested bool, judgeables []judgeableSession) *JudgeSummary {
	if !requested {
		return store.JudgeNotRun("Run the LLM judge to add faithfulness / actionability / clarity scores.")
	}
	if len(judgeables) == 0 {
		return store.JudgeNotRun("No analyzed sessions to judge yet.")
	}
	if s.Config == nil || !s.Config.IsConfigured() {
		return store.JudgeNotRun("Configure an AI provider to run the LLM judge.")
	}
	judgeSvc := s.getJudge()
	if judgeSvc == nil {
		return store.JudgeNotRun("Configure an AI provider to run the LLM judge.")
	}

	sample := judgeables
	if len(sample) > JudgeMaxSessions {
		sample = sample[:JudgeMaxSessions]
	}

	type lensVerdict struct {
		score ai.JudgeScore
		ok    bool
	}
	panels := make([][]lensVerdict, len(sample))
	for i := range panels {
		panels[i] = make([]lensVerdict, len(JudgeLenses))
	}

	sem := make(chan struct{}, judgeConcurrency)
	var wg sync.WaitGroup
	for i, j := range sample {
		sIdx := i
		jb := j
		for lIdx, lens := range JudgeLenses {
			lensIdx := lIdx
			lensStr := lens
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				sc, ok := s.judgeWithLens(ctx, judgeSvc, jb, lensStr)
				panels[sIdx][lensIdx] = lensVerdict{score: sc, ok: ok}
			}()
		}
	}
	wg.Wait()

	cases := make([]JudgedCase, 0, len(sample))
	for i, jb := range sample {
		verdicts := make([]ai.JudgeScore, 0, len(JudgeLenses))
		for _, v := range panels[i] {
			if v.ok {
				verdicts = append(verdicts, v.score)
			}
		}
		if len(verdicts) > 0 {
			cases = append(cases, ensembleJudgeVerdicts(jb, verdicts))
		}
	}

	if len(cases) == 0 {
		return store.JudgeNotRun("The LLM judge was unavailable (the model did not respond). Showing deterministic checks only.")
	}

	faithSum := 0.0
	actionSum := 0.0
	claritySum := 0.0
	for _, c := range cases {
		faithSum += c.Faithfulness
		actionSum += c.Actionability
		claritySum += c.Clarity
	}
	n := float64(len(cases))

	return &JudgeSummary{
		Ran:              true,
		Note:             "",
		JudgedSessions:   len(cases),
		AvgFaithfulness:  Round1(faithSum / n),
		AvgActionability: Round1(actionSum / n),
		AvgClarity:       Round1(claritySum / n),
		Cases:            cases,
	}
}

func (s *EvalService) judgeWithLens(
	ctx context.Context,
	judgeSvc *ai.AnalysisJudgeService,
	j judgeableSession,
	lens string,
) (ai.JudgeScore, bool) {
	analysisBytes, err := json.Marshal(j.analysis)
	analysisStr := "{}"
	if err == nil {
		analysisStr = string(analysisBytes)
	}
	sc, err := judgeSvc.Judge(
		ctx,
		ai.FenceTranscript(buildJudgeDigest(j.steps)),
		ai.FenceAnalysis(truncateStr(analysisStr, evalMaxAnalysisChars)),
		lens,
	)
	if err != nil {
		log.Printf("WARN  EvalService - Judge lens failed for %s: %s", config.EscapeLog(j.id), config.EscapeLog(err.Error()))
		return ai.JudgeScore{}, false
	}
	return ai.JudgeScore{
		Faithfulness:  ClampScore(sc.Faithfulness),
		Actionability: ClampScore(sc.Actionability),
		Clarity:       ClampScore(sc.Clarity),
		Comment:       sc.Comment,
	}, true
}

func ensembleJudgeVerdicts(j judgeableSession, verdicts []ai.JudgeScore) JudgedCase {
	comment := ""
	for _, v := range verdicts {
		if strings.TrimSpace(v.Comment) != "" {
			comment = v.Comment
			break
		}
	}

	minOverall := math.MaxFloat64
	maxOverall := -math.MaxFloat64
	faithSum := 0
	actionSum := 0
	claritySum := 0

	for _, v := range verdicts {
		faithSum += v.Faithfulness
		actionSum += v.Actionability
		claritySum += v.Clarity
		overall := float64(v.Faithfulness+v.Actionability+v.Clarity) / 3.0
		if overall < minOverall {
			minOverall = overall
		}
		if overall > maxOverall {
			maxOverall = overall
		}
	}

	n := float64(len(verdicts))
	return JudgedCase{
		SessionID:     j.id,
		Title:         j.title,
		Faithfulness:  Round1(float64(faithSum) / n),
		Actionability: Round1(float64(actionSum) / n),
		Clarity:       Round1(float64(claritySum) / n),
		Samples:       len(verdicts),
		PanelMin:      Round1(minOverall),
		PanelMax:      Round1(maxOverall),
		Comment:       comment,
	}
}

func buildJudgeDigest(steps []map[string]any) string {
	var sb strings.Builder
	for _, step := range steps {
		if sb.Len() >= evalMaxDigestChars {
			sb.WriteString("...(truncated)\n")
			break
		}
		toolCalls := stepToolCalls(step)
		stepType := stepString(step, "type")
		if len(toolCalls) > 0 {
			names := make([]string, 0, len(toolCalls))
			for _, t := range toolCalls {
				name := stepString(t, "name")
				if name == "" {
					name = "unknown"
				}
				names = append(names, name)
			}
			sb.WriteString("TOOL: ")
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
			sb.WriteString(truncateStr(strings.TrimSpace(errStr), 200))
			sb.WriteString("\n")
		} else if stepType == "USER_INPUT" {
			content := strings.TrimSpace(stepString(step, "content"))
			if content != "" {
				sb.WriteString("USER: ")
				sb.WriteString(truncateStr(content, 300))
				sb.WriteString("\n")
			}
		}
	}
	if sb.Len() == 0 {
		return "(no significant steps)"
	}
	return sb.String()
}

// ClampScore clamps an integer judge score to [1, 5].
func ClampScore(v int) int {
	if v < 1 {
		return 1
	}
	if v > 5 {
		return 5
	}
	return v
}

// Round1 rounds v to 1 decimal place.
func Round1(v float64) float64 {
	return math.Round(v*10.0) / 10.0
}
