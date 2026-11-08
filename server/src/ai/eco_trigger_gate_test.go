package ai

import (
	"testing"
)

// The eco gate is an OR: compaction fires when content crosses the configured
// trigger OR no longer fits the model's context window. The resolver only
// receives a single number (its `fits()` budget), so the caller must hand it
// the TIGHTER of the two ceilings. Handing it the window-derived budget alone
// makes the window-fit check veto the threshold check: the request path
// compacts on trigger, the resolver answers "full history fits — unchanged"
// and folds 0. Reproduced live on strata-swift: trigger 64000, content
// ~165000, window 262144 => budget 221834 => every turn fired the trigger and
// compaction never happened.
//
// The tests below pin both halves of that.

// TestResolver_TriggerCeilingCompactsWindowFitHistory: history that fits the
// window comfortably but is over the trigger MUST be compacted when the
// resolver is aimed at the trigger ceiling. Before the clamp, this shape
// returned the history unchanged with foldFrom == 0.
func TestResolver_TriggerCeilingCompactsWindowFitHistory(t *testing.T) {
	// ~66k est tokens of content (chars/3): over a 64000 trigger.
	msgs := mkConv(80, 2400)
	got := approxTokens(msgs)
	if got <= 64000 {
		t.Fatalf("setup: want content over the 64000 trigger, got ~%d tokens", got)
	}

	window := 262144
	windowBudget := window - (4096 + 2048)

	// Control: the old (window-only) target answers "fits" — this is the defect.
	unchanged, foldFrom := resolveEcoContextCompaction(msgs, window, windowBudget)
	if len(unchanged) != len(msgs) || foldFrom != 0 {
		t.Fatalf("control expects the window budget to veto compaction; got %d messages, foldFrom %d",
			len(unchanged), foldFrom)
	}

	// Fix: the trigger-derived ceiling compacts the same history.
	out, fold := resolveEcoContextCompaction(msgs, window, 64000-(4096+2048))
	if len(out) == len(msgs) {
		t.Fatalf("trigger ceiling must compact over-trigger history; got it unchanged (%d messages)", len(out))
	}
	if fold <= 0 {
		t.Fatalf("trigger ceiling must report a fold boundary, got %d", fold)
	}
	if approxTokens(out) > 64000 {
		t.Fatalf("staged view must fit the trigger ceiling: ~%d tokens", approxTokens(out))
	}
	// The newest user turn must survive — the resolver must never fold the live
	// request away (defect #161 shipped the full history from an empty tail).
	if indexOfLastUser(out) < 0 {
		t.Fatalf("staged view has no user message; the chat template will 400")
	}
}

// TestResolver_WindowBudgetIsTheOperativeCeilingForSmallWindows: the second
// half of the OR. Content that does not fit the window must compact, and
// compaction must aim at the window — not at a (larger) configured trigger.
// This is the case that already worked before the clamp and must keep working.
func TestResolver_WindowBudgetIsTheOperativeCeilingForSmallWindows(t *testing.T) {
	msgs := mkConv(60, 2400) // ~50k est tokens
	window := 32768
	// Trigger far above the window: the window budget must be operative.
	out, fold := resolveEcoContextCompaction(msgs, window, window-(4096+2048))
	if len(out) == len(msgs) {
		t.Fatalf("over-window history must compact, got it unchanged (%d messages)", len(out))
	}
	if fold <= 0 {
		t.Fatalf("want a fold boundary for over-window history, got %d", fold)
	}
}

// TestResolver_FitsBelowBothCeilingsStaysByteStable: below BOTH the trigger and
// the window the view must be returned byte-identically, which is what keeps
// the KV prompt cache hot. Guards against the clamp making eco megacave
// every turn.
func TestResolver_FitsBelowBothCeilingsStaysByteStable(t *testing.T) {
	msgs := mkConv(4, 200)
	out, fold := resolveEcoContextCompaction(msgs, 262144, 64000-(4096+2048))
	if len(out) != len(msgs) {
		t.Fatalf("below-trigger: expected %d messages unchanged, got %d", len(msgs), len(out))
	}
	if fold != 0 {
		t.Fatalf("below-trigger: expected foldFrom 0, got %d", fold)
	}
	for i := range msgs {
		if out[i].TextContent() != msgs[i].TextContent() {
			t.Fatalf("message %d changed below trigger — KV cache would break", i)
		}
	}
}

// The durable call sites (tool_loop.go, ecoCutoffIndex) share ecoContentBudget,
// which applies the same clamp; the resolver-level tests above pin the
// behaviour, and the clamp arithmetic is asserted there.
