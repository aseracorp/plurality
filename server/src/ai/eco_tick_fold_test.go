package ai

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_tick_fold_test.go — the NO-CHECKPOINT-PAIR path, i.e. the path EVERY
// NEW CONVERSATION runs on. Live evidence, conv a44a43258742259a60be242d
// (sub-agent, 68 rows seq 0..67, sum(compacted)=0, NO pair, max
// prompt_tokens 52709), /app/data/server.log 2026-11-21:
//
//	10:51:02 [Eco] conv a44a43258742259a60be242d BOOTSTRAP compaction: sent view 46331
//	               tokens over tail budget (100000 chars) with no checkpoint pair,
//	               lastPT=52709 below trigger=150000 — compacting toward target=50000
//	10:51:02 [Eco] conv a44a43258742259a60be242d cutoff=67 (of 68 messages), prior checkpoint end=-1
//	10:51:47 [Eco] conv a44a43258742259a60be242d durable fold removed 0 rows (cutoff=67 of 68)
//	12:35:19 [Eco] capping tail by budget: kept 15 msgs / 221162 chars (budget 100000, start idx 530)
//
// Three defects pinned here:
//   - capTail's tool/assistant-wall fallback anchor was unbounded: 221162
//     chars shipped on a 100000-char budget (TestCapTailToolWallBoundedByBudget);
//   - the tick fold's boundary came from findCutoffIndex's prompt_tokens
//     arithmetic even with NO pair, so targetDrop=2709 picked cutoff=67 of 68
//     — fold EVERYTHING older than the newest turn — on a view (46331) that
//     already fit the target (50000) (TestTickFoldBoundaryNoPair*);
//   - the fold must obey the hysteresis floor, or it rewrites the prompt
//     head every turn for ~2 rows (cache_read=24137 FLAT across 8 turns
//     while tok_before grew 31949 -> 107780, ttfb_ms 4475 -> 74034)
//     (TestTickFoldBoundaryHysteresisRefusesTinyFold).

// captureStdlibLog redirects the standard logger (utils.Log writes through
// log.Println) into a buffer for the duration of the test.
func captureStdlibLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return &buf
}

// wallTurn2: an assistant(tool_call)+tool row pair with NO user row — the
// mid-turn shape of an agent chat.
func wallTurn2(n, chars int) []utils.Message {
	callID := "wall2_" + itoa(n)
	asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("wall " + itoa(n))}
	asst.ToolCalls = []utils.ToolCall{{ID: callID, Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
	tool := mkMsg("tool", strings.Repeat("z", chars), "system_tools__shell_exec")
	tool.ToolCallID = callID
	return []utils.Message{asst, tool}
}

// TestCapTailToolWallBoundedByBudget pins the fix for the 12:35:19 line: on a
// tail ENDING in a tool/assistant wall (the normal shape mid-turn, so the
// common anchor branch, not the rare one) the fallback anchor must never push
// the window over maxTailChars.
//
// RED on 83aa917: the pre-fix fallback `for start > 0 && tail[start].Role !=
// "user" { start-- }` has no bound — this fixture ships ~209k chars on a
// 100000 budget (production shipped 221162).
// GREEN after: the bounded walk-back stops the moment the next chain row would
// bust the budget, and the head snap keeps the window off an orphan tool row.
func TestCapTailToolWallBoundedByBudget(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 30; i++ {
		msgs = append(msgs, bootTurn(i, 2_000)...)
	}
	msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("deploy " + itoa(99))})
	for i := 0; i < 20; i++ {
		msgs = append(msgs, wallTurn2(i, 11_000)...)
	}

	out := capTail(msgs)
	if len(out) == 0 {
		t.Fatal("capTail shipped an empty window")
	}
	if out[0].Role == "tool" {
		t.Fatal("window opens on an orphaned tool result — its parent call was dropped")
	}
	kept := shippedChars(out)
	// These rows are all under maxKeepTailChars, so NOTHING is truncated:
	// BUDGET = WHAT SHIPS exactly, no maxKeepTailChars slack in this shape.
	if kept > maxTailChars+truncationBannerSlack {
		t.Fatalf("tool-wall cap shipped %d chars on a %d budget — the fallback anchor is unbounded "+
			"(live: kept 15 msgs / 221162 chars, budget 100000, conv a44a43258742259a60be242d, 2026-11-21 12:35:19)",
			kept, maxTailChars)
	}
	if last := out[len(out)-1]; last.Role != "tool" || last.ToolCallID != "wall2_19" {
		t.Fatalf("newest wall row evicted (last kept role %q) — capTail that drops the newest work answers nothing", last.Role)
	}
	t.Logf("tool wall bounded: kept %d/%d rows, %d chars (budget %d)", len(out), len(msgs), kept, maxTailChars)
}

