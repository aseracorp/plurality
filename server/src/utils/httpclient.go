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
// The 90s ResponseHeaderTimeout bounds the time to the FIRST RESPONSE HEADER.
// That is safe for STREAMING calls, where it means "time to the first SSE
// event"; a long stream is not affected. Dial/TLS are bounded so no call can
// hang on connect either.
//
// It is NOT safe for a Stream:false one-shot LLM call. There, headers arrive
// only when the whole completion has been generated, so 90s caps the ANSWER at
// ~90s of generation — measured 1116-19683 tokens at the deployed backend's
// 12.4-218.7 tok/s. Any one-shot LLM call must use a client whose transport
// raises ResponseHeaderTimeout to its own round-trip budget; see
// ai.summaryHTTPClient, which CLONES this transport for exactly that reason
// after the 90s silently killed every durable-fold summary on the deployed
// build (net/http: timeout awaiting response headers, at exactly 90s).
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

// CloudLLMSem is a permissive semaphore for CLOUD / auxiliary LLM calls
// (OpenRouter and other hosted providers, title generation, checkpoint
// summaries). It exists because LLMSem is capacity-1 and is held for the
// whole stream by self-hosted slow-prefill models; if cloud calls shared it,
// a single slow local prefill (60-120s+) would hold up every deepseek/etc.
// request flowing through the same server, and cloud responses would in
// turn delay the local model. Cloud providers have their own request
// throttling and never contend for our local GPU, so they must not be
// serialized behind it. Capacity 8 keeps unbounded concurrency in check
// (many parallel agent workflows) without re-introducing the serialization
// stall.
var CloudLLMSem = make(chan struct{}, 8)

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

