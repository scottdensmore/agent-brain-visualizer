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
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
	"github.com/scottdensmore/agent-brain-visualizer/internal/transcript"
)

const (
	// MaxSessions is the maximum number of sessions collected per source for fleet analysis.
	MaxSessions = 150
	// MaxDrilldown caps the drilled-in session list so a very common item stays responsive to render.
	MaxDrilldown = 50
	fleetTopN    = 10
)

// NameCount is an alias for store.NameCount ({Name, Count}).
type NameCount = store.NameCount

// SessionRef is an alias for transcript.SessionRef ({ID, Title}).
type SessionRef = transcript.SessionRef

// Session represents one session's normalized timeline steps and optional cached analysis JSON.
type Session struct {
	ID            string
	Steps         []map[string]any
	CachedSummary map[string]any
}

// Collected holds the gathered sessions for a flavor and the true total count in the store.
type Collected struct {
	TotalSessionCount int
	Sessions          []Session
}

// SessionCollector gathers a source's sessions from SessionRepository.
type SessionCollector struct {
	SessionRepo *store.SessionRepository
}

// NewSessionCollector creates a SessionCollector backed by sessionRepo.
func NewSessionCollector(sessionRepo *store.SessionRepository) *SessionCollector {
	return &SessionCollector{SessionRepo: sessionRepo}
}

// Collect returns the true total session count and up to MaxSessions parsed Sessions for flavor.
func (c *SessionCollector) Collect(flavor string) (Collected, error) {
	if c == nil || c.SessionRepo == nil {
		return Collected{TotalSessionCount: 0, Sessions: []Session{}}, nil
	}
	total, err := c.SessionRepo.CountBySource(flavor)
	if err != nil {
		return Collected{}, err
	}
	rows, err := c.SessionRepo.Collect(flavor, MaxSessions)
	if err != nil {
		return Collected{}, err
	}
	gathered := make([]Session, 0, len(rows))
	for _, row := range rows {
		steps := parseStepsArray(row.StepsJSON)
		var summary map[string]any
		if row.HasSummary && strings.TrimSpace(row.SummaryJSON) != "" {
			summary = parseJSONMap(row.SummaryJSON)
		}
		gathered = append(gathered, Session{
			ID:            row.ID,
			Steps:         steps,
			CachedSummary: summary,
		})
	}
	return Collected{
		TotalSessionCount: total,
		Sessions:          gathered,
	}, nil
}

func parseStepsArray(jsonArray string) []map[string]any {
	var arr []map[string]any
	if err := json.Unmarshal([]byte(jsonArray), &arr); err != nil || arr == nil {
		return []map[string]any{}
	}
	return arr
}

