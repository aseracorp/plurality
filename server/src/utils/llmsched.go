package utils

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Prefix-cache-aware dispatch scheduling.
//
// Root cause of "cache reads collapsed to 24576 tokens (= the system prompt
// only), input cost ~5x" observed on strata-swift on the simplify box:
//
//   OpenRouter's DeepSeek prompt-caching docs describe caching as
//   "probabilistic, load-balanced, and rate-limited": cache hits are routed to
//   the node that holds the prefix, and "high load and bursts of concurrent
//   requests can evict your cache sooner". The same guidance appears for
//   Gemini ("Cache hit rates are reduced by load balancing... minimize
//   concurrent requests and space out sequential requests") and Anthropic
//   (routing affinity plus a per-key ceiling on cache writes).
//
//   Plurality fanned requests out with no pacing at all: CloudLLMSem has
//   capacity 8 and every LLM call — chat stream, title generation, eco
//   checkpoint summary, embeddings, sub-agent runs — dispatches the moment it
//   is ready. One turn fires title generation + a checkpoint summary + the
//   next turn's stream within the same second, and parallel sub-agents fan out
//   further. Each request in a burst lands on a different load-balanced node,
//   finds only the shared system prompt cached, and reports
//   input_read_tokens == system prompt size. A full conversation history is
//   then billed as fresh input on every single turn.
//
// Two knobs, both taken from the providers' own documented mitigations:
//
//  1. Dispatch spacing per model (default 1200ms, LLM_CACHE_GAP_MS):
//     de-bursting is the documented mitigation ("space out sequential
//     requests"). Cost when a model's requests are already naturally spaced
//     (a human typing) is ZERO — the gate only engages when the previous
//     dispatch for the same model is younger than the gap.
//
//  2. Cold-window coalescing (default 300s, LLM_CACHE_TTL_S): once a model
//     has been idle past the provider's cache TTL the prefix is gone, and the
//     first request after that pays a full prefill no matter what we do. When
//     several requests are queued at that moment, the one that crossed the TTL
//     goes FIRST and the rest follow it immediately: they ride the cache its
//     predecessor just wrote (same-model deployments share the system prompt,
//     tool list and config across conversations) instead of every queued
//     request paying its own cold prefill.
//
// The gap sits deliberately far below the TTL: spacing dispatches ~1.2s apart
// keeps a hot prefix hot, whereas pushing requests toward the TTL boundary
// would trade a small queueing win for near-certain evictions.
//
// Scope discipline: this file decides only WHEN a request leaves the process.
// It never reorders across models (no cross-tenant starvation), never adds
// more than one gap of latency, always honours ctx cancellation, and fails
// OPEN (dispatch immediately) if the clock cannot be trusted to advance — a
// scheduler must never become the thing that stalls a turn.
//
// Tests: llmsched_test.go
// ---------------------------------------------------------------------------

const (
	defaultCacheGap = 1200 * time.Millisecond
	defaultCacheTTL = 300 * time.Second

	// globalDispatchFloor spaces ANY two outbound LLM dispatches apart, so a
	// fan-out across DIFFERENT models cannot stampede the provider's per-key
	// request rate either. Kept small: cross-model requests share no prefix,
	// so this is provider-rate courtesy, not cache protection.
	globalDispatchFloor = 250 * time.Millisecond

	// maxSchedulePasses bounds the pacing loop. Normal operation needs 1 pass
	// (2 with cross-model floor contention); the cap exists so a pathological
	// clock can never turn the scheduler into a livelock.
	maxSchedulePasses = 8
)

// modelSchedState is the per-model dispatch history the two rules read.
type modelSchedState struct {
	lastDispatch time.Time
	gap          time.Duration
	ttl          time.Duration
}

// llmScheduler is the pacing state machine. `now` is injectable so the TTL and
// gap branches are testable without long wall-clock sleeps.
type llmScheduler struct {
	mu         sync.Mutex
	perModel   map[string]*modelSchedState
	defaultGap time.Duration
	defaultTTL time.Duration
	enabled    bool
	now        func() time.Time
	lastAny    time.Time
}

var scheduler = newLLMScheduler()

func newLLMScheduler() *llmScheduler {
	s := &llmScheduler{
		perModel:   map[string]*modelSchedState{},
		defaultGap: defaultCacheGap,
		defaultTTL: defaultCacheTTL,
		enabled:    true,
		now:        time.Now,
	}
	if v := os.Getenv("LLM_CACHE_GAP_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			s.defaultGap = time.Duration(ms) * time.Millisecond
		}
	}
	if v := os.Getenv("LLM_CACHE_TTL_S"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
			s.defaultTTL = time.Duration(sec) * time.Second
		}
	}
	switch strings.ToLower(os.Getenv("LLM_CACHE_SCHEDULING")) {
	case "off", "false", "0", "none":
		s.enabled = false
	}
	return s
}

// SetModelSchedulingEnabled flips the gate at runtime (used by tests and by
// the escape hatch; deployment control is the LLM_CACHE_SCHEDULING env var).
func SetModelSchedulingEnabled(on bool) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	scheduler.enabled = on
}

