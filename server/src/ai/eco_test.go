package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// checkpointMsg builds the assistant half of an eco checkpoint pair — a
// synthetic assistant message whose only tool call is the checkpoint tool.
func checkpointMsg() utils.Message {
	return utils.Message{
		Role:    "assistant",
		Content: utils.MessageContent{},
		ToolCalls: []utils.ToolCall{{
			ID:   "eco_regression",
			Type: "function",
			Function: utils.FunctionCall{
				Name:      db.EcoCheckpointToolName,
				Arguments: "{}",
			},
		}},
	}
}

// toolMsg builds the tool half of the checkpoint pair.
func toolMsg() utils.Message {
	return utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent("checkpoint summary"),
		ToolCallID: "eco_regression",
		Name:       db.EcoCheckpointToolName,
	}
}

// TestFilterCheckpointsForRequest_BudgetCapLastElementIsPanicRegression
// reproduces the crash that killed the server whenever "Debug WordPress
// Slowness" hit the eco tail-cap path:
//
//	panic: runtime error: index out of range [N] with length N
//	github.com/azukaar/plurality/src/ai.filterCheckpointsForRequest
//	    server/src/ai/eco.go:89
//
// The budget walker runs backwards from the end and, when the LAST message
// alone tips the budget over maxTailChars, increments start one past the
// end (== len(tail)). That is a valid *slice* start but an invalid index,
// and the subsequent "back up to a user boundary" loop dereferenced
// tail[start] before the fix. This test pins the scenario: tail length is
// not huge (the bug is about position, not size).

func TestCompactConversationForContext_TruncatesOversizedMessage(t *testing.T) {
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("sys")},
		{Role: "user", Content: utils.NewTextContent("hi")},
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("q", 250_000))}, // monster inline blob
		{Role: "assistant", Content: utils.NewTextContent("ok")},
	}
	got := CompactConversationForContext(msgs, 30)
	if len(got) != 4 {
		t.Fatalf("expected all 4 messages preserved (truncated, not dropped), got %d", len(got))
	}
	for _, m := range got {
		if m.Role == "user" && len(m.TextContent()) >= 250_000 {
			t.Fatalf("expected oversized user message truncated, got %d chars", len(m.TextContent()))
		}
		if len(m.TextContent()) > 150_000 {
			t.Fatalf("message still oversized after compaction: %d chars", len(m.TextContent()))
		}
	}
}

// TestResolver_FoldBoundaryOnNeverCompacted: the staged resolver is the only
// fold-boundary authority now (there is no "target tokens" knob). A giant
// never-compacted conversation must fold strictly past the existing checkpoint
// pair, never fold the newest user turn, and never leave a live tail that
// starts on a tool row.
func TestResolver_FoldBoundaryOnNeverCompacted(t *testing.T) {
	mk := func(role string, n int) utils.Message {
		return utils.Message{Role: role, Content: utils.NewTextContent(strings.Repeat("x", n))}
	}
	var msgs []utils.Message
	msgs = append(msgs, checkpointMsg(), toolMsg()) // 0,1 existing checkpoint pair
	// Sized to OVERFLOW the default window: ecoContentBudget("unknown model")
	// is 131072 - 6656 = 124416 tokens, and 30 turns x 30,020 chars is ~300k
	// est tokens (chars/3), so the history genuinely does not fit. A smaller
	// fixture fits the window and correctly folds nothing.
	for turn := 0; turn < 30; turn++ {
		msgs = append(msgs, mk("user", 20))
		for i := 0; i < 30; i++ {
			msgs = append(msgs, mk("assistant", 500), mk("tool", 500))
		}
	}
	newestUser := len(msgs)
	msgs = append(msgs, mk("user", 20))

	_, foldFrom := resolveEcoContextCompaction(msgs, 65536, ecoContentBudget("gpt-test"))
	if foldFrom <= 1 {
		t.Fatalf("expected the fold to advance past the checkpoint pair (idx 1), got %d (len=%d)", foldFrom, len(msgs))
	}
	if foldFrom >= len(msgs) {
		t.Fatalf("fold must leave live messages, got %d (len=%d)", foldFrom, len(msgs))
	}
	if foldFrom > newestUser {
		t.Fatalf("fold %d must never fold the newest user turn (idx %d)", foldFrom, newestUser)
	}
	if msgs[foldFrom].Role == "tool" {
		t.Fatalf("kept tail must not start on a tool row, got idx %d role %q", foldFrom, msgs[foldFrom].Role)
	}
}

// TestResolver_FitsFoldsNothing pins that a conversation that already fits the
// window is returned UNCHANGED with foldFrom == 0: nothing is folded, the
// prefix stays byte-stable and the KV cache stays hot.
func TestResolver_FitsFoldsNothing(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			utils.Message{Role: "user", Content: utils.NewTextContent("short ask")},
			utils.Message{Role: "assistant", Content: utils.NewTextContent("short answer")})
	}
	out, foldFrom := resolveEcoContextCompaction(msgs, 65536, 60000)
	if foldFrom != 0 {
		t.Fatalf("fitting conversation must not be compacted, got foldFrom %d", foldFrom)
	}
	if len(out) != len(msgs) {
		t.Fatalf("fitting conversation must be returned unchanged: %d vs %d", len(out), len(msgs))
	}
}
