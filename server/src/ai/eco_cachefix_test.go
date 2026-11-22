package ai

import (
	"bytes"
	"context"
	"go/ast"
	"strings"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_cachefix_test.go — the KV-prompt-cache invariants the #197-#201
// regression destroyed, pinned from the production traces.
//
// Deployed build (master @ bed2276, #201), /app/data/server.log:
//
//	10:23:51 capping tail by budget: kept 18 msgs / 158005 chars (budget 100000)
//	10:39:56 capping tail by budget: kept 33 msgs / 178176 chars (budget 100000)
//	20:12:43 capping tail by budget: kept 45 msgs / 340693 chars (budget 100000)
//	20:14:34 BOOTSTRAP compaction: sent view 679865 tokens over tail budget (100000 chars)
//	20:15:19 machine-only FALLBACK fold (LLM summariser unavailable: summary request
//	           failed: ...: context deadline exceeded): 700 rows, folded=2038862 chars
//	20:15:19 [ERROR] durable fold failed : context deadline exceeded
//
// Proxy PERF for the same window: cache_read FLAT at ~20-24k while tok_before
// grew 31949 -> 107780, ttfb_ms 4475 -> 74034. The reverted build (#196):
// cache_read 46610 of tok_before 50209, 100% of requests cached.
//
// Two mechanisms, pinned here:
//
//  1. the bootstrap tail window must be BUDGET = WHAT SHIPS, and its head must
//     stay byte-stable across turns so the prompt prefix GROWS (the cache
//     grows) and only advances in a quantum, not every turn;
//  2. the durable fold's DB write must get its OWN deadline, created after
//     the summariser — a lost fold is what leaves a conversation sliding the
//     window (and the prompt head) forever.

// TestCapTailBootstrapWindowStaysABytePrefix is the cache invariant for a
// conversation with NO checkpoint pair — i.e. every new conversation: the
// sent wire bytes of turn N must be a byte prefix of turn N+1, so the
// provider's prefix cache grows instead of dying at the head.
//
// RED on a window that fills the budget exactly: the head moves on every
// append (the window grows by a turn, so a row drops off the front), the
// first byte of the sent history changes every turn, cache_read pins at the
// system block and the whole window re-prefills. Measured live: cache_read
// 73289 FLAT across 20 turns, ttfb 4475 -> 74034 ms.
//
// GREEN after: capTailWindow anchors the head on the OLDEST user row whose
// shipped suffix fits maxTailChars, so appending rows keeps the head where it
// is (the suffix from it still fits) and the sent view grows at the tail.
func TestCapTailBootstrapWindowStaysABytePrefix(t *testing.T) {
	// Past the budget on purpose: the cap path must be live, and it must stay
	// byte-stable WHILE it is live. ~90 turns x ~3k chars ≈ 270k chars.
	hist := ucHistory("ship the cache fix", 90, 3_000)
	liveChars := 0
	for _, m := range hist {
		liveChars += m.Content.TotalContentChars()
	}
	if liveChars <= maxTailChars {
		t.Fatalf("fixture is %d chars, must exceed maxTailChars (%d) to exercise the cap path",
			liveChars, maxTailChars)
	}
	for _, m := range hist {
		if db.IsCheckpointMessage(m) {
			t.Fatal("fixture must not carry a checkpoint pair — this is the bootstrap (capTail) path")
		}
	}

	prev, _ := ucRenderWire(t, hist)
	if len(prev) == 0 {
		t.Fatal("empty render")
	}
	const turns = 10
	for turn := 0; turn < turns; turn++ {
		hist = append(hist, ucTurn(500+turn, "system_tools__shell_exec",
			"output "+strings.Repeat("k", 1_000))...)
		next, _ := ucRenderWire(t, hist)
		if len(next) <= len(prev) {
			t.Fatalf("turn %d: sent view did not grow (%d -> %d)", turn, len(prev), len(next))
		}
		if !bytes.HasPrefix(next, prev) {
			t.Fatalf("turn %d: the bootstrap window's head moved (old %d bytes, new %d bytes, first divergence at %d) — "+
				"the prompt prefix is destroyed every turn, cache_read pins at the system block and the whole window re-prefills "+
				"(live: cache_read 73289 FLAT over 20 turns, ttfb 4475 -> 74034 ms)",
				turn, len(prev), len(next), firstDivergence(prev, next))
		}
		prev = next
	}
}

// TestCapTailShipsWithinTheBoundOnLiveShapes replays the live log shapes: fat
// rows (at/over the per-message cap) mixed with normal rows, ending in the
// tool/assistant wall that is the NORMAL mid-turn shape of an agent chat.
//
// RED on #199/#200/#201: the walk-back's budget base SKIPPED rows at the
// per-message cap (`if c >= maxCompactMessageChars { addedFat++; continue }`),
// so the bound was a lie and the cap shipped 158005, 178176 and 340693 chars
// against a 100000 budget. Every over-budget window fires the preemptive
// guard, forces a fold, and rewrites the prompt head.
//
// GREEN after: every row is counted at shippedRowChars in every branch; the
// window is within capTailWalkBackBound (the budget plus at most ONE bounded
// row), never opens on an orphan tool result, and keeps the newest row.
func TestCapTailShipsWithinTheBoundOnLiveShapes(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs, bootTurn(i, 3_000)...)
	}
	// A fat dump mid-conversation (the 890KB tool-result shape that evicted
	// 98 rows in one turn).
	msgs = append(msgs, bootGiant(90, 300_000)...)
	for i := 91; i < 100; i++ {
		msgs = append(msgs, bootTurn(i, 4_000)...)
	}
	// The tool/assistant wall: the live 10:23:51 / 10:39:56 shape.
	for i := 0; i < 12; i++ {
		msgs = append(msgs, wallTurn2(200+i, 30_000)...)
	}

	out := capTail(msgs)
	if len(out) == 0 {
		t.Fatal("capTail shipped an empty window")
	}
	kept := shippedChars(out)
	bound := capTailWalkBackBound()
	if kept > bound {
		t.Fatalf("capTail shipped %d chars on a %d budget (%d rows) — production shipped 178176/100000 this way; "+
			"every over-budget window forces a fold and rewrites the prompt head",
			kept, maxTailChars, len(out))
	}
	if out[0].Role == "tool" {
		t.Fatal("window opens on an orphaned tool result")
	}
	last := out[len(out)-1]
	if last.Role != "tool" || last.ToolCallID != "wall2_211" {
		t.Fatalf("newest wall row evicted (role %q, id %q) — dropping the newest work answers nothing",
			last.Role, last.ToolCallID)
	}
	t.Logf("live shape: kept %d/%d rows, %d chars (budget %d, bound %d)",
		len(out), len(msgs), kept, maxTailChars, bound)
}

