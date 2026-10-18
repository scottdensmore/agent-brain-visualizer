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
	"unicode/utf16"

	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
)

// EstimateTokens estimates the token count of text using the 4 UTF-16 code units per token
// heuristic, returning at least 1.
func EstimateTokens(text string) int {
	n := len(utf16.Encode([]rune(text))) / 4
	if n < 1 {
		return 1
	}
	return n
}

// TokenCounter estimates token counts for prompt budgeting and chunk splitting.
type TokenCounter struct {
	Config     *config.AiConfig
	EstimateFn func(string) int
}

// NewTokenCounter creates a TokenCounter backed by the 4-chars-per-token heuristic.
func NewTokenCounter(cfg *config.AiConfig) *TokenCounter {
	return &TokenCounter{Config: cfg}
}

// Estimate returns the estimated token count for text.
func (tc *TokenCounter) Estimate(text string) int {
	if tc != nil && tc.EstimateFn != nil {
		return tc.EstimateFn(text)
	}
	return EstimateTokens(text)
}
