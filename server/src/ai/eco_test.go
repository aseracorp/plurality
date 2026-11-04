package ai

import (
	"fmt"
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

func TestFindCharCutoffIndex_AdvancesOnNeverCompacted(t *testing.T) {
	// lastPT <= target previously made findCutoffIndex return -1 forever, so
	// eco ticks logged "no satisfying cutoff ... skip" and never compacted a
	// giant never-compacted conversation. The char fallback must return a
	// real cutoff that advances past the existing checkpoint while always
	// preserving the newest user turn.
	mk := func(roles []string) []utils.Message {
		var msgs []utils.Message
		for _, r := range roles {
			msgs = append(msgs, utils.Message{Role: r, Content: utils.NewTextContent("x")})
		}
		return msgs
	}
	// Shape: checkpoint pair at 0/1, then several OLD turns whose tool walls
	// make the raw history dwarf the target budget, then a small newest user
	// turn at the very end (the realistic "giant never-compacted" shape).
	msgs := mk([]string{"assistant", "tool"}) // tidx 0,1 (existing checkpoint)
	for turn := 0; turn < 8; turn++ {
		msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("old ask %d", turn))})
		for i := 0; i < 30; i++ {
			msgs = append(msgs,
				utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("A", 500))},
				utils.Message{Role: "tool", Content: utils.NewTextContent(strings.Repeat("B", 500)), ToolCallID: fmt.Sprintf("t%d_%d", turn, i)},
			)
		}
	}
	newestUser := len(msgs)
	msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("NEWEST ASK")})

	// lastPT (100) <= target (5000 → 20k chars) while the raw history is
	// ~240k chars — the exact "no satisfying cutoff ... skip" every-tick
	// shape (observed live: raw history ≈1.75M chars vs a ~100k-token
	// target). The char fallback must return a real cutoff.
	cut := findCutoffIndex(msgs, 100, 5000, 1)
	if cut <= 1 {
		t.Fatalf("expected char-fallback cutoff to advance past checkpoint (idx 1), got %d (len=%d)", cut, len(msgs))
	}
	if cut <= 1 || cut >= len(msgs) {
		t.Fatalf("cutoff must leave live messages, got %d (len=%d)", cut, len(msgs))
	}
	if cut > newestUser {
		t.Fatalf("cutoff %d must never fold the newest user turn (idx %d)", cut, newestUser)
	}
	if msgs[cut].Role == "tool" {
		t.Fatalf("kept tail must not start on a tool row, got index %d role %q", cut, msgs[cut].Role)
	}
}