// RegisterModelScheduling records per-model pacing from a model's config
// params so a deployment can tune ONE backend (a home-brew llama.cpp that
// re-keys on every request, a hosted provider with a 120s TTL) without moving
// global defaults:
//
//	params: { "cache_gap_ms": "400", "cache_ttl_s": "120" }
//
// Absent params keep the defaults; an explicit 0 disables that rule for the
// model (gap 0 = no spacing, ttl 0 = treat the prefix as permanently hot).
// Mirrors how the `serialize` flag is controlled. Called per request next to
// modelRequiresSerialization, so it needs no config-load hook.
func RegisterModelScheduling(m Model) {
	if m.Name == "" || len(m.Params) == 0 {
		return
	}
	key := strings.ToLower(m.Name)
	gap, hasGap := paramDurationMS(m.Params["cache_gap_ms"])
	ttl, hasTTL := paramDurationSec(m.Params["cache_ttl_s"])
	if !hasGap && !hasTTL {
		return
	}

	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	st, ok := scheduler.perModel[key]
	if !ok {
		st = &modelSchedState{gap: scheduler.defaultGap, ttl: scheduler.defaultTTL}
		scheduler.perModel[key] = st
	}
	if hasGap {
		st.gap = gap
	}
	if hasTTL {
		st.ttl = ttl
	}
}

func paramDurationMS(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

func paramDurationSec(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec < 0 {
		return 0, false
	}
	return time.Duration(sec) * time.Second, true
}

// scheduleModelSlot blocks until a request for `model` may be dispatched, then
// records the dispatch. An empty model name means "unmodelled" (a request body
// we could not classify) and is left ungated. ctx cancellation aborts the
// wait: the caller gets the ctx error and nothing is left queued.
func scheduleModelSlot(ctx context.Context, model string) error {
	if model == "" {
		return ctx.Err()
	}
	return scheduler.schedule(ctx, model)
}

// schedule is the testable core. Each pass computes the exact remaining sleep,
// releases the lock, sleeps, and re-checks; sleeping outside the lock keeps
// different models independent.
func (s *llmScheduler) schedule(ctx context.Context, model string) error {
	key := strings.ToLower(model)
	var prevNow time.Time

	// Hard cap on how long ONE request may be paced, enforced across all
	// passes: the scheduler must never add more than about one gap of latency
	// to a turn, whatever the clock does. Without this, a clock that does not
	// advance makes every pass re-sleep the FULL gap and the request pays N
	// gaps for N passes.
	budget := s.defaultGap + globalDispatchFloor
	var slept time.Duration

	for pass := 0; ; pass++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		s.mu.Lock()

		// Fail open: scheduling disabled, or the clock is not advancing
		// (pass N saw the same logical time as pass N-1), in which case the
		// gap can never elapse and a livelock would hang the turn.
		if !s.enabled || pass >= maxSchedulePasses {
			s.mu.Unlock()
			return nil
		}
		now := s.now()
		// A non-advancing clock can never let the gap elapse: fail open.
		// Comparing against lastAny catches it on the first pass (the previous
		// dispatch recorded this same logical instant), so a stalled clock
		// costs zero latency instead of one full gap per request.
		if !s.lastAny.IsZero() && now.Equal(s.lastAny) {
			s.mu.Unlock()
			return nil
		}
		if !prevNow.IsZero() && !now.After(prevNow) {
			s.mu.Unlock()
			return nil
		}

		st := s.stateLocked(key)

		// Cold window (prefix past TTL) is checked BEFORE the global floor: a
		// cold prefill is coming at this request either way, so making it wait
		// behind an unrelated model's dispatch buys nothing.
		cold := !st.lastDispatch.IsZero() && now.Sub(st.lastDispatch) > st.ttl

		var wait time.Duration
		switch {
		case cold:
			// Dispatch at once: this request pays the prefill, and the queue
			// behind it rides the cache it writes.
		case st.lastDispatch.IsZero():
			// First request for this model: no prefix to protect, so the
			// per-model gap does not apply — only the global floor, which is
			// about the provider's request rate and applies to every dispatch.
			if !s.lastAny.IsZero() {
				wait = globalDispatchFloor - now.Sub(s.lastAny)
				if wait < 0 {
					wait = 0
				}
			}
		default:
			wait = st.gap - now.Sub(st.lastDispatch)
			if !s.lastAny.IsZero() {
				if d := globalDispatchFloor - now.Sub(s.lastAny); d > wait {
					wait = d
				}
			}
			if wait < 0 {
				wait = 0
			}
		}

		if wait > 0 {
			if left := budget - slept; left < wait {
				wait = left
			}
			if wait <= 0 {
				// Out of pacing budget: dispatch now (fail open) rather than
				// delay the turn any further.
				st.lastDispatch = now
				s.lastAny = now
				s.mu.Unlock()
				return nil
			}
			prevNow = now
			s.mu.Unlock()
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
			slept += wait
			continue
		}

		st.lastDispatch = now
		s.lastAny = now
		s.mu.Unlock()
		return nil
	}
}

// stateLocked returns the per-model state, seeding it from the defaults.
// Callers must hold s.mu.
func (s *llmScheduler) stateLocked(key string) *modelSchedState {
	st, ok := s.perModel[key]
	if !ok {
		st = &modelSchedState{gap: s.defaultGap, ttl: s.defaultTTL}
		s.perModel[key] = st
	}
	return st
}

// modelFromLLMRequest pulls the model name out of an outbound chat/embedding
// request body so the scheduler can group requests by backend. Cheap (one
// field from an already-snapshot body) and best-effort: an unparseable body
// yields "", which schedules ungated.
func modelFromLLMRequest(req *http.Request) string {
	if req == nil {
		return ""
	}
	var body []byte
	if req.GetBody != nil {
		if rc, err := req.GetBody(); err == nil {
			body, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Model
}
