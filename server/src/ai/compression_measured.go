package ai

import (
	"net/http"
	"strconv"
)

// Measured compression.
//
// The compression ratio shown in the UI used to be computed as
// (estimated pre-proxy prompt tokens) / (provider prompt_tokens). Both sides
// of that division measured a different thing at a different place, so the
// quotient was estimator bias, not compression. Measured on this instance:
// models with NO proxy in their path (kimi-k2.6, gpt-oss-120b,
// deepseek-v4.1-flash — headroom:false, caveman:false in litellm_config.yaml)
// reported 29-39% "savings", and one turn reported 97%. Nothing was saved on
// any of those turns. The display gate was ratio > 1.01, which sits inside the
// ~4-8% inflation of the chars/3 divisor, so the badge fired on the divisor
// alone, on every turn, for every model.
//
// Headroom — the proxy that does the compressing — already publishes both
// counts on its response:
//
//	x-headroom-tokens-before / -after / -saved, x-headroom-compressed
//
// Verified end to end here (HEADROOM_MODE=cache, deepseek-v4-flash-0731, 57 KB
// tool payload): before=17254 after=17254 saved=0, provider
// prompt_tokens=17307. Chat for a headroom model goes straight to headroom
// (chatURLFrom → HeadroomBaseURL), so the headers arrive intact; LiteLLM
// publishes no compression headers, which is exactly why a model on the
// LiteLLM path reports NO ratio instead of a fabricated one.
//
// The ratio is therefore the proxy's own PAIRED counts: one tokenizer, one
// measurement point, no cross-proxy division, no chars-per-token constant.
// No header → no ratio → the UI shows nothing.

const (
	headroomTokensBeforeHeader = "X-Headroom-Tokens-Before"
	headroomTokensAfterHeader  = "X-Headroom-Tokens-After"
	headroomTokensSavedHeader  = "X-Headroom-Tokens-Saved"
)

// chatMetrics carries what SendChatCompletion learned from one response about
// compression. It deliberately does NOT carry the byte estimate: that
// heuristic belongs to the preemptive context-window guard, which computes and
// consumes it in place, and it must never travel toward the UI — where
// dividing it by provider prompt_tokens became a fake saving.
type chatMetrics struct {
	// HrBefore / HrAfter are the proxy's own paired token counts. 0/0 means
	// the path reported nothing (direct LiteLLM, proxy off, or a headroom
	// build without the middleware) — read as "not measured", never as 1.0.
	HrBefore int
	HrAfter  int
}

// chatMetricsFrom reads the counts at response start. This has to happen here:
// the body is a stream the caller consumes after this function returns, and
// by then the headers are gone.
func chatMetricsFrom(resp *http.Response) *chatMetrics {
	if resp == nil {
		return &chatMetrics{}
	}
	m := &chatMetrics{
		HrBefore: headerInt(resp.Header, headroomTokensBeforeHeader),
		HrAfter:  headerInt(resp.Header, headroomTokensAfterHeader),
	}
	// Cross-check the explicit saved header when present. If the pair and the
	// saved count disagree, the sample is unusable: a wrong number is worse
	// than no number.
	if saved := headerInt(resp.Header, headroomTokensSavedHeader); saved > 0 &&
		m.HrBefore-m.HrAfter != saved {
		m.HrBefore, m.HrAfter = 0, 0
	}
	if m.HrBefore <= 0 || m.HrAfter <= 0 {
		m.HrBefore, m.HrAfter = 0, 0
	}
	return m
}

func headerInt(h http.Header, name string) int {
	n, err := strconv.Atoi(h.Get(name))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// measuredCompressionRatio turns the proxy's paired counts into the ratio the
// UI renders: >1 means the proxy genuinely shrank the prompt. Returns 0 when
// nothing was measured, so callers can tell "no compression" from "not
// observable" — and so an absent signal can never render a badge.
func measuredCompressionRatio(before, after int) float64 {
	if before <= 0 || after <= 0 {
		return 0
	}
	return float64(before) / float64(after)
}
