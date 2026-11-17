package db

import (
	"context"
	"strings"
	"testing"
)

// Regression for the fold wedge + the roll-up shrink cadence (PR #183).
//
// Live evidence, conversation 141c6b2d33616fed6f2dd8dc, 2026-11-17 13:26-13:31:
//
//	13:26:26 ~150801 > trigger -> stageA folded 153 rows -> ~112509 fits=true
//	13:29:15 ~148508 <= trigger -> sending unchanged
//	13:31:46 ~150089 > trigger -> stageA folded 0 rows -> ~114089 fits=FALSE
//	                          -> stageD: summary -3946 chars -> durable fold: 0 rows
//
// A 74-token overshoot was "fixed" by rewriting the head object. The pair is
// the FIRST live row, so the foldable range is empty, the durable fold can only
// match 0 rows (216 + 51 "folded 0 rows" lines, one per message), and stage D
// re-shrinks the FULL stored roll-up every turn. The shrink is thrown away and
// recomputed against a tail that grows every turn, so the prompt head moves
// ~every 12 KiB of growth: the engine's 8192/16384/24576 floor family.
//
// UpdateCheckpointSummaryShrink is the durable half of the fix: persist the
// shrink on the SAME row, so the stored text is a monotone step function and
// the head stops moving.

// latchLiveIDs returns the id and seq of the live checkpoint tool row.
func latchLiveIDs(t *testing.T, ctx context.Context, convID string) (int64, int64) {
	t.Helper()
	userID, _ := ctx.Value("userID").(string)
	d, err := GetUserDB(userID)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	var id, seq int64
	if err := d.QueryRowContext(ctx,
		"SELECT id, seq FROM messages WHERE conversation_id = ? AND role = 'tool' AND name = ? AND compacted = 0 ORDER BY seq DESC LIMIT 1",
		convID, EcoCheckpointToolName).Scan(&id, &seq); err != nil {
		t.Fatalf("live tool row: %v", err)
	}
	return id, seq
}

// latchAllRows snapshots every live (id, seq) of a conversation.
func latchAllRows(t *testing.T, ctx context.Context, convID string) [][2]int64 {
	t.Helper()
	userID, _ := ctx.Value("userID").(string)
	d, err := GetUserDB(userID)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	rows, err := d.QueryContext(ctx,
		"SELECT id, seq FROM messages WHERE conversation_id = ? AND compacted = 0 ORDER BY seq ASC", convID)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var k [2]int64
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, k)
	}
	return out
}

func latchFirst30(s string) string {
	if r := []rune(s); len(r) > 30 {
		return string(r[:30]) + "..."
	}
	return s
}

// TestUpdateCheckpointSummaryShrink_WritesInPlace is the core contract: the
// shrunk text lands on the SAME row id at the SAME seq, and no other live row
// is touched. A fold MOVES the pair (correct, when the boundary moved); a
// shrink must not, because nothing moved. Moving it rewrites the first tens of
// KB of every prompt and shifts the seq of every live row.
func TestUpdateCheckpointSummaryShrink_WritesInPlace(t *testing.T) {
	openTestUserDB(t, "sam")
	ctx := context.WithValue(context.Background(), "userID", "sam")
	const cid = "c_latch_inplace"
	insertConversation(t, ctx, cid)
	prior := "ROLL-UP START " + strings.Repeat("a", 4000) + " ROLL-UP END"
	seedCheckpointPair(t, ctx, cid, prior)
	// The live tail, as in the wedge: the pair heads the view.
	mkRaw(t, ctx, cid, 3, "user", "the live ask")
	mkRaw(t, ctx, cid, 4, "assistant", "the live reply")

	beforeID, beforeSeq := latchLiveIDs(t, ctx, cid)
	before := latchAllRows(t, ctx, cid)

	shrunk := strings.TrimSpace(prior[len(prior)-2000:])
	written, err := UpdateCheckpointSummaryShrink(ctx, cid, shrunk)
	if err != nil {
		t.Fatalf("latch: %v", err)
	}
	if !written {
		t.Fatal("latch refused a genuine shrink")
	}
	if s := liveToolSummary(t, ctx, cid); s != shrunk {
		t.Fatalf("stored summary is not the shrunk text (len %d): %q", len(s), latchFirst30(s))
	}
	afterID, afterSeq := latchLiveIDs(t, ctx, cid)
	if afterID != beforeID {
		t.Fatalf("latch wrote a different row: id %d -> %d", beforeID, afterID)
	}
	if afterSeq != beforeSeq {
		t.Fatalf("latch moved the pair's seq: %d -> %d — every live row shifts and the prompt head churns", beforeSeq, afterSeq)
	}
	after := latchAllRows(t, ctx, cid)
	if len(after) != len(before) {
		t.Fatalf("live row count changed: %d -> %d", len(before), len(after))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Fatalf("latch changed live row %d: %v -> %v (seq shift / pair churn)", i, before[i], after[i])
		}
	}
}

