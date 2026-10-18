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

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/testhttp"
)

// newFastRetryClient returns a client whose retry backoff is near-instant, so
// retry tests exercise the real retry path without real waits.
func newFastRetryClient(baseURL string) *Client {
	c := New(baseURL, "")
	c.backoff = time.Millisecond
	return c
}

func TestManifestParsesTheIdToHashMap(t *testing.T) {
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/ingest/manifest" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("source"); got != "codex" {
			t.Errorf("source = %q, want codex", got)
		}
		_, _ = io.WriteString(w, `{"a":"h1","b":"h2"}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").Manifest(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "h1" || got["b"] != "h2" || len(got) != 2 {
		t.Fatalf("manifest = %v", got)
	}
}

func TestManifestLargerThanOneMiBRoundTripsUntruncated(t *testing.T) {
	// A corpus of ~9k+ sessions serializes well past 1 MiB. The client must read
	// the whole success body — a capped read would truncate the manifest and
	// silently break the sync for large corpora.
	want := make(map[string]string, 16000)
	for i := 0; i < 16000; i++ {
		want[fmt.Sprintf("session-%05d", i)] = hashLike(i)
	}
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").Manifest(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("manifest has %d entries, want %d — the success body must not be truncated", len(got), len(want))
	}
	if got["session-15999"] != want["session-15999"] {
		t.Errorf("late entries must survive intact, got %q", got["session-15999"])
	}
}

// hashLike builds a distinct 64-hex-char string, the shape of a real
// content hash, so the fake manifest above has realistic entry sizes.
func hashLike(i int) string {
	return fmt.Sprintf("%064x", i)
}

func TestTransientServerErrorsAreRetried(t *testing.T) {
	var calls atomic.Int32
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"try again"}`)
			return
		}
		_, _ = io.WriteString(w, `{"a":"h1"}`)
	}))
	defer srv.Close()

	got, err := newFastRetryClient(srv.URL).Manifest(context.Background(), "codex")
	if err != nil {
		t.Fatalf("two 500s then a 200 must succeed via retry, got %v", err)
	}
	if got["a"] != "h1" || len(got) != 1 {
		t.Fatalf("manifest = %v", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("server saw %d request(s), want 3 (two failures, then the success)", n)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"malformed batch"}`)
	}))
	defer srv.Close()

	if _, err := newFastRetryClient(srv.URL).Manifest(context.Background(), "codex"); err == nil {
		t.Fatal("a 400 must surface as an error")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("server saw %d request(s), want 1 — a 4xx is the caller's fault, not transient", n)
	}
}

func TestRetriesGiveUpAfterTheAttemptBudget(t *testing.T) {
	var calls atomic.Int32
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := newFastRetryClient(srv.URL).Manifest(context.Background(), "codex"); err == nil {
		t.Fatal("a server that never recovers must still fail")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("server saw %d request(s), want exactly the attempt budget of 3", n)
	}
}

func TestPushRetryResendsTheFullBody(t *testing.T) {
	var calls atomic.Int32
	var retriedBody []PushSession
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []PushSession
		_ = json.NewDecoder(r.Body).Decode(&batch)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		retriedBody = batch
		_, _ = io.WriteString(w, `{"ingested":1,"skipped":0,"failed":0}`)
	}))
	defer srv.Close()

	res, err := newFastRetryClient(srv.URL).Push(context.Background(),
		[]PushSession{{Source: "codex", ID: "a", SourceMtime: 42, Raw: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ingested != 1 {
		t.Fatalf("result = %+v", res)
	}
	// The first attempt consumed the request body; the retry must rewind and
	// resend it in full, not POST an empty body.
	if len(retriedBody) != 1 || retriedBody[0].ID != "a" || retriedBody[0].Raw != "x" {
		t.Fatalf("retried request carried %+v, want the original batch", retriedBody)
	}
}

func TestPushSendsTheBatchAndReturnsCounts(t *testing.T) {
	var body []PushSession
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/ingest/sessions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"ingested":1,"skipped":0,"failed":0}`)
	}))
	defer srv.Close()

	in := []PushSession{{Source: "codex", ID: "a", SourceMtime: 42, Raw: "x"}}
	res, err := New(srv.URL, "").Push(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ingested != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(body) != 1 || body[0].ID != "a" || body[0].SourceMtime != 42 || body[0].Raw != "x" {
		t.Fatalf("server received %+v", body)
	}
}

