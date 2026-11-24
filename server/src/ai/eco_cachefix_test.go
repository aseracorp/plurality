package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"strings"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_cachefix_test.go — the KV-prompt-cache and context-completeness
// invariants the #197-#201 regression destroyed, pinned from production data.
//
// Two measurements frame everything here.
//
// The build the user is running (#196), /app/data/server.log:
//
//	14:35:44 [Eco] capping tail by budget: kept 228 msgs / 117987 chars (start idx 15)
//	  same request, proxy PERF:  tok_before=177562 cache_read=176045
//
// The cap logged 118k CHARS for a request the proxy measured at 177k TOKENS
// (~530k chars). The gap is the assistant rows' tool-call arguments, which the
// char measures ignored entirely. #196 shipped the user's context whole —
// "context is over 100K on #196, this is what I am expecting" — and cached 99 %
// of it, because its unbounded walk-back pinned the window head (start idx 3
// appears 677 times, idx 25 308 times) and a head that never moves is a
// byte-stable prefix.
//
// The build the user rejected (#202, my first attempt):
//
//	15:xx  tok_before=107529 cache_read=16384  ttfb_ms=91649
//
// A 200k-CHAR budget is ~66k TOKENS, against an eco trigger of 150000 PROMPT
// TOKENS on a 262144-token model: the char cap bound first, at a QUARTER of the
// window, compaction never governed, and the model answered from a fragment
// ("sent 27K tokens ... not containing any real history"). Raising a global
// char constant cannot fix that — the budget has to be denominated in the
// model's window.
//
// So the invariant is NOT "a capped window is a byte prefix" (impossible at
// steady state: a window that bounds must eventually drop rows). It is:
//
//	a. under the model-derived budget the cap must NOT run at all, and the sent
//	   view is a byte prefix that grows every turn;
//	b. once the cap binds (the safety-net regime) the head is MONOTONE — it
//	   never re-ships a row it dropped, never opens on a tool row, ships the
//	   newest row, and stays within the bound.
//
// #199-#201 broke (a) by capping far below the trigger and (b) by making the
// head slide every turn: cache_read pinned at 16384-24137 while tok_before grew
// 31949 -> 107780 and ttfb_ms climbed 4475 -> 74034.

// ucRenderWireWindow renders the wire bytes exactly as SendChatCompletion
// would, for a model with the given context window. ucRenderWire passes no
// window, which exercises the fallback floor; this passes the model's window,
// which is the production path (index.go threads windowTokens through).
func ucRenderWireWindow(t *testing.T, msgs []utils.Message, windowTokens int) []byte {
	t.Helper()
	visible := filterCheckpointsForRequest(msgs, true, windowTokens)
	all := append([]utils.Message{ucSystemMsg()}, visible...)
	rendered, _ := convertMessagesToOpenAI(all, utils.Model{Name: ucModelName})
	var out []byte
	for i := range rendered {
		b, err := json.Marshal(rendered[i])
		if err != nil {
			t.Fatalf("marshal rendered message: %v", err)
		}
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, b...)
	}
	return out
}

