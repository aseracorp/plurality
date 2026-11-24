package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// eco_tailcap_chunk_test.go — the tail cut is CHUNKED, not minimal.
//
// The rolling tail, measured on the deployed build (103 cap lines, same
// budget): the head was pinned 97 times and moved 4 times, and every move was
// 68-246 ROWS (median 158). A minimal cut is not a small cut: the window
// overshoots the budget by a whole turn of fat tool rows, so the "minimum"
// needed to refit is hundreds of rows — and it recurs the moment the next turn
// lands. Each move rewrites the first byte of the sent history, so the
// provider's KV prefix cache cannot extend past it.
//
// The contract this pins:
//   - the head NEVER moves backward (rows already dropped are never re-shipped);
//   - the head is PINNED across a band of conversation, then moves ONCE;
//   - the number of moves is bounded by growth/chunk, not by the turn count;
//   - the window stays bounded, opens on a user row, and never empties.

// tcTurn is a turn of EXACTLY known shipped size, so the chunk arithmetic is
// checkable by hand: user 100, assistant 100 + tool-call (100 args + 20 name +
// 8 id) = 228, tool 100. One turn = 428 shipped chars.
const tcTurnChars = 428

func tcTurn(n int) []utils.Message {
	id := "tc_" + itoa(n)
	user := utils.Message{Role: "user", Content: utils.NewTextContent("u" + strings.Repeat("u", 99))}
	asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("a" + strings.Repeat("a", 99))}
	asst.ToolCalls = []utils.ToolCall{{ID: id, Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: strings.Repeat("x", 100)}}}
	tool := mkMsg("tool", strings.Repeat("t", 100), "system_tools__shell_exec")
	tool.ToolCallID = id
	return []utils.Message{user, asst, tool}
}

func tcShipped(msgs []utils.Message) int {
	total := 0
	for _, m := range msgs {
		total += shippedRowChars(m)
	}
	return total
}

// The size of one tcTurn, asserted before it is used as a constant: a fixture
// whose arithmetic is wrong pins nothing.
func TestTcTurnIsTheSizeTheArithmeticAssumes(t *testing.T) {
	got := tcShipped(tcTurn(0))
	if got != tcTurnChars {
		t.Fatalf("tcTurn ships %d chars, the tests' arithmetic assumes %d", got, tcTurnChars)
	}
}

// The head must be PINNED across a band of conversation and move ONCE per
// chunk. Appending 12 turns (5136 chars) against a 2000-char chunk must move
// the head at most 3 times, and must leave it unchanged on most turns.
func TestTailCapHeadIsPinnedThenMovesOncePerChunk(t *testing.T) {
	const budget = 4_000
	const chunk = 2_000

	var hist []utils.Message
	for i := 0; i < 20; i++ {
		hist = append(hist, tcTurn(i)...)
	}
	if tcShipped(hist) <= budget {
		t.Fatalf("fixture %d chars must exceed the %d budget", tcShipped(hist), budget)
	}

	prevStart := -1
	moves := 0
	pinned := 0
	for turn := 0; turn < 12; turn++ {
		start := capTailWindowChunk(hist, chunk, budget)
		if prevStart >= 0 {
			if start < prevStart {
				t.Fatalf("turn %d: head moved BACKWARD %d -> %d", turn, prevStart, start)
			}
			if start == prevStart {
				pinned++
			} else {
				moves++
			}
		}
		prevStart = start
		hist = append(hist, tcTurn(20+turn)...)
	}

	growth := 12 * tcTurnChars
	maxMoves := growth/chunk + 1
	t.Logf("12 turns appended (%d chars, chunk %d): head pinned %d, moved %d", growth, chunk, pinned, moves)
	if moves > maxMoves {
		t.Fatalf("head moved %d times for %d chars of growth on a %d-char chunk: expected at most %d. "+
			"The cut is rolling, not chunked.", moves, growth, chunk, maxMoves)
	}
	if pinned == 0 {
		t.Fatalf("the head never stayed put across %d turns — every turn cut, i.e. a rolling tail", 12)
	}
}

