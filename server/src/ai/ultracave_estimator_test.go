package ai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// ultracave_estimator_test.go — the trigger measure, the overflow boundary,
// the clock tail's placement, and the eco-off path.

// TestUltracaveSentViewTokensSkipsFoldedRows: the trigger must measure the
// SENT view. Counting folded rows keeps the measure above the trigger after
// every fold, which makes compaction fire on every turn and rewrite the
// prompt head — the cache-killer the whole subsystem exists to remove.
func TestUltracaveSentViewTokensSkipsFoldedRows(t *testing.T) {
	msgs := ucHistory("estimate me", 10, 1_000)
	full := SentViewTokens(msgs)
	if full <= 0 {
		t.Fatalf("estimator returned %d on a 10-turn history", full)
	}
	// Mark everything except the newest user turn folded: the estimator must
	// report the LIVE view only, i.e. drop by (almost) the whole amount.
	for i := range msgs {
		if i < len(msgs)-1 {
			msgs[i].Compacted = true
		}
	}
	live := SentViewTokens(msgs)
	if live >= full/5 {
		t.Fatalf("folded rows are still counted: full=%d live=%d", full, live)
	}
	// Folding EVERYTHING (even the newest row) counts 0: nothing is sent.
	all := ucHistory("x", 2, 100)
	for i := range all {
		all[i].Compacted = true
	}
	if got := SentViewTokens(all); got != 0 {
		t.Fatalf("all-folded view = %d tokens, want 0", got)
	}
}

// TestUltracaveSentViewCountsRowsWhole: the estimator counts every row at its
// FULL stored size, because the request path now ships the stored bytes
// whole — the per-message caps it used to apply (maxKeepTailChars /
// maxCompactMessageChars) are deleted, so an estimator that clamped would
// under-count the wire and let an oversized request ship un-flagged.
//
// The clamps were also the reason the guard and the wire disagreed: a row
// counted at the cap while the sender truncated it to cap+banner is a
// measure of a request nobody ships.
func TestUltracaveSentViewCountsRowsWhole(t *testing.T) {
	blob := strings.Repeat("B", 300_000)
	row := utils.Message{Role: "user", Content: utils.NewTextContent(blob)}
	if got := SentViewRowChars(row); got != 300_000 {
		t.Fatalf("oversized row counted at %d chars, want its full 300000 (the sender ships it whole)", got)
	}
	if SentViewTokens([]utils.Message{row}) != 100_000 {
		t.Fatalf("estimator over/under-counted the oversized row")
	}
	small := utils.Message{Role: "user", Content: utils.NewTextContent("hello")}
	if SentViewRowChars(small) != len("hello") {
		t.Fatalf("small row mis-sized: %d", SentViewRowChars(small))
	}
	// The estimator is a pure function of the stored row: same input, twice.
	if SentViewTokens([]utils.Message{blob2msg(), small}) != SentViewTokens([]utils.Message{blob2msg(), small}) {
		t.Fatal("estimator is not deterministic")
	}
	// The stored caveman projection is NOT subtracted from SentViewRowChars:
	// the sender ships it only for caveman-enabled models
	// (cavemanCompressApplies), so counting it would make the TRIGGER measure
	// model-dependent, and the trigger is evaluated per conversation whose
	// next turn may use any model. Counting the raw body is the conservative
	// direction: the guard folds marginally early on a compressed model, and
	// can never under-count and ship an oversized request. The model-aware
	// measure the guard itself uses is SentViewRowCharsShipped, which is what
	// takes the projection into account.
	proj := utils.Message{Role: "tool", Name: "system_tools__shell_exec",
		Content:           utils.NewTextContent(strings.Repeat("r", 400_000)),
		CompressedContent: strings.Repeat("p", 1_000)}
	if got := SentViewRowChars(proj); got != 400_000 {
		t.Fatalf("trigger measure: got %d, want the raw body counted whole", got)
	}
}