// TestBootstrapBudgetTracksModelWindow pins that the budget is DENOMINATED IN
// THE MODEL'S WINDOW, not a global constant — the defect behind the user's
// "200k is still low/wrong: 200k chars are below the 150k token trigger".
func TestBootstrapBudgetTracksModelWindow(t *testing.T) {
	prev := 0
	for _, c := range []struct {
		window int
		name   string
	}{
		{262_144, "this deployment (kimi-k3)"},
		{131_072, "a 128k model (deepseek-v4, glm-5, qwen-3)"},
		{32_000, "a small model"},
		{0, "unknown window"},
	} {
		got := bootstrapBudgetChars(c.window)
		toks := got / charsPerToken
		if got < 0 {
			t.Fatalf("%s: budget %d must never be negative", c.name, got)
		}
		if c.window > 0 && c.window <= promptOverheadTokens+guardReplyReserve && got != 0 {
			t.Fatalf("%s: a window of %d cannot fit the %d-token prompt overhead plus the %d-token reply reserve, "+
				"so the honest history budget is 0, got %d — inventing history room is how the cap ships a request "+
				"the provider rejects", c.name, c.window, promptOverheadTokens, guardReplyReserve, got)
		}
		if c.window > promptOverheadTokens+guardReplyReserve && got <= 0 {
			t.Fatalf("%s: budget %d must be positive when the window has room for history", c.name, got)
		}
		if c.window == 0 && got != maxTailChars {
			t.Fatalf("unknown window: budget %d, want the fallback %d", got, maxTailChars)
		}
		if c.window > 0 && c.window < 100_000 && got >= maxTailChars {
			t.Fatalf("%s: a small window must buy a budget BELOW the fallback (%d >= %d) — clamping up to the floor is "+
				"what produced the '100k chars is safe for any 128k model' comment on models where it was not safe",
				c.name, got, maxTailChars)
		}
		if c.window > 0 && got > 0 && toks+promptOverheadTokens+guardReplyReserve > c.window {
			t.Fatalf("%s: history budget %d tokens + %d prompt overhead + %d reply reserve exceeds the window %d — "+
				"the cap would be the thing that busts the context",
				c.name, toks, promptOverheadTokens, guardReplyReserve, c.window)
		}
		// THE RULE: on a real model the history budget must sit ABOVE the eco
		// trigger, so compaction (trigger/target) governs the context and the
		// cap stays the safety net it claims to be. Every global char constant
		// tried (100k, 200k) violated this and became the context policy.
		if c.window == 262_144 && toks <= 150_000 {
			t.Fatalf("%s: history budget is %d tokens, at/below the 150000 eco trigger — the cap binds before compaction "+
				"governs, so the model answers from a fragment the user can see is full", c.name, toks)
		}
		if c.window == 262_144 && prev > 0 && got <= prev {
			t.Fatalf("a larger window must buy a larger budget: %d -> %d", prev, got)
		}
		prev = got
		t.Logf("%-40s window=%7d -> budget %7d chars (~%d tokens)", c.name, c.window, got, toks)
	}
}

// TestToolCallArgumentsAreCountedInMeasures is the unit fix underneath
// everything above. The char measures summed CONTENT ONLY, so an agent turn —
// whose payload IS the assistant's tool-call arguments (file writes, shell
// commands, patch bodies) — cost zero. Live, this conversation, #196:
//
//	14:35:44 [Eco] capping tail by budget: kept 228 msgs / 117987 chars
//	  same request: PERF tok_before=177562 cache_read=176045
//
// A bound derived from a measure that ignores most of the request cannot bound
// anything, and the preemptive guard (which adds the tool SCHEMAS on top of
// this measure) under-estimates, so a context-busting request ships un-flagged.
func TestToolCallArgumentsAreCountedInMeasures(t *testing.T) {
	args := `{"command":"` + strings.Repeat("make -j8 && ./deploy --force", 200) + `"}`
	row := utils.Message{
		Role:    "assistant",
		Content: utils.NewTextContent("deploying"),
		ToolCalls: []utils.ToolCall{{
			ID:       "call_deploy_1",
			Type:     "function",
			Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: args},
		}},
	}
	contentOnly := row.Content.TotalContentChars()
	if len(args) < 3*contentOnly {
		t.Fatalf("fixture: arguments (%d) must dominate the content (%d) — that is what an agent turn looks like",
			len(args), contentOnly)
	}
	if got := shippedRowChars(row); got < contentOnly+len(args) {
		t.Fatalf("shippedRowChars = %d, must count the tool-call arguments (%d chars): the row ships them, the cap must see them",
			got, len(args))
	}
	if got := SentViewRowChars(row); got < contentOnly+len(args) {
		t.Fatalf("SentViewRowChars = %d, must count the tool-call arguments (%d chars): the preemptive guard sizes the request "+
			"from this measure, so undercounting lets a context-busting request ship un-flagged (live: 117987 chars logged, tok_before=177562)",
			got, len(args))
	}
}

