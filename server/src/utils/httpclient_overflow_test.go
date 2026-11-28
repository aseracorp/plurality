package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// httpclient_overflow_test.go — a provider rejection that says "the request is
// too big" is TERMINAL, not retryable, and on the serialized local-GPU path a
// retry of it is a freeze.
//
// The production shape, verbatim from the live log (2026-11-27, conversations
// 76c17afbb288581be0d23812 and fb3a4de4204d715e90863d9f):
//
//	21:51:46 [LLMLoop] Calling LLM with model strata-swift
//	21:51:46 [Eco] compression-aware guard: est 242493 tokens / factor 1.08 = 224193
//	21:51:49 litellm.acompletion(...) Exception litellm.BadRequestError:
//	         OpenAIException - request exceeds the context window: prompt (262705
//	         tokens) leaves no room to answer in the context (262144); requests
//	         are never truncated
//	21:51:49 [LLM] provider rejected attempt 1 with status 500; backing off 2m0s and retrying
//	21:52:24 [LLMLoop] Error processing stream : context canceled
//	21:53:21 [LLMLoop] Starting for conversation 9f0ab2352bf2bdc40e77384b
//
// Three things fall out of those six lines.
//
// One: the guard's estimate and the provider's measurement DISAGREE IN OPPOSITE
// DIRECTIONS for the same request. The guard, discounted by a compression factor
// of 1.08, said 224193 — fits, ship it. The backend, which compiles a 150-token
// tool schema into its own chat template on top of the shipped messages,
// measured 262705 — 561 tokens over. The compression discount buys the guard's
// silence exactly when the request needs folding.
//
// Two: LiteLLM re-publishes that 400 as **500 Internal Server Error** after its
// own model-fallback, and 500 is in RetryableStatus — so Plurality retried the
// IDENTICAL oversized payload 15 times over ~30 minutes.
//
// Three: the 21:52:24 "context canceled" is the OTHER conversation, 33 seconds
// into its own request. The crash dump at 21:45:54 shows why:
//
//	goroutine 263534 [select, 23 minutes]:
//	  utils.HoldLLMSlot(...)               httpclient.go:151
//	  utils.DoLLMHTTPWithRetryHeld(...)    httpclient.go:336
//	  ai.SendChatCompletion(...)           index.go:569
//	  ai.(*ActiveRequest).RunLLMLoop(...)  tool_loop.go:198
//
// A loop parked 23 MINUTES waiting for the capacity-1 slot. The retrying
// conversation held that slot across its whole backoff storm, so every other
// serialized request on the box queued behind a payload that can never succeed.
// The user saw "working" with nothing arriving, and the only way out was a
// manual cancel — which produced the "[HandleCancel] No active request" 404s.
//
// The fix under test: the rejection body decides. Retrying cannot shrink a
// prompt, so an overflow rejection returns immediately, carrying the provider's
// own words so the LLM loop's compaction recovery (isContextLengthError) fires.

// overflowBody is the provider's rejection, copied from the live log.
const overflowBody = `{"error":{"message":"request exceeds the context window: prompt (262705 tokens) leaves no room to answer in the context (262144); requests are never truncated. Received Model Group=strata-swift","type":"context_window_error","code":400}}`

func chatReq(t *testing.T, url string) *http.Request {
	t.Helper()
	// The exact request shape SendChatCompletion builds: json.Marshal into a
	// bytes.Buffer, so ContentLength is set and GetBody is nil.
	body, err := json.Marshal(map[string]any{"model": "strata-swift", "messages": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestOverflowRejectionIsNotRetriedOnHeldPath: a 500 whose body is the
// context-window message must return on the FIRST attempt, release the slot,
// and carry the provider's words for the loop to recognise.
func TestOverflowRejectionIsNotRetriedOnHeldPath(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, overflowBody, http.StatusInternalServerError)
	}))
	defer srv.Close()

	if !RetryableStatus(http.StatusInternalServerError) {
		t.Fatal("500 must stay retryable in general — this test is about the BODY, not the status")
	}

	req := chatReq(t, srv.URL)
	start := time.Now()
	_, err := DoLLMHTTPWithRetryHeld(context.Background(), req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an overflow rejection must be an error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the oversized payload was sent %d times: a context-window rejection must be terminal. "+
			"Every extra attempt is minutes of backoff holding the capacity-1 local GPU slot.", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the overflow path took %v — it must return at once, not back off", elapsed)
	}
	// The provider's words must survive: the LLM loop's compaction recovery
	// (ai.isContextLengthError) matches on them. An error that reads only
	// "rejected with status 500 after 15 retries" is the bug.
	if !isContextLengthErrorShaped(err.Error()) {
		t.Fatalf("the returned error must read as a context-length error so the loop compacts; got: %v", err)
	}
	// The slot must be free: a leaked slot freezes every later conversation.
	if n := len(LLMSem); n != 0 {
		t.Fatalf("the overflow path left %d held slot(s) behind: the capacity-1 local GPU queue is now wedged", n)
	}
}

// TestOverflowRejectionIsNotRetriedOnCloudPath: same rule on the non-serialized
// variant, so a 15-attempt storm cannot be built there either.
func TestOverflowRejectionIsNotRetriedOnCloudPath(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, overflowBody, http.StatusInternalServerError)
	}))
	defer srv.Close()

	req := chatReq(t, srv.URL)
	_, err := DoLLMHTTPWithRetry(context.Background(), req)
	if err == nil {
		t.Fatal("an overflow rejection must be an error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the oversized payload was sent %d times on the cloud path; want 1", got)
	}
	if !isContextLengthErrorShaped(err.Error()) {
		t.Fatalf("the returned error must read as a context-length error; got: %v", err)
	}
}

