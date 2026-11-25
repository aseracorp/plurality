package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_windowfold_test.go — the two defects behind the 2026-11-25 catastrophe,
// pinned from production data (conversation 9f0ab2352bf2bdc40e77384b).
//
// The provider's own count, stored on the first assistant row after the fold:
//
//	seq 2437  prompt_tokens 24683  ts 2026-11-25T06:14:54Z
//
// and the fold that produced it:
//
//	06:13:52 [Eco] preemptive context guard: estimated 707728 tokens
//	  (sent view 679333 + schemas 28395, marshalled body 525889) vs window 262144
//	06:13:52 [Eco] stage 2 FAILED, storing the stage-1 megacave (stage 2 body
//	  does not fit the window: head 36 + schemas 28395 + megacaved body 230642
//	  tokens + safety 2048 + output floor 1024 vs window 262144)
//	06:13:52 [LLMLoop] durable fold: 1196 rows folded, history 2439 -> 5
//	  messages (view ~34042 tokens, fold #1)
//
// 24683 prompt_tokens on a 262144-token window, of which ~21521 is the system
// block + tool schemas (cache_read). The model received ~3160 tokens of
// history. 1196 rows of conversation, gone.
//
// DEFECT 1 — the guard measures raw stored content, the wire ships the stored
// caveman projection. The raw measure over-counts the request the guard is
// measuring, so it fires a fold on a request that fits.
//
// DEFECT 2 — the fold's budget is denominated in the eco TARGET (144000), and
// the overflow path halves it again (72000). The roll-up's room is
// (target - live tail) * 3, so with a prior roll-up already at ~105805 chars
// the room goes NEGATIVE, chars collapses to 0, the ceiling collapses to the
// per-row floor, and the stored roll-up is clamped to the 100000-char global
// constant (~33333 tokens) — which is exactly the "34K" the user named
// (34042 logged). The window never enters the arithmetic at all.

// wfToolRows builds n tool rows with a raw body of rawChars and a stored
// caveman projection of projChars, the shape of an agent conversation whose
// tool output is compressed on disk.
func wfToolRows(n, rawChars, projChars int) []utils.Message {
	rows := make([]utils.Message, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, utils.Message{
			Role:              "tool",
			Name:              "system_tools__shell_exec",
			Content:           utils.NewTextContent(strings.Repeat("x", rawChars)),
			CompressedContent: strings.Repeat("y", projChars),
		})
	}
	return rows
}

func TestSentViewTokensShippedCountsTheStoredProjection(t *testing.T) {
	// 40 rows, 30000 raw chars each, 3000-char projection: the wire ships a
	// TENTH of what the raw measure counts.
	msgs := wfToolRows(40, 30_000, 3_000)

	raw := SentViewTokens(msgs)
	if want := 40 * 30_000 / 3; raw != want {
		t.Fatalf("raw measure %d tokens, want %d", raw, want)
	}

	shipped := SentViewTokensShipped(msgs, cavemanStoneSet{readLess: true})
	if want := 40 * 3_000 / 3; shipped != want {
		t.Fatalf("shipped measure %d tokens, want %d (the projection is what ships)", shipped, want)
	}
	if shipped*3 >= raw {
		t.Fatalf("shipped measure %d is not materially under the raw measure %d - the guard "+
			"over-counts the request it is measuring and folds conversations that fit", shipped, raw)
	}

	// NEVER UNDER-COUNT. A model without the read_less stone ships the raw
	// body, so for that model the shipped measure must equal the raw one.
	if got := SentViewTokensShipped(msgs, cavemanStoneSet{}); got != raw {
		t.Fatalf("shipped measure with no stone active: %d, want %d (raw). Under-counting here "+
			"is how a context-busting request ships un-flagged", got, raw)
	}

	// A row with NO stored projection counts its raw body in both modes.
	bare := []utils.Message{{Role: "tool", Name: "system_tools__shell_exec",
		Content: utils.NewTextContent(strings.Repeat("x", 30_000))}}
	if got, want := SentViewTokensShipped(bare, cavemanStoneSet{readLess: true}), 30_000/3; got != want {
		t.Fatalf("row without a projection: %d tokens, want %d", got, want)
	}
}

