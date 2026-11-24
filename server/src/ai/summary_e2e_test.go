package ai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// End-to-end proof of the deployed failure and of the fix, at second scale.
//
// The server answers a Stream:false completion only AFTER a delay — exactly
// how a one-shot chat completion behaves: response headers arrive when the
// whole answer is generated, not when the request is accepted.
//
// With the header budget BELOW the generation time, the client fails with the
// exact string from the deployed log:
//
//	net/http: timeout awaiting response headers
//
// With the header budget raised to the round-trip budget (the fix), the same
// slow request SUCCEEDS. Same server, same request, same client timeout — only
// the transport's header budget differs, which is the single line this PR
// changes.
func TestSummaryRoundTripSurvivesASlowOneShotCompletion(t *testing.T) {
	const generation = 1200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(generation) // the whole completion, then headers
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"the checkpoint"}}]}`))
	}))
	defer srv.Close()

	post := func(headerBudget time.Duration) (*http.Response, error) {
		client := &http.Client{
			Timeout:   5 * time.Second, // the summariser's Client.Timeout, unchanged
			Transport: summaryTransportWithHeaderTimeout(headerBudget),
		}
		req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":false}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}

	// (1) The deployed shape: a header budget far below generation time.
	resp, err := post(90 * time.Millisecond)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("the 90ms header budget SUCCEEDED against a 1.2s completion — the test no longer reproduces the defect")
	}
	if !strings.Contains(err.Error(), "net/http: timeout awaiting response headers") {
		t.Fatalf("want the deployed error string, got: %v", err)
	}

	// (2) The fix: the header budget equals the round-trip budget.
	resp, err = post(5 * time.Second)
	if err != nil {
		t.Fatalf("with the raised header budget the summary must complete, got: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}