// TestBootstrapUnderModelBudgetShipsEverything is the regime this deployment
// actually runs in and the one the user measured. The model is 262144 tokens;
// bootstrapBudgetChars derives ~696k chars from it, and the eco trigger fires
// long before the cap binds — so on a normal conversation the cap NEVER RUNS,
// the whole history ships, and the sent view is a byte PREFIX that GROWS every
// turn. That is #196's behaviour (tok_before=177562, cache_read=176045, 99 %
// warm) and it is what every char-capped "fix" destroyed.
func TestBootstrapUnderModelBudgetShipsEverything(t *testing.T) {
	const window = 262_144
	budget := bootstrapBudgetChars(window)

	hist := ucHistory("ship the cache fix", 70, 3_000) // ~210k chars
	liveChars := 0
	for _, m := range hist {
		liveChars += shippedRowChars(m)
	}
	if liveChars <= 2*maxTailChars {
		t.Fatalf("fixture is %d chars; must exceed the historical 200k budget to prove the cap no longer runs", liveChars)
	}
	if liveChars >= budget {
		t.Fatalf("fixture is %d chars, must stay under the model budget %d", liveChars, budget)
	}
	for _, m := range hist {
		if db.IsCheckpointMessage(m) {
			t.Fatal("fixture must not carry a checkpoint pair — this is the bootstrap (capTail) path")
		}
	}

	prev := ucRenderWireWindow(t, hist, window)
	if len(prev) == 0 {
		t.Fatal("empty render")
	}
	for turn := 0; turn < 8; turn++ {
		hist = append(hist, ucTurn(600+turn, "system_tools__shell_exec",
			"output "+strings.Repeat("k", 1_000))...)
		next := ucRenderWireWindow(t, hist, window)
		if len(next) <= len(prev) {
			t.Fatalf("turn %d: sent view did not grow (%d -> %d) — under the model budget the cap must not drop anything",
				turn, len(prev), len(next))
		}
		if !bytes.HasPrefix(next, prev) {
			t.Fatalf("turn %d: the sent view is not a byte prefix of the next request (divergence at byte %d of %d) — "+
				"the prefix cache dies at the head and the whole prompt re-prefills",
				turn, firstDivergence(prev, next), len(next))
		}
		prev = next
	}
}

