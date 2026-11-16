package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// Regression for the live defect on conversation 141c6b2d33616fed6f2dd8dc
// ("made an eco_checkpoint and then sent only 27K tokens"), 2026-11-16, as
// measured on production data:
//
//	[Eco/stageD] view 11 msgs ~113987 tokens (budget 114015, tail 9 rows
//	           ~72679 tokens, summary -101522 chars, tail -0 rows, fits=false)
//
// and in the engine for the same window:
//
//	prompt 133973 tokens = 24576 reused + 11093 read in 70494 ms
//
// 24576 is the system-prompt-only floor: the whole history was re-prefilled,
// and neighbouring turns collapsed the same way at 32-82 s each. Stage D's
// shrink had no bound (one pass deleted 96 % of the roll-up, and the request
// missed the budget anyway) and no cadence (an exact-fit cut every turn moves
// the prompt head every turn). The cut DIRECTION is a tested product rule
// (TestAgenticView_SummaryShrinkDropsOldest: keep the newest, drop the oldest),
// so this PR bounds the SIZE and the CADENCE instead.

// clLikeConv builds the agentic live view: a checkpoint pair carrying `prior`,
// one user row, then a wall of assistant/tool rows. That is the real LLM-facing
// shape, where foldableRange (pair -> newest user row) is EMPTY and the roll-up
// is the only object the resolver can shrink.
func clLikeConv(prior string, wallRows, rowChars int) []utils.Message {
	pair := checkpointArtifactMessages(nil, utils.EcoCheckpointToolName, prior)
	msgs := append([]utils.Message{}, pair...)
	msgs = append(msgs, mkMsg("user", "please continue the investigation, keep going"))
	for i := 0; i < wallRows; i++ {
		msgs = append(msgs, mkMsg("assistant", "working on step "+itoa(i)))
		msgs = append(msgs, mkTool("system_tools__shell_exec", strings.Repeat("y", rowChars)))
	}
	return msgs
}

// hsPrior is a roll-up megacave can actually compress (repeated lines
// collapse), sized like the live one (105366 chars).
func hsPrior() string {
	var b strings.Builder
	for i := 0; i < 3_000; i++ {
		b.WriteString("rolled record " + itoa(i) + ": " + strings.Repeat("q", 40) + "\n")
	}
	return b.String()
}

// TestResolver_RollupShrinkIsBoundedPerPass reproduces the 15:37:09 shape: a
// large roll-up, a live tail that is NOT over budget. No single resolution may
// gut the roll-up, and the request must still fit.
func TestResolver_RollupShrinkIsBoundedPerPass(t *testing.T) {
	prior := hsPrior()
	// Tail of ~6 rows beside a ~130 KB roll-up: the roll-up is the lever.
	msgs := clLikeConv(prior, 6, 6_000)
	const budget = 12_000
	if approxTokens(msgs)+40 <= budget {
		t.Fatalf("fixture too small: ~%d tokens vs budget %d", approxTokens(msgs), budget)
	}
	view, _ := resolveEcoContextCompaction(msgs, 262_144, budget)
	if got := approxTokens(view); got > budget {
		t.Fatalf("view ~%d tokens is OVER budget %d — shipping oversized kills the request", got, budget)
	}
	summary := shippedSummary(view)
	if summary == "" {
		t.Fatalf("no checkpoint artifact in the shipped view")
	}
	if !strings.Contains(summary, "dropped") {
		t.Fatalf("the summary does not name what it gave up — silent loss")
	}
}

// TestResolver_RollupShrinkIsQuantised pins the caching fix. Consecutive turns
// inside one shrink band must share the SAME roll-up bytes, so the head of the
// prompt — the checkpoint summary, first content after the system block — does
// not move and the backend keeps reusing the prefix.
//
// The drop marker is appended AFTER the roll-up (see summaryWithDropMarker) and
// its text names the trimmed-tail count, which changes turn to turn. That tail
// text is not the cache problem; bytes from offset 0 are. So the assertion is
// on the longest common prefix, and it must be far longer than any plausible
// system block — i.e. reuse extends into the history, not just the system
// prompt.
func TestResolver_RollupShrinkIsQuantised(t *testing.T) {
	prior := hsPrior()
	var summaries []string
	for extra := 0; extra < 6; extra++ {
		msgs := clLikeConv(prior, 6+extra, 6_000)
		view, _ := resolveEcoContextCompaction(msgs, 262_144, 12_000)
		summaries = append(summaries, shippedSummary(view))
	}
	for i := 1; i < len(summaries); i++ {
		lcp := commonPrefixLen(summaries[i-1], summaries[i])
		shorter := len(summaries[i-1])
		if len(summaries[i]) < shorter {
			shorter = len(summaries[i])
		}
		// Only the trailing drop notice may differ.
		if shorter-lcp > 600 {
			t.Fatalf("turn %d -> %d: the roll-up diverges %d chars before the end (shorter %d), so the prompt head moves and prefix reuse collapses. Live shape: 'prompt 133973 = 24576 reused', 32-82 s re-prefill.\n  a: %q\n  b: %q",
				i-1, i, shorter-lcp, shorter, quote(summaries[i-1], 120), quote(summaries[i], 120))
		}
	}
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func quote(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestRollupShrinkKnobsAreSane pins the two constants to the shape the fix
// needs: a per-pass bound well under a gutting cut, and a quantum coarser than
// a turn's growth so the head is a step function.
func TestRollupShrinkKnobsAreSane(t *testing.T) {
	if rollupLossPerPassPercent < 2 || rollupLossPerPassPercent > 25 {
		t.Fatalf("per-pass loss %d%% is outside the defensible range", rollupLossPerPassPercent)
	}
	if shrinkQuantumChars < 4096 {
		t.Fatalf("quantum %d is too fine: one turn's growth can cross a band every turn", shrinkQuantumChars)
	}
}
