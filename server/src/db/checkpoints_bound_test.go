package db

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// checkpoints_bound_test.go — a fold's roll-up bound is the FOLD'S OWN budget,
// not a global char constant.
//
// The stored roll-up plus the live tail a fold leaves is what the next request
// ships, so the bound is the part of the eco TARGET the live tail does not use.
// The constant cannot express that: on the 262144-token deployment the target
// is 144000 tokens and 100_000 chars is ~33333 tokens — a FIFTH of it. Every
// fold stored a roll-up far under the room it had bought, the post-fold view
// landed at 34125 tokens against a 144000 target, and the view crossed the
// trigger again within turns. That fold cadence IS the context-size "jumping"
// the user reported, and each fold rewrites the pair — the head of the prompt
// (cache_read pinned at 21521).

func TestComposeDurableSummaryHonoursTheFoldsBound(t *testing.T) {
	prior := strings.Repeat("p", 60_000)
	excerpt := strings.Repeat("e", 250_000)
	folded := []utils.Message{userMsg("U newest")}

	// A caller naming a 400k bound (the room a 144000-token target affords)
	// must get the chain it asked for, not the 100k constant.
	wide := composeDurableSummary(func([]utils.Message) string { return excerpt }, prior, folded, 400_000)
	if len(wide) <= maxDurableSummaryChars {
		t.Fatalf("with a 400000-char bound the roll-up is %d chars: the caller's bound was ignored and the "+
			"100k constant applied — the fold stores a fifth of the room it bought and re-crosses the "+
			"trigger within turns", len(wide))
	}
	if len(wide) > 400_000 {
		t.Fatalf("the roll-up is %d chars, over the bound the caller asked for: the bound must stay a ceiling", len(wide))
	}

	// Callers that name no bound keep the historical constant: the write can
	// still never grow the roll-up without limit.
	def := composeDurableSummary(func([]utils.Message) string { return excerpt }, prior, folded)
	if len(def) > maxDurableSummaryChars+200 {
		t.Fatalf("with no bound the roll-up is %d chars, over the %d default", len(def), maxDurableSummaryChars)
	}
}

// TestDurableFoldStoresTheCallersRollupBound: the bound must reach the WRITE,
// not just the renderer. A fold that renders 250k chars and stores 100k is a
// fold that silently drops 150k chars of history the renderer produced.
func TestDurableFoldStoresTheCallersRollupBound(t *testing.T) {
	userID := "bound-user"
	d := openFrozenDB(t, userID)
	convID := "bound-conv"
	seedConv(t, d, convID)

	var all []utils.Message
	for i := 0; i < 12; i++ {
		all = append(all, bsGiantTurn(i, 2_000)...)
	}
	rows := appendRows(t, d, convID, all...)

	if _, err := CompactConversationDurable(frozenCtx(userID), convID, rows, len(rows)-3,
		func([]utils.Message) string { return strings.Repeat("W", 250_000) }, 400_000); err != nil {
		t.Fatalf("fold: %v", err)
	}
	p, err := GetCheckpoint(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if p == nil {
		t.Fatal("the fold minted no checkpoint pair")
	}
	if len(p.Summary) <= maxDurableSummaryChars {
		t.Fatalf("the fold stored %d chars: the caller's 400000-char bound never reached the write, so the "+
			"storage layer truncates text its own renderer produced", len(p.Summary))
	}
	if len(p.Summary) > 400_000 {
		t.Fatalf("the stored roll-up is %d chars, over the bound the caller named", len(p.Summary))
	}
}