// liveSubagentShape builds the exact storage shape of the live conversation:
// the ONLY user row is seq 0 (the sub-agent task), every later row is
// assistant/tool, the newest assistant carries maxPT as prompt_tokens.
func liveSubagentShape(rows, chars, maxPT int) []utils.Message {
	msgs := []utils.Message{{Role: "user", Content: utils.NewTextContent("sub-agent task")}}
	assts := 0
	for i := 1; i < rows; i++ {
		if i%2 == 0 {
			m := mkMsg("tool", strings.Repeat("x", chars), "system_tools__shell_exec")
			msgs = append(msgs, m)
			continue
		}
		m := mkMsg("assistant", strings.Repeat("a", chars), "")
		assts++
		m.PromptTokens = maxPT * assts / 32
		msgs = append(msgs, m)
	}
	if len(msgs) > 1 {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "assistant" {
				msgs[i].PromptTokens = maxPT
				break
			}
		}
	}
	return msgs
}

// TestTickFoldBoundarySubagentShapeRefuses: the live 68-row shape (user row
// ONLY at 0). The newest-user-row FLOOR leaves no legal live window, so the
// boundary is refused LOUDLY. Pre-fix this shape walked into findCutoffIndex,
// produced cutoff=67 of 68, and the fold died silently at the storage layer.
func TestTickFoldBoundarySubagentShapeRefuses(t *testing.T) {
	msgs := liveSubagentShape(68, 2_100, 52_709)
	if SentViewTokens(msgs)*3 <= maxTailChars {
		t.Fatalf("fixture must be over the tail budget: %d tokens", SentViewTokens(msgs))
	}
	if got := tickFoldBoundary(msgs, 52_709, 50_000, -1); got != -1 {
		t.Fatalf("sub-agent shape (only user row at 0) gave boundary %d, want -1 — the newest user row is the FLOOR", got)
	}
}

// TestEcoTickRefusesSubagentBootstrapFold: the whole tick, end to end, on the
// live shape. The tick must REFUSE with a loud named reason and mint NO pair —
// no write, so the pair (and the prompt head) stays byte-identical.
//
// RED on 83aa917: the tick logs "cutoff=67 (of 68 messages)" then "durable
// fold removed 0 rows" with NO reason — "tick fold refused" is absent.
func TestEcoTickRefusesSubagentBootstrapFold(t *testing.T) {
	ctx, d, convID := ucDB(t)
	if _, err := d.Exec(`UPDATE conversations SET model_selected = ? WHERE id = ?`,
		`{"eco_mode":true}`, convID); err != nil {
		t.Fatalf("set eco mode: %v", err)
	}
	ucPush(t, ctx, convID, liveSubagentShape(68, 2_100, 52_709)...)

	buf := captureStdlibLog(t)
	runEcoSummary(ctx, convID)
	logged := buf.String()

	if !strings.Contains(logged, "tick fold refused") {
		t.Fatalf("runEcoSummary must refuse the fold with a named, loud reason; log tail:\n%s",
			logged[max(0, len(logged)-2_000):])
	}
	for _, m := range ucLLMView(t, ctx, convID) {
		if db.IsCheckpointMessage(m) {
			t.Fatal("a refused fold minted a checkpoint pair — a refusal must write NOTHING")
		}
	}
}