// TestBootstrapHeadIsMonotoneAboveTheBudget pins the safety-net regime: a
// never-compacted conversation that has outgrown the model budget. A binding
// cap must eventually drop rows — what makes it cache-safe, and what the
// #199-#201 walk-back destroyed, is the SHAPE of the drop:
//
//   - the head is MONOTONE: never re-ships a row it dropped. Re-shipping
//     rewrites the prefix behind the head, which is the churn that pinned
//     cache_read at 16384-24137 (live: start idx 655 -> 702 -> 701 -> 702);
//   - the head never opens on a tool result (hard provider error);
//   - the newest row ALWAYS ships — dropping the newest work answers nothing;
//   - the shipped total stays within capTailWalkBackBound, so the window can
//     never force the fold churn (production shipped 178176 / 340693 chars on a
//     100000 budget);
//   - the head advances only by what the append costs (a 2-row append moves it
//     at most a few rows), so the prefix rewrites a SLIVER per turn, not the
//     whole window — the #199-#201 walk-back rewrote it every turn.
//
// Uses a 131072 window so the cap binds on a small fixture.
func TestBootstrapHeadIsMonotoneAboveTheBudget(t *testing.T) {
	const window = 131_072
	budget := bootstrapBudgetChars(window)

	var hist []utils.Message
	for i := 0; i < 140; i++ {
		hist = append(hist, ucTurn(i, "system_tools__shell_exec", "out "+strings.Repeat("m", 2_500))...)
	}
	if shipped := shippedChars(hist); shipped <= budget {
		t.Fatalf("fixture is %d chars, must exceed the %d budget to exercise the cap", shipped, budget)
	}

	bound := budget + maxKeepTailChars + truncationBannerChars
	prevStart := -1
	drops := 0
	growth := 0
	const turnChars = 2_500 + 200 // the appended turn's shipped size, with slack
	for turn := 0; turn < 20; turn++ {
		out := capTail(hist, budget)
		if len(out) == 0 {
			t.Fatalf("turn %d: capTail shipped an empty window", turn)
		}
		if out[0].Role == "tool" {
			t.Fatalf("turn %d: window opens on an orphaned tool result", turn)
		}
		got := shippedChars(out)
		if got > bound {
			t.Fatalf("turn %d: shipped %d chars on a %d budget (bound %d) — production shipped 178176/100000 this way; "+
				"every over-budget window forces a fold and rewrites the prompt head", turn, got, budget, bound)
		}
		tail := hist[len(hist)-1]
		last := out[len(out)-1]
		if last.ToolCallID != tail.ToolCallID || last.Role != tail.Role {
			t.Fatalf("turn %d: newest row evicted (want role %q id %q, got role %q id %q) — dropping the newest work answers nothing",
				turn, tail.Role, tail.ToolCallID, last.Role, last.ToolCallID)
		}

		start := len(hist) - len(out)
		if prevStart >= 0 {
			if start < prevStart {
				t.Fatalf("turn %d: the window head moved BACKWARD (%d -> %d) — rows already dropped were re-shipped, which "+
					"rewrites the prefix behind the head (live: start idx 701 -> 702 -> 701, cache_read pinned at 16384)",
					turn, prevStart, start)
			}
			if start > prevStart {
				drops++
			}
		}
		prevStart = start

		hist = append(hist, ucTurn(140+turn, "system_tools__shell_exec", "out "+strings.Repeat("m", 2_500))...)
		growth += turnChars
	}
	if drops == 0 {
		t.Fatal("the cap never dropped rows on a fixture past the budget — the cap is not bounding anything")
	}
	// The head must be PINNED for most turns and move at most once per chunk
	// of growth. 20 turns of ~2.5k appends is ~50k of growth on a 50k chunk:
	// one cut, not twenty.
	maxMoves := growth/tailCutChunkChars() + 1
	if drops > maxMoves {
		t.Fatalf("head moved %d times for %d chars of growth on a %d-char chunk: expected at most %d. "+
			"The cut is rolling, not chunked.", drops, growth, tailCutChunkChars(), maxMoves)
	}
	if pinned := 20 - drops; pinned < drops {
		t.Fatalf("the head moved on %d/20 turns and stayed put on %d — a rolling tail moves more than it pins", drops, pinned)
	}
	t.Logf("head pinned %d/20, cut %d times for %d chars of growth (chunk %d), bound %d chars",
		20-drops, drops, growth, tailCutChunkChars(), bound)
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
func TestCapTailShipsWithinTheBoundOnLiveShapes(t *testing.T) {
	var msgs []utils.Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs, bootTurn(i, 3_000)...)
	}
	msgs = append(msgs, bootGiant(90, 300_000)...) // the 890KB tool-result shape
	for i := 91; i < 100; i++ {
		msgs = append(msgs, bootTurn(i, 4_000)...)
	}
	for i := 0; i < 12; i++ { // the tool wall: live 10:23:51 / 10:39:56 shape
		msgs = append(msgs, wallTurn2(200+i, 30_000)...)
	}

	out := capTail(msgs)
	if len(out) == 0 {
		t.Fatal("capTail shipped an empty window")
	}
	kept := shippedChars(out)
	bound := capTailWalkBackBound()
	if kept > bound {
		t.Fatalf("capTail shipped %d chars on a %d budget (%d rows) — production shipped 178176/100000 this way",
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
// budget BEFORE the summariser started, and production proved the cost — 45s
// to the second, on a 2 MB fold:
//
//	20:14:34 BOOTSTRAP compaction: sent view 679865 tokens over tail budget
//	20:15:19 machine-only FALLBACK fold (LLM summariser unavailable: ... context deadline exceeded): 700 rows
//	20:15:19 [ERROR] durable fold failed : context deadline exceeded
//
// The fold MINTS the checkpoint pair. With every fold lost, no conversation
// ever reached the frozen path and capTail slid the window — and the prompt
// head — every single turn.
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
	if len(writeCtxPos) < 2 {
		t.Fatalf("durableWriteContext is created %d time(s) in durableFold: the LLM and the machine-only "+
			"fallback branches each need their own post-summariser write context", len(writeCtxPos))
	}
}

// tokenPos makes the AST position comparison legible in failure output.
type tokenPos int

// TestDurableFoldSurvivesExpiredCallerContext: the fold must land even when
// the caller's context is already dead, because the durable WRITE is the
// artifact and losing it is what leaves a conversation churning on the sliding
// window.
func TestDurableFoldSurvivesExpiredCallerContext(t *testing.T) {
	ctx, _, convID := ucDB(t)

	hist := ucHistory("survive the dead caller", 14, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)

	dead, cancelDead := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelDead()
	<-dead.Done()

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

// TestEcoWindowPolicyIsDenominatedInTheWindow pins the compaction policy that
// caused the "it sent 27K tokens, not containing any real history" complaint
// on the build WITH THE WORKING CACHE — the second defect, independent of the
// cache fix, and the one that only appeared once folds started SUCCEEDING.
//
// The operator's configured pair is trigger=150000 / target=50000, GLOBAL, and
// this box runs a 262144-token model. Compaction then fires at 57 % of the
// window and folds the live view toward 50000, so a 147K-token conversation is
// reduced to a ~27K history. That is what the user saw:
//
//	15:23  tok_before=107529  (107529 - ~80000 prompt+schemas ≈ 27K of history)
//
// The #196 build never bit because its folds all died (durable fold failed :
// context deadline exceeded), no checkpoint pair ever existed, and capTail
// shipped 177562 tokens raw. Fixing the fold deadline made compaction real;
// a real policy aimed at 50K on a 260K window bites.
func TestEcoWindowPolicyIsDenominatedInTheWindow(t *testing.T) {
	const cfgTrigger, cfgTarget = 150_000, 50_000

	// The user's model: compaction must not fire at 57 % of the window, and a
	// fold must not keep less history than the window can hold.
	trigger, target := ecoWindowPolicy(cfgTrigger, cfgTarget, 262_144)
	if trigger <= cfgTrigger {
		t.Fatalf("on a 262144-token window the trigger must be RAISED above the configured %d (got %d) — "+
			"firing at 57 %% of the window is what folded the conversation to a fragment", cfgTrigger, trigger)
	}
	if trigger > 262_144-guardReplyReserve {
		t.Fatalf("trigger %d exceeds the window headroom %d — compaction could never keep the request inside the window",
			trigger, 262_144-guardReplyReserve)
	}
	if target <= cfgTarget {
		t.Fatalf("on a 262144-token window the fold target must be RAISED above the configured %d (got %d) — "+
			"folding toward 50000 discards ~100K tokens of context the window has room for", cfgTarget, target)
	}
	if target >= trigger {
		t.Fatalf("target %d >= trigger %d: the fold has nothing to remove and every fold is refused, "+
			"so the conversation churns the checkpoint pair every turn (cache_read pinned at 16384)", target, trigger)
	}
	// A 147K-token conversation (the live measurement) must NOT be folded on a
	// 262144-token window: the window has room, so history is kept whole.
	if 147_689 > trigger {
		t.Fatalf("a 147689-token conversation folds on a 262144-token window (trigger %d) — "+
			"that is the context loss the user reported", trigger)
	}

	// A 128k model: the configured 150000 sits ABOVE the window and can never
	// fire, so the request stays inside the window only by luck, rescued by the
	// post-400 overflow path (the context_length_exceeded spiral of the
	// 1866-message conversation, 1830 -> 1855 live rows, every turn).
	trigger, target = ecoWindowPolicy(cfgTrigger, cfgTarget, 131_072)
	if trigger >= 131_072 {
		t.Fatalf("on a 131072-token window the trigger must be LOWERED below the window (got %d) — "+
			"a trigger that can never fire leaves compaction to the overflow path", trigger)
	}
	if target <= 0 || target >= trigger {
		t.Fatalf("131072 window: target %d must sit below the trigger %d", target, trigger)
	}

	// A small window buys a small target: the window sizes the policy, and a
	// configured target ABOVE the window is clamped, not obeyed.
	trigger, target = ecoWindowPolicy(cfgTrigger, cfgTarget, 32_000)
	if target >= 32_000 {
		t.Fatalf("32000 window: target %d exceeds the window — a configured 50000 must not be obeyed verbatim", target)
	}

	// Unknown window: no policy invented, the configured pair is used verbatim.
	trigger, target = ecoWindowPolicy(cfgTrigger, cfgTarget, 0)
	if trigger != cfgTrigger || target != cfgTarget {
		t.Fatalf("unknown window must return the configured pair verbatim, got (%d, %d)", trigger, target)
	}
	t.Logf("policy 262144 -> trigger/target from the window; 131072, 32000 clamped; 0 = configured")
}

// TestEcoConversationWindowMatchesTheRequestPath pins the model resolution
// behind the compaction policy. The window sizes the (trigger, target) pair;
// an unresolved window yields 0, and 0 makes ecoWindowPolicy return the
// CONFIGURED pair verbatim — trigger 150000, target 50000. On this
// 262144-token deployment that is the 27K-token context loss: the tick folds a
// 148K conversation toward 50000.
//
// The pre-fix reader used conv.ModelSelected.Text directly. The REQUEST path
// (index.go) uses SelectModel, which also resolves through Vision — so a
// conversation with only Vision pinned (or any selection shape SelectModel
// handles and the direct read does not) compacts under a fallback policy the
// request it is protecting knows nothing about. Policy and request must
// resolve the model the same way, by construction.
func TestEcoConversationWindowMatchesTheRequestPath(t *testing.T) {
	// Nothing selected: 0 is correct, and the policy falls back to the
	// configured pair — the documented contract.
	if got := ecoConversationWindow(&utils.Conversation{}); got != 0 {
		t.Fatalf("empty selection: window %d, want 0 (configured fallback)", got)
	}

	// Vision-only selection: SelectModel returns the Vision model, so the
	// window must resolve — the direct Text read returned 0 and silently
	// armed the 150000/50000 fold.
	visionName := "kimi-k3"
	convVision := &utils.Conversation{ModelSelected: utils.ModelSelected{
		Vision: &utils.Model{Name: visionName},
	}}
	want := Models.MaxContextTokensForPrefixed(visionName)
	if got := ecoConversationWindow(convVision); got != want {
		t.Fatalf("vision-only selection: window %d, want %d (SelectModel resolves Vision; a 0 here arms the "+
			"configured 150000/50000 pair — the 27K-token fold on a 262144-token model)", got, want)
	}

	// Text set: same resolution as the request path (Text wins when no
	// images are present).
	convText := &utils.Conversation{ModelSelected: utils.ModelSelected{
		Text:   &utils.Model{Name: visionName},
		Vision: &utils.Model{Name: "a-vision-model"},
	}}
	if got := ecoConversationWindow(convText); got != want {
		t.Fatalf("text+vision selection: window %d, want %d", got, want)
	}

	// Nil conversation: 0, never a panic — durableCompact runs on a ticker
	// and a panic there kills the compaction subsystem.
	if got := ecoConversationWindow(nil); got != 0 {
		t.Fatalf("nil conversation: window %d, want 0", got)
	}
}

// TestBootstrapGateIgnoresTheFallbackFloorOnABigWindow: the self-locking gate
// compared the sent view against the GLOBAL 100000-char fallback (~33333
// tokens), a FIFTH of the trigger on this 262144-token model. A ~90k-token
// view passed the gate, folded toward the 50000 target, and the model answered
// from ~27k tokens of history. With the model-derived budget (696000 chars) the
// same view must stay quiet: folding early is not compacting safely, it is
// losing context the window had room for.
func TestBootstrapGateIgnoresTheFallbackFloorOnABigWindow(t *testing.T) {
	const window = 262_144
	budget := bootstrapBudgetChars(window)

	// Live #202 shape: no pair, lastPT ~90k (below the configured 150000
	// trigger), sent view ~90k tokens / ~270k chars.
	sentView := 270_000 / 3
	if !bootstrapFoldNeeded(-1, 90_000, 150_000, sentView, maxTailChars) {
		t.Fatal("the pre-fix gate must fold this view — that is the defect being pinned")
	}
	if bootstrapFoldNeeded(-1, 90_000, 150_000, sentView, budget) {
		t.Fatalf("on a %d-token window a %d-token view must NOT fold (model budget %d chars): folding here "+
			"throws away context the window can hold, which is the 27K-token request the user reported",
			window, sentView, budget)
	}
	// Past the model budget the gate must still fire: the cap is a safety net,
	// and a view over it is a context-busting request (the 1866-message shape).
	if !bootstrapFoldNeeded(-1, 90_000, 150_000, (budget+3000)/3, budget) {
		t.Fatal("a view over the model budget must fold durably (mints the pair, hands off to the frozen path)")
	}
}
