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

// Package client talks to the visualizer's /api/ingest endpoints: it reads a
// source's manifest and pushes batches of trajectories.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP client for the ingest API. An empty token leaves requests
// unauthenticated, which the server accepts when INGEST_TOKEN is unset.
type Client struct {
	baseURL  string
	token    string
	http     *http.Client
	attempts int           // total tries per request, including the first
	backoff  time.Duration // wait before the second try; doubles each retry
}

// New returns a Client for baseURL, sending a bearer token when one is given.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// A finite timeout so a wedged server fails the sync instead of hanging it.
		http:     &http.Client{Timeout: 60 * time.Second},
		attempts: 3,
		backoff:  250 * time.Millisecond,
	}
}

// PushSession mirrors the server's IngestSession record; the JSON field names
// must match it exactly or the server deserializes nulls and rejects the batch.
// PushFile is one file the transcript referenced, carried with the session so the
// viewer can preview it without the file being on the machine serving the UI.
type PushFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type PushSession struct {
	Source      string     `json:"source"`
	ID          string     `json:"id"`
	Title       string     `json:"title,omitempty"`
	SourceMtime int64      `json:"sourceMtime"`
	Raw         string     `json:"raw"`
	Summary     string     `json:"summary,omitempty"`
	Files       []PushFile `json:"files,omitempty"`
}

// PushSummary mirrors the server's IngestSummary record: a cached analysis pushed
// on its own for a session whose transcript is already stored.
type PushSummary struct {
	Source  string `json:"source"`
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// Result mirrors the server's IngestResult.
type Result struct {
	Ingested int `json:"ingested"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}

// Manifest returns the server's id -> contentHash map for a source, so the
// caller can push only what changed.
func (c *Client) Manifest(ctx context.Context, source string) (map[string]string, error) {
	u := c.baseURL + "/api/ingest/manifest?source=" + url.QueryEscape(source)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var manifest map[string]string
	if err := c.do(req, &manifest); err != nil {
		return nil, fmt.Errorf("fetching manifest for %s: %w", source, err)
	}
	return manifest, nil
}

// Push uploads a batch of trajectories and returns the server's tally.
func (c *Client) Push(ctx context.Context, sessions []PushSession) (Result, error) {
	payload, err := json.Marshal(sessions)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/ingest/sessions", bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	var result Result
	if err := c.do(req, &result); err != nil {
		return Result{}, fmt.Errorf("pushing %d session(s): %w", len(sessions), err)
	}
	return result, nil
}

// SummaryManifest returns the server's id -> summaryContentHash map for a source,
// so the caller can push only the summaries the store is missing or that changed.
func (c *Client) SummaryManifest(ctx context.Context, source string) (map[string]string, error) {
	u := c.baseURL + "/api/ingest/summaries/manifest?source=" + url.QueryEscape(source)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var manifest map[string]string
	if err := c.do(req, &manifest); err != nil {
		return nil, fmt.Errorf("fetching summary manifest for %s: %w", source, err)
	}
	return manifest, nil
}

// PushSummaries uploads a batch of standalone summaries and returns the server's tally.
func (c *Client) PushSummaries(ctx context.Context, summaries []PushSummary) (Result, error) {
	payload, err := json.Marshal(summaries)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/ingest/summaries", bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	var result Result
	if err := c.do(req, &result); err != nil {
		return Result{}, fmt.Errorf("pushing %d summary(ies): %w", len(summaries), err)
	}
	return result, nil
}

// do sends the request, adds auth, treats any non-2xx as an error (never a
// silent empty result), and decodes a successful body into out. Transient
// failures — a connection error, a 429, or a 5xx — are retried a few times with
// exponential backoff; every ingest endpoint is idempotent, so a retry can
// never double-ingest. The backoff wait ends early when the context is
// cancelled (e.g. Ctrl-C).
func (c *Client) do(req *http.Request, out any) error {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	backoff := c.backoff
	for attempt := 1; ; attempt++ {
		if attempt > 1 && req.GetBody != nil {
			// The previous attempt consumed the body; rewind it for the retry.
			body, err := req.GetBody()
			if err != nil {
				return fmt.Errorf("rewinding %s request body for retry: %w", req.URL.Path, err)
			}
			req.Body = body
		}
		retryable, err := c.doOnce(req, out)
		if err == nil || !retryable || attempt >= c.attempts {
			return err
		}
		select {
		case <-req.Context().Done():
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// doOnce performs a single attempt, reporting whether a failure is transient
// (worth retrying) alongside the error.
func (c *Client) doOnce(req *http.Request, out any) (retryable bool, _ error) {
	resp, err := c.http.Do(req)
	if err != nil {
		// Name the target and hint at the cause; the bare net/http error ("dial
		// tcp …: connection refused") doesn't say what to do about it. A cancelled
		// context is deliberate (Ctrl-C), not transient — never retry it.
		return req.Context().Err() == nil, fmt.Errorf("cannot reach the visualizer at %s (is it running? check --server): %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The error body only feeds the message, so cap it — a misbehaving server
		// must not make the CLI buffer an unbounded response.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return retry, fmt.Errorf("%s returned %d: %s", req.URL.Path, resp.StatusCode, serverMessage(body, resp.Status))
	}
	// A success body is read in full, never capped: the manifest for a large
	// corpus easily exceeds any fixed limit, and truncating it would silently
	// break the sync.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, fmt.Errorf("reading %s response: %w", req.URL.Path, err)
	}
	if out == nil {
		return false, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("decoding %s response: %w", req.URL.Path, err)
	}
	return false, nil
}

// serverMessage prefers the API's {"error": "..."} text, falling back to the
// HTTP status line, so a failure reads as a cause rather than a bare code.
func serverMessage(body []byte, status string) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != "" {
		return envelope.Error
	}
	if len(body) > 0 {
		return strings.TrimSpace(string(body))
	}
	return status
}
