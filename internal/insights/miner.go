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
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/scottdensmore/agent-brain-visualizer/internal/ai"
	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
)

const (
	minerTopN           = 12
	minSequenceSessions = 2
	maxMinerValueChars  = 200
)

// FixPair represents a recurring failure and how it was worked around, with how many sessions hit it.
type FixPair struct {
	Error string `json:"error"`
	Fix   string `json:"fix"`
	Count int    `json:"count"`
}

// Patterns holds the structural evidence mined from a set of sessions (ported from PatternMiner.Patterns).
type Patterns struct {
	ToolSequences    []NameCount
	FailureFixes     []FixPair
	Recommendations  []NameCount
	AnalyzedSessions int
}

// MiningReport is the output of the skill / AGENTS.md miner for one source (ported from MiningReport.java).
type MiningReport struct {
	Flavor           string             `json:"flavor"`
	SessionCount     int                `json:"sessionCount"`
	SampledSessions  int                `json:"sampledSessions"`
	AnalyzedSessions int                `json:"analyzedSessions"`
	AiGenerated      bool               `json:"aiGenerated"`
	Note             string             `json:"note"`
	ToolSequences    []NameCount        `json:"toolSequences"`
	FailureFixes     []FixPair          `json:"failureFixes"`
	Recommendations  []NameCount        `json:"recommendations"`
	Skills           []ai.SkillProposal `json:"skills"`
	AgentsRules      []ai.AgentsRule    `json:"agentsRules"`
	ToolingGaps      []string           `json:"toolingGaps"`
}

// MarshalJSON ensures all slice fields on MiningReport serialize as [] rather than null.
func (m MiningReport) MarshalJSON() ([]byte, error) {
	type wire struct {
		Flavor           string             `json:"flavor"`
		SessionCount     int                `json:"sessionCount"`
		SampledSessions  int                `json:"sampledSessions"`
		AnalyzedSessions int                `json:"analyzedSessions"`
		AiGenerated      bool               `json:"aiGenerated"`
		Note             string             `json:"note"`
		ToolSequences    []NameCount        `json:"toolSequences"`
		FailureFixes     []FixPair          `json:"failureFixes"`
		Recommendations  []NameCount        `json:"recommendations"`
		Skills           []ai.SkillProposal `json:"skills"`
		AgentsRules      []ai.AgentsRule    `json:"agentsRules"`
		ToolingGaps      []string           `json:"toolingGaps"`
	}
	fixes := m.FailureFixes
	if fixes == nil {
		fixes = []FixPair{}
	}
	skills := m.Skills
	if skills == nil {
		skills = []ai.SkillProposal{}
	}
	rules := m.AgentsRules
	if rules == nil {
		rules = []ai.AgentsRule{}
	}
	gaps := m.ToolingGaps
	if gaps == nil {
		gaps = []string{}
	}
	return json.Marshal(wire{
		Flavor:           m.Flavor,
		SessionCount:     m.SessionCount,
		SampledSessions:  m.SampledSessions,
		AnalyzedSessions: m.AnalyzedSessions,
		AiGenerated:      m.AiGenerated,
		Note:             m.Note,
		ToolSequences:    nonNilNameCounts(m.ToolSequences),
		FailureFixes:     fixes,
		Recommendations:  nonNilNameCounts(m.Recommendations),
		Skills:           skills,
		AgentsRules:      rules,
		ToolingGaps:      gaps,
	})
}

type fixAccumulator struct {
	errorText string
	fix       string
	count     int
}

func (f *fixAccumulator) offerFix(candidate string) {
	if strings.TrimSpace(f.fix) == "" && strings.TrimSpace(candidate) != "" {
		f.fix = candidate
	}
}

// MinePatterns deterministically mines recurring tool sequences, failure->fix pairs, and recommendations
// across sessions (ported from PatternMiner.mine).
func MinePatterns(sessions []Session) Patterns {
	seqFreq := newOrderedCounter()
	fixes := make(map[string]*fixAccumulator)
	var fixOrder []string
	recFreq := newOrderedCounter()
	analyzed := 0

	for _, session := range sessions {
		for _, seq := range SequencesInSteps(session.Steps) {
			seqFreq.inc(seq)
		}

		if session.CachedSummary == nil {
			continue
		}
		analyzed++

		sessionErrors := make(map[string]struct{})
		for _, issue := range summaryIssues(session.CachedSummary) {
			errText := cleanSummaryItem(stepString(issue, "error"))
			if errText == "" {
				continue
			}
			fixText := cleanSummaryItem(stepString(issue, "circumvention"))
			key := strings.ToLower(errText)
			acc, exists := fixes[key]
			if !exists {
				acc = &fixAccumulator{errorText: errText}
				fixes[key] = acc
				fixOrder = append(fixOrder, key)
			}
			acc.offerFix(fixText)
			if _, seen := sessionErrors[key]; !seen {
				sessionErrors[key] = struct{}{}
				acc.count++
			}
		}

		sessionRecs := make(map[string]struct{})
		for _, rec := range summaryRecommendations(session.CachedSummary) {
			text := cleanSummaryItem(rec)
			if text != "" {
				if _, seen := sessionRecs[text]; !seen {
					sessionRecs[text] = struct{}{}
					recFreq.inc(text)
				}
			}
		}
	}

	return Patterns{
		ToolSequences:    topMinedSequences(seqFreq),
		FailureFixes:     topMinedFixes(fixes, fixOrder),
		Recommendations:  recFreq.topN(minerTopN),
		AnalyzedSessions: analyzed,
	}
}

