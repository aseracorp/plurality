package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

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
