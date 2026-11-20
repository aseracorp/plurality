package db

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// checkpoint_pair_stability_test.go — the PROMPT-PREFIX BYTE-IDENTITY claim of
// the durable fold, at the storage layer.
//
// The pair is the first content of the prompt after the system block, so the
// contract is not "the fold rewrites the pair the cheap way" — it is "a fold
// that has nothing new to say leaves the pair byte-identical". A no-growth fold
// that fires the UPDATE anyway touches the row and collapses backend KV prefix
// reuse to the system block: the production trace was cache_read 26048 -> 16384
// and FLAT for 20 turns, i.e. the prefix never accumulated.
//
// The machine renderer is writeDurableSummaryText: deterministic, no clock, no
// model, so a fold of the same rows composes byte-for-byte the same roll-up —
// which is exactly the input the no-growth guard has to recognise.

// deterministicMachine is the storage layer's own mechanical render, used as the
// machineSummary: pure, so an unchanged fold set composes an identical roll-up.
func deterministicMachine(msgs []utils.Message) string {
	var b strings.Builder
	writeDurableSummaryText(&b, msgs)
	return b.String()
}

// TestDurableFoldNoGrowthSkipsTheWrite: the recovery fold recomputes the
// roll-up on the view it just reloaded. When the fold set has NOT grown, the
// composition is byte-for-byte the stored text, so the fold must return 0 rows
// and leave the pair's stored bytes IDENTICAL — the UPDATE must not fire at
// all. A stale view (Compacted flags not re-read) is the shape that reaches the
// guard: it re-renders rows the first fold already marked.
func TestDurableFoldNoGrowthSkipsTheWrite(t *testing.T) {
	const user = "ps_nogrowth"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_nogrowth"
	seedConv(t, db, convID)

	rows := appendRows(t, db, convID,
		userMsg("U1 fold me"),
		asstMsg("A1 fold me", "call_1", "shell_exec"),
		toolMsg("shell_exec", "T1 fold me", "call_1"),
		userMsg("U2 live anchor"),
	)
	boundary := lastUserIdx(rows)
	if boundary <= 0 {
		t.Fatal("no user anchor")
	}

	first, err := CompactConversationDurable(ctx, convID, rows, boundary, deterministicMachine)
	if err != nil {
		t.Fatalf("fold 1: %v", err)
	}
	if first == 0 {
		t.Fatal("fold 1 removed 0 rows, expected progress")
	}
	before := checkpointRows(t, db, convID)
	if before.Summary == "" {
		t.Fatal("fold 1 stored no summary")
	}

	// Second fold on the SAME view (the fold set did not grow): 0 rows, and the
	// pair's stored bytes must not move by a single byte.
	second, err := CompactConversationDurable(ctx, convID, rows, boundary, deterministicMachine)
	if err != nil {
		t.Fatalf("fold 2: %v", err)
	}
	if second != 0 {
		t.Fatalf("no-growth fold reported %d rows folded, expected 0", second)
	}
	after := checkpointRows(t, db, convID)
	if after.RawTool != before.RawTool {
		t.Fatalf("a no-growth fold rewrote the checkpoint pair's stored content (%d -> %d bytes) — prompt-head rewrite, KV prefix reuse collapse",
			len(before.RawTool), len(after.RawTool))
	}
	if after.RawAssistant != before.RawAssistant {
		t.Fatal("a no-growth fold touched the assistant half")
	}
	if after.AssistantID != before.AssistantID || after.ToolID != before.ToolID {
		t.Fatalf("pair rows changed: %d/%d -> %d/%d", before.AssistantID, before.ToolID, after.AssistantID, after.ToolID)
	}
	if after.AssistantSeq != before.AssistantSeq || after.ToolSeq != before.ToolSeq {
		t.Fatalf("pair seqs moved: %d/%d -> %d/%d", before.AssistantSeq, before.ToolSeq, after.AssistantSeq, after.ToolSeq)
	}
	if after.Count != 2 {
		t.Fatalf("checkpoint rows = %d, expected 2", after.Count)
	}

	// And the true recovery shape: a RELOADED view (folded rows gone) folds 0
	// rows and leaves the pair byte-identical too.
	reloaded, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	n, err := CompactConversationDurable(ctx, convID, reloaded.Messages, lastUserIdx(reloaded.Messages), deterministicMachine)
	if err != nil {
		t.Fatalf("reloaded fold: %v", err)
	}
	if n != 0 {
		t.Fatalf("reloaded view folded %d rows, expected 0 (they are already folded)", n)
	}
	if got := checkpointRows(t, db, convID); got.RawTool != before.RawTool {
		t.Fatal("the reload-path fold rewrote the pair")
	}
}