// SequencesInSteps returns the ordered set of distinct consecutive tool-call 2- and 3-grams in one session's steps.
func SequencesInSteps(steps []map[string]any) []string {
	tools := make([]string, 0)
	for _, step := range steps {
		for _, tool := range stepToolCalls(step) {
			name := stepString(tool, "name")
			if name == "" {
				name = "unknown"
			}
			tools = append(tools, name)
		}
	}

	seen := make(map[string]struct{})
	grams := make([]string, 0)
	for n := 2; n <= 3; n++ {
		for i := 0; i+n <= len(tools); i++ {
			gram := strings.Join(tools[i:i+n], " → ")
			if _, exists := seen[gram]; !exists {
				seen[gram] = struct{}{}
				grams = append(grams, gram)
			}
		}
	}
	return grams
}

func topMinedSequences(freq *orderedCounter) []NameCount {
	type indexed struct {
		name  string
		count int
		idx   int
	}
	items := make([]indexed, 0, len(freq.order))
	for i, k := range freq.order {
		c := freq.counts[k]
		if c >= minSequenceSessions {
			items = append(items, indexed{name: k, count: c, idx: i})
		}
	}
	if len(items) == 0 {
		return []NameCount{}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		if len(items[i].name) != len(items[j].name) {
			return len(items[i].name) > len(items[j].name)
		}
		return items[i].idx < items[j].idx
	})
	if len(items) > minerTopN {
		items = items[:minerTopN]
	}
	out := make([]NameCount, len(items))
	for i, it := range items {
		out[i] = NameCount{Name: it.name, Count: it.count}
	}
	return out
}

func topMinedFixes(fixes map[string]*fixAccumulator, order []string) []FixPair {
	if len(order) == 0 {
		return []FixPair{}
	}
	type indexed struct {
		acc *fixAccumulator
		idx int
	}
	items := make([]indexed, len(order))
	for i, k := range order {
		items[i] = indexed{acc: fixes[k], idx: i}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].acc.count != items[j].acc.count {
			return items[i].acc.count > items[j].acc.count
		}
		return items[i].idx < items[j].idx
	})
	if len(items) > minerTopN {
		items = items[:minerTopN]
	}
	out := make([]FixPair, len(items))
	for i, it := range items {
		out[i] = FixPair{
			Error: it.acc.errorText,
			Fix:   it.acc.fix,
			Count: it.acc.count,
		}
	}
	return out
}

// MinerService gathers a source's sessions, mines structural patterns with MinePatterns, and
// optionally runs MinerAdvisorService to phrase evidence into skills, AGENTS.md rules, and tooling gaps.
type MinerService struct {
	Collector *SessionCollector
	Advisor   *ai.MinerAdvisorService
	Config    *config.AiConfig
}

// NewMinerService constructs a MinerService wired to collector, advisor, and cfg.
func NewMinerService(collector *SessionCollector, advisor *ai.MinerAdvisorService, cfg *config.AiConfig) *MinerService {
	if advisor == nil && cfg != nil && cfg.IsConfigured() {
		advisor = ai.NewMinerAdvisorService(ai.NewChatModel(cfg))
	}
	return &MinerService{
		Collector: collector,
		Advisor:   advisor,
		Config:    cfg,
	}
}

func (s *MinerService) getAdvisor() *ai.MinerAdvisorService {
	if s.Advisor != nil {
		return s.Advisor
	}
	if s.Config != nil && s.Config.IsConfigured() {
		return ai.NewMinerAdvisorService(ai.NewChatModel(s.Config))
	}
	return nil
}

