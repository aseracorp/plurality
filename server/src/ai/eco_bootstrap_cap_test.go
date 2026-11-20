package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// eco_bootstrap_cap_test.go — the bootstrap capTail path: the sliding window
// a conversation runs on BEFORE its first durable fold mints a checkpoint
// pair. Production evidence (this deployment, 2026-11-20, conversation
// c363b2b3, verbatim from /app/data/server.log):
//
//	12:03:46 capping tail by budget: kept 101 msgs / 100402 chars (start idx 1509)
//	12:04:05 capping tail by budget: kept 6 msgs / 359315 chars (start idx 1607)
//	12:31:09 capping tail by budget: kept 11 msgs / 981406 chars (start idx 1628)
//	12:32:57 capping tail by budget: kept 13 msgs / 1189684 chars (start idx 1628)
//	   ...    [Eco] conv c363b2b3 lastPT=85121 below trigger=150000 — skip
//
// maxTailChars is 100_000. Three defects, pinned one test each:
//
//  1. The budget walker summed PRE-truncation row sizes, so a 100k budget
//     shipped 1.19M chars (the log line itself is the evidence).
//  2. The user-boundary anchor walked BACKWARDS, re-adding rows the walker
//     had just excluded — one 890KB tool result evicted 98 rows of context
//     in a single turn (idx 1509 -> 1607). That is the "context loss": the
//     sliding window, not the fold.
//  3. The trigger is self-locking: capTail caps the prompt at ~33k tokens,
//     so lastPT can never reach trigger=150000, so the fold that mints the
//     pair never runs, so the window runs forever. A conversation with NO
//     pair whose sent view is over the tail budget must fold now.
//     End-to-end through the real fold: checkpoint_bootstrap_test.go in the
//     db package, which owns the frozen-compaction DB harness.