func parseJSONMap(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// InsightsReport is the cross-session fleet analytics payload returned by GET /api/insights.
type InsightsReport struct {
	Flavor             string      `json:"flavor"`
	SessionCount       int         `json:"sessionCount"`
	SampledSessions    int         `json:"sampledSessions"`
	AnalyzedSessions   int         `json:"analyzedSessions"`
	ToolCallTotal      int         `json:"toolCallTotal"`
	SessionsWithErrors int         `json:"sessionsWithErrors"`
	CleanSessions      int         `json:"cleanSessions"`
	AvgToolsPerSession float64     `json:"avgToolsPerSession"`
	AvgDurationSeconds float64     `json:"avgDurationSeconds"`
	TopTools           []NameCount `json:"topTools"`
	TopErrors          []NameCount `json:"topErrors"`
	TopRecommendations []NameCount `json:"topRecommendations"`
	TopIssues          []NameCount `json:"topIssues"`
}

// MarshalJSON ensures slices on InsightsReport serialize as [] rather than null.
func (r InsightsReport) MarshalJSON() ([]byte, error) {
	type wire struct {
		Flavor             string      `json:"flavor"`
		SessionCount       int         `json:"sessionCount"`
		SampledSessions    int         `json:"sampledSessions"`
		AnalyzedSessions   int         `json:"analyzedSessions"`
		ToolCallTotal      int         `json:"toolCallTotal"`
		SessionsWithErrors int         `json:"sessionsWithErrors"`
		CleanSessions      int         `json:"cleanSessions"`
		AvgToolsPerSession float64     `json:"avgToolsPerSession"`
		AvgDurationSeconds float64     `json:"avgDurationSeconds"`
		TopTools           []NameCount `json:"topTools"`
		TopErrors          []NameCount `json:"topErrors"`
		TopRecommendations []NameCount `json:"topRecommendations"`
		TopIssues          []NameCount `json:"topIssues"`
	}
	return json.Marshal(wire{
		Flavor:             r.Flavor,
		SessionCount:       r.SessionCount,
		SampledSessions:    r.SampledSessions,
		AnalyzedSessions:   r.AnalyzedSessions,
		ToolCallTotal:      r.ToolCallTotal,
		SessionsWithErrors: r.SessionsWithErrors,
		CleanSessions:      r.CleanSessions,
		AvgToolsPerSession: r.AvgToolsPerSession,
		AvgDurationSeconds: r.AvgDurationSeconds,
		TopTools:           nonNilNameCounts(r.TopTools),
		TopErrors:          nonNilNameCounts(r.TopErrors),
		TopRecommendations: nonNilNameCounts(r.TopRecommendations),
		TopIssues:          nonNilNameCounts(r.TopIssues),
	})
}

func nonNilNameCounts(s []NameCount) []NameCount {
	if s == nil {
		return []NameCount{}
	}
	return s
}

// DrilldownResult lists the sessions behind one Insights or Mine tally item.
type DrilldownResult struct {
	Category     string       `json:"category"`
	Key          string       `json:"key"`
	TotalMatches int          `json:"totalMatches"`
	Sessions     []SessionRef `json:"sessions"`
}

// MarshalJSON ensures Sessions serializes as [] rather than null.
func (d DrilldownResult) MarshalJSON() ([]byte, error) {
	type wire struct {
		Category     string       `json:"category"`
		Key          string       `json:"key"`
		TotalMatches int          `json:"totalMatches"`
		Sessions     []SessionRef `json:"sessions"`
	}
	sessions := d.Sessions
	if sessions == nil {
		sessions = []SessionRef{}
	}
	return json.Marshal(wire{
		Category:     d.Category,
		Key:          d.Key,
		TotalMatches: d.TotalMatches,
		Sessions:     sessions,
	})
}

// orderedCounter tracks string counts while preserving first-seen insertion order for tie-breaking.
type orderedCounter struct {
	counts map[string]int
	order  []string
}

func newOrderedCounter() *orderedCounter {
	return &orderedCounter{
		counts: make(map[string]int),
	}
}

func (c *orderedCounter) inc(key string) {
	if _, exists := c.counts[key]; !exists {
		c.order = append(c.order, key)
	}
	c.counts[key]++
}

func (c *orderedCounter) topN(limit int) []NameCount {
	if len(c.order) == 0 {
		return []NameCount{}
	}
	type indexed struct {
		name  string
		count int
		idx   int
	}
	items := make([]indexed, len(c.order))
	for i, k := range c.order {
		items[i] = indexed{name: k, count: c.counts[k], idx: i}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].idx < items[j].idx
	})
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]NameCount, len(items))
	for i, it := range items {
		out[i] = NameCount{Name: it.name, Count: it.count}
	}
	return out
}

// InsightsService computes cross-session fleet analytics and drill-down session lists.
type InsightsService struct {
	Collector *SessionCollector
}

// NewInsightsService constructs an InsightsService backed by collector.
func NewInsightsService(collector *SessionCollector) *InsightsService {
	return &InsightsService{Collector: collector}
}

// ForFlavor aggregates fleet-wide metrics for flavor.
func (s *InsightsService) ForFlavor(flavor string) (InsightsReport, error) {
	if s == nil || s.Collector == nil {
		return AggregateFleet(flavor, 0, nil), nil
	}
	collected, err := s.Collector.Collect(flavor)
	if err != nil {
		return InsightsReport{}, err
	}
	return AggregateFleet(flavor, collected.TotalSessionCount, collected.Sessions), nil
}

