package utils

// httpclient_slot_backoff_test.go — the capacity-1 local-GPU slot must never be
// held across a retry backoff.
//
// DoLLMHTTPWithRetryHeld holds LLMSlot for the whole request: two giant prompts
// must not prefill on one GPU at once. That is the right rule for a live stream
// and the wrong rule for a SLEEP — a backoff touches no GPU, and the queue has
// capacity 1, so a conversation that is sleeping blocks every other serialized
// conversation on the box for the duration.
//
// The live cost, from the crash dump at 2026-11-27 21:45:54:
//
//	goroutine 263534 [select, 23 minutes]:
//	  utils.HoldLLMSlot(...)              httpclient.go:151
//	  utils.DoLLMHTTPWithRetryHeld(...)   httpclient.go:336
//	  ai.SendChatCompletion(...)          index.go:569
//	  ai.(*ActiveRequest).RunLLMLoop(...) tool_loop.go:198
//
// 23 minutes in a slot queue behind a holder that was in a backoff. Two
// conversations showed "working" and received nothing; the user cancelled them,
// which logged "[HandleCancel] No active request" because the loops were parked
// where no cancel could reach them.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestHeldPathReleasesSlotDuringBackoff is the freeze, reproduced: the provider
// returns a retryable 500 with a 1s Retry-After. While the first conversation
// backs off, a SECOND conversation's request must reach the provider.
//
// Pre-fix this deadlocks: the holder sleeps with the slot, the second request
// blocks in HoldLLMSlot, and the test only ends when the timeout fires.
func TestHeldPathReleasesSlotDuringBackoff(t *testing.T) {
	// This test is about the SLOT, so prefix-cache pacing is switched off: both
	// requests name the same model, and the scheduler's per-model gap (1.2s
	// default) runs BEFORE the slot is acquired, so it would block the second
	// request for reasons that have nothing to do with the slot — the first
	// version of this test failed on exactly that, on FIXED code.
	SetModelSchedulingEnabled(false)
	defer SetModelSchedulingEnabled(true)

	var firstCalls, secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := firstCalls.Add(1)
		if n == 1 {
			// 3s backoff, LONGER than the second conversation's slot-wait
			// timeout below. That is the whole point: pre-fix the first
			// conversation HOLDS the capacity-1 slot across this sleep, so the
			// second times out (starved). Post-fix the slot is released for the
			// sleep and the second completes at once. A backoff shorter than the
			// timeout would let the second merely wait it out and the test would
			// pass on unfixed code (that was an earlier, too-weak version).
			w.Header().Set("Retry-After", "3")
			http.Error(w, "upstream overloaded", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer first.Close()

	// The second endpoint records that it was reached, and holds the slot only
	// for its own round-trip.
	secondHit := make(chan struct{})
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		close(secondHit)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer second.Close()

	// Start the retrying conversation. The success path releases the slot when
	// the response BODY is closed (releaseOnCloseReadCloser), so the body must
	// be closed here — a test that drops the body leaks the slot on purpose.
	done := make(chan error, 1)
	go func() {
		resp, err := DoLLMHTTPWithRetryHeld(context.Background(), slotReq(t, first.URL))
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()

	// Give it time to take the slot and enter the backoff.
	deadline := time.Now().Add(3 * time.Second)
	for len(LLMSem) == 0 && time.Now().Before(deadline) && firstCalls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if firstCalls.Load() == 0 {
		t.Fatal("the first request never reached the provider")
	}

	// While it is sleeping (3s), a second conversation must get through. Its
	// slot-wait timeout (1s) is SHORTER than the backoff: pre-fix it starves.
	secondDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		resp, err := DoLLMHTTPWithRetryHeld(ctx, slotReq(t, second.URL))
		if err == nil {
			resp.Body.Close()
		}
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("the second conversation was blocked behind the first's backoff: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the second conversation was starved: the retrying holder kept the capacity-1 slot across its backoff")
	}
	if secondHits.Load() != 1 {
		t.Fatalf("the second request reached the provider %d times, want 1", secondHits.Load())
	}

	if err := <-done; err != nil {
		t.Fatalf("the retrying conversation failed after its backoff: %v", err)
	}
	if got := firstCalls.Load(); got != 2 {
		t.Fatalf("first request attempts = %d, want 2 (one 503, one success)", got)
	}
	// Both loops finished: the slot must be free.
	if n := len(LLMSem); n != 0 {
		t.Fatalf("after both requests: len(LLMSem) = %d, want 0 — a slot leaked", n)
	}
}

// TestBackoffOffSlotSlotContract pins the accounting the fix depends on:
// success re-holds, cancellation does not, and no path leaks.
func TestBackoffOffSlotSlotContract(t *testing.T) {
	// Success: the caller's hold is restored.
	if err := HoldLLMSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backoffOffSlot(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("backoffOffSlot: %v", err)
	}
	if n := len(LLMSem); n != 1 {
		t.Fatalf("after a successful backoff: len(LLMSem) = %d, want 1 (slot must be re-held)", n)
	}
	ReleaseLLMSlotHeld()
	if n := len(LLMSem); n != 0 {
		t.Fatalf("len(LLMSem) = %d, want 0", n)
	}

	// Cancelled during the sleep: the slot is NOT held, and no slot is burned.
	if err := HoldLLMSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := backoffOffSlot(ctx, 5*time.Second); err == nil {
		t.Fatal("backoffOffSlot must honour ctx cancellation")
	}
	if n := len(LLMSem); n != 0 {
		t.Fatalf("after a cancelled backoff: len(LLMSem) = %d, want 0 — the helper must leave no held slot", n)
	}

}

// TestHeldPathReleasesSlotOnTransportErrorBackoff covers the transport-error
// branch (connection refused) with a cancelled context: the helper's release
// must leave nothing behind.
func TestHeldPathReleasesSlotOnTransportErrorBackoff(t *testing.T) {
	// A server that is closed immediately: every attempt fails at the transport.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := DoLLMHTTPWithRetryHeld(ctx, slotReq(t, url)); err == nil {
		t.Fatal("want a transport/context error")
	}
	if n := len(LLMSem); n != 0 {
		t.Fatalf("len(LLMSem) = %d, want 0 — the transport-error backoff leaked a slot", n)
	}
}

func slotReq(t *testing.T, url string) *http.Request {
	t.Helper()
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
