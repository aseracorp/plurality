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
		Role: "assistant",
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
func TestFilterCheckpointsForRequest_BudgetCapLastElementIsPanicRegression(t *testing.T) {
	msgs := []utils.Message{
		// One checkpoint pair (assistant + tool half).
		checkpointMsg(),
		toolMsg(),
		// One live user turn that alone exceeds the 100k-char budget.
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}

	// Must not panic. With eco ON the tail after the checkpoint is the
	// single oversized user message: the walker breaks at start==1, then
	// increments to start==2 == len(tail) (one-past-end), clamps down to
	// the last element, stays on the user boundary, and keeps the message.
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != 1 {
		t.Fatalf("expected 1 kept message (the oversized user turn), got %d", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("expected kept role user, got %q", got[0].Role)
	}
}

// TestFilterCheckpointsForRequest_BudgetCapLongTail exercises the normal
// capping path: a long tail where the budget is exceeded somewhere in the
// middle must back up to a user boundary and keep the suffix.
func TestFilterCheckpointsForRequest_BudgetCapLongTail(t *testing.T) {
	var msgs []utils.Message
	msgs = append(msgs, checkpointMsg(), toolMsg())
	// 30 alternating user/assistant turns of ~5k chars each (~150k total),
	// so the cap triggers mid-tail and must resync to a user boundary.
	for i := 0; i < 30; i++ {
		msgs = append(msgs,
			utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("user %d %s", i, strings.Repeat("a", 5_000)))},
			utils.Message{Role: "assistant", Content: utils.NewTextContent(fmt.Sprintf("assistant %d %s", i, strings.Repeat("b", 5_000)))},
		)
	}

	got := filterCheckpointsForRequest(msgs, true)
	if len(got) == 0 {
		t.Fatal("expected a non-empty tail")
	}
	// The kept tail must start exactly on a user boundary so tool results
	// never get orphaned.
	if got[0].Role != "user" {
		t.Fatalf("expected kept tail to start with a user message, got %q", got[0].Role)
	}
	// Total kept chars must respect the budget (plus up to one boundary
	// message of slack).
	total := 0
	for _, m := range got {
		total += len(m.TextContent())
	}
	if total > 100_000+5_100 {
		t.Fatalf("kept tail %d chars exceeds budget", total)
	}
}

// TestFilterCheckpointsForRequest_EcoOff drops checkpoint pairs entirely.
func TestFilterCheckpointsForRequest_EcoOff(t *testing.T) {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("hi")},
		checkpointMsg(),
		toolMsg(),
		{Role: "assistant", Content: utils.NewTextContent("hello")},
	}
	got := filterCheckpointsForRequest(msgs, false)
	for _, m := range got {
		if db.IsCheckpointMessage(m) {
			t.Fatalf("checkpoint message leaked through with eco off: %+v", m)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
}