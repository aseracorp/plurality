package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The request view must never be empty and must never rewrite a row.
//
// The tail cap this file used to test (truncateOversizedTail) is DELETED. It
// dropped oversized tool results whose parent assistant call had been
// dropped, and truncated everything else with a banner. Both halves of that
// were lossy, and the truncation was the prefix-killer: the banner made the
// row oversized on its own output (100000 + a 104-char banner = 100104 over
// the bound), so every later pass re-truncated it and rewrote the banner's
// "original length" — 332 stored rows carried the banner and 6 carried it
// NESTED TWICE, with the inner length equal to the outer cut.
//
// What the frozen path guarantees now:
//   - every live row ships, so the window can never be emptied;
//   - rows ship byte-identical, so the KV prefix behind them survives;
//   - the view is bounded by the preemptive guard + a durable fold, which
//     records what it folded and tells the user.
//
// wneSys / wneModel are local: an unregistered model gets all-false caveman
// stones, so the render is a pure function of the stored rows.

func wneSys() utils.Message {
	return utils.Message{Role: "system", Content: utils.NewTextContent("WNE SYSTEM PROMPT")}
}

const wneModel = "wne-model-not-registered"

func wneToolRow(text string) utils.Message {
	return utils.Message{
		Role:       "tool",
		Name:       "system_tools__shell_exec",
		ToolCallID: "call_x",
		Content:    utils.NewTextContent(text),
	}
}

// TestSentViewNeverEmptiesTheWindow: a window of nothing but oversized orphan
// tool results (no surviving assistant tool_calls anchoring them). Under the
// deleted cap every row hit the drop rule and the window came back EMPTY:
// the model got the system prompt, the schemas and nothing else, and
// appendCurrentTimeTail took its len(msgs)==0 branch and invented a clock-only
// prompt. Observed live: a turn arrived as ONLY "The current time is ...",
// and one stored tool row carried 18 copies of it because the model narrated
// the timestamp into its own tool output.
func TestSentViewNeverEmptiesTheWindow(t *testing.T) {
	var tail []utils.Message
	huge := strings.Repeat("Z", 100_010)
	for i := 0; i < 5; i++ {
		tail = append(tail, wneToolRow(huge))
	}

	out := filterCheckpointsForRequest(tail, true)
	if len(out) == 0 {
		t.Fatalf("the sent view is empty - the request would ship with NO history")
	}
	// Every row here is an orphan tool result (no surviving assistant call), so
	// the legality repair wants to drop ALL of them. The history invariant wins:
	// exactly one row is kept, and the request is never history-less.
	if len(out) != 1 {
		t.Fatalf("kept %d rows for an all-orphan window, want the 1-row guard", len(out))
	}

	// And the rendered request must carry history, not just a clock line.
	all := append([]utils.Message{wneSys()}, out...)
	rows, _ := convertMessagesToOpenAI(all, utils.Model{Name: wneModel})
	history := 0
	for _, r := range rows {
		if r.Role != "system" {
			history++
		}
	}
	if history == 0 {
		t.Errorf("rendered request has no history row: appendCurrentTimeTail would invent a clock-only prompt")
		return
	}

	withClock := appendCurrentTimeTail(append([]StandardMessageReq(nil), rows...))
	if len(withClock) == 1 && withClock[0].Role == "system" {
		if s, ok := withClock[0].Content.(string); ok && strings.HasPrefix(s, "The current time is") {
			t.Errorf("EMPTY REQUEST: the model receives only %q", s)
		}
	}
}

// TestSentViewIsByteStableAcrossPasses: the prefix-stability contract, stated
// directly. Running the request-path view builder twice over the same stored
// rows must produce identical bytes, because any row whose bytes change
// between turns discards the KV-cached prefix behind it. This is what the
// truncation broke, and what "frozen prefix, ship stored bytes" guarantees.
func TestSentViewIsByteStableAcrossPasses(t *testing.T) {
	var tail []utils.Message
	huge := strings.Repeat("Q", 100_010)
	for i := 0; i < 3; i++ {
		tail = append(tail, wneToolRow(huge))
	}

	p1 := filterCheckpointsForRequest(tail, true)
	p2 := filterCheckpointsForRequest(tail, true)
	if len(p1) != len(p2) {
		t.Fatalf("row count changed across passes: %d -> %d", len(p1), len(p2))
	}
	for i := range p1 {
		a := p1[i].Content.TextContent()
		b := p2[i].Content.TextContent()
		if a != b {
			t.Errorf("row %d is not byte-stable: %d chars -> %d chars", i, len(a), len(b))
		}
		if len(a) != len(huge) {
			t.Errorf("row %d was rewritten: %d chars, want the stored %d", i, len(a), len(huge))
		}
	}
	// Feeding pass 1 back in is a no-op too: the view builder has no
	// per-pass state and no banner that can compound.
	p3 := filterCheckpointsForRequest(p1, true)
	if len(p3) != len(p1) {
		t.Fatalf("re-feeding the view changed the row count: %d -> %d", len(p1), len(p3))
	}
	for i := range p3 {
		if p3[i].Content.TextContent() != p1[i].Content.TextContent() {
			t.Errorf("re-feeding the view rewrote row %d", i)
		}
	}
}
