package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

func textMsg(role string, n int, compacted bool) utils.Message {
	m := utils.Message{Role: role, Content: utils.NewTextContent(strings.Repeat("x", n))}
	m.Compacted = compacted
	return m
}

// TestSentViewTokensSkipsCompacted pins the eco-trigger contract: the trigger
// must estimate the size of the view that is ACTUALLY sent.
//
// The request path loads conversations with includeCompacted=true, so the raw
// slice carries rows durable compaction already folded into a checkpoint. They
// are not sent, and counting them made the trigger read ~1.25M tokens while the
// sent view was ~23k (observed live: a 54x disagreement on the same request).
// The trigger then fired durable compaction on EVERY turn; each firing inserts
// a checkpoint pair at the kept-window boundary, rewriting the HEAD of the
// history — the pattern that collapses KV prompt-cache reuse to the stable
// system block (reproduced on strata: reuse 32049 -> 17116).
func TestSentViewTokensSkipsCompacted(t *testing.T) {
	msgs := []utils.Message{
		textMsg("user", 3000, true),      // folded: must NOT count
		textMsg("assistant", 3000, true), // folded: must NOT count
		textMsg("user", 900, false),      // live: counts (900/3 = 300)
		textMsg("tool", 600, false),      // live: counts (600/3 = 200)
	}

	got := SentViewTokens(msgs)
	want := (900 + 600) / 3
	if got != want {
		t.Fatalf("SentViewTokens = %d, want %d (compacted rows must be excluded)", got, want)
	}

	// Control: the naive full-slice count is the bug this guards against.
	all := 0
	for i := range msgs {
		all += msgs[i].Content.TotalContentChars() / 3
	}
	if all <= got {
		t.Fatalf("control expects the full-slice count (%d) to exceed the sent view (%d)", all, got)
	}
	t.Logf("full-slice count %d vs sent-view %d (ratio %.1fx) — the 54x live blowup is this bug", all, got, float64(all)/float64(got))
}

// TestSentViewTokensEmptyIsZero guards the no-history case (must not panic and
// must not report a phantom budget).
func TestSentViewTokensEmptyIsZero(t *testing.T) {
	if got := SentViewTokens(nil); got != 0 {
		t.Fatalf("SentViewTokens(nil) = %d, want 0", got)
	}
	if got := SentViewTokens([]utils.Message{}); got != 0 {
		t.Fatalf("SentViewTokens(empty) = %d, want 0", got)
	}
}

// TestSentViewTokensAllCompactedIsZero models a conversation whose every loaded
// row is folded: the sent view is the checkpoint pair only, so the trigger must
// not fire from history volume at all.
func TestSentViewTokensAllCompactedIsZero(t *testing.T) {
	msgs := []utils.Message{
		textMsg("user", 5000, true),
		textMsg("assistant", 5000, true),
		textMsg("tool", 5000, true),
	}
	if got := SentViewTokens(msgs); got != 0 {
		t.Fatalf("SentViewTokens(all compacted) = %d, want 0", got)
	}
}
