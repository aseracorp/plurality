package ai

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The compression badge used to be built from
// (chars/3 estimate of the marshalled request) / (provider prompt_tokens).
// Those two numbers are measured at different places by different tokenizers,
// so the quotient tracked the divisor heuristic: a model with no compression
// proxy anywhere in its path displayed "29-39% saved", and one turn displayed
// 97%. Nothing was saved on any of those turns.
//
// These tests pin the replacement: the ratio comes from the proxy's own paired
// counts, and a path that reports no counts reports NO ratio.

func TestChatMetricsFrom_NoHeadersIsNotARatio(t *testing.T) {
	// The direct LiteLLM path. Measured on this instance: LiteLLM publishes
	// zero x-headroom-* headers, so this is the common case for every model
	// with headroom:false (kimi-k2.6, gpt-oss-120b, deepseek-v4.1-flash).
	resp := httptest.NewRecorder().Result()
	m := chatMetricsFrom(resp)
	if m.HrBefore != 0 || m.HrAfter != 0 {
		t.Fatalf("no headers must read as unmeasured, got %d/%d", m.HrBefore, m.HrAfter)
	}
	if r := measuredCompressionRatio(m.HrBefore, m.HrAfter); r != 0 {
		t.Fatalf("unmeasured path must yield ratio 0 (no badge), got %v", r)
	}
}

func TestChatMetricsFrom_NilResponseIsSafe(t *testing.T) {
	m := chatMetricsFrom(nil)
	if m == nil {
		t.Fatal("nil response must still yield usable metrics")
	}
	if measuredCompressionRatio(m.HrBefore, m.HrAfter) != 0 {
		t.Fatal("nil response must yield ratio 0")
	}
}

// Measured live on this instance (HEADROOM_MODE=cache, model
// deepseek-v4-flash-0731, a 57 KB tool payload through headroom at :8787):
// before=17254 after=17254 saved=0, provider prompt_tokens=17307.
// The proxy compressed nothing, so the honest answer is "no ratio" - where the
// old code reported a large saving because its numerator was invented.
func TestChatMetricsFrom_MeasuredZeroCompressionYieldsNoBadge(t *testing.T) {
	resp := &http.Response{Header: http.Header{
		"X-Headroom-Tokens-Before": []string{"17254"},
		"X-Headroom-Tokens-After":  []string{"17254"},
		"X-Headroom-Tokens-Saved":  []string{"0"},
	}}
	m := chatMetricsFrom(resp)
	if m.HrBefore != 17254 || m.HrAfter != 17254 {
		t.Fatalf("paired counts lost: %d/%d", m.HrBefore, m.HrAfter)
	}
	if r := measuredCompressionRatio(m.HrBefore, m.HrAfter); r > 1.0 {
		t.Fatalf("a turn that saved nothing must not clear the >1.0 display gate, got %v", r)
	}
}

func TestChatMetricsFrom_MeasuredCompression(t *testing.T) {
	resp := &http.Response{Header: http.Header{
		"X-Headroom-Tokens-Before": []string{"20000"},
		"X-Headroom-Tokens-After":  []string{"5000"},
		"X-Headroom-Tokens-Saved":  []string{"15000"},
	}}
	m := chatMetricsFrom(resp)
	if r := measuredCompressionRatio(m.HrBefore, m.HrAfter); r != 4.0 {
		t.Fatalf("20000/5000 must read as 4.0x, got %v", r)
	}
}

func TestChatMetricsFrom_SavedHeaderMustAgreeWithThePair(t *testing.T) {
	// A proxy that publishes an inconsistent triple is a broken sample. A
	// wrong number in the UI is worse than no number.
	resp := &http.Response{Header: http.Header{
		"X-Headroom-Tokens-Before": []string{"20000"},
		"X-Headroom-Tokens-After":  []string{"5000"},
		"X-Headroom-Tokens-Saved":  []string{"1000"},
	}}
	m := chatMetricsFrom(resp)
	if m.HrBefore != 0 || m.HrAfter != 0 {
		t.Fatalf("inconsistent counts must be discarded, got %d/%d", m.HrBefore, m.HrAfter)
	}
	if measuredCompressionRatio(m.HrBefore, m.HrAfter) != 0 {
		t.Fatal("discarded sample must yield ratio 0")
	}
}

func TestChatMetricsFrom_GarbageHeaders(t *testing.T) {
	for _, h := range []http.Header{
		{"X-Headroom-Tokens-Before": []string{"not-a-number"}, "X-Headroom-Tokens-After": []string{"10"}},
		{"X-Headroom-Tokens-Before": []string{"10"}, "X-Headroom-Tokens-After": []string{"-3"}},
		{"X-Headroom-Tokens-Before": []string{"0"}, "X-Headroom-Tokens-After": []string{"0"}},
		{"X-Headroom-Tokens-Before": []string{"100"}},
	} {
		m := chatMetricsFrom(&http.Response{Header: h})
		if r := measuredCompressionRatio(m.HrBefore, m.HrAfter); r != 0 {
			t.Fatalf("header set %v must yield ratio 0, got %v", h, r)
		}
	}
}

func TestRecordCompression_RejectsUnmeasuredAndExtremes(t *testing.T) {
	const model = "unit-test-compression-model"
	// Unmeasured turns must not enter the rolling factor at all.
	RecordCompression(model, 0, 0)
	if f := CompressionFactor(model); f != 1.0 {
		t.Fatalf("unmeasured turns must leave the factor at 1.0, got %v", f)
	}
	// Real signal feeds the factor.
	RecordCompression(model, 20000, 5000)
	if f := CompressionFactor(model); f <= 1.0 {
		t.Fatalf("measured 4x compression should move the factor, got %v", f)
	}
	// Out-of-band samples are dropped so one proxy hiccup cannot relax the
	// context-window guard by 100000x.
	RecordCompression("unit-test-bounds-model", 100000, 1)
	if f := CompressionFactor("unit-test-bounds-model"); f != 1.0 {
		t.Fatalf("100000x sample must be rejected, got %v", f)
	}
}