// bootTurn: user -> assistant(tool_call) -> tool(result), a turn that fits
// the budget whole.
func bootTurn(n int, toolChars int) []utils.Message {
	callID := "call_" + itoa(n)
	user := utils.Message{Role: "user", Content: utils.NewTextContent("turn " + itoa(n))}
	asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("run " + itoa(n))}
	asst.ToolCalls = []utils.ToolCall{{ID: callID, Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
	tool := mkMsg("tool", strings.Repeat("r", toolChars), "system_tools__shell_exec")
	tool.ToolCallID = callID
	return []utils.Message{user, asst, tool}
}

// bootGiant: a tool result over the per-message bound, with its parent call
// (so keptCalls keeps it, and truncateOversizedTail truncates it) — the
// 890KB docker-list shape that triggered the 98-row eviction.
func bootGiant(n int, chars int) []utils.Message {
	callID := "giant_" + itoa(n)
	user := utils.Message{Role: "user", Content: utils.NewTextContent("giant dump " + itoa(n))}
	asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("run dump " + itoa(n))}
	asst.ToolCalls = []utils.ToolCall{{ID: callID, Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
	tool := mkMsg("tool", strings.Repeat("D", chars), "system_tools__shell_exec")
	tool.ToolCallID = callID
	return []utils.Message{user, asst, tool}
}

func shippedChars(msgs []utils.Message) int {
	total := 0
	for _, m := range msgs {
		total += m.Content.TotalContentChars()
	}
	return total
}

// TestCapTailWindowIsSatisfiable: what capTail ships must fit the budget it
// logs. Red on master: 30x3k turns (90k wire chars) plus one 300k-row dump —
// master keeps ~47 rows and ships ~220k chars on a 100k budget; the fix ships
// at most 100k whole + one bounded row.
func TestCapTailWindowIsSatisfiable(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 30; i++ {
		msgs = append(msgs, bootTurn(i, 3_000)...)
	}
	msgs = append(msgs, bootGiant(99, 300_000)...)

	out := capTail(msgs)
	if len(out) == 0 {
		t.Fatal("capTail shipped an empty window")
	}
	kept := shippedChars(out)
	bound := maxTailChars + maxKeepTailChars + truncationBannerSlack
	if kept > bound {
		t.Fatalf("capTail shipped %d chars on a %d budget (%d rows) — budget unsatisfiable, the log lies",
			kept, maxTailChars, len(out))
	}
	// The window must open on a user row: no orphaned tool result at the head.
	if out[0].Role == "tool" {
		t.Fatal("window opens on a tool result — its parent call was dropped")
	}
	// The newest turn must survive: capTail that drops the newest work answers
	// nothing.
	if out[len(out)-1].Role != "tool" {
		t.Fatalf("newest row evicted (last kept: %q)", out[len(out)-1].Role)
	}
	t.Logf("capTail shipped %d rows / %d chars (budget %d)", len(out), kept, maxTailChars)
}

// TestCapTailOneFatRowDoesNotEvictHistory: one 300k dump mid-conversation
// must not evict the whole conversation. Master evicted98 rows in one turn
// (idx1509 →1607) exactly this way: the walker broke at the fat row, the
// backwards user-anchor dragged98 rows back in, and the next turn's window
// dropped them all. Post-bound accounting keeps newest normal rows; the
// evicted set is bounded by the fat row's own size.
func TestCapTailOneFatRowDoesNotEvictHistory(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 25; i++ {
		msgs = append(msgs, bootTurn(i, 2_000)...)
	}
	msgs = append(msgs, bootGiant(50, 300_000)...)
	for i := 51; i < 60; i++ {
		msgs = append(msgs, bootTurn(i, 2_000)...)
	}

	out := capTail(msgs)
	if len(out) < 3 {
		t.Fatalf("capTail evicted to %d rows — one fat row must not evict everything", len(out))
	}
	// The newest normal turns survive.
	if out[len(out)-1].Role != "tool" {
		t.Fatalf("last kept row is %q, expected the newest tool result", out[len(out)-1].Role)
	}
	if out[len(out)-1].ToolCallID != "call_59" {
		t.Fatalf("newest tool result evicted (kept %q)", out[len(out)-1].ToolCallID)
	}
	kept := shippedChars(out)
	bound := maxTailChars + maxKeepTailChars + truncationBannerSlack
	if kept > bound {
		t.Fatalf("window shipped %d chars, over the satisfiable bound", kept)
	}
	if out[0].Role == "tool" {
		t.Fatal("window opens on an orphaned tool result")
	}
	t.Logf("kept %d/%d rows, %d chars", len(out), len(msgs), kept)
}

// truncationBannerSlack bounds the banner truncateOversizedTail appends to a
// truncated row: the text part is clamped to maxKeepTailChars, so a shipped
// row can exceed it by the banner only.
const truncationBannerSlack = 2_000

// TestCapTailEndsInToolWall: a conversation that ends mid-tool-chain (the
// newest user row sits before the walker's start) must keep the newest rows,
// open on a user row, and stay bounded. The forward-anchor search finds no
// user row at/after the walker's start, so the bounded backwards fallback
// runs: newest user row, not98 rows of drag.
func TestCapTailEndsInToolWall(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 40; i++ {
		msgs = append(msgs, bootTurn(i, 2_000)...)
	}
	// The newest user row: a big ask, then a wall of fat tool rows.
	msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("deploy step " + itoa(99))})
	for i := 0; i < 12; i++ {
		callID := "wall_" + itoa(i)
		asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("wall " + itoa(i))}
		asst.ToolCalls = []utils.ToolCall{{ID: callID, Type: "function",
			Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
		tool := mkMsg("tool", strings.Repeat("w", 11_000), "system_tools__shell_exec")
		tool.ToolCallID = callID
		msgs = append(msgs, asst, tool)
	}

	out := capTail(msgs)
	if len(out) == 0 {
		t.Fatal("empty window")
	}
	if out[0].Role == "tool" {
		t.Fatal("window opens on an orphaned tool result")
	}
	if out[len(out)-1].Role != "tool" {
		t.Fatalf("wall evicted entirely (last kept: %q)", out[len(out)-1].Role)
	}
	kept := shippedChars(out)
	bound := maxTailChars + maxKeepTailChars + truncationBannerSlack
	if kept > bound {
		t.Fatalf("tool-wall window shipped %d chars, over the satisfiable bound", kept)
	}
	t.Logf("tool wall: kept %d/%d rows, %d chars", len(out), len(msgs), kept)
}

// TestBootstrapFoldNeededGate: the self-locking trigger, pinned. A
// conversation with no pair, sent view over the tail budget, and lastPT
// below the trigger MUST fold. Master's gate skipped it forever.
func TestBootstrapFoldNeededGate(t *testing.T) {
	// Production numbers: no pair, lastPT=85121, trigger=150000, sent view
	// ~396k tokens (1.19M chars / 3).
	if !bootstrapFoldNeeded(-1, 85_121, 150_000, 396_561) {
		t.Fatal("bootstrap case must fold: no pair + over-budget sent view + self-locked trigger")
	}
	// Normal trigger crossing folds, pair or not.
	if !bootstrapFoldNeeded(7, 200_000, 150_000, 20_000) {
		t.Fatal("lastPT above trigger must fold")
	}
	// Pair exists: the frozen path owns the view; the trigger alone gates.
	if bootstrapFoldNeeded(7, 85_121, 150_000, 396_561) {
		t.Fatal("a frozen conversation below trigger must NOT fold (the pair path bounds the view)")
	}
	// No pair, sent view under budget: nothing to compact, stay quiet.
	if bootstrapFoldNeeded(-1, 10_000, 150_000, 20_000) {
		t.Fatal("a small conversation must not fold")
	}
}

// TestBootstrapCutoffFoldsToTarget: when lastPT is BELOW the eco target
// (fat rows are truncated, so the provider sees a small prompt while the
// stored view is huge), findCutoffIndex's PT arithmetic returns -1 — the
// self-lock survives in that shape. The bootstrap cutoff is char-based: fold
// the oldest rows until the REMAINING sent view fits the target.
func TestBootstrapCutoffFoldsToTarget(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 25; i++ {
		msgs = append(msgs, bootTurn(i, 2_000)...)
	}
	msgs = append(msgs, bootGiant(50, 300_000)...)
	for i := 51; i < 70; i++ {
		msgs = append(msgs, bootTurn(i, 2_000)...)
	}
	view := SentViewTokens(msgs)
	if view <= 5_000 {
		t.Fatalf("fixture must exceed target: view %d tokens", view)
	}

	cutoff := bootstrapCutoff(msgs, 5_000)
	if cutoff <= 0 {
		t.Fatalf("bootstrapCutoff: %d, want > 0 (view %d tokens)", cutoff, view)
	}
	if cutoff >= len(msgs) {
		t.Fatalf("bootstrapCutoff %d empties the live window", cutoff)
	}
	if msgs[cutoff].Role != "user" {
		t.Fatalf("cutoff %d lands on %q, must land on a user row", cutoff, msgs[cutoff].Role)
	}
	remaining := SentViewTokens(msgs[cutoff:])
	if remaining > 5_000 {
		t.Fatalf("cutoff %d leaves %d tokens, above target 5000 — fold cannot converge", cutoff, remaining)
	}
	// Nothing foldable: a slice under target must return -1 (no fold).
	small := bootTurn(0, 100)
	if bootstrapCutoff(small, 5_000) != -1 {
		t.Fatal("a conversation under target must not fold (cutoff -1)")
	}
	t.Logf("bootstrapCutoff: %d of %d rows folded, view %d -> %d tokens", cutoff, len(msgs), view, remaining)
}

// TestBootstrapCutoffFloorsAtNewestUserTurn: the newest turn ALONE exceeds
// the target (a single >300k-char tool result). No cutoff can land the view
// under target: the cutoff must be the newest user row (fold everything
// older — strictly better than the pre-fix "ship the over-target window
// forever"), never -1, never a window start that empties the live tail.
func TestBootstrapCutoffFloorsAtNewestUserTurn(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, bootTurn(i, 1_000)...)
	}
	// Newest turn alone: 300k chars = 100k tokens, target 5_000.
	msgs = append(msgs, bootGiant(99, 300_000)...)

	cutoff := bootstrapCutoff(msgs, 5_000)
	if cutoff <= 0 {
		t.Fatalf("cutoff %d: over-target newest turn must still fold the older rows", cutoff)
	}
	if msgs[cutoff].Role != "user" {
		t.Fatalf("cutoff %d lands on %q, must land on a user row", cutoff, msgs[cutoff].Role)
	}
	if cutoff != len(msgs)-3 {
		t.Fatalf("cutoff %d, want the newest user row %d (fold everything older)", cutoff, len(msgs)-3)
	}
	if SentViewTokens(msgs[cutoff:]) <= 5_000 {
		t.Fatal("fixture must be over target (it is the point of the test)")
	}
}