func blob2msg() utils.Message {
	return utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("Z", 100_010))}
}

// TestUltracaveOverflowFoldFromGuards: the overflow boundary must be > 0,
// must never fold the newest user turn, must never land on a tool row, and
// must refuse when nothing safe can be folded.
func TestUltracaveOverflowFoldFromGuards(t *testing.T) {
	// Shape with a checkpoint pair + a long live tail carrying prompt_tokens.
	msgs := []utils.Message{checkpointMsg(), toolMsg()}
	msgs = append(msgs, ucHistory("live tail", 20, 1_000)...)
	for i := range msgs {
		if msgs[i].Role == "assistant" && msgs[i].PromptTokens == 0 {
			msgs[i].PromptTokens = 30_000 + 1_000*i
		}
	}
	got := UltracaveOverflowFoldFrom(msgs, 8)
	if got <= 0 {
		t.Fatalf("boundary = %d, want > 0", got)
	}
	if got >= len(msgs) {
		t.Fatalf("boundary %d empties the live window (len %d)", got, len(msgs))
	}
	if msgs[got].Role == "tool" {
		t.Fatalf("boundary %d lands on a tool row — orphaned tool result at the request head", got)
	}
	if db.IsCheckpointMessage(msgs[got]) {
		t.Fatalf("boundary %d lands on the checkpoint pair", got)
	}
	lu := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && !db.IsCheckpointMessage(msgs[i]) {
			lu = i
			break
		}
	}
	if got > lu {
		t.Fatalf("boundary %d folds past the newest user row (%d)", got, lu)
	}

	// No user row at all: nothing safe to fold, must refuse (-1).
	noUser := []utils.Message{
		{Role: "assistant", Content: utils.NewTextContent("hi")},
		{Role: "assistant", Content: utils.NewTextContent("hi")},
	}
	if got := UltracaveOverflowFoldFrom(noUser, 8); got != -1 {
		t.Fatalf("no-user history: got %d, want -1", got)
	}
	// Only one user row at index 0: folding anything folds the whole
	// conversation, which the storage layer refuses; the boundary must -1.
	oneUser := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("the only ask")},
		{Role: "assistant", Content: utils.NewTextContent("ok")},
	}
	if got := UltracaveOverflowFoldFrom(oneUser, 1); got != -1 {
		t.Fatalf("single-user history: got %d, want -1", got)
	}
	// Empty slice.
	if got := UltracaveOverflowFoldFrom(nil, 8); got != -1 {
		t.Fatalf("empty history: got %d, want -1", got)
	}
	// Tail ends on an orphan tool row: the boundary must back off it.
	withToolTail := append(ucHistory("ask", 12, 900),
		utils.Message{Role: "tool", Name: "x", ToolCallID: "dangling", Content: utils.NewTextContent("result")})
	for i := range withToolTail {
		if withToolTail[i].Role == "assistant" {
			withToolTail[i].PromptTokens = 60_000
		}
	}
	b := UltracaveOverflowFoldFrom(withToolTail, 1)
	if b > 0 && withToolTail[b].Role == "tool" {
		t.Fatalf("boundary %d sits on a tool row", b)
	}
}

