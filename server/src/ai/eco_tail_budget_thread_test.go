package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// TestEcoTailUsesOverheadAwareBudget pins the regression this PR fixes:
// filterCheckpointsForRequestBudget received an overhead-aware tail budget
// (computed by SendChatCompletion from the REAL tool schema + system prompt
// cost) but DROPPED it and re-capped the tail with the STATIC
// maxTailChars(contextWindow) instead — so a request whose true overhead
// consumed most of the window could still ship a tail that made the prompt
// exceed the model's context window (observed: prompt 65933 tokens vs 65536
// window → litellm 500 → dead conversation).
//
// The test deliberately uses a CALLER budget (4000 chars) far below the
// static window budget (16384 chars on a 65k window). With the fix the tail
// is capped to ~3000 chars; without it the static budget keeps ~15000 chars
// and the assertions fail.
func TestEcoTailUsesOverheadAwareBudget(t *testing.T) {
	checkpoint := utils.Message{
		Role: "assistant",
		ToolCalls: []utils.ToolCall{{
			ID: "eco_x", Type: "function",
			Function: utils.FunctionCall{Name: db.EcoCheckpointToolName, Arguments: "{}"},
		}},
	}
	msgs := append([]utils.Message{}, checkpoint)
	msgs = append(msgs, mkMsg("tool", "summary text", db.EcoCheckpointToolName))
	// 10 live tool results of 3000 chars each = 30k chars total.
	for i := 0; i < 10; i++ {
		msgs = append(msgs, mkMsg("user", "continue", ""))
		msgs = append(msgs, mkMsg("tool", strings.Repeat("y", 3_000), "system_tools__shell_exec"))
	}

	const window = 65_536
	const callerBudget = 4_000 // overhead leaves almost no room for the tail
	static := maxTailChars(window)
	if callerBudget >= static {
		t.Fatalf("test setup invalid: caller budget %d must be smaller than static %d", callerBudget, static)
	}

	out := filterCheckpointsForRequestBudget(msgs, true, window, callerBudget)

	total := 0
	maxMsg := 0
	for _, m := range out {
		c := m.Content.TotalContentChars()
		total += c
		if c > maxMsg {
			maxMsg = c
		}
	}
	t.Logf("output: %d msgs, %d total chars, max msg %d (callerBudget=%d static=%d)",
		len(out), total, maxMsg, callerBudget, static)

	if total == 0 {
		t.Fatal("empty tail returned")
	}
	// With the caller budget threaded through, the tail is capped hard:
	// one tool result (3000) + its user turn (~8) survives. Without the fix
	// the static 16384-char budget keeps ~5 tool results (~15000 chars).
	if total > callerBudget+1_000 {
		t.Fatalf("tail %d chars exceeds caller budget %d + boundary turn: caller budget was dropped and static was used",
			total, callerBudget)
	}
	// No single message may exceed the per-message context bound either —
	// truncateOversizedTail must still apply after the budget cap.
	if maxMsg > maxKeepTailChars(window) {
		t.Fatalf("oversized message %d chars not truncated to %d", maxMsg, maxKeepTailChars(window))
	}
}