// TestTickFoldBoundaryNoPairRefusesFittingView: the NO-PAIR RULE in its
// refusing direction. lastPT=12000 > target 10000 makes findCutoffIndex's
// arithmetic (targetDrop=2000) happily return a late cutoff — that is exactly
// how a 46331-token view with lastPT=52709 got cutoff=67 of 68 on a brand-new
// conversation. When the SENT VIEW (the measure the frozen path ships) already
// fits the target, the answer is -1: no fold, pair byte-identical.
func TestTickFoldBoundaryNoPairRefusesFittingView(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 12; i++ {
		tr := bootTurn(i, 2_000)
		tr[1].PromptTokens = 1_000 * (i + 1)
		msgs = append(msgs, tr...)
	}
	target := 10_000
	view := SentViewTokens(msgs)
	if view > target {
		t.Fatalf("fixture must FIT the target: view %d tokens > %d", view, target)
	}
	lastPT := 12_000
	if cut := findCutoffIndex(msgs, lastPT, target, -1); cut <= 0 {
		t.Fatalf("fixture must be a shape the PT arithmetic cuts (got %d) — that is what the no-pair rule exists to ignore", cut)
	}
	if got := tickFoldBoundary(msgs, lastPT, target, -1); got != -1 {
		t.Fatalf("no-pair view %d fitting target %d: boundary %d, want -1 — folding here churns the prompt head "+
			"(live: view 46331 < target 50000, cutoff=67 of 68, conv a44a43258742259a60be242d 10:51:02)",
			view, target, got)
	}
}

// TestTickFoldBoundaryNoPairUsesBootstrapCutoff: the NO-PAIR RULE in its
// folding direction — the boundary comes ONLY from bootstrapCutoff (the char
// measure the view ships), and the PT arithmetic's late cutoff (fold
// everything older than the newest turn) is NOT used.
func TestTickFoldBoundaryNoPairUsesBootstrapCutoff(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 60; i++ {
		tr := bootTurn(i, 2_000)
		tr[1].PromptTokens = 500 * (i + 1)
		msgs = append(msgs, tr...)
	}
	target := 10_000
	view := SentViewTokens(msgs)
	if view <= target {
		t.Fatalf("fixture must be over target: view %d tokens", view)
	}
	lastPT := 30_000
	ptCutoff := findCutoffIndex(msgs, lastPT, target, -1)
	if ptCutoff < 0 {
		t.Fatal("fixture must be a shape the PT arithmetic cuts")
	}
	want := bootstrapCutoff(msgs, target)
	if want <= 0 {
		t.Fatal("bootstrapCutoff must give a boundary on an over-target view")
	}
	got := tickFoldBoundary(msgs, lastPT, target, -1)
	if got != want {
		t.Fatalf("no-pair boundary = %d, want bootstrapCutoff %d (findCutoffIndex gives %d)", got, want, ptCutoff)
	}
	if got >= ptCutoff {
		t.Fatalf("boundary %d is not older-side than the PT cutoff %d — the fold must not fold-everything-toward-target", got, ptCutoff)
	}
	if msgs[got].Role != "user" {
		t.Fatalf("boundary %d lands on %q, must land on a user row", got, msgs[got].Role)
	}
	if bought := ultracaveFoldProgressTokens(msgs, got); bought < ultracaveMinFoldProgressTokens() {
		t.Fatalf("accepted boundary %d buys %d tokens, under the hysteresis floor %d",
			got, bought, ultracaveMinFoldProgressTokens())
	}
	t.Logf("no-pair boundary: bootstrap %d (PT arithmetic wanted %d), buys %d tokens (floor %d)",
		got, ptCutoff, ultracaveFoldProgressTokens(msgs, got), ultracaveMinFoldProgressTokens())
}

// TestTickFoldBoundaryHysteresisRefusesTinyFold: gate 4. A view barely over
// the target whose boundary buys a token-trickle (the 2026-11-20 c363b2b3
// churn: 20 folds x ~2 rows, live rows 1828 -> 1855 UP) is REFUSED. No write,
// pair byte-identical, cache intact.
func TestTickFoldBoundaryHysteresisRefusesTinyFold(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 100; i++ {
		msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent(strings.Repeat("u", 10))})
		for k := 0; k < 4; k++ {
			msgs = append(msgs, mkMsg("assistant", strings.Repeat("a", 300), ""))
		}
	}
	view := SentViewTokens(msgs)
	target := view - 100 // the churn regime: over target by a hair
	if view <= target {
		t.Fatalf("fixture must be over target: view %d", view)
	}
	got := tickFoldBoundary(msgs, 0, target, -1)
	if got != -1 {
		t.Fatalf("boundary %d buys %d tokens, under the floor %d — hysteresis must refuse it (view %d, target %d)",
			got, ultracaveFoldProgressTokens(msgs, got), ultracaveMinFoldProgressTokens(), view, target)
	}
}