// TestUpdateCheckpointSummaryShrink_IsMonotone is the property the 24K cap
// actually needs: the stored roll-up becomes a NON-INCREASING step function, so
// the same bytes ship turn after turn and prefix reuse extends THROUGH the
// summary (~55k tokens on the live conversation) instead of stalling at 24576.
//
// Growth is refused in every direction. The request path re-runs the shrink
// every turn against a tail that grows every turn, so an unguarded writer would
// re-inflate the durable state — the #177 shape, where the stored summary comes
// back BIGGER than the request that was measured as fitting, so compaction
// reports progress and never converges.
func TestUpdateCheckpointSummaryShrink_IsMonotone(t *testing.T) {
	openTestUserDB(t, "sam")
	ctx := context.WithValue(context.Background(), "userID", "sam")
	const cid = "c_latch_monotone"
	insertConversation(t, ctx, cid)
	base := strings.Repeat("ROLLUP ", 2000) // ~13 KB, like the live roll-up
	seedCheckpointPair(t, ctx, cid, base)
	mkRaw(t, ctx, cid, 3, "user", "live ask")

	steps := []struct {
		in       string
		accepted bool
		why      string
	}{
		{strings.Repeat("ROLLUP ", 1800), true, "shrink"},
		{strings.Repeat("ROLLUP ", 1900), false, "longer than stored"},
		{strings.Repeat("ROLLUP ", 1800), false, "identical bytes = real no-op"},
		{strings.Repeat("ROLLUP ", 1500), true, "shrink"},
		{base, false, "re-inflation to the original size"},
		{strings.Repeat("ROLLUP ", 1200), true, "shrink"},
		{"", false, "blanking the summary deletes every folded row"},
		{strings.Repeat("ROLLUP ", 1200), false, "identical bytes = real no-op"},
	}
	stored := len(base)
	for i, st := range steps {
		written, err := UpdateCheckpointSummaryShrink(ctx, cid, st.in)
		if err != nil {
			t.Fatalf("step %d (%s): %v", i, st.why, err)
		}
		got := len(strings.TrimSpace(liveToolSummary(t, ctx, cid)))
		if !st.accepted {
			if written {
				t.Fatalf("step %d (%s): accepted a non-shrink %d -> %d — monotonicity IS the latch",
					i, st.why, stored, len(strings.TrimSpace(st.in)))
			}
			if got != stored {
				t.Fatalf("step %d (%s): a refused write changed the stored text: %d -> %d", i, st.why, stored, got)
			}
			continue
		}
		if !written {
			t.Fatalf("step %d (%s): refused a genuine shrink %d -> %d", i, st.why, stored, len(st.in))
		}
		if got >= stored {
			t.Fatalf("step %d (%s): stored text grew %d -> %d", i, st.why, stored, got)
		}
		stored = got
	}
	if stored >= len(base) {
		t.Fatalf("no net shrink landed: %d -> %d", len(base), stored)
	}

	// Turn-to-turn byte-identity, stated directly: a turn that sends the same
	// bytes must not move the stored text at all, so the prompt head is stable
	// and reuse keeps extending through the summary.
	first := liveToolSummary(t, ctx, cid)
	if _, err := UpdateCheckpointSummaryShrink(ctx, cid, first); err != nil {
		t.Fatalf("idempotent latch: %v", err)
	}
	if second := liveToolSummary(t, ctx, cid); second != first {
		t.Fatal("stored summary changed on a turn that sent identical bytes — the head moves and reuse collapses")
	}
}

// TestUpdateCheckpointSummaryShrink_IgnoresSupersededPair: the pair is located
// through the LIVE assistant half, and a superseded (compacted=1) pair must
// never be revived — that would put a stale summary back into the LLM view.
func TestUpdateCheckpointSummaryShrink_IgnoresSupersededPair(t *testing.T) {
	openTestUserDB(t, "sam")
	ctx := context.WithValue(context.Background(), "userID", "sam")
	const cid = "c_latch_superseded"
	insertConversation(t, ctx, cid)
	seedCheckpointPair(t, ctx, cid, "OLD SUPERSEDED SUMMARY long enough to shrink into")
	userID, _ := ctx.Value("userID").(string)
	d, err := GetUserDB(userID)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE messages SET compacted = 1 WHERE conversation_id = ?`, cid); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	written, err := UpdateCheckpointSummaryShrink(ctx, cid, "short")
	if err != nil {
		t.Fatalf("latch: %v", err)
	}
	if written {
		t.Fatal("latch wrote to a superseded pair — a compacted=1 row came back into the LLM view")
	}
	if rows := latchAllRows(t, ctx, cid); len(rows) != 0 {
		t.Fatalf("revived %d compacted rows: %v", len(rows), rows)
	}
}