// ForFlavor mines patterns and proposes skills/rules for flavor (ported from MinerService.forFlavor).
func (s *MinerService) ForFlavor(ctx context.Context, flavor string) (MiningReport, error) {
	var collected Collected
	if s != nil && s.Collector != nil {
		var err error
		collected, err = s.Collector.Collect(flavor)
		if err != nil {
			return MiningReport{}, err
		}
	}

	p := MinePatterns(collected.Sessions)
	hasEvidence := len(p.ToolSequences) > 0 || len(p.FailureFixes) > 0 || len(p.Recommendations) > 0

	if !hasEvidence {
		return degradedMiningReport(
			flavor,
			collected,
			p,
			"Not enough recurring patterns yet — run more sessions (and AI analysis on them) to mine skills.",
		), nil
	}

	if s.Config == nil || !s.Config.IsConfigured() {
		return degradedMiningReport(
			flavor,
			collected,
			p,
			"Showing structural evidence only. Configure an AI provider to generate skills and AGENTS.md rules.",
		), nil
	}

	advisor := s.getAdvisor()
	if advisor == nil {
		return degradedMiningReport(
			flavor,
			collected,
			p,
			"Showing structural evidence only. Configure an AI provider to generate skills and AGENTS.md rules.",
		), nil
	}

	proposal, err := advisor.Propose(ctx, BuildEvidenceDigest(p))
	if err != nil {
		log.Printf("WARN  MinerService - Miner advisor failed; returning evidence only: %s", config.EscapeLog(err.Error()))
		return degradedMiningReport(
			flavor,
			collected,
			p,
			"AI proposal was unavailable (the model did not respond). Showing structural evidence only.",
		), nil
	}

	skills := proposal.Skills
	if skills == nil {
		skills = []ai.SkillProposal{}
	}
	rules := proposal.AgentsRules
	if rules == nil {
		rules = []ai.AgentsRule{}
	}
	gaps := proposal.ToolingGaps
	if gaps == nil {
		gaps = []string{}
	}

	return MiningReport{
		Flavor:           flavor,
		SessionCount:     collected.TotalSessionCount,
		SampledSessions:  len(collected.Sessions),
		AnalyzedSessions: p.AnalyzedSessions,
		AiGenerated:      true,
		Note:             "",
		ToolSequences:    p.ToolSequences,
		FailureFixes:     p.FailureFixes,
		Recommendations:  p.Recommendations,
		Skills:           skills,
		AgentsRules:      rules,
		ToolingGaps:      gaps,
	}, nil
}

func degradedMiningReport(flavor string, collected Collected, p Patterns, note string) MiningReport {
	return MiningReport{
		Flavor:           flavor,
		SessionCount:     collected.TotalSessionCount,
		SampledSessions:  len(collected.Sessions),
		AnalyzedSessions: p.AnalyzedSessions,
		AiGenerated:      false,
		Note:             note,
		ToolSequences:    p.ToolSequences,
		FailureFixes:     p.FailureFixes,
		Recommendations:  p.Recommendations,
		Skills:           []ai.SkillProposal{},
		AgentsRules:      []ai.AgentsRule{},
		ToolingGaps:      []string{},
	}
}

func minerEvidenceItem(text string) string {
	return ai.BoundedLine(text, maxMinerValueChars)
}

// BuildEvidenceDigest renders mined evidence into a compact prompt fenced as untrusted data.
func BuildEvidenceDigest(p Patterns) string {
	var sb strings.Builder

	sb.WriteString("RECURRING TOOL SEQUENCES (consecutive tool calls, with the number of sessions):\n")
	if len(p.ToolSequences) == 0 {
		sb.WriteString("- (none)\n")
	} else {
		for _, s := range p.ToolSequences {
			sb.WriteString(fmt.Sprintf("- %s (%d sessions)\n", minerEvidenceItem(s.Name), s.Count))
		}
	}

	sb.WriteString("\nRECURRING FAILURE → FIX PAIRS:\n")
	if len(p.FailureFixes) == 0 {
		sb.WriteString("- (none)\n")
	} else {
		for _, f := range p.FailureFixes {
			fix := minerEvidenceItem(f.Fix)
			if strings.TrimSpace(fix) == "" {
				fix = "(not recorded)"
			}
			sb.WriteString(fmt.Sprintf("- Error: %s | Fix: %s (%d sessions)\n", minerEvidenceItem(f.Error), fix, f.Count))
		}
	}

	sb.WriteString("\nRECOMMENDATIONS FROM PRIOR ANALYSES:\n")
	if len(p.Recommendations) == 0 {
		sb.WriteString("- (none)\n")
	} else {
		for _, r := range p.Recommendations {
			sb.WriteString(fmt.Sprintf("- %s (%d sessions)\n", minerEvidenceItem(r.Name), r.Count))
		}
	}

	return ai.FenceEvidence(sb.String())
}
