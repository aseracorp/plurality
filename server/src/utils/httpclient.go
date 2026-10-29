package utils

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPClient is the shared, bounded HTTP client used for ALL outbound HTTP
// calls (LiteLLM, embeddings, images, audio, maps, news, search, version
// checks). It keeps a bound on connect/response time so no single call can
// hang forever and wedge a goroutine.
//
// The 90s ResponseHeaderTimeout still allows long SSE chat streams; only the
// time to first response byte is bounded. Dial/TLS are bounded so no call can
// hang on connect either.
var HTTPClient = &http.Client{
	Timeout: 0, // no overall deadline: SSE streams can run for minutes
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
	},
}

// ---------------------------------------------------------------------------
// Outbound LLM concurrency + retry.
//
// Root cause of the recurring "workflows stop and need to be restarted"
// issue (from the server error log):
//
//   OpenRouter 402:
//     "... would exceed your available credits given your current in-flight
//      requests. Retry after in-flight requests settle"
//     Retry-After: "120"
//
//   -> LiteLLM surfaces it as 500 Internal Server Error
//   -> [LLMLoop] Error calling LLM ... -> the workflow goroutine exits
//
// The server fires several LLM/embedding calls at once (chat stream, async
// message embeddings, title generation, eco checkpoint summary, a second
// chat). Free / low-tier OpenRouter budgets allow only a couple of in-flight
// requests; exceeding that returns 402, LiteLLM wraps it as 500, and the LLM
// loop treats it as fatal — killing the workflow. That also explains why a
// turn that ends with a 60-minute wait never crashes: there are zero
// in-flight requests during the wait.
//
// Two-part fix:
//   1. LLMSem — a global semaphore capping concurrent outbound LLM/embedding
//      HTTP requests (capacity 1: a self-hosted backend — e.g. a single GPU
//      llama.cpp/strata instance — serves one prefill at a time; queueing a
//      second prefill behind the first keeps TTFT bounded instead of two
//      giant prompts thrashing the same GPU). Every LLM/embed caller acquires
//      it around the request (released once response headers arrive, so long
//      SSE streams do not hold the slot).
//   2. DoLLMHTTPWithRetry — issues the request under the semaphore and, on a
//      transient provider rejection (402/429 or 5xx), backs off for the
//      Retry-After header (OpenRouter sends 120s) and retries instead of
//      returning a fatal error. Bounded so it cannot retry forever.
// ---------------------------------------------------------------------------

// LLMSem caps the number of in-flight outbound LLM/embedding requests.
// Tuned for OpenRouter free-tier budgets AND single-GPU self-hosted
// backends: capacity 1 serializes requests so a slow local prefill is never
// competing with a second concurrent prefill on the same GPU (the
// "conversation hangs until the client cancels" stall — two big prompts
// queued simultaneously on llama.cpp/strata can each take many minutes to
// even start decoding, blowing the stream idle timeout).
//
// The slot is normally released once response headers arrive — but
// HoldLLMSlot (used for self-hosted slow-prefill models) keeps it held for
// the whole stream instead, so a second conversation's big prompt is only
// sent AFTER the first stream has finished decoding. Holding across the
// stream is what actually serializes local GPU work: headers arrive before
// the prefill even starts, so releasing at headers alone still lets two
// giant prompts hit the GPU back-to-back.
var LLMSem = make(chan struct{}, 1)

// AcquireLLMSlot blocks until an outbound LLM slot is free, or the given
// context is done (client cancelled, request superseded). Returns the
// context's error when the wait is abandoned so callers never deadlock a
// cancelled workflow behind a long queue.
func AcquireLLMSlot(ctx context.Context) error {
	select {
	case LLMSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseLLMSlot frees an outbound LLM slot.
func ReleaseLLMSlot() { <-LLMSem }

// HoldLLMSlot acquires the LLM slot and keeps it held until the caller
// explicitly releases it with ReleaseLLMSlotHeld. Used by chat completions
// on self-hosted slow-prefill backends: the slot must cover the whole SSE
// stream (prefill + generation), not just the response headers, so a second
// workflow's giant prompt is never queued onto a GPU that is still decoding
// the first one. Returns ctx.Err() if the wait is abandoned.
func HoldLLMSlot(ctx context.Context) error {
	select {
	case LLMSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseLLMSlotHeld frees a slot held by HoldLLMSlot.
func ReleaseLLMSlotHeld() { <-LLMSem }

// LLMRetryBudget is how many times we'll retry a rate/budget-limited call.
// OpenRouter's in-flight-budget 402 resolves as other in-flight streams
// settle (Retry-After ~120s). 15 attempts x up to 120s backoff = up to
// ~30 minutes of retrying a budget stall — long enough for even a busy key
// to clear — so a transient 402 no longer kills the workflow. Bounded so a
// genuinely dead provider still eventually errors out.
const LLMRetryBudget = 15

// ParseRetryAfter extracts the Retry-After header (seconds). Returns 120s if
// unparseable (OpenRouter's default for in-flight budget 402s).
func ParseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 120 * time.Second
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 120 * time.Second
	}
	if sec, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
		if sec < 1 {
			sec = 1
		}
		if sec > 300 {
			sec = 300 // never back off more than 5 min per attempt
		}
		return time.Duration(sec) * time.Second
	}
	return 120 * time.Second
}

// RetryableStatus returns true for statuses that indicate a transient
// provider-side rejection where a retry after backoff is appropriate.
func RetryableStatus(code int) bool {
	// 402 = in-flight budget / credit exhaustion (OpenRouter)
	// 429 = rate limited
	// 5xx = upstream overloaded / temporarily failing
	return code == 402 || code == 429 || (code >= 500 && code < 600)
}

// ensureRetryableBody makes a request retry-safe by installing a GetBody
// closure that re-creates the request body from the marshalled payload on
// every attempt. Without it a retry re-sends the SAME *http.Request whose
// body was already consumed by the first attempt — the transport then sends
// "ContentLength=N with Body length 0" (the exact error seen in production)
// and every retry fails, burning the whole LLMRetryBudget in a ~40 minute
// backoff storm that stalls the workflow for the duration.
//
// http.NewRequest with a *bytes.Reader sets ContentLength eagerly but leaves
// GetBody nil; http.NewRequest with a *bytes.Buffer leaves BOTH nil (and the
// transport then falls back to chunked encoding). Snapshotting the bytes once
// and closing over them gives an exact, cheap replay for both shapes.
func ensureRetryableBody(req *http.Request) {
	if req != nil && req.Body != nil && req.GetBody == nil {
		if bodyBytes, err := io.ReadAll(req.Body); err == nil {
			req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(bodyBytes)), nil
			}
			req.ContentLength = int64(len(bodyBytes))
		}
	}
}