func TestPushSerializesTheServersFieldNames(t *testing.T) {
	var raw map[string]any
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var arr []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&arr)
		if len(arr) > 0 {
			raw = arr[0]
		}
		_, _ = io.WriteString(w, `{"ingested":0,"skipped":0,"failed":0}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").Push(context.Background(),
		[]PushSession{{Source: "codex", ID: "a", Title: "t", SourceMtime: 42, Raw: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	// The field names must match the server's IngestSession record exactly, or it
	// deserializes to nulls and every push fails.
	for _, k := range []string{"source", "id", "title", "sourceMtime", "raw"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("pushed JSON is missing field %q; got keys %v", k, keysOf(raw))
		}
	}
}

func TestPushSummaryFieldIsSentWhenSetAndOmittedWhenEmpty(t *testing.T) {
	var payloads []map[string]any
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var arr []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&arr)
		payloads = arr
		_, _ = io.WriteString(w, `{"ingested":0,"skipped":0,"failed":0}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").Push(context.Background(), []PushSession{
		{Source: "antigravity-cli", ID: "a", Raw: "x", Summary: `{"summary":"s"}`},
		{Source: "codex", ID: "b", Raw: "y"}, // no summary
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := payloads[0]["summary"]; got != `{"summary":"s"}` {
		t.Errorf("summary should be sent as the raw JSON string, got %v", got)
	}
	if _, present := payloads[1]["summary"]; present {
		t.Errorf("an empty summary must be omitted (omitempty), got keys %v", keysOf(payloads[1]))
	}
}

func TestSummaryManifestParsesTheIdToHashMap(t *testing.T) {
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ingest/summaries/manifest" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("source"); got != "antigravity-cli" {
			t.Errorf("source = %q", got)
		}
		_, _ = io.WriteString(w, `{"s1":"h1"}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").SummaryManifest(context.Background(), "antigravity-cli")
	if err != nil {
		t.Fatal(err)
	}
	if got["s1"] != "h1" || len(got) != 1 {
		t.Fatalf("summary manifest = %v", got)
	}
}

func TestPushSummariesSendsTheBatchWithTheServersFieldNames(t *testing.T) {
	var raw map[string]any
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/ingest/summaries" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var arr []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&arr)
		if len(arr) > 0 {
			raw = arr[0]
		}
		_, _ = io.WriteString(w, `{"ingested":1,"skipped":0,"failed":0}`)
	}))
	defer srv.Close()

	res, err := New(srv.URL, "").PushSummaries(context.Background(),
		[]PushSummary{{Source: "antigravity-cli", ID: "s1", Summary: `{"summary":"s"}`}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ingested != 1 {
		t.Fatalf("result = %+v", res)
	}
	// Field names must match the server's IngestSummary record exactly.
	for _, k := range []string{"source", "id", "summary"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("pushed JSON missing field %q; got keys %v", k, keysOf(raw))
		}
	}
}

func TestAuthorizationHeaderSentOnlyWhenTokenSet(t *testing.T) {
	var auth string
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	_, _ = New(srv.URL, "").Manifest(context.Background(), "codex")
	if auth != "" {
		t.Errorf("no token configured, but sent Authorization %q", auth)
	}
	_, _ = New(srv.URL, "s3cret").Manifest(context.Background(), "codex")
	if auth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", auth)
	}
}

func TestNon2xxIsAnErrorNotASilentEmptyResult(t *testing.T) {
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"database unreachable"}`)
	}))
	defer srv.Close()

	if _, err := newFastRetryClient(srv.URL).Manifest(context.Background(), "codex"); err == nil {
		t.Fatal("expected a 503 to surface as an error")
	}
	if _, err := newFastRetryClient(srv.URL).Push(context.Background(), []PushSession{{Source: "codex", ID: "a"}}); err == nil {
		t.Fatal("expected a 503 to surface as an error")
	}
}

func TestUnauthorizedIsReportedClearly(t *testing.T) {
	srv := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Ingest requires a valid Authorization: Bearer <INGEST_TOKEN>."}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").Push(context.Background(), []PushSession{{Source: "codex", ID: "a"}})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected a 401 error mentioning the status, got %v", err)
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