// Drilldown returns the sessions behind one tally item (tool, error, recommendation, issue, workflow).
func (s *InsightsService) Drilldown(flavor, category, key string) (DrilldownResult, error) {
	if s == nil || s.Collector == nil {
		return DrilldownResult{Category: category, Key: key, TotalMatches: 0, Sessions: []SessionRef{}}, nil
	}
	collected, err := s.Collector.Collect(flavor)
	if err != nil {
		return DrilldownResult{}, err
	}
	all := SessionsFor(category, key, collected.Sessions)
	capped := all
	if len(capped) > MaxDrilldown {
		capped = capped[:MaxDrilldown]
	}
	return DrilldownResult{
		Category:     category,
		Key:          key,
		TotalMatches: len(all),
		Sessions:     capped,
	}, nil
}

// AggregateFleet computes an InsightsReport from sessions (ported from FleetInsights.aggregate).
func AggregateFleet(flavor string, totalSessionCount int, sessions []Session) InsightsReport {
	toolFreq := newOrderedCounter()
	errorFreq := newOrderedCounter()
	recFreq := newOrderedCounter()
	issueFreq := newOrderedCounter()

	toolCallTotal := 0
	sessionsWithErrors := 0
	analyzedSessions := 0
	var totalToolCalls int64
	durations := make([]float64, 0)

	for _, session := range sessions {
		sessionTools := 0
		sessionHasError := false
		minTs := int64(math.MaxInt64)
		maxTs := int64(math.MinInt64)

		for _, step := range session.Steps {
			if ts, ok := epochMillis(stepString(step, "created_at")); ok {
				if ts < minTs {
					minTs = ts
				}
				if ts > maxTs {
					maxTs = ts
				}
			}

			for _, tool := range stepToolCalls(step) {
				name := stepString(tool, "name")
				if name == "" {
					name = "unknown"
				}
				toolFreq.inc(name)
				sessionTools++
				toolCallTotal++
			}

			if IsStepError(step) {
				sessionHasError = true
				errorFreq.inc(stepErrorKey(step))
			}
		}

		totalToolCalls += int64(sessionTools)
		if sessionHasError {
			sessionsWithErrors++
		}
		if maxTs > minTs {
			durations = append(durations, float64(maxTs-minTs)/1000.0)
		}

		if session.CachedSummary != nil {
			analyzedSessions++
			for _, rec := range summaryRecommendations(session.CachedSummary) {
				text := cleanSummaryItem(rec)
				if text != "" {
					recFreq.inc(text)
				}
			}
			for _, iss := range summaryIssues(session.CachedSummary) {
				text := cleanSummaryItem(stepString(iss, "error"))
				if text != "" {
					issueFreq.inc(text)
				}
			}
		}
	}

	sampled := len(sessions)
	avgTools := 0.0
	if sampled > 0 {
		avgTools = float64(totalToolCalls) / float64(sampled)
	}
	avgDuration := 0.0
	if len(durations) > 0 {
		sum := 0.0
		for _, d := range durations {
			sum += d
		}
		avgDuration = sum / float64(len(durations))
	}

	return InsightsReport{
		Flavor:             flavor,
		SessionCount:       totalSessionCount,
		SampledSessions:    sampled,
		AnalyzedSessions:   analyzedSessions,
		ToolCallTotal:      toolCallTotal,
		SessionsWithErrors: sessionsWithErrors,
		CleanSessions:      sampled - sessionsWithErrors,
		AvgToolsPerSession: Round1(avgTools),
		AvgDurationSeconds: Round1(avgDuration),
		TopTools:           toolFreq.topN(fleetTopN),
		TopErrors:          errorFreq.topN(fleetTopN),
		TopRecommendations: recFreq.topN(fleetTopN),
		TopIssues:          issueFreq.topN(fleetTopN),
	}
}