func TestGuardDoesNotFoldARequestThatFitsOnceCompressionIsCounted(t *testing.T) {
	// The production shape: a raw view far over the window that ships well
	// under it. The raw measure fires the guard (and fired a fold that
	// destroyed the conversation); the shipped measure must not.
	msgs := wfToolRows(68, 30_000, 2_000) // raw ~680000 tok, shipped ~45333 tok
	const window = 262_144

	if err := preemptiveContextGuard("strata-swift", SentViewTokens(msgs), nil, 0, window, len(msgs)); err == nil {
		t.Fatalf("raw measure %d vs window %d: expected the guard to fire - that is the live behaviour",
			SentViewTokens(msgs), window)
	}
	shipped := SentViewTokensShipped(msgs, cavemanStoneSet{readLess: true})
	if err := preemptiveContextGuard("strata-swift", shipped, nil, 0, window, len(msgs)); err != nil {
		t.Fatalf("shipped measure %d vs window %d: guard must NOT fire, the request fits: %v",
			shipped, window, err)
	}
}

// --- DEFECT 2: the fold's budget is the WINDOW, not half the eco target -----

func TestOverflowFoldBudgetFillsTheWindow(t *testing.T) {
	const window = 262_144
	budget := overflowFoldBudgetTokens(window)
	if budget <= 0 {
		t.Fatalf("budget %d for window %d: must be positive", budget, window)
	}
	// The blind cut the user named: 34K. The budget must be a large fraction
	// of the window, not a fifth of it.
	if budget < window*3/4 {
		t.Fatalf("overflow budget %d tokens for a %d window: under 3/4 of the window - this is the "+
			"blind cut, the model is left with a fragment (observed: 24683 prompt_tokens)", budget, window)
	}
	// It must leave room for the reply and the prompt overhead, so it can
	// never exceed the window.
	if budget >= window-guardReplyReserve {
		t.Fatalf("overflow budget %d leaves less than guardReplyReserve of the %d window", budget, window)
	}
	// Unknown window: 0, so the caller keeps the configured fallback.
	if got := overflowFoldBudgetTokens(0); got != 0 {
		t.Fatalf("window 0: budget %d, want 0 (configured fallback)", got)
	}
}

func TestOverflowFoldTailLeavesTheRollUpRealRoom(t *testing.T) {
	// THE COUPLING. The roll-up's room is (budget - live tail) * 3, so a tail
	// at the budget leaves the roll-up NOTHING: the render collapses to its
	// per-row floor and the stored checkpoint loses everything it summarised.
	// The tail share is therefore capped, and the room the roll-up gets must
	// clear the global 100000-char constant - the clamp that produced the 34K.
	const window = 262_144
	budget := overflowFoldBudgetTokens(window)
	tail := overflowFoldTailBudgetTokens(budget)
	if tail >= budget {
		t.Fatalf("tail budget %d is not under the fold budget %d: the roll-up gets no room", tail, budget)
	}
	room := overflowFoldRollupRoomChars(budget, tail)
	if room <= db.MaxDurableSummaryChars {
		t.Fatalf("roll-up room %d chars, at or under the global clamp %d: the clamp binds and the "+
			"stored roll-up is ~%d tokens on a %d-token window (observed: view 34042, provider 24683)",
			room, db.MaxDurableSummaryChars, db.MaxDurableSummaryChars/3, window)
	}
	// Tail + roll-up must fit the budget, in tokens.
	if used := tail + room/3; used > budget {
		t.Fatalf("tail %d + roll-up %d = %d tokens, over the budget %d", tail, room/3, used, budget)
	}
}