// TestUltracaveDurableFoldForLoopStopsAfterBudget: the loop-protection for
// the recovery path. A fold that removes rows is returned once the budget is
// spent the helper must return 0 rows so the caller falls through to the
// in-memory trim and then to the error path — never fold/retry forever.
func TestUltracaveDurableFoldForLoopStopsAfterBudget(t *testing.T) {
	ctx, _, convID := ucDB(t)
	hist := ucHistory("loop protect", 25, 1_200)
	for i := range hist {
		if hist[i].Role == "assistant" {
			hist[i].PromptTokens = 80_000
		}
	}
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)

	// Budget NOT spent: a real fold, with a reloaded view.
	reloaded, folded, err := ultracaveDurableFoldForLoop(ctx, convID, view, 0)
	if err != nil {
		t.Fatalf("durable fold: %v", err)
	}
	if folded == 0 || reloaded == nil {
		t.Fatalf("expected a durable fold, got rows=%d view=%v", folded, reloaded)
	}
	for _, m := range reloaded {
		if m.Compacted {
			t.Fatal("reloaded view carries folded rows — GetConversationByIdInternal must exclude them")
		}
	}
	// Budget SPENT: no fold, and no error — the caller trims in memory.
	if _, rows, err := ultracaveDurableFoldForLoop(ctx, convID, view, ultracaveMaxFoldsPerTurn); err != nil || rows != 0 {
		t.Fatalf("budget spent: rows=%d err=%v, want 0/<nil>", rows, err)
	}
	// A fold that can remove nothing reports 0 rows and leaves the pair
	// byte-identical: a no-progress turn must never rewrite the prompt head.
	again, rows2, err := ultracaveDurableFoldForLoop(ctx, convID, reloaded, 1)
	if err != nil {
		t.Fatalf("second fold: %v", err)
	}
	pairBefore := ucPairBytes(t, reloaded)
	if rows2 > 0 && again != nil {
		if p := ucPairBytes(t, again); p != nil && string(p) == string(pairBefore) {
			t.Fatal("pair moved on a fold that removed rows: summary must be rewritten in place")
		}
	}
}

// TestUltracaveClockTailStaysAtTheEnd: the clock rides inside the NEWEST
// content-bearing non-assistant row. Everything BEFORE that attachment point
// must be byte-identical, and the clock must never appear anywhere else and
// never claim a turn. Appending it is the ONE allowed tail mutation of the
// frozen prefix (ai/clock_tail.go: it appends into existing content, adds no
// row, claims no role) — do not "fix" it.
//
// appendCurrentTimeTail mutates the slice it is given, so the pre-clock state
// is snapshotted by copying the row structs first (a shallow copy is enough:
// string content is immutable and attachClock builds a NEW parts slice
// rather than writing into the old one).
func TestUltracaveClockTailStaysAtTheEnd(t *testing.T) {
	msgs := ucHistory("clock check", 6, 800)
	_, rows := ucRenderWire(t, msgs)
	pre := append([]StandardMessageReq(nil), rows...)
	preBytes := make([][]byte, len(pre))
	for i := range pre {
		preBytes[i] = mustRowBytes(t, pre[i])
	}

	withClock := appendCurrentTimeTail(rows)
	if len(withClock) != len(pre) {
		t.Fatalf("clock added a row: %d -> %d", len(pre), len(withClock))
	}

	// Locate the attachment point: the LAST row whose bytes changed.
	idx := -1
	for i := range withClock {
		if !bytes.Equal(preBytes[i], mustRowBytes(t, withClock[i])) {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no row carried the clock")
	}
	// It must be at the END: the newest row, or the row just before a
	// trailing assistant tool_calls row (assistant rows are skipped by
	// design — the clock must never read as the model's own past speech).
	if idx < len(withClock)-3 {
		t.Fatalf("clock attached at %d of %d rows — it must be at the END", idx, len(withClock))
	}
	if withClock[idx].Role == "assistant" {
		t.Fatal("clock landed on an assistant row (the model narrates the timestamp)")
	}
	// Everything strictly before the attachment point is byte-identical.
	head := ucMarshalRows(t, withClock[:idx])
	wantHead := bytes.Join(preBytes[:idx], []byte(","))
	if !bytes.Equal(head, wantHead) {
		t.Fatalf("clock moved bytes BEFORE the attachment point (prefix break at %d)", firstDivergence(head, wantHead))
	}
	// The carrier row keeps all its original bytes: the clock is appended.
	before, after := rowText(pre[idx]), rowText(withClock[idx])
	if !strings.HasPrefix(after, before) {
		t.Fatal("clock rewrote the carrier row instead of appending to it")
	}
	if !strings.HasSuffix(after, strings.TrimPrefix(clockTailText(), "\n\n")) {
		t.Fatalf("clock text not appended at the end: %q", after[len(after)-40:])
	}
	// Exactly one row carries it.
	n := 0
	for i := range withClock {
		if strings.Contains(rowText(withClock[i]), "The current time is") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("clock appears in %d rows, want 1", n)
	}
}

// TestUltracaveClockTailDoesNotBreakTheFrozenPrefix: the clock is the only
// per-request mutation the request path is allowed to make, and it never
// touches the checkpoint pair at the head.
func TestUltracaveClockTailDoesNotBreakTheFrozenPrefix(t *testing.T) {
	ctx, _, convID := ucDB(t)
	ucPush(t, ctx, convID, ucHistory("head stability", 15, 1_000)...)
	view := ucLLMView(t, ctx, convID)
	if _, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 6); err != nil {
		t.Fatalf("fold: %v", err)
	}
	v := ucLLMView(t, ctx, convID)
	_, rows := ucRenderWire(t, v)
	pair := mustRowBytes(t, rows[1])
	pairTool := mustRowBytes(t, rows[2])
	for i := 0; i < 5; i++ {
		clocked := appendCurrentTimeTail(append([]StandardMessageReq(nil), rows...))
		if !bytes.Equal(pair, mustRowBytes(t, clocked[1])) || !bytes.Equal(pairTool, mustRowBytes(t, clocked[2])) {
			t.Fatalf("clock mutated the checkpoint pair on pass %d", i)
		}
	}
}

