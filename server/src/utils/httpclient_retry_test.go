package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestDoLLMHTTPWithRetryReplaysBodyOnRetry is the regression test for the
// "workflows stall then die" report: the serialized retry path (#93) re-sent
// the SAME *http.Request on every retry, but its body had already been
// consumed by the first attempt — the transport then failed with
//   http: ContentLength=426270 with Body length 0
// (observed verbatim in the production server log), every retry failed the
// same way, the whole LLMRetryBudget burned down in a ~40 minute backoff
// storm, and the workflow died with "[LLMLoop] Error calling LLM".
//
// The request body here is built EXACTLY like SendChatCompletion does it:
// json.Marshal -> bytes.NewBuffer. That shape is what http.NewRequest turns
// into ContentLength=N with GetBody=nil, which is the shape that broke.
func TestDoLLMHTTPWithRetryReplaysBodyOnRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			t.Error("retry attempt arrived with nil body")
			http.Error(w, "nil body", http.StatusInternalServerError)
			return
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("attempt %d: read body: %v", calls.Load()+1, err)
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		if len(got) == 0 {
			t.Errorf("attempt %d: EMPTY body (the 'Body length 0' failure)", calls.Load()+1)
			http.Error(w, "empty body", http.StatusInternalServerError)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(got, &payload); err != nil {
			t.Errorf("attempt %d: body not valid JSON: %v", calls.Load()+1, err)
			http.Error(w, "bad json", http.StatusInternalServerError)
			return
		}
		if payload["key"] != "value" {
			t.Errorf("attempt %d: unexpected payload %v", calls.Load()+1, payload)
		}
		n := calls.Add(1)
		if n == 1 {
			// First attempt: transient provider rejection with a short
			// Retry-After so the test does not actually wait 2 minutes.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		// Second attempt: success — only possible if the body was replayed.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	jsonData, err := json.Marshal(map[string]string{"key": "value"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Bounded by a derived context so a regression cannot hang the suite for
	// the full Retry-After backoff.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := DoLLMHTTPWithRetry(ctx, req)
	if err != nil {
		t.Fatalf("DoLLMHTTPWithRetry failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", calls.Load())
	}
}