// AcquireCloudLLMSlot is AcquireLLMSlot but on the cloud/aux semaphore.
func AcquireCloudLLMSlot(ctx context.Context) error {
	select {
	case CloudLLMSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseCloudLLMSlot frees a cloud/aux slot.
func ReleaseCloudLLMSlot() { <-CloudLLMSem }

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

// providerRejectionIsOverflow reports whether a rejection body says the
// REQUEST IS TOO BIG for the model's context window.
//
// WHY THIS IS TERMINAL, NOT RETRYABLE (2026-11-27, conversations
// 76c17afbb288581be0d23812 + fb3a4de4204d715e90863d9f): the local backend
// answers an oversized prompt with
//
//	400 - request exceeds the context window: prompt (262705 tokens) leaves no
//	      room to answer in the context (262144); requests are never truncated
//
// and LiteLLM re-publishes that as **500 Internal Server Error** after its own
// model-fallback attempt. 500 is in RetryableStatus, so Plurality retried the
// IDENTICAL oversized payload 15 times over ~30 minutes. Retrying cannot shrink
// a prompt: the only recovery is compaction, and the LLM loop has that recovery
// path — keyed on the provider's context-length text. The retry layer converted
// a recognisable overflow into
//
//	"LLM provider rejected request with status 500 after 15 retries"
//
// which contains no such text, so the loop never compacted. The payload is
// fixed-size, the answer is fixed, and the loop is capped: retrying it is
// bounded only by the budget, and every attempt burns the one local-GPU slot.
//
// The needles mirror the ai package's isContextLengthError list on purpose: the
// body must reach that matcher, so the fragments that make it through must be
// the fragments that match.
func providerRejectionIsOverflow(body string) bool {
	low := strings.ToLower(body)
	for _, n := range providerOverflowNeedles {
		if strings.Contains(low, n) {
			return true
		}
	}
	return false
}

var providerOverflowNeedles = []string{
	"context_length_exceeded",
	"maximum context length",
	"exceeds the context window",
	"context window",
	"exceeded the context length",
	"context length is",
	"input too long",
	"max_prompt_tokens",
}

// providerOverflowErrorf shapes the terminal overflow error. The provider's own
// words are carried through, because the LLM loop's compaction recovery matches
// on them (isContextLengthError).
func providerOverflowErrorf(status int, body string) error {
	return fmt.Errorf("LLM provider rejected the request as a context-window overflow (status %d): %s",
		status, providerRejectionSnippet(body))
}

// providerRejectionSnippet keeps the provider's message readable in a log and
// in the loop's error matching, without pasting a whole HTML error page into
// the conversation.
func providerRejectionSnippet(body string) string {
	b := strings.TrimSpace(body)
	if len(b) <= 512 {
		return b
	}
	return b[:512] + "…"
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
	// Prefix-cache pacing (see llmsched.go): spaced BEFORE the semaphore, so
	// a request never sits on a scarce slot while it waits out the gap.
	if err := scheduleModelSlot(ctx, modelFromLLMRequest(req)); err != nil {
		return nil, err
	}
	retries := 0
	for {
		// Cloud/auxiliary calls use the cloud semaphore, NOT the capacity-1
		// local-GPU LLMSem. See CloudLLMSem: sharing the local slot with
		// hosted providers made a single slow local prefill block every
		// cloud (deepseek etc.) request through the same server, and cloud
		// responses delayed the local model in turn — upstream Plurality,
		// which has no such global serialization, felt noticeably faster.
		if err := AcquireCloudLLMSlot(ctx); err != nil {
			return nil, err
		}
		resp, err := HTTPClient.Do(req.WithContext(ctx))
		ReleaseCloudLLMSlot()

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
			// Drain + close the body so the connection can be reused. The body
			// decides what happens next, so it is read before the retry branch.
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if providerRejectionIsOverflow(string(respBody)) {
				return nil, providerOverflowErrorf(resp.StatusCode, string(respBody))
			}
			wait := ParseRetryAfter(resp)
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

// backoffOffSlot sleeps a retry backoff with the capacity-1 local-GPU slot
// RELEASED, then re-acquires it before the next attempt.
//
// WHY (2026-11-27): DoLLMHTTPWithRetryHeld holds LLMSlot for the whole request
// so two giant prompts never share one GPU. That is right for a live stream and
// wrong for a sleep — a backoff touches no GPU at all, and the queue is
// capacity-1. A conversation in a retry loop therefore parked every OTHER
// serialized conversation behind it for the whole backoff. The crash dump at
// 21:45:54 shows the cost:
//
//	goroutine 263534 [select, 23 minutes]:
//	  utils.HoldLLMSlot(...)            httpclient.go:151
//	  utils.DoLLMHTTPWithRetryHeld(...) httpclient.go:336
//	  ai.SendChatCompletion(...)        index.go:569
//	  ai.(*ActiveRequest).RunLLMLoop(...) tool_loop.go:198
//
// 23 MINUTES in a slot queue, while the holder slept. The user saw "working"
// with nothing arriving, and cancelling produced "[HandleCancel] No active
// request" because the loop was not where the client expected it.
//
// SLOT CONTRACT: the caller arrives holding the slot. The helper releases it,
// sleeps, and re-acquires it. On SUCCESS the slot is held again, exactly as the
// caller left it. On ERROR (context done, at either point) the slot is NOT held
// and the caller must NOT release it — every error return in the callers follows
// that rule, and TestHeldPathReleasesSlotDuringBackoff pins the accounting.
func backoffOffSlot(ctx context.Context, d time.Duration) error {
	ReleaseLLMSlotHeld()
	select {
	case <-time.After(d):
	case <-ctx.Done():
		return ctx.Err()
	}
	return HoldLLMSlot(ctx)
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
	// Prefix-cache pacing (see llmsched.go). A serialized local-GPU model is
	// already serialized, so the gate's value here is the TTL coalescing
	// branch: when the local cache is cold, the queued conversations dispatch
	// back-to-back and share the one prefill instead of each paying its own.
	if err := scheduleModelSlot(ctx, modelFromLLMRequest(req)); err != nil {
		return nil, err
	}
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
				// Sleep OFF the slot: a backoff uses no GPU, and holding the
				// capacity-1 queue for its duration freezes every other
				// serialized conversation (backoffOffSlot).
				if berr := backoffOffSlot(ctx, backoff); berr != nil {
					return nil, berr // slot already released by the helper
				}
				continue
			}
			ReleaseLLMSlotHeld()
			return nil, err
		}

		if RetryableStatus(resp.StatusCode) {
			// Drain + close the body so the connection can be reused. The body
			// decides what happens next, so it is read before the retry branch:
			// a context-window overflow is TERMINAL (providerRejectionIsOverflow),
			// and on this path the slot is the scarce resource — the whole reason
			// 2026-11-27 froze two conversations for 23 minutes was retries of a
			// payload that can never succeed, holding the one local-GPU slot.
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if providerRejectionIsOverflow(string(respBody)) {
				ReleaseLLMSlotHeld()
				return nil, providerOverflowErrorf(resp.StatusCode, string(respBody))
			}
			wait := ParseRetryAfter(resp)
			if retries < LLMRetryBudget {
				retries++
				Log("[LLM] provider rejected attempt %d with status %d; backing off %v and retrying",
					retries, resp.StatusCode, wait)
				// Sleep OFF the slot (see backoffOffSlot). A 402 budget stall
				// backs off for minutes; no conversation on this box may be
				// queued behind a sleep.
				if berr := backoffOffSlot(ctx, wait); berr != nil {
					return nil, berr // slot already released by the helper
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