// DoLLMHTTPWithRetry issues an LLM/embedding HTTP request under the global
// concurrency semaphore, retrying on 402/429/5xx with Retry-After backoff.
// Returns the 2xx response (caller owns closing Body) or nil + error after
// the retry budget is exhausted. The semaphore slot is released as soon as
// response headers arrive, so long SSE streams do not count against the
// in-flight budget.
//
// ctx is honoured at every blocking point: waiting for the semaphore slot,
// the HTTP round-trip (a cancelled request aborts the pending connection),
// the retry backoff and the error backoff all bail out immediately when the
// caller's context is done. A workflow the user cancelled therefore never
// stays parked behind a busy GPU queue.
func DoLLMHTTPWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	ensureRetryableBody(req)
	retries := 0
	for {
		if err := AcquireLLMSlot(ctx); err != nil {
			return nil, err
		}
		resp, err := HTTPClient.Do(req.WithContext(ctx))
		ReleaseLLMSlot()

		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if retries < LLMRetryBudget {
				retries++
				backoff := time.Duration(20*retries) * time.Second
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}
			return nil, err
		}

		if RetryableStatus(resp.StatusCode) {
			wait := ParseRetryAfter(resp)
			// Drain + close the body so the connection can be reused.
			io.ReadAll(resp.Body)
			resp.Body.Close()
			if retries < LLMRetryBudget {
				retries++
				Log("[LLM] provider rejected attempt %d with status %d; backing off %v and retrying",
					retries, resp.StatusCode, wait)
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}
			return nil, fmt.Errorf("LLM provider rejected request with status %d after %d retries", resp.StatusCode, LLMRetryBudget)
		}

		return resp, nil
	}
}

// releaseOnCloseReadCloser wraps an io.ReadCloser so that closing it also
// runs a one-shot release callback. Used by DoLLMHTTPWithRetryHeld to free
// the LLM slot exactly when the streamed response body is closed — the
// stream processor always closes the body (defer), so the slot cannot leak
// and no second prompt is admitted while the first stream is still live.
type releaseOnCloseReadCloser struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (r *releaseOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}

// DoLLMHTTPWithRetryHeld is DoLLMHTTPWithRetry for self-hosted slow-prefill
// backends (see HoldLLMSlot): the slot is acquired up-front and held until
// the response body is closed, so the whole SSE stream — prefill AND
// generation — is covered. A second conversation's big prompt is therefore
// only sent once the first stream has finished decoding, instead of two
// giant prompts queueing on the same GPU and both blowing the idle timeout.
//
// Retry semantics mirror DoLLMHTTPWithRetry; a retry reuses the same held
// slot (which also naturally rate-limits how fast failed attempts can be
// re-queued). The slot is released on every error path, on retry exhaustion,
// and via the wrapped body on success.
func DoLLMHTTPWithRetryHeld(ctx context.Context, req *http.Request) (*http.Response, error) {
	ensureRetryableBody(req)
	if err := HoldLLMSlot(ctx); err != nil {
		return nil, err
	}

	retries := 0
	for {
		resp, err := HTTPClient.Do(req.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				ReleaseLLMSlotHeld()
				return nil, ctx.Err()
			}
			if retries < LLMRetryBudget {
				retries++
				backoff := time.Duration(20*retries) * time.Second
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					ReleaseLLMSlotHeld()
					return nil, ctx.Err()
				}
				continue
			}
			ReleaseLLMSlotHeld()
			return nil, err
		}

		if RetryableStatus(resp.StatusCode) {
			wait := ParseRetryAfter(resp)
			// Drain + close the body so the connection can be reused.
			io.ReadAll(resp.Body)
			resp.Body.Close()
			if retries < LLMRetryBudget {
				retries++
				Log("[LLM] provider rejected attempt %d with status %d; backing off %v and retrying",
					retries, resp.StatusCode, wait)
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					ReleaseLLMSlotHeld()
					return nil, ctx.Err()
				}
				continue
			}
			ReleaseLLMSlotHeld()
			return nil, fmt.Errorf("LLM provider rejected request with status %d after %d retries", resp.StatusCode, LLMRetryBudget)
		}

		// Success — hand the body back wrapped so Close() releases the
		// held slot (the stream processor closes it via defer).
		resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: ReleaseLLMSlotHeld}
		return resp, nil
	}
}