// mustRowBytes marshals one rendered row (its exact wire bytes).
func mustRowBytes(t *testing.T, m StandardMessageReq) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	return b
}

// TestUltracaveEcoOffKeepsFullHistory: auto-compaction OFF ships the raw
// history. Two cases, because an existing pair changes the arithmetic:
//
//   - no pair: every stored row ships, nothing dropped;
//   - a pair EXISTS (minted by the context-overflow fallback, which fires
//     whatever the setting is, because the alternative is a provider 400 on
//     every turn): the LLM loader excludes rows flagged compacted, so dropping
//     the pair removes the only record of those rows and the folded history is
//     silently gone. The pair ships, and the view stays lossless.
func TestUltracaveEcoOffKeepsFullHistory(t *testing.T) {
	raw := ucHistory("raw", 30, 5_000)
	got := filterCheckpointsForRequest(raw, false)
	if len(got) != len(raw) {
		t.Fatalf("eco off must send the full history: %d rows, want %d", len(got), len(raw))
	}
	for _, m := range got {
		if db.IsCheckpointMessage(m) {
			t.Fatal("checkpoint leaked with eco off")
		}
	}

	msgs := []utils.Message{checkpointMsg(), toolMsg()}
	msgs = append(msgs, ucHistory("raw", 30, 5_000)...)
	off := filterCheckpointsForRequest(msgs, false)
	if len(off) != len(msgs) {
		t.Fatalf("eco off with an existing fold shipped %d rows, want all %d: the pair "+
			"stands in for rows the loader already excluded", len(off), len(msgs))
	}
	// eco ON with the same rows: the pair is kept and NOTHING is dropped, so
	// the frozen view is the pair + the same live rows.
	on := filterCheckpointsForRequest(msgs, true)
	if len(on) != len(msgs) {
		t.Fatalf("eco on dropped %d of %d rows: the frozen view is the pair + every live row",
			len(msgs)-len(on), len(msgs))
	}
}

// rowText renders a StandardMessageReq's text for the clock assertions.
func rowText(m StandardMessageReq) string {
	var b strings.Builder
	switch c := m.Content.(type) {
	case string:
		b.WriteString(c)
	case []StandardContentReq:
		for _, p := range c {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
