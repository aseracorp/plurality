package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_chain_test.go — the stored roll-up is a CHAIN of megacaved summaries, so
// "no real context is ever fully lost".
//
// The composition contract (db.composeDurableSummary + the escape valve the ai
// layer supplies):
//
//   - the PRIOR roll-up is carried VERBATIM while the total fits the bound;
//   - when prior + new section exceed maxDurableSummaryChars (100_000), the
//     PRIOR is re-megacaved (the deterministic lossy-compress operation), never
//     silently dropped and never re-summarised by an LLM;
//   - a marker line names how many chars of prior roll-up were megacaved down;
//   - the newest folded section is what the escape valve protects: the PRIOR is
//     the thing that gets compressed, because the newest rows are the freshest
//     facts the live model is about to lose;
//   - the stored roll-up stays <= 100_000.

// ucChainHistory: `turns` tool round-trips whose rows all carry `marker` in a
// SHORT user row (short prose rows survive the megacave prose transform, which
// cuts long runs to head+tail). `chars` is the per-row tool-result size.
func ucChainHistory(marker string, turns, chars int) []utils.Message {
	msgs := []utils.Message{{Role: "user", Content: utils.NewTextContent(marker + " opens the chain")}}
	for i := 0; i < turns; i++ {
		if i > 0 && i%4 == 0 {
			msgs = append(msgs, utils.Message{Role: "user",
				Content: utils.NewTextContent(marker + " checkpoint " + itoa(i))})
		}
		body := strings.Repeat("x", chars)
		msgs = append(msgs, ucTurn(i, "system_tools__shell_exec", body)...)
	}
	return msgs
}

// TestChainCompositionBoundsAndKeepsPrior drives the production fold path twice
// on a history big enough that the second fold's prior roll-up is near the
// bound, so the composition MUST use the escape valve. Asserts: stored roll-up
// <= bound, the prior's content is still present (megacaved, not dropped), a
// marker names the compression, the newest section is present, exactly ONE
// section banner, and the chain is deterministic.
func TestChainCompositionBoundsAndKeepsPrior(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-chain-composition", 131072, nil)

	// Fold 1: fold a ~250k-char history down, leaving a tiny live tail, so the
	// stored roll-up lands near the renderer's 100k ceiling.
	first := ucChainHistory("chain generation one", 90, 2_800)
	ucPush(t, ctx, convID, first...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	folded1, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 2, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold 1: %v", err)
	}
	if folded1 == 0 {
		t.Fatal("fold 1 removed 0 rows")
	}
	prior := ucSummaryText(t, ctx, convID)
	if prior == "" {
		t.Fatal("fold 1 stored no roll-up")
	}
	if !strings.Contains(prior, "chain generation one") {
		t.Fatal("roll-up 1 lost its own content")
	}
	t.Logf("chain: prior roll-up after fold 1 = %d chars (bound %d)", len(prior), db.MaxDurableSummaryChars)

	// Fold 2 on top of the fat prior: prior + new section > bound.
	second := ucChainHistory("chain generation two", 90, 2_800)
	ucPush(t, ctx, convID, second...)
	view = ucLLMView(t, ctx, convID)
	conv = ucConvOf(t, ctx, convID, view, model)
	priorText := PriorSummaryFromView(view)
	if len(priorText) < db.MaxDurableSummaryChars/2 {
		t.Fatalf("fixture did not produce a fat prior: %d chars — the escape valve cannot be reached", len(priorText))
	}
	folded2, err := CompactConversationMachineOnly(ctx, convID, view, priorText, 2, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold 2: %v", err)
	}
	if folded2 == 0 {
		t.Fatal("fold 2 removed 0 rows")
	}

	stored := ucSummaryText(t, ctx, convID)
	if stored == "" {
		t.Fatal("fold 2 stored no roll-up")
	}
	t.Logf("chain: stored roll-up after fold 2 = %d chars, prior was %d, bound %d",
		len(stored), len(priorText), db.MaxDurableSummaryChars)

	// 1. BOUNDED. The composition must never exceed the storage bound, and it
	// must not do it by dropping the newest rows.
	if len(stored) > db.MaxDurableSummaryChars {
		t.Fatalf("stored roll-up is %d chars, over the %d bound — the escape valve did not fire",
			len(stored), db.MaxDurableSummaryChars)
	}
	// 2. THE PRIOR SURVIVES, in compressed form.
	if !strings.Contains(stored, "chain generation one") {
		t.Fatal("the prior roll-up's content vanished — it must be megacaved, never dropped")
	}
	// 3. THE MARKER NAMES THE COMPRESSION.
	if !strings.Contains(stored, "megacaved") {
		t.Fatalf("stored roll-up does not name how much prior was megacaved down:\n%s",
			headRunes([]byte(stored), 600))
	}
	// 4. THE NEWEST SECTION IS PRESENT (the prior is the thing that shrinks).
	if !strings.Contains(stored, "chain generation two") {
		t.Fatal("the newest folded section is missing from the chained roll-up")
	}
	// 5. EXACTLY ONE SECTION BANNER: generations must not nest.
	if n := strings.Count(stored, newerHistoryBannerName); n != 1 {
		t.Fatalf("section banner appears %d times, want exactly 1", n)
	}
	// 6. DETERMINISM: the escape valve is a pure transform, so re-composing the
	// same (prior, excerpt) renders byte-identical text.
	chainA := MegacaveSummaryChain(priorText, 40_000)
	chainB := MegacaveSummaryChain(priorText, 40_000)
	if chainA == "" || chainA != chainB {
		t.Fatal("MegacaveSummaryChain is not deterministic")
	}
	if len(chainA) > 40_000 {
		t.Fatalf("MegacaveSummaryChain returned %d chars for a 40000-char room", len(chainA))
	}
	if !strings.Contains(chainA, "chain generation one") {
		t.Fatal("MegacaveSummaryChain dropped the prior's content instead of compressing it")
	}
}