// SessionsFor returns the sessions that contributed a given tally item (tool, error, recommendation, issue, workflow).
func SessionsFor(category, key string, sessions []Session) []SessionRef {
	refs := make([]SessionRef, 0)
	if category == "" || key == "" {
		return refs
	}
	for _, session := range sessions {
		if matchesSessionCategory(category, key, session) {
			refs = append(refs, SessionRef{
				ID:    sessionIDOf(session),
				Title: sessionTitleOf(session),
			})
		}
	}
	return refs
}

func matchesSessionCategory(category, key string, session Session) bool {
	switch category {
	case "tool":
		for _, step := range session.Steps {
			for _, tool := range stepToolCalls(step) {
				name := stepString(tool, "name")
				if name == "" {
					name = "unknown"
				}
				if name == key {
					return true
				}
			}
		}
		return false
	case "error":
		for _, step := range session.Steps {
			if IsStepError(step) && stepErrorKey(step) == key {
				return true
			}
		}
		return false
	case "recommendation":
		if session.CachedSummary == nil {
			return false
		}
		for _, rec := range summaryRecommendations(session.CachedSummary) {
			if cleanSummaryItem(rec) == key {
				return true
			}
		}
		return false
	case "issue":
		if session.CachedSummary == nil {
			return false
		}
		for _, iss := range summaryIssues(session.CachedSummary) {
			if cleanSummaryItem(stepString(iss, "error")) == key {
				return true
			}
		}
		return false
	case "workflow":
		for _, seq := range SequencesInSteps(session.Steps) {
			if seq == key {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func sessionIDOf(s Session) string {
	if s.ID == "" {
		return "unknown"
	}
	return s.ID
}

func sessionTitleOf(s Session) string {
	if s.CachedSummary != nil {
		title := strings.TrimSpace(stepString(s.CachedSummary, "shortTitle"))
		if title != "" {
			return truncateStr(title, 80)
		}
	}
	for _, step := range s.Steps {
		if stepString(step, "type") == "USER_INPUT" {
			content := strings.TrimSpace(stepString(step, "content"))
			if content != "" {
				return truncateStr(content, 80)
			}
		}
	}
	return sessionIDOf(s)
}

// IsStepError reports whether a normalized step represents an error (shared between FleetInsights and EvalScorer).
func IsStepError(step map[string]any) bool {
	status := stepString(step, "status")
	stepType := stepString(step, "type")
	content := stepString(step, "content")
	return status == "ERROR" ||
		stepType == "ERROR_MESSAGE" ||
		(stepType == "RUN_COMMAND" && strings.Contains(content, "The command failed"))
}

func stepErrorKey(step map[string]any) string {
	if stepString(step, "type") == "RUN_COMMAND" {
		return "Command execution failed"
	}
	var source string
	if _, hasErr := step["error"]; hasErr {
		source = stepString(step, "error")
	} else {
		source = stepString(step, "content")
	}
	return NormalizeError(source)
}

func epochMillis(iso string) (int64, bool) {
	trimmed := strings.TrimSpace(iso)
	if trimmed == "" {
		return 0, false
	}
	if t, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

func cleanSummaryItem(text string) string {
	return truncateStr(strings.TrimSpace(text), 200)
}

func truncateStr(text string, maxChars int) string {
	if len(text) > maxChars {
		return text[:maxChars]
	}
	return text
}

func stepToolCalls(step map[string]any) []map[string]any {
	if step == nil {
		return nil
	}
	switch raw := step["tool_calls"].(type) {
	case []map[string]any:
		return raw
	case []any:
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok && m != nil {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func summaryRecommendations(summary map[string]any) []string {
	if summary == nil {
		return nil
	}
	switch raw := summary["recommendations"].(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func summaryIssues(summary map[string]any) []map[string]any {
	if summary == nil {
		return nil
	}
	switch raw := summary["issues"].(type) {
	case []map[string]any:
		return raw
	case []any:
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok && m != nil {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func summaryFlow(summary map[string]any) ([]string, bool) {
	if summary == nil {
		return nil, false
	}
	switch raw := summary["flow"].(type) {
	case []string:
		return raw, true
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func stepString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