// TestGenuineTransient500StillRetries: the gate is the BODY. A 500 with no
// context-window text (a proxy restart, a 402 budget stall) must keep the
// bounded retry that #18/#94 exist for.
func TestGenuineTransient500StillRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// 1s Retry-After: the retry must happen, the test must not sit in
			// the 120s default backoff.
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	req := chatReq(t, srv.URL)
	resp, err := DoLLMHTTPWithRetry(context.Background(), req)
	if err != nil {
		t.Fatalf("a transient 500 must be retried, not surfaced: %v", err)
	}
	resp.Body.Close()
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 (one 500, then success)", got)
	}
}

// TestOverflowFreesTheSlotForOtherConversations is the freeze, reproduced with
// a capacity-1 channel: the retry storm holds the slot, so the second
// conversation blocks. With the fix the first one is terminal and the second
// proceeds.
func TestOverflowFreesTheSlotForOtherConversations(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, overflowBody, http.StatusInternalServerError)
	}))
	defer srv.Close()

	first := chatReq(t, srv.URL)
	if _, err := DoLLMHTTPWithRetryHeld(context.Background(), first); err == nil {
		t.Fatal("want the overflow error")
	}

	// The slot is free, so a second conversation's request completes.
	var ok atomic.Int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := DoLLMHTTPWithRetryHeld(ctx, chatReq(t, srv2.URL))
	if err != nil {
		t.Fatalf("the next conversation was blocked behind the overflow: %v", err)
	}
	resp.Body.Close()
	if ok.Load() != 1 {
		t.Fatal("the second request never reached the provider")
	}
	if calls.Load() != 1 {
		t.Fatalf("overflow attempts = %d, want 1", calls.Load())
	}
}

// TestOverflowDetectionNeedlesMatchTheProviderPhrases: the needles must cover
// the phrasings observed in this deployment and the ones ai.isContextLengthError
// already knows, so a rejection cannot slip past into the retry loop.
func TestOverflowDetectionNeedlesMatchTheProviderPhrases(t *testing.T) {
	observed := []string{
		`request exceeds the context window: prompt (262705 tokens) leaves no room to answer in the context (262144); requests are never truncated`,
		`{"error":{"message":"This model's maximum context length is 262144 tokens, prompt is 300000"}}`,
		`context_length_exceeded`,
		`This model's context length is 32768 tokens`,
		`max_prompt_tokens exceeded`,
	}
	for _, b := range observed {
		if !providerRejectionIsOverflow(b) {
			t.Fatalf("overflow not detected for: %.80s", b)
		}
	}
	notOverflow := []string{
		`Internal Server Error`,
		`upstream connect error`,
		`Would exceed your available credits given your current in-flight requests`,
		`rate limit reached, retry after 120`,
		`<!DOCTYPE html><html>502 Bad Gateway</html>`,
	}
	for _, b := range notOverflow {
		if providerRejectionIsOverflow(b) {
			t.Fatalf("false overflow: %.80s — a transient rejection must keep its bounded retry", b)
		}
	}
}

// TestOverflowDetectionIsRaceClean runs the detector from concurrent goroutines:
// it is called on every provider rejection from every conversation.
func TestOverflowDetectionIsRaceClean(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := overflowBody
			if i%2 == 0 {
				body = "Internal Server Error"
			}
			_ = providerRejectionIsOverflow(body)
			_ = providerRejectionSnippet(body)
		}(i)
	}
	wg.Wait()
}

// --- helpers ---------------------------------------------------------------

// isContextLengthErrorShaped mirrors ai.isContextLengthError's needle list. The
// ai package cannot be imported from utils (db->ai->utils is a cycle), so the
// list is duplicated deliberately: the test pins the CONTRACT between the two
// packages, which is what the production bug broke.
func isContextLengthErrorShaped(msg string) bool {
	low := strings.ToLower(msg)
	for _, needle := range []string{
		"context_length_exceeded", "maximum context length", "input too long",
		"max_prompt_tokens", "context window", "exceeded the context length", "context length is",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// TestHeldSlotIsReleasedOnEveryRetryPath pins the slot accounting the freeze
// depends on: a leak is invisible in the conversation (it stays "working") and
// fatal for every other one (the queue is capacity-1).
func TestHeldSlotIsReleasedOnEveryRetryPath(t *testing.T) {
	// Overflow: terminal, slot released.
	var n1 atomic.Int32
	overflowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1.Add(1)
		http.Error(w, overflowBody, http.StatusInternalServerError)
	}))
	defer overflowSrv.Close()
	if _, err := DoLLMHTTPWithRetryHeld(context.Background(), chatReq(t, overflowSrv.URL)); err == nil {
		t.Fatal("want the overflow error")
	}
	if n := len(LLMSem); n != 0 {
		t.Fatalf("overflow path: %d slot(s) still held", n)
	}

	// A cancelled wait on a busy slot must not consume a slot.
	blocked := make(chan struct{})
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
		w.WriteHeader(http.StatusOK)
	}))
	defer busy.Close()
	defer close(blocked)

	// Occupy the slot, then ask for it with a context that is already done.
	if err := HoldLLMSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := HoldLLMSlot(ctx); err == nil {
		t.Fatal("HoldLLMSlot must honour a cancelled context")
	}
	if n := len(LLMSem); n != 1 {
		t.Fatalf("a cancelled slot wait consumed/leaked slots: len(LLMSem) = %d, want 1", n)
	}
	ReleaseLLMSlotHeld()
	if n := len(LLMSem); n != 0 {
		t.Fatalf("after release: len(LLMSem) = %d, want 0", n)
	}
}