// TestDurableFoldAppendsOnly: when the fold set DOES grow, the bytes stored by
// the previous fold are a verbatim PREFIX of the bytes stored now — the new
// section is APPENDED, the prior is carried untouched (never re-rendered, never
// re-bounded at a fresh budget). The prior roll-up is the only representation of
// every row an earlier fold marked compacted, so a fold that re-rendered it
// would destroy history and move the prompt head.
func TestDurableFoldAppendsOnly(t *testing.T) {
	const user = "ps_append"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_append"
	seedConv(t, db, convID)

	appendRows(t, db, convID,
		userMsg("U1 generation one"),
		asstMsg("A1 generation one", "", ""),
		userMsg("U2 live anchor"),
	)
	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	folded, err := CompactConversationDurable(ctx, convID, conv.Messages, lastUserIdx(conv.Messages), deterministicMachine)
	if err != nil {
		t.Fatalf("fold 1: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold 1 removed 0 rows")
	}
	stored1 := checkpointRows(t, db, convID).Summary
	if stored1 == "" {
		t.Fatal("fold 1 stored no summary")
	}

	// Grow the fold set by exactly ONE row: a new user row moves the boundary
	// and the row it leaves behind is the one new row this fold renders.
	appendRows(t, db, convID, userMsg("U3 generation two"))
	v, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	boundary := lastUserIdx(v.Messages)
	folded2, err := CompactConversationDurable(ctx, convID, v.Messages, boundary, deterministicMachine)
	if err != nil {
		t.Fatalf("fold 2: %v", err)
	}
	if folded2 != 1 {
		t.Fatalf("fold 2 folded %d rows, expected exactly 1 (the fold set grew by one row)", folded2)
	}
	stored2 := checkpointRows(t, db, convID).Summary

	// THE APPEND-ONLY CLAIM: every byte stored by fold 1 is still there, at the
	// same offset, in the order the LLM reads it.
	if !strings.HasPrefix(stored2, stored1) {
		t.Fatalf("fold 2 did not carry the prior roll-up verbatim as a prefix\n--- stored 1 (%d bytes) ---\n%s\n--- stored 2 (%d bytes) ---\n%s",
			len(stored1), head(stored1, 400), len(stored2), head(stored2, 400))
	}
	if len(stored2) <= len(stored1) {
		t.Fatalf("stored roll-up %d -> %d bytes: the new folded row was not appended", len(stored1), len(stored2))
	}
	appended := stored2[len(stored1):]
	if !strings.Contains(appended, "U2 live anchor") {
		t.Fatalf("the appended section does not render the newly folded row:\n%s", head(appended, 400))
	}
	if strings.Contains(appended, "U1 generation one") {
		t.Fatal("the appended section re-renders rows the first fold already represented — duplication re-inflates the reload")
	}
	// ONE section banner: the chain never nests (nested generations moved the
	// head of every prompt).
	if n := strings.Count(stored2, newerHistoryBanner); n != 1 {
		t.Fatalf("section banner count %d, want exactly 1", n)
	}
	// The guard must NOT fire when there IS new content to represent.
	if got := checkpointRows(t, db, convID); got.RawTool == stored1 {
		t.Fatal("a growing fold skipped its write")
	}
}

