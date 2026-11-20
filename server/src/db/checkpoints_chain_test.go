package db

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// checkpoints_chain_test.go — the roll-up CHAIN and its escape valve, at the
// storage layer.
//
// The invariant this file pins: when prior roll-up + new section exceed
// maxDurableSummaryChars, the PRIOR is compressed by the registered chain
// compressor (the ai layer's megacave transform, deterministic) and a marker
// names how many chars were megacaved down. Content is never dropped silently,
// and the newest folded section is what the escape valve protects.
//
// The compressor is injected: db cannot import ai (ai imports db), and the
// escape valve must be the same deterministic megacave operation the ai layer
// uses for every other excerpt.

// chainCompressor is a stand-in for ai.MegacaveSummaryChain: deterministic, and
// it keeps the tail (like capMegacaveExcerpt) within the room.
func chainCompressor(text string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if len(text) <= maxChars {
		return text
	}
	// Build the marker FIRST, then size the kept tail from it, and never
	// return more than maxChars: the production escape valve rejects a
	// compressor that overshoots its room (that is its contract), so a
	// stand-in that overshoots tests the fallback branch by accident.
	marker := "<<chain compressed " + itoaTest(len(text)) + " chars>>"
	keep := maxChars - len(marker)
	if keep < 0 {
		keep = 0
	}
	out := marker + text[len(text)-keep:]
	if len(out) > maxChars {
		out = out[len(out)-maxChars:]
	}
	return out
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestChainEscapeValveCompressesPrior: a fat prior plus an excerpt that does not
// fit the remaining room -> the prior is compressed by the chain compressor,
// the total stays bounded, the prior's content is present, a marker names the
// compression, and the newest section survives verbatim.
func TestChainEscapeValveCompressesPrior(t *testing.T) {
	SetSummaryChainCompressor(chainCompressor)
	t.Cleanup(func() { SetSummaryChainCompressor(nil) })

	fatPrior := strings.Repeat("prior section line with facts\n", 6_000) // ~150k chars
	if len(fatPrior) <= maxDurableSummaryChars {
		t.Fatalf("fixture prior is only %d chars", len(fatPrior))
	}
	// An excerpt that cannot fit the room left by a bounded prior: the escape
	// valve is the only path that keeps both.
	// 40k excerpt: it does NOT fit the room a 150k prior leaves (100k
	// bound), so the escape valve must compress the PRIOR and keep this whole.
	excerpt := strings.Repeat("E", 40_000)
	got := composeDurableSummary(func([]utils.Message) string { return excerpt }, fatPrior, []utils.Message{userMsg("U newest")})

	if got == "" {
		t.Fatal("composition stored nothing")
	}
	if len(got) > maxDurableSummaryChars {
		t.Fatalf("stored roll-up is %d chars, over the %d bound", len(got), maxDurableSummaryChars)
	}
	if !strings.Contains(got, "megacaved") {
		t.Fatalf("no marker naming how much prior was megacaved down:\n%s", head(got, 400))
	}
	// The newest section is intact — the PRIOR is the thing that shrank.
	if !strings.Contains(got, excerpt) {
		t.Fatal("the newest folded excerpt was cut; the escape valve must compress the PRIOR")
	}
	// The compressor ran (its marker is in the output) and the prior's content
	// is carried, not dropped.
	if !strings.Contains(got, "<<chain compressed") {
		t.Fatal("the chain compressor was not used on the prior roll-up")
	}
	if !strings.Contains(got, "prior section line with facts") {
		t.Fatal("the prior roll-up's content vanished (it must be compressed, never dropped)")
	}
	// Exactly one section banner.
	if n := strings.Count(got, newerHistoryBanner); n != 1 {
		t.Fatalf("section banner appears %d times, want exactly 1", n)
	}
}

// TestChainEscapeValveIsDeterministic: the escape valve is a pure function of
// (prior, excerpt), so the same inputs store byte-identical text — the prompt
// head must not churn.
func TestChainEscapeValveIsDeterministic(t *testing.T) {
	SetSummaryChainCompressor(chainCompressor)
	t.Cleanup(func() { SetSummaryChainCompressor(nil) })

	fatPrior := strings.Repeat("facts from generation one\n", 6_000)
	excerpt := strings.Repeat("N", 40_000)
	a := composeDurableSummary(func([]utils.Message) string { return excerpt }, fatPrior, []utils.Message{userMsg("U")})
	b := composeDurableSummary(func([]utils.Message) string { return excerpt }, fatPrior, []utils.Message{userMsg("U")})
	if a != b {
		t.Fatalf("escape valve is not deterministic:\n--- A ---\n%s\n--- B ---\n%s", head(a, 200), head(b, 200))
	}
}

// TestChainNoCompressorRegisteredKeepsBound: with no compressor registered (a
// db-only build/test), the composition must STILL stay bounded — the outer
// backstop keeps its promise, with a marker, and must never drop the newest
// section silently.
func TestChainNoCompressorRegisteredKeepsBound(t *testing.T) {
	SetSummaryChainCompressor(nil)
	fatPrior := strings.Repeat("legacy prior facts\n", 9_000)
	excerpt := strings.Repeat("Z", 40_000)
	got := composeDurableSummary(func([]utils.Message) string { return excerpt }, fatPrior, []utils.Message{userMsg("U")})
	if got == "" {
		t.Fatal("composition stored nothing")
	}
	if len(got) > maxDurableSummaryChars {
		t.Fatalf("stored roll-up is %d chars, over the %d bound with no compressor registered", len(got), maxDurableSummaryChars)
	}
	if !strings.Contains(got, "legacy prior facts") {
		t.Fatal("the prior roll-up vanished entirely with no compressor registered")
	}
}

// TestChainPriorFitsVerbatim: when prior + new section fit the bound, the prior
// is carried VERBATIM (the chain's existing contract — no compression, no
// re-summarising) and no escape-valve marker is emitted.
func TestChainPriorFitsVerbatim(t *testing.T) {
	SetSummaryChainCompressor(chainCompressor)
	t.Cleanup(func() { SetSummaryChainCompressor(nil) })

	prior := "generation one facts: file=/srv/app/module_0.go"
	excerpt := "generation two facts: file=/srv/app/module_9.go"
	got := composeDurableSummary(func([]utils.Message) string { return excerpt }, prior, []utils.Message{userMsg("U")})
	if !strings.Contains(got, prior) {
		t.Fatalf("a prior that FITS was not carried verbatim:\n%s", got)
	}
	if strings.Contains(got, "megacaved") {
		t.Fatal("the escape-valve marker appeared on a chain that fits — compression ran when it was not needed")
	}
	if !strings.Contains(got, excerpt) {
		t.Fatal("new section missing")
	}
}
