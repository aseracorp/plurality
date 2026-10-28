package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The tool-schema tax regression: a wide MCP toolset (github-api + playwright
// + wordpress + searxng + bundled) costs ~25-30k tokens of schemas per
// request. On a 65k-window model the old window/2 char budget blew the window
// (observed: prompt 65933 tokens vs 65536 → litellm 500 → dead conversation).
// The overhead-aware budget must shrink the tail so the sum (tools + system +
// tail + reply) fits the window.
func TestMaxTailCharsForOverheadSmallWindow(t *testing.T) {
	window := 65536
	// Realistic overhead: ~28k tokens of tool schemas + ~1.5k system prompt.
	overhead := 28000 + 1500
	budget := maxTailCharsForOverhead(window, overhead)
	// Available tokens: 65536 - 29500 - 6144 (reply reserve) ≈ 29892 tokens.
	// At 3 chars/token ≈ 89676 chars; capped at 100k → ~89k chars.
	if budget <= 0 {
		t.Fatalf("budget must be positive, got %d", budget)
	}
	// The tail budget in chars / 3 must NOT exceed (window - overhead - replyReserve).
	maxTokensFromTail := budget / 3
	if maxTokensFromTail > window-overhead-4096-2048 {
		t.Fatalf("tail budget %d chars ≈ %d tokens leaves no room for tools+reply (window %d, overhead %d)",
			budget, maxTokensFromTail, window, overhead)
	}
}

func TestMaxTailCharsForOverheadHandlesHugeOverhead(t *testing.T) {
	// If the tools alone eat the whole window, the floor kicks in so we
	// never ship an empty tail and never panic.
	window := 32768
	budget := maxTailCharsForOverhead(window, 1000000)
	if budget <= 0 {
		t.Fatalf("budget must be positive even with impossible overhead, got %d", budget)
	}
}

func TestMaxTailCharsForOverheadClampsAt100k(t *testing.T) {
	// Huge window + tiny overhead → the 100k cap prevents unbounded tails.
	budget := maxTailCharsForOverhead(1048576, 1000)
	if budget > 100_000 {
		t.Fatalf("budget %d should be clamped to 100k", budget)
	}
}

// capTailBudget must honor an explicit budget (the overhead-aware one) even
// when the plain maxTailChars(contextWindow) would allow more — this is the
// exact mechanism that prevents the 65k-window blow-up.
func TestCapTailBudgetHonorsExplicitBudget(t *testing.T) {
	// 60 messages of 1000 chars = 60k chars. Budget 30k chars.
	var msgs []utils.Message
	for i := 0; i < 60; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, utils.Message{Role: role, Content: utils.NewTextContent(strings.Repeat("a", 1000))})
	}
	out := capTailBudget(msgs, 30_000, 65536)
	total := 0
	for _, m := range out {
		total += len(m.TextContent())
	}
	if total > 33_000 { // allow the last-user-boundary overshoot
		t.Fatalf("capTailBudget exceeded explicit budget: %d chars (30k budget)", total)
	}
	if len(out) == len(msgs) {
		t.Fatalf("capTailBudget did not truncate anything")
	}
	// The tail must start on a user message (turn boundary preserved).
	if out[0].Role != "user" {
		t.Fatalf("tail must start on a user message, got role %q", out[0].Role)
	}
}

// The old window/2 heuristic over-shipped for 65k models; the new
// maxTailChars(65536) is quartered so the tool tax has room.
func TestMaxTailCharsQuarteredForSmallWindow(t *testing.T) {
	small := maxTailChars(65536)
	if small > 65536/4+1 {
		t.Fatalf("maxTailChars(65536) = %d, expected ≤ %d", small, 65536/4)
	}
	big := maxTailChars(131072)
	if big <= small {
		t.Fatalf("maxTailChars(131072) = %d should exceed small-window budget %d", big, small)
	}
}