// TestDurableFoldPairStableAcrossFiveFolds: the pair stays EXACTLY 2 rows over
// 5 folds, minted once at seq -2/-1, never re-minted, never renumbered, never
// marked compacted, and the roll-up stays an append-only byte extension of
// itself the whole time.
func TestDurableFoldPairStableAcrossFiveFolds(t *testing.T) {
	const user = "ps_five"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_five"
	seedConv(t, db, convID)

	rows := appendRows(t, db, convID,
		userMsg("U1"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
		userMsg("U2"), asstMsg("A2", "call_2", "shell_exec"), toolMsg("shell_exec", "T2", "call_2"),
		userMsg("U3 live anchor"),
	)

	var first frozenPair
	priorSummary := ""
	for round := 0; round < 5; round++ {
		if round > 0 {
			// One new user row per round: the boundary moves, so exactly one row
			// is newly foldable.
			appendRows(t, db, convID, userMsg("U turn "+itoaTest(round)))
		}
		conv, err := GetConversationByIdInternal(ctx, convID)
		if err != nil {
			t.Fatalf("round %d load: %v", round, err)
		}
		boundary := lastUserIdx(conv.Messages)
		if boundary <= 0 {
			t.Fatalf("round %d: no user anchor", round)
		}
		folded, err := CompactConversationDurable(ctx, convID, conv.Messages, boundary, deterministicMachine)
		if err != nil {
			t.Fatalf("round %d fold: %v", round, err)
		}
		if folded == 0 {
			t.Fatalf("round %d: fold removed 0 rows, expected progress", round)
		}

		p := checkpointRows(t, db, convID)
		if p.Count != 2 {
			t.Fatalf("round %d: %d checkpoint rows after folds, expected exactly 2 (pair accumulation)", round, p.Count)
		}
		if round == 0 {
			first = p
			// Minted once, FIRST in the seq-ordered view: MIN(seq)-2 / MIN(seq)-1
			// over a conversation whose first row is seq 0 -> -2 / -1.
			if p.AssistantSeq != -2 || p.ToolSeq != -1 {
				t.Fatalf("pair minted at seq %d/%d, expected -2/-1", p.AssistantSeq, p.ToolSeq)
			}
			priorSummary = p.Summary
			continue
		}
		if p.AssistantID != first.AssistantID || p.ToolID != first.ToolID {
			t.Fatalf("round %d: pair rows replaced (%d/%d -> %d/%d) — must be rewritten in place",
				round, first.AssistantID, first.ToolID, p.AssistantID, p.ToolID)
		}
		if p.AssistantSeq != -2 || p.ToolSeq != -1 {
			t.Fatalf("round %d: pair seqs moved to %d/%d, expected -2/-1", round, p.AssistantSeq, p.ToolSeq)
		}
		if p.ToolCallID != first.ToolCallID {
			t.Fatalf("round %d: tool_call_id %s -> %s — prompt-head churn", round, first.ToolCallID, p.ToolCallID)
		}
		if p.RawAssistant != first.RawAssistant {
			t.Fatalf("round %d: the assistant half was rewritten", round)
		}
		// APPEND-ONLY across every fold: the bytes stored last round are a
		// byte-for-byte PREFIX of the bytes stored this round — the newest
		// section is appended at the tail and the bytes before the append point
		// never change.
		if !strings.HasPrefix(p.Summary, priorSummary) {
			t.Fatalf("round %d: the roll-up is not a byte extension of the previous one (diverged at byte %d, %d -> %d bytes)",
				round, firstDivergingByte(priorSummary, p.Summary), len(priorSummary), len(p.Summary))
		}
		// THE PROMPT HEAD IS BYTE-STABLE: every fold keeps the FIRST fold's
		// roll-up bytes at offset 0 — the byte identity the KV prefix cache
		// needs. The head never moves; only the tail grows.
		if !strings.HasPrefix(p.Summary, first.Summary) {
			t.Fatalf("round %d: the prompt head moved — the first fold's %d bytes are no longer a prefix at offset 0 (diverged at byte %d)",
				round, len(first.Summary), firstDivergingByte(first.Summary, p.Summary))
		}
		if n := strings.Count(p.Summary, newerHistoryBanner); n != 1 {
			t.Fatalf("round %d: section banner count %d, want exactly 1 (the chain must not nest)", round, n)
		}
		priorSummary = p.Summary
	}
	if rows2 := checkpointRows(t, db, convID); rows2.Count != 2 {
		t.Fatalf("after 5 folds: %d checkpoint rows, expected 2", rows2.Count)
	}
	// The live tail never lost its anchor rows (non-destructive contract).
	client, err := GetConversationById(ctx, convID)
	if err != nil {
		t.Fatalf("client load: %v", err)
	}
	if len(client.Messages) != len(rows)+4 {
		t.Fatalf("client view shows %d rows, expected the full %d", len(client.Messages), len(rows)+4)
	}
}

func firstDivergingByte(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
