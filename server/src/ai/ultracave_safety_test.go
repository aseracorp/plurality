package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// ultracave_safety_test.go — the safety properties the frozen path must keep
// after the per-request sliding char window was removed (task 3).
//
// Four pre-existing assertions in eco_test.go pin the OLD shape — "the tail
// is cut down to maxTailChars and starts on a user row" — which is precisely
// the behaviour this change removes, so they now fail by design:
//
//	TestFilterCheckpointsForRequest_BudgetCapLastElementIsPanicRegression
//	TestFilterCheckpointsForRequest_BudgetCapLongTail
//	TestFilterCheckpointsForRequest_TruncatesOversizedSingleMessage
//	TestFilterCheckpointsForRequest_TruncatesOversizedToolResult
//
// What those tests were PROTECTING against (a request that cannot fit the
// window) is not dropped: the per-MESSAGE bound survives, and it is the
// byte-stable one. These tests pin that, on the same fixtures.

// TestUltracaveFrozenPathStillBoundsOversizedRows: with eco on and a pair
// present, NOTHING is dropped from the head (frozen prefix) and every
// oversized row is still bounded per-message, so a 4.9MB inline blob can
// never ship whole.
func TestUltracaveFrozenPathStillBoundsOversizedRows(t *testing.T) {
	blob := strings.Repeat("B", 4_900_000)
	msgs := []utils.Message{checkpointMsg(), toolMsg()}
	msgs = append(msgs,
		utils.Message{Role: "user", Content: utils.NewTextContent("continue")},
		// The call that owns the huge result MUST be present: a tool result
		// with no surviving parent call is dropped by design (pure context
		// waste), which is a different rule from the per-message bound.
		utils.Message{Role: "assistant", Content: utils.NewTextContent("reading"),
			ToolCalls: []utils.ToolCall{{ID: "call_huge", Type: "function",
				Function: utils.FunctionCall{Name: "filesystem_server__fs_read", Arguments: "{}"}}}},
		utils.Message{Role: "tool", ToolCallID: "call_huge", Name: "filesystem_server__fs_read",
			Content: utils.NewPartsContent([]utils.ContentPart{
				{Type: "text", Text: "[Attachment att_99, bytes 0-50000 of 4901042]"},
				{Type: "text", Text: blob},
			})},
		utils.Message{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	)
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != len(msgs) {
		t.Fatalf("frozen path dropped %d rows; it must keep the pair + every live row", len(msgs)-len(got))
	}
	for _, m := range got {
		if total := m.Content.TotalContentChars(); total > maxKeepTailChars+2_000 {
			t.Fatalf("row over the per-message bound shipped: %d chars (cap %d)", total, maxKeepTailChars)
		}
	}
	// The estimator agrees with the sender on exactly this shape: the blob
	// counts at the cap, not at 4.9MB.
	if est := SentViewTokens(got); est > 6*(maxKeepTailChars/3) {
		t.Fatalf("estimator over-counted a bounded tail: %d tokens", est)
	}
}

// TestUltracaveBootstrapStaysBounded: a conversation with NO pair yet (the
// bootstrap case, "WordPress Security Review") keeps the budget-capped tail,
// so a never-compacted history is bounded on every request.
func TestUltracaveBootstrapStaysBounded(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 1_900; i++ {
		role := "tool"
		if i%4 == 0 {
			role = "user"
		} else if i%4 == 1 {
			role = "assistant"
		}
		content := strings.Repeat("y", 50_000)
		if i == 0 {
			content = strings.Repeat("z", 200_000)
		}
		m := utils.Message{Role: role, Content: utils.NewTextContent(content)}
		if role == "tool" {
			m.Name = "system_tools__shell_exec"
		}
		msgs = append(msgs, m)
	}
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) == 0 {
		t.Fatal("empty bootstrap tail")
	}
	total := 0
	for _, m := range got {
		total += m.Content.TotalContentChars()
	}
	if total > maxTailChars+100_000+200_000 {
		t.Fatalf("bootstrap tail is not bounded: %d chars", total)
	}
	// The overflow path is what mints the pair for this conversation: the
	// boundary arithmetic must return a fold point > 0.
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			msgs[i].PromptTokens = 700_000
		}
	}
	if b := UltracaveOverflowFoldFrom(msgs, 24); b <= 0 {
		t.Fatalf("no overflow boundary for a never-compacted huge history: %d", b)
	}
}

// TestUltracaveEstimatorSkipsFoldedRowsMatchesView: the estimator's char/3
// model tracks the real render closely enough to gate on, on the shape the
// live loop produces (prose + tool rows).
func TestUltracaveEstimatorSkipsFoldedRowsMatchesView(t *testing.T) {
	msgs := ucHistory("estimate fidelity", 20, 2_000)
	est := SentViewTokens(msgs)
	_, rows := ucRenderWire(t, msgs)
	renderedChars := 0
	for _, r := range rows {
		renderedChars += len(mustRowBytes(t, r))
	}
	// The estimator counts message bodies only (no JSON keys/escaping, no
	// system prompt), so it must be POSITIVE, in the same order of magnitude
	// as the wire, and never wildly over it.
	if est <= 0 {
		t.Fatalf("estimator %d", est)
	}
	if est*3 > renderedChars*3/2 {
		t.Fatalf("estimator over-counts the wire: est*3=%d rendered=%d", est*3, renderedChars)
	}
	if est*3 < renderedChars/2 {
		t.Fatalf("estimator under-counts the wire (dangerous): est*3=%d rendered=%d", est*3, renderedChars)
	}
}