// TestDurableWriteContextIsCreatedAfterTheSummariser pins the fold-deadline
// fix as a structural contract (the style of ultracave_llm_guard_test.go):
// the durable write's context must be created AFTER the summariser call, in
// every branch, so the LLM's latency can never consume the write's budget.
//
// RED pre-fix: the write ctx was derived from the eco goroutine's 45s DB
// budget BEFORE the summariser started, and production proved the cost —
// 45s to the second, on a 2 MB fold:
//
//	20:14:34 BOOTSTRAP compaction: sent view 679865 tokens over tail budget
//	20:15:19 machine-only FALLBACK fold (LLM summariser unavailable: ... context deadline exceeded): 700 rows
//	20:15:19 [ERROR] durable fold failed : context deadline exceeded
//
// The fold MINTS the checkpoint pair. With every fold lost, no conversation
// ever reached the frozen path and capTail slid the window — and the prompt
// head — every single turn (start idx 655 -> 702 in one afternoon).
func TestDurableWriteContextIsCreatedAfterTheSummariser(t *testing.T) {
	g := installLLMGuard(t)
	fd := g.funcs["durableFold"]
	if fd == nil || fd.Body == nil {
		t.Fatal("durableFold not found")
	}

	var summariserPos, writeCtxPos []tokenPos
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callsSummariser(ce) {
			summariserPos = append(summariserPos, tokenPos(ce.Pos()))
		}
		if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "durableWriteContext" {
			writeCtxPos = append(writeCtxPos, tokenPos(ce.Pos()))
		}
		return true
	})
	if len(summariserPos) == 0 {
		t.Fatal("durableFold no longer calls the summariser")
	}
	if len(writeCtxPos) == 0 {
		t.Fatal("durableFold has no durableWriteContext call: the write ctx is inline, " +
			"which is how the summariser came to consume the write's deadline " +
			"(live: durable fold failed : context deadline exceeded, 2026-11-21 20:15:19)")
	}
	firstWrite := writeCtxPos[0]
	for _, p := range summariserPos {
		if firstWrite < p {
			t.Fatalf("the durable write context is created BEFORE the summariser call (write ctx at %d, summariser at %d): "+
				"a slow LLM consumes the write's budget and the fold is lost — the root cause of the cache regression",
				firstWrite, p)
		}
	}
	// Both branches (LLM success, machine-only fallback) need their own write
	// ctx created after the summariser; ONE shared early ctx is the bug.
	if len(writeCtxPos) < 2 {
		t.Fatalf("durableWriteContext is created %d time(s) in durableFold: the LLM and the machine-only "+
			"fallback branches each need their own post-summariser write context", len(writeCtxPos))
	}
}

// tokenPos makes the AST position comparison legible in failure output.
type tokenPos int

// TestDurableFoldSurvivesExpiredCallerContext: the fold must land even when
// the caller's context is already dead, because the durable WRITE is the
// artifact and losing it is what leaves a conversation churning on the
// sliding window.
func TestDurableFoldSurvivesExpiredCallerContext(t *testing.T) {
	ctx, _, convID := ucDB(t)

	hist := ucHistory("survive the dead caller", 14, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)

	dead, cancelDead := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelDead()
	<-dead.Done()

	// No chat model in the payload: the summariser fails fast and the
	// machine-only render is what stores.
	folded, err := durableFold(dead, convID, view, 8, "", utils.Model{}, ChatPayload{})
	if err != nil {
		t.Fatalf("the fold died with the caller's context: %v — a lost fold leaves the conversation on the "+
			"sliding window (live: durable fold failed : context deadline exceeded)", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows")
	}
	if stored := ucSummaryText(t, ctx, convID); stored == "" {
		t.Fatal("no roll-up stored")
	}
}
