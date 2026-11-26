package db

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The APPEND-ONLY invariant, at the storage layer.
//
// The owner's requirement: the fold's LLM call must not re-emit the existing
// roll-up, because writing 100k tokens at ~40 tokens/second is a 40-minute fold
// and the bytes are a 1:1 replay of what the server already stores. The ai
// layer therefore writes only the NEW section. This file pins what the storage
// layer owes that contract:
//
//	1. the prior is carried byte-for-byte and the new section is appended at the
//	   tail, so the stored text GROWS BY APPEND. That is what lets the backend
//	   KV prefix extend over the whole chain instead of diverging at the first
//	   byte of the new section - the pair is the first content after the system
//	   block, so a rewrite there collapses prefix reuse for the entire prompt;
//	2. the section banner exists exactly once, at the first append point, so the
//	   chain never nests and the bytes before the append point never move;
//	3. when the chain cannot fit, the escape valve compresses the PRIOR and
//	   protects the newest section - append-only is not "grow forever", and the
//	   compression is announced rather than silent.

func TestChainGrowsByAppendAndKeepsThePrefixBytes(t *testing.T) {
	SetSummaryChainCompressor(chainCompressor)
	t.Cleanup(func() { SetSummaryChainCompressor(nil) })

	first := composeDurableSummary(
		func([]utils.Message) string { return "[2026-11-25] user: start the deploy plan" },
		"", []utils.Message{userMsg("U1")}, 20000)
	if strings.Contains(first, newerHistoryBanner) {
		t.Fatal("the first section must not carry a section banner: there is nothing to append onto")
	}

	// The second fold appends. The text stored by fold one must be a literal
	// prefix of the text stored by fold two.
	second := composeDurableSummary(
		func([]utils.Message) string { return "[2026-11-26] assistant: the fold protocol landed" },
		first, []utils.Message{userMsg("U2")}, 20000)
	if !strings.HasPrefix(second, first) {
		t.Fatalf("the chain did not grow by append - the KV prefix over the prior would diverge:\nprior=%q\nnext=%q", first, second)
	}
	if n := strings.Count(second, newerHistoryBanner); n != 1 {
		t.Fatalf("the section banner appears %d times: the chain nests, and every byte before the append point moves", n)
	}
	if !strings.HasSuffix(second, "fold protocol landed") {
		t.Fatal("the new section is not at the tail")
	}

	// Third fold: same rule. Append, banner count stays 1, prefix intact.
	third := composeDurableSummary(
		func([]utils.Message) string { return "[2026-11-27] user: ship it" },
		second, []utils.Message{userMsg("U3")}, 20000)
	if !strings.HasPrefix(third, second) {
		t.Fatal("the third append rewrote the chain")
	}
	if n := strings.Count(third, newerHistoryBanner); n != 1 {
		t.Fatalf("banner count drifted to %d", n)
	}
}

// A chain that cannot fit is the escape-valve case: the PRIOR compresses, the
// newest section survives, and the compression is announced.
func TestChainEscapeValveProtectsTheNewestSection(t *testing.T) {
	SetSummaryChainCompressor(chainCompressor)
	t.Cleanup(func() { SetSummaryChainCompressor(nil) })

	prior := newerHistoryBanner + "\n" + strings.Repeat("fact; ", 3000)
	excerpt := "[2026-11-26] the newest facts the model is about to lose"
	got := composeDurableSummary(func([]utils.Message) string { return excerpt }, prior,
		[]utils.Message{userMsg("U")}, 4000)
	if !strings.Contains(got, excerpt) {
		t.Fatal("the escape valve dropped the NEWEST section: the freshest facts are what must survive")
	}
	if !strings.Contains(got, chainCompressionMarker) {
		t.Fatal("the escape valve ran silently")
	}
	if strings.Contains(got, newerHistoryBanner+newerHistoryBanner) {
		t.Fatal("the chain nested")
	}
	if len(got) > 4000+2 {
		t.Fatalf("the escape valve let the chain overflow its bound: %d chars", len(got))
	}
}
