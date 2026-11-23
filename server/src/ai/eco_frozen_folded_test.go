package ai

import (
	"go/ast"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_frozen_folded_test.go — the two defects that made compaction destroy
// context on the 2026-11-22 incident, pinned on the LIVE row shapes of
// conversation af46ee1c387e35a6e5467822 (559 rows, 552 flagged compacted,
// pair at seq -2/-1), read out of /app/data/server.log:
//
//	21:09:42 [Eco] cutoff=383 (of 552) — folded 383 rows, view 374403 -> 110299
//	21:58:35 PERF msgs=557 tok_before=241327 cache_read=21521
//	21:59:50 [Eco] lastPT=241649 > trigger=216000 — compacting toward target=144000
//	21:59:50 [Eco] cutoff=171 (of 175 messages), prior checkpoint end=1
//	22:00:35 [Eco] view 111080 -> 34125 tokens (view rows 6)
//
// Helpers are prefixed ff_ : mkMsg (eco_filter_test.go), checkpointMsg /
// toolMsg (eco_test.go) and itoa (ultracave_prefix_test.go) already exist in
// this package with different signatures.

// ffTurn: user -> assistant(tool_call) -> tool, the agent-chat row triple.
func ffTurn(n, chars int) []utils.Message {
	callID := "ff_" + itoa(n)
	user := utils.Message{Role: "user", Content: utils.NewTextContent("ff ask " + itoa(n))}
	asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("ff run " + itoa(n))}
	asst.ToolCalls = []utils.ToolCall{{ID: callID, Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
	tool := mkMsg("tool", strings.Repeat("x", chars), "system_tools__shell_exec")
	tool.ToolCallID = callID
	return []utils.Message{user, asst, tool}
}

// ffIncidentView: the slice the 21:58:35 request shipped. The pair sits at
// slice indices 0/1 (seq -2/-1), the 383 rows fold #1 flagged compacted=1 sit
// at 2..384 — i.e. AFTER the pair — and the live rows follow. This is exactly
// the row set db.PushMessage hands RunLLMLoop at the start of a turn:
// getConversationFromDB's DEFAULT set, folded rows included.
func ffIncidentView(foldedTurns, liveTurns, chars int) []utils.Message {
	msgs := []utils.Message{checkpointMsg(), toolMsg()}
	for i := 0; i < foldedTurns; i++ {
		for _, m := range ffTurn(i, chars) {
			m.Compacted = true
			msgs = append(msgs, m)
		}
	}
	for i := 0; i < liveTurns; i++ {
		msgs = append(msgs, ffTurn(10_000+i, chars)...)
	}
	return msgs
}

// TestFrozenPrefixNeverShipsFoldedRows is the load-bearing test.
//
// The frozen branch slices from the pair, and folded rows carry seq 0..N —
// they sit AFTER the pair — so slicing from the pair re-shipped the entire
// folded history. That single defect produced all three symptoms:
//   - the prompt re-inflates to the full stored history, so lastPT (the
//     provider's own count) measures a view the frozen path never intends to
//     ship and the eco trigger fires on a phantom (241649 on a 111080 view);
//   - findCutoffIndex's cumulative-PT arithmetic is then taken against rows
//     whose prompt_tokens came from those fat requests, targetDrop lands below
//     the PT floor of every live row, and the cutoff lands on the newest row —
//     fold everything (cutoff=171 of 175);
//   - the fold rewrites the pair, which is the head of the prompt, so the KV
//     prefix collapses to cache_read=21521 (system block + tool schemas).
func TestFrozenPrefixNeverShipsFoldedRows(t *testing.T) {
	msgs := ffIncidentView(383, 20, 200)
	got := filterCheckpointsForRequest(msgs, true, 262144)

	folded := 0
	for _, m := range got {
		if m.Compacted {
			folded++
		}
	}
	if folded != 0 {
		t.Fatalf("the frozen view ships %d rows durable compaction folded: the pair sits at seq -2/-1 "+
			"and folded rows carry seq 0..N, so slicing from the pair re-ships the whole folded history. "+
			"This is the defect that inflated tok_before to 241327 on a 171-row view (21:58:35).", folded)
	}
	want := 2 + 20*3
	if len(got) != want {
		t.Fatalf("frozen view is %d rows, want %d (the pair + the live rows only)", len(got), want)
	}
	if !db.IsCheckpointMessage(got[0]) || !db.IsCheckpointMessage(got[1]) {
		t.Fatal("the frozen view must OPEN on the checkpoint pair — it is the first content after the " +
			"system block and the anchor of the KV prefix")
	}
}

// TestFrozenPrefixUnchangedForTheInternalView: the filter must be a NO-OP on
// the slice the loop reloads (db.GetConversationByIdInternal, "AND compacted
// = 0"). Byte-stability of the frozen head is the whole point of ultracave; a
// filter that dropped live rows would destroy the prefix it exists to protect.
func TestFrozenPrefixUnchangedForTheInternalView(t *testing.T) {
	full := ffIncidentView(383, 20, 200)
	internal := append([]utils.Message{full[0], full[1]}, full[2+383*3:]...)

	withFolded := filterCheckpointsForRequest(full, true, 262144)
	clean := filterCheckpointsForRequest(internal, true, 262144)
	if len(withFolded) != len(clean) {
		t.Fatalf("dropping folded rows changed the live view: %d rows vs %d for the same live history",
			len(withFolded), len(clean))
	}
	for i := range withFolded {
		a, b := withFolded[i], clean[i]
		if a.Role != b.Role || a.TextContent() != b.TextContent() {
			t.Fatalf("row %d differs: role %q/%q — the folded-row filter must touch nothing but folded rows",
				i, a.Role, b.Role)
		}
	}
}

// TestTickFoldBoundaryPairBranchRefusesAViewThatFits pins the second defect.
//
// The no-pair branch refuses a view that already fits the target. The PAIR
// branch had no such gate: it folded on lastPT alone. Live, 21:59:50, the sent
// view was 111080 tokens and the target 144000 — the view FIT — and
// lastPT=241649 drove targetDrop=97649 below the prompt_tokens floor of every
// live row, so every assistant "crossed", the cutoff landed on the newest row
// and 169 of 175 rows folded: 111080 -> 34125 tokens. A fold that lands under
// the target on a view that already fits is not compaction, it is context
// loss, and it rewrites the prompt head to do it.
func TestTickFoldBoundaryPairBranchRefusesAViewThatFits(t *testing.T) {
	const target = 144_000
	msgs := ffIncidentView(169, 0, 4000)

	// The live tail, with the cumulative prompt_tokens the pre-fold era left
	// behind: rows whose PT was measured on requests that shipped 241K tokens.
	user := utils.Message{Role: "user", Content: utils.NewTextContent("deploy step 99")}
	a1 := utils.Message{Role: "assistant", Content: utils.NewTextContent("wall 1")}
	a1.ToolCalls = []utils.ToolCall{{ID: "ff_live_1", Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
	a1.PromptTokens = 241303
	t1 := mkMsg("tool", strings.Repeat("z", 900), "system_tools__shell_exec")
	t1.ToolCallID = "ff_live_1"
	a2 := utils.Message{Role: "assistant", Content: utils.NewTextContent("wall 2")}
	a2.PromptTokens = 241649
	msgs = append(msgs, user, a1, t1, a2)

	view := SentViewTokens(msgs)
	if view > target {
		t.Fatalf("fixture must model a view that FITS the target: view %d, target %d", view, target)
	}
	lastPT := lastAssistantPromptTokens(msgs)
	if lastPT <= target {
		t.Fatalf("fixture must model lastPT above the target: lastPT %d", lastPT)
	}

	got := tickFoldBoundary(msgs, lastPT, target, 1)
	if got != -1 {
		t.Fatalf("tickFoldBoundary folded %d of %d rows on a %d-token view that already fits the %d target "+
			"(lastPT=%d measures the REQUEST, not the frozen view): this is the 111080 -> 34125 collapse",
			got, len(msgs), view, target, lastPT)
	}
}

// TestFoldSummariserDoesNotInheritTheTickDeadline pins the third defect.
//
// durableFold is handed the tick's dbCtx, whose 45s bound exists to keep the
// SQLite path off a wedged single-connection pool. The summariser is not DB
// work: it is a chat round-trip on a 262k-token prompt, measured at ~3.5 min.
// Inheriting the 45s bound killed it on EVERY fold of the incident, so no
// conversation ever got an LLM checkpoint — each fold fell to the machine-only
// render (21:10:27, 22:00:35, both "context deadline exceeded"), which is the
// cheap lossy roll-up the user is now reading.
func TestFoldSummariserDoesNotInheritTheTickDeadline(t *testing.T) {
	g := installLLMGuard(t)
	fd := g.funcs["durableFold"]
	if fd == nil || fd.Body == nil {
		t.Fatal("durableFold not found")
	}

	var summariserArgs []ast.Expr
	detached := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callsSummariser(ce) && len(ce.Args) > 0 {
			summariserArgs = append(summariserArgs, ce.Args[0])
		}
		if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "summariserCallContext" {
			detached = true
		}
		return true
	})
	if len(summariserArgs) == 0 {
		t.Fatal("durableFold no longer calls the summariser")
	}
	if !detached {
		t.Fatal("durableFold builds no summariserCallContext: the summariser inherits the tick's 45s DB " +
			"budget, which is why every fold died with 'context deadline exceeded' and fell to the " +
			"machine-only render")
	}
	for _, a := range summariserArgs {
		if id, ok := a.(*ast.Ident); ok && id.Name == "ctx" {
			t.Fatal("the summariser is handed the caller's ctx directly: the tick's 45s DB deadline bounds " +
				"a chat completion. Pass the detached summariser context.")
		}
	}
}