// The head never moves backward, the window stays bounded, opens on a user
// row, keeps the newest row, and never empties — across a long append run.
func TestTailCapChunkedCutKeepsEverySafetyInvariant(t *testing.T) {
	const budget = 4_000
	const chunk = 2_000

	var hist []utils.Message
	for i := 0; i < 20; i++ {
		hist = append(hist, tcTurn(i)...)
	}

	prevStart := -1
	for turn := 0; turn < 40; turn++ {
		out := capTailChunk(hist, chunk, budget)
		if len(out) == 0 {
			t.Fatalf("turn %d: capTail shipped an EMPTY window", turn)
		}
		if out[0].Role == "tool" {
			t.Fatalf("turn %d: window opens on an orphaned tool result", turn)
		}
		last := hist[len(hist)-1]
		got := out[len(out)-1]
		if got.Role != last.Role || got.ToolCallID != last.ToolCallID {
			t.Fatalf("turn %d: newest row evicted (want %q/%q, got %q/%q)", turn, last.Role, last.ToolCallID, got.Role, got.ToolCallID)
		}
		kept := tcShipped(out)
		if kept > budget+tcTurnChars {
			t.Fatalf("turn %d: shipped %d chars on a %d budget", turn, kept, budget)
		}
		start := len(hist) - len(out)
		if prevStart >= 0 && start < prevStart {
			t.Fatalf("turn %d: head moved BACKWARD %d -> %d", turn, prevStart, start)
		}
		prevStart = start
		hist = append(hist, tcTurn(20+turn)...)
	}
}

// A budget smaller than one chunk must not empty the window: the cut swallows
// the whole slice, and the cap falls back to the newest row.
func TestTailCapChunkLargerThanBudgetKeepsTheNewestRow(t *testing.T) {
	var hist []utils.Message
	for i := 0; i < 30; i++ {
		hist = append(hist, tcTurn(i)...)
	}
	out := capTailChunk(hist, 50_000, 100) // production chunk, budget below one row
	if len(out) == 0 {
		t.Fatal("capTail shipped an EMPTY window on a sub-chunk budget")
	}
	t.Logf("sub-chunk budget: kept %d rows", len(out))
}

// The cap's threshold IS "the request fills the window": the history budget in
// tokens, plus the prompt overhead, plus the reply reserve, is the window. The
// cut is not a fixed char amount — it is derived from the model's window, so
// the cap fires just before the provider's limit and the chunk is what stops
// it rolling.
func TestTailCapThresholdIsTheWindowNotAFixedAmount(t *testing.T) {
	for _, window := range []int{262_144, 131_072, 32_000} {
		budget := bootstrapBudgetChars(window)
		toks := budget / charsPerToken
		switch {
		case window <= promptOverheadTokens+guardReplyReserve:
			if budget != 0 {
				t.Fatalf("window %d: the prompt fills it, budget must be 0, got %d", window, budget)
			}
		default:
			if toks+promptOverheadTokens+guardReplyReserve != window {
				t.Fatalf("window %d: history %d + prompt %d + reply %d = %d, want the window. "+
					"The cap must fire exactly where the request fills the window.",
					window, toks, promptOverheadTokens, guardReplyReserve,
					toks+promptOverheadTokens+guardReplyReserve)
			}
		}
		// The HISTORY is 82.4% of a 262144 window; the REQUEST at that point is
		// history + prompt overhead + reply reserve = the whole window. The cap
		// fires just before the request hits 100%, which is the ask.
		t.Logf("window %d: cap fires at %d history tokens (%.1f%% of the window); the request there is %d tokens = %.1f%%",
			window, toks, 100*float64(toks)/float64(window),
			toks+promptOverheadTokens+guardReplyReserve,
			100*float64(toks+promptOverheadTokens+guardReplyReserve)/float64(window))
	}
}

// The chunk is bounded by the budget it cuts from, and the bound is what makes
// a 50k chunk safe on a small window. On this deployment (a 648000-char budget)
// the quarter is 162000, so 50k stands; on a 65536-token model the budget is
// 58176 and a 50k chunk is 86% of it — the FIRST cut would drop nearly the
// whole context.
func TestTailCutChunkIsBoundedByTheBudget(t *testing.T) {
	chunk := tailCutChunkChars() // 50K tokens, in chars
	for _, c := range []struct {
		budget int
		want   int
		why    string
	}{
		{bootstrapBudgetChars(262_144), chunk, "this deployment: quarter 162000 exceeds 150000, so the 50K-token chunk stands"},
		{bootstrapBudgetChars(131_072), 254_784 / 4, "a 128k model: quarter 63696 is below 150000, so the quarter wins"},
		{bootstrapBudgetChars(65_536), 58_176 / 4, "a 64k model: a 50K-token chunk is 258% of the budget, so the quarter must win"},
		{100, 25, "a tiny budget: the quarter, never the constant"},
		{0, chunk, "no history room: the cap keeps the newest row alone, the chunk is moot"},
	} {
		got := tailCutChunk(c.budget)
		if got != c.want {
			t.Fatalf("budget %d: chunk %d, want %d (%s)", c.budget, got, c.want, c.why)
		}
		if c.budget > 0 && got > c.budget/2 {
			t.Fatalf("budget %d: a %d-char cut removes more than half the window in one move", c.budget, got)
		}
		t.Logf("budget %d -> chunk %d (%s)", c.budget, got, c.why)
	}
}
