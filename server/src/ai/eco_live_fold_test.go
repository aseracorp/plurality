package ai

import (
	"strconv"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// The live annihilation, conversation 9f0ab2352bf2bdc40e77384b, 2026-11-25
// 14:13:09, on the #215 build (container recreated 14:11:47):
//
//	[Eco] preemptive context guard: estimated 333811 tokens (sent view 305416
//	      + schemas 28395, marshalled body 133479) vs window 262144, 116 live rows
//	[Eco] stage 2 FAILED, storing the stage-1 megacave (...): 3136 rows,
//	      room=545051 chars, folded=7052851 chars, budget=545051 chars,
//	      megacaved=1088968 chars, per-row show=102
//	[LLMLoop] durable fold: 112 rows folded, history 3138 -> 4 messages
//	      (view ~34283 tokens, fold #1)
//	14:14:06 PERF ... cache_read=0
//	14:17:22 [PrefixWatch] CACHE BREAK at byte 6001 (region: HISTORY)
//
// Two numbers in that block cannot describe one fold:
//
//   - the WIRE carries 305416 tokens = 916248 chars (the guard's own measure,
//     116 live rows);
//   - the fold measured 7052851 chars and budgeted 3136 rows.
//
// The gap is 7.7x, and it is rows an earlier fold marked compacted: the
// LLM-facing view loads with "AND compacted = 0" (db.getConversationFromDB) and
// db.CompactConversationDurable skips them when it builds its render set, so
// those rows are not on the wire and are not rendered. The ai layer summed
// messages[:foldFrom] — the RAW prefix — for the boundary, the excerpt ceiling
// and the hysteresis floor. All three numbers were fictions, and "112 rows
// folded, history 3138 -> 4" is the arithmetic consequence.
//
// These tests build that slice shape and pin the three measures to the wire.

const (
	livePairChars      = 105_805 // the stored roll-up, live seq -1
	liveCompactedRows  = 3_020   // rows an earlier fold marked compacted
	liveCompactedChars = 2_000   // per row: 3020 x 2000 = 6,040,000 chars
	liveTailRows       = 40      // live rows after the newest live user row
	liveTailChars      = 20_000  // per row: 40 x 20000 = 800,000 chars
	liveUserChars      = 34      // the newest live user row
)

// liveShapeFixture is the live slice: the checkpoint pair at the head, a wall
// of already-compacted rows, then the newest live user row and its tool chain.
//
// The compacted wall is the fixture's point: ~6M chars of history the wire
// stopped shipping turns ago, sitting BEFORE the newest live user row — exactly
// the live shape (the newest live user rows are seq 3135/3138; every user row
// older than them carries compacted = 1).
func liveShapeFixture(t *testing.T) []utils.Message {
	t.Helper()
	msgs := []utils.Message{
		{
			Role:      "assistant",
			ID:        "-2",
			ToolCalls: []utils.ToolCall{{ID: "eco_x", Type: "function", Function: utils.FunctionCall{Name: db.EcoCheckpointToolName, Arguments: "{}"}}},
		},
		{
			Role:       "tool",
			ID:         "-1",
			Name:       db.EcoCheckpointToolName,
			ToolCallID: "eco_x",
			Content:    utils.NewTextContent(strings.Repeat("p", livePairChars)),
		},
	}
	for i := 0; i < liveCompactedRows; i++ {
		msgs = append(msgs, utils.Message{
			Role:      "tool",
			Name:      "system_tools__shell_exec",
			ID:        strconv.Itoa(i + 1),
			Compacted: true,
			Content:   utils.NewTextContent(strings.Repeat("x", liveCompactedChars)),
		})
	}
	msgs = append(msgs, utils.Message{
		Role:    "user",
		ID:      "9000",
		Content: utils.NewTextContent(strings.Repeat("u", liveUserChars)),
	})
	for i := 0; i < liveTailRows; i++ {
		msgs = append(msgs, utils.Message{
			Role:       "tool",
			Name:       "system_tools__shell_exec",
			ID:         strconv.Itoa(9100 + i),
			ToolCallID: "call_" + strconv.Itoa(i),
			Content:    utils.NewTextContent(strings.Repeat("y", liveTailChars)),
		})
	}
	return msgs
}

func rawChars(msgs []utils.Message) int {
	n := 0
	for i := range msgs {
		n += SentViewRowChars(msgs[i])
	}
	return n
}

func wireChars(msgs []utils.Message) int { return SentViewTokens(msgs) * summaryCharsPerToken }

// TestFoldMeasuresMatchTheWire pins the fold's three measures to the wire.
//
// The fixture reproduces the live ratio: the raw prefix is ~7.7x the sent view,
// so a fold that measures the prefix believes it is compacting a conversation
// seven times bigger than the request it is protecting.
func TestFoldMeasuresMatchTheWire(t *testing.T) {
	msgs := liveShapeFixture(t)

	raw := rawChars(msgs)
	wire := wireChars(msgs)
	if raw <= wire*5 {
		t.Fatalf("fixture does not reproduce the live over-measure: raw %d chars vs wire %d chars (live log: 7052851 vs 916248, 7.7x)", raw, wire)
	}

	// Hysteresis: progress is what the fold REMOVES from the wire. Compacted
	// rows are not on the wire, so folding them buys nothing. Counting them is
	// what let the live fold pass the floor and take the whole conversation.
	bought := ultracaveFoldProgressTokens(msgs, len(msgs))
	wireTokens := SentViewTokens(msgs)
	if bought > wireTokens {
		t.Fatalf("progress counts rows the wire does not ship: fold buys %d tokens, the whole wire is %d", bought, wireTokens)
	}
	if min := (liveTailRows * liveTailChars) / summaryCharsPerToken; bought < min {
		t.Fatalf("progress under-counts the live rows it removes: %d tokens, live tail is %d", bought, min)
	}

	// The render set: what the stored roll-up will represent. The pair is the
	// summary CARRIER, never a fold target, so it is excluded too.
	set := liveFoldSet(msgs)
	if len(set) != liveTailRows+1 {
		t.Fatalf("liveFoldSet rendered %d rows, want %d (compacted rows and the checkpoint pair excluded)", len(set), liveTailRows+1)
	}
	setChars := 0
	for i := range set {
		setChars += SentViewRowChars(set[i])
	}
	if want := liveTailRows*liveTailChars + liveUserChars; setChars != want {
		t.Fatalf("render set is %d chars, want %d (the live rows only)", setChars, want)
	}
}

// TestOverflowFoldRefusesTheAnnihilationBoundary is the anti-annihilation test.
//
// On the live shape the boundary walk has no legal cut: the newest live user
// row's own tool chain is already over the tail budget, and every row older
// than it is compacted. Pre-fix, bootstrapCutoff measured the RAW prefix (6M
// chars of compacted history), found no fit, returned the newest user row as
// the floor, and the hysteresis floor PASSED because it counted those same
// compacted rows as progress. The fold then ran: 112 rows folded, history
// 3138 -> 4 messages.
//
// With the measures on the wire, the fold buys 0 tokens at that boundary, the
// hysteresis floor refuses it, and the caller bounds the request in memory
// WITHOUT touching the checkpoint pair.
func TestOverflowFoldRefusesTheAnnihilationBoundary(t *testing.T) {
	msgs := liveShapeFixture(t)

	const window = 262_144
	budget := overflowFoldBudgetTokens(window)
	if budget <= 0 {
		t.Fatalf("no fold budget for window %d", window)
	}
	tailBudget := overflowFoldTailBudgetTokens(budget)

	lu := -1
	for i, m := range msgs {
		if m.Role == "user" && !m.Compacted && !db.IsCheckpointMessage(m) {
			lu = i
		}
	}
	if lu <= 0 {
		t.Fatalf("fixture has no live user row")
	}
	if got := bootstrapCutoff(msgs, tailBudget); got != lu {
		t.Fatalf("bootstrapCutoff = %d, want the floor %d (the live cliff)", got, lu)
	}
	if got := bootstrapCutoffRefined(msgs, tailBudget, lu); got != -1 {
		t.Fatalf("bootstrapCutoffRefined = %d, want -1: the newest live turn alone is over the tail budget, so no cut inside it fits", got)
	}

	// And the fold is refused, not executed.
	if got := UltracaveOverflowFoldFrom(msgs, 24, window); got != -1 {
		t.Fatalf("UltracaveOverflowFoldFrom = %d, want -1: a boundary that removes 0 wire tokens must be refused, not folded (the live fold at this boundary took 112 rows and left 4 messages)", got)
	}
}

// TestRollupBoundCoversPriorPlusExcerpt is the anti-rolling-summary test.
//
// db.composeDurableSummary truncates the PRIOR roll-up when prior + excerpt
// exceeds the bound it was handed. durableFold passed the EXCERPT ceiling as
// that bound, so the excerpt was rendered up to the whole limit and the prior
// could never fit next to it: the escape valve fired on every fold, by
// construction. The prior sits at the head of the prompt, so every fold moved
// the KV prefix — the live trace:
//
//	14:13:09 room=545051 chars, budget=545051 chars   (bound == excerpt ceiling)
//	14:14:06 cache_read=0
//	14:17:22 [PrefixWatch] CACHE BREAK at byte 6001 (region: HISTORY)
//
// The bound is the room the ROLL-UP has; the excerpt ceiling is what is left of
// it after the prior and the chain's own markers are paid for. The two must not
// be the same number.
func TestRollupBoundCoversPriorPlusExcerpt(t *testing.T) {
	const (
		budgetTokens = 216_000 // window 262144 - promptOverheadTokens - guardReplyReserve
		tailTokens   = 34_283  // the live tail the fold left (the logged "view ~34283")
		priorChars   = livePairChars
		overhead     = megacaveLabelOverheadChars
	)
	excerptCeiling := durableFoldRoomChars(budgetTokens, tailTokens, priorChars, overhead)
	if excerptCeiling <= 0 {
		t.Fatalf("excerpt ceiling %d: the fold has no room, the render collapses to its floor", excerptCeiling)
	}
	bound := durableRollupBound(budgetTokens, tailTokens)

	if bound < excerptCeiling+priorChars+durableChainOverheadChars {
		t.Fatalf("roll-up bound %d cannot hold prior %d + excerpt %d + chain %d = %d: the escape valve fires and the prompt head moves every fold",
			bound, priorChars, excerptCeiling, durableChainOverheadChars,
			excerptCeiling+priorChars+durableChainOverheadChars)
	}
	if bound <= excerptCeiling {
		t.Fatalf("roll-up bound %d is at or below the excerpt ceiling %d: that is the #215 arithmetic", bound, excerptCeiling)
	}
}

// baseFoldedChars is #215's measure for durableFold's excerpt ceiling: the RAW
// prefix messages[:foldFrom], compacted rows included, at SentViewRowChars.
// baseSummaryBound is #215's roll-up bound: that same `chars` number, handed to
// db.CompactConversationDurable as the bound on prior + excerpt.
//
// Inlined so this test runs on the FIXED tree and pins what the fix replaced.
func baseFoldedChars(msgs []utils.Message, foldFrom int) int {
	n := 0
	for i := 0; i < foldFrom && i < len(msgs); i++ {
		n += SentViewRowChars(msgs[i])
	}
	return n
}

// TestBaseArithmeticReproducesTheLiveNumbers is the falsification test.
//
// It runs #215's arithmetic on the live slice shape and asserts it lands on the
// numbers the live log printed. If this test ever fails, the fixture stopped
// describing production — and the fix's premise with it.
//
// Live, conv 9f0ab2352bf2bdc40e77384b, 2026-11-25 14:13:09:
//
//	guard: sent view 305416 tokens, 116 live rows  -> 916248 chars on the wire
//	fold:  3136 rows, folded=7052851 chars, budget=545051 chars
//
// The fold measured 7.7x the wire, and its bound equalled its excerpt ceiling.
func TestBaseArithmeticReproducesTheLiveNumbers(t *testing.T) {
	msgs := liveShapeFixture(t)
	foldFrom := len(msgs)

	base := baseFoldedChars(msgs, foldFrom)
	wire := wireChars(msgs)
	if ratio := base / (wire + 1); ratio < 7 {
		t.Fatalf("fixture no longer reproduces the live over-measure: base %d vs wire %d (ratio %d, live ratio 7.7)", base, wire, ratio)
	}

	// #215 handed the excerpt ceiling to db as the bound on prior + excerpt, so
	// prior + excerpt cannot fit and the escape valve fires on every fold.
	const (
		budgetTokens = 216_000
		tailTokens   = 34_283
		priorChars   = livePairChars
	)
	excerptCeiling := durableFoldRoomChars(budgetTokens, tailTokens, priorChars, megacaveLabelOverheadChars)
	t.Logf("#215 bound == excerpt ceiling (%d): prior %d + excerpt %d = %d > %d, so the escape valve fires every fold",
		excerptCeiling, priorChars, excerptCeiling, priorChars+excerptCeiling, excerptCeiling)
	if fixed := durableRollupBound(budgetTokens, tailTokens); fixed <= excerptCeiling+priorChars {
		t.Fatalf("fixed bound %d still cannot hold prior %d + excerpt %d", fixed, priorChars, excerptCeiling)
	}

	// The fixed render set is the live rows only, so the fold's ceiling is sized
	// for what the roll-up will actually represent.
	fixed := 0
	for _, m := range liveFoldSet(msgs) {
		fixed += SentViewRowChars(m)
	}
	if fixed >= base {
		t.Fatalf("fixed measure %d is not smaller than #215's %d: the render set is not the live set", fixed, base)
	}
	t.Logf("measures: #215 %d chars, fixed %d chars, wire %d chars", base, fixed, wire)
}