// wfCliffConversation reproduces the boundary cliff measured on the live
// conversation: user turns are agent turns, and the sent-view suffix jumps
// from 233063 tokens straight to 53273 across one user boundary.
//
//	live 2026-11-25, suffix tokens at each user boundary:
//	  idx 2677  249602
//	  idx 2690  233063
//	  idx 2793   53273   <- ~100 rows, no user row in between
//	  idx 2818   30797
func wfCliffConversation(t *testing.T) []utils.Message {
	t.Helper()
	const fatRow = 6_000 // chars per row; ~200 of them is a ~400k-token turn
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("sys")},
	}
	// Old, foldable history: many cheap user turns.
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			utils.Message{Role: "user", Content: utils.NewTextContent("ask " + strings.Repeat("a", 60))},
			utils.Message{Role: "assistant", Content: utils.NewTextContent("reply " + strings.Repeat("b", 60))})
	}
	// ONE giant agent turn: a user row, then ~100 fat tool/assistant rows and
	// NO user row until the end. This is the cliff.
	msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("big task")})
	for i := 0; i < 100; i++ {
		call := utils.Message{Role: "assistant", Content: utils.NewTextContent("call")}
		call.ToolCalls = []utils.ToolCall{{
			ID:       "c",
			Type:     "function",
			Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: strings.Repeat("z", fatRow)},
		}}
		msgs = append(msgs, call)
		msgs = append(msgs, utils.Message{Role: "tool", Name: "system_tools__shell_exec",
			Content: utils.NewTextContent(strings.Repeat("t", fatRow))})
	}
	// The newest user turn: the floor.
	msgs = append(msgs,
		utils.Message{Role: "user", Content: utils.NewTextContent("newest ask")},
		utils.Message{Role: "assistant", Content: utils.NewTextContent("newest reply")})
	return msgs
}

func wfNewestUserIdx(msgs []utils.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && !db.IsCheckpointMessage(msgs[i]) {
			return i
		}
	}
	return -1
}

func TestOverflowFoldRefinesInsideATurnWhenUserBoundariesCliff(t *testing.T) {
	msgs := wfCliffConversation(t)
	const window = 262_144
	budget := overflowFoldBudgetTokens(window)

	b := UltracaveOverflowFoldFrom(msgs, 24, window)
	if b <= 0 {
		t.Fatalf("no fold boundary on a conversation over the window")
	}
	tail := SentViewTokens(msgs[b:])
	lu := wfNewestUserIdx(msgs)
	// THE FLOOR IS THE BUG. Landing on the newest user row folds the whole
	// conversation: that is the 1196 rows -> 5 messages of 2026-11-25.
	if b >= lu {
		t.Fatalf("boundary %d is the newest user row (%d): the fold takes the whole conversation, "+
			"the model is left with the newest turn only", b, lu)
	}
	// And the tail must USE the window, not a fraction of it.
	if tail < budget/2 {
		t.Fatalf("boundary %d leaves a tail of %d tokens against a budget of %d: the fold cut to a "+
			"fraction of the window (observed: 24683 prompt_tokens on a %d window)", b, tail, budget, window)
	}
	if tail > budget {
		t.Fatalf("boundary %d leaves a tail of %d tokens, over the budget %d: the retry overflows again",
			b, tail, budget)
	}
	// A window may legally open on an assistant row; it may never open on a
	// tool row (its parent call would be folded, orphaning the result).
	if msgs[b].Role == "tool" {
		t.Fatalf("boundary %d opens the window on a tool row", b)
	}
}

func TestOverflowFoldStillHonoursTheNewestUserTurnFloor(t *testing.T) {
	// The refinement must never fold the user's live ask, at any window.
	msgs := wfCliffConversation(t)
	lu := wfNewestUserIdx(msgs)
	for _, w := range []int{0, 32_000, 64_000, 128_000, 262_144} {
		b := UltracaveOverflowFoldFrom(msgs, 24, w)
		if b <= 0 {
			continue
		}
		if b >= lu {
			t.Fatalf("window %d: boundary %d folds the newest user row %d", w, b, lu)
		}
		if msgs[b].Role == "tool" {
			t.Fatalf("window %d: boundary %d opens on a tool row", w, b)
		}
	}
}
