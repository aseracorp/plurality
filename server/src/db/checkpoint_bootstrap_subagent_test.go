package db

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// checkpoint_bootstrap_subagent_test.go — the storage half of the 2026-11-21
// trace (conv a44a43258742259a60be242d, /app/data/server.log):
//
//	10:51:02 [Eco] conv a44a43258742259a60be242d cutoff=67 (of 68 messages), prior checkpoint end=-1
//	10:51:47 [Eco] conv a44a43258742259a60be242d durable fold removed 0 rows (cutoff=67 of 68)
//
// and NO [DurableCompact] line between them: CompactConversationDurable had
// FOUR `return 0, nil` guards that logged NOTHING, so the 0-row fold was
// unattributable in production. The shape is a SUB-AGENT conversation — its
// only user row is seq 0 (the task), every later row is assistant/tool — so
// lastUserIdx == 0 and the NO_USER_ROW_FLOOR guard fired silently.
//
// Test 1 pins the guard is now LOUD and attributable (names the guard, the
// boundary, the clamped first-kept index, the slice length, the role sitting
// there, lastUserIdx, the conv id) and its semantics UNCHANGED: 0 rows folded,
// pair untouched.
//
// Test 2 pins the shape the brief demands: the same 68 rows with the newest
// row a legal user-row boundary (index 67) FOLDS 67 rows and mints the pair —
// the storage layer is fine with this conversation, it is the CALLER-side
// boundary (the tick's cutoff=67 on a slice whose newest user row is 0) that
// is the defect, fixed on the ai side (tickFoldBoundary).

// subRows builds the live 68-row sub-agent shape. newestUserAtEnd=true puts a
// user row at index 67 (a legal live-window start); false reproduces the live
// conversation: the ONLY user row is index 0.
func subRows(t *testing.T, rows int, chars int, newestUserAtEnd bool) []utils.Message {
	t.Helper()
	if rows < 4 {
		t.Fatalf("rows %d too small", rows)
	}
	last := rows
	if newestUserAtEnd {
		last = rows - 1 // index rows-1 is the user row, the loop stops before it
	}
	msgs := []utils.Message{{Role: "user", Content: utils.NewTextContent("sub-agent task")}}
	for i := 1; i < last; i++ {
		if i%2 == 0 {
			m := utils.Message{Role: "tool", Name: "system_tools__shell_exec",
				Content: utils.NewTextContent(strings.Repeat("x", chars))}
			m.ToolCallID = "sa_" + itoaTest(i-1)
			msgs = append(msgs, m)
			continue
		}
		m := utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("a", chars))}
		m.ToolCalls = []utils.ToolCall{{ID: "sa_" + itoaTest(i), Type: "function",
			Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
		msgs = append(msgs, m)
	}
	if newestUserAtEnd {
		msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("the newest ask")})
	}
	return msgs
}

// captureLog redirects the standard logger (utils.Log writes through
// log.Println) for the duration of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return &buf
}

// TestSubagentFoldWithLegalBoundaryFolds67Rows: 68 rows, NO pair, all
// compacted=0, msg.ID carrying the DB row id, foldFrom=67 with the newest row
// a user row. The fold MUST work: 67 rows folded, ONE pair minted at the head
// of the LLM view, the client view kept whole. This pins the storage layer is
// NOT the defect for this shape.
func TestSubagentFoldWithLegalBoundaryFolds67Rows(t *testing.T) {
	userID := "sub-fold-user"
	db := openFrozenDB(t, userID)
	convID := "sub-conv"
	seedConv(t, db, convID)

	rows := appendRows(t, db, convID, subRows(t, 68, 40, true)...)
	if len(rows) != 68 {
		t.Fatalf("fixture rows %d, want 68", len(rows))
	}
	// The fixture is the live shape: msg.ID carries the DB row id.
	for i, m := range rows {
		if m.ID == "" {
			t.Fatalf("row %d has no DB row id — foldSetRowIDs must address by stable identity", i)
		}
	}

	folded, err := CompactConversationDurable(frozenCtx(userID), convID, rows, 67, nil)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded != 67 {
		t.Fatalf("folded %d rows, want 67 (rows 0..66; row 67 is the legal user-row boundary)", folded)
	}

	after, err := GetConversationByIdInternal(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	pairs := 0
	for i, m := range after.Messages {
		if IsCheckpointMessage(m) {
			pairs++
			if i != 0 && i != 1 {
				t.Fatalf("pair row at view index %d, want the pair to head the view", i)
			}
		}
	}
	if pairs != 2 {
		t.Fatalf("checkpoint rows %d, want 2 (one pair minted)", pairs)
	}
	var compacted int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND compacted = 1`, convID).
		Scan(&compacted); err != nil {
		t.Fatalf("count compacted: %v", err)
	}
	if compacted != 67 {
		t.Fatalf("compacted rows %d, want 67 (non-destructive: flagged, never deleted)", compacted)
	}
	client, err := GetConversationById(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("client load: %v", err)
	}
	if len(client.Messages) < 68 {
		t.Fatalf("client view shrank to %d rows — the fold must be non-destructive", len(client.Messages))
	}
	t.Logf("sub-agent shape with legal boundary: folded %d, pair minted, LLM view %d rows, client %d rows",
		folded, len(after.Messages), len(client.Messages))
}

// TestSubagentFoldSilentGuardIsLoud: the LIVE shape (only user row at 0) at
// the live boundary (foldFrom=67). Semantics unchanged — 0 rows, pair
// untouched — but the guard must now be LOUD and attributable, which is what
// /app/data/server.log could not show at 10:51:47.
//
// RED on 83aa917: this fold emits NO [DurableCompact] line at all (the guard
// returned 0 in silence), so the assertion on the guard line fails.
// GREEN after: the NO_USER_ROW_FLOOR guard logs its name, boundary, clamped
// first-kept index, slice length, the role at that index, lastUserIdx and the
// conversation id.
func TestSubagentFoldSilentGuardIsLoud(t *testing.T) {
	userID := "sub-silent-user"
	db := openFrozenDB(t, userID)
	convID := "sub-conv2"
	seedConv(t, db, convID)

	rows := appendRows(t, db, convID, subRows(t, 68, 40, false)...)
	if lastUserIdx(rows) != 0 {
		t.Fatalf("fixture must be the live sub-agent shape: lastUserIdx=%d, want 0", lastUserIdx(rows))
	}

	buf := captureLog(t)
	folded, err := CompactConversationDurable(frozenCtx(userID), convID, rows, 67, nil)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded != 0 {
		t.Fatalf("folded %d rows, want 0 — the newest user row is the FLOOR and it sits at 0", folded)
	}
	logged := buf.String()
	for _, want := range []string{"[DurableCompact]", "guard NO_USER_ROW_FLOOR", convID, "boundary=67", "sliceLen=68", "lastUserIdx=0"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("silent 0-row fold: log must contain %q. Full log:\n%s", want, logged)
		}
	}

	// 0 rows folded = 0 writes: pair absent, no row flagged.
	var compacted int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND compacted = 1`, convID).
		Scan(&compacted); err != nil {
		t.Fatalf("count compacted: %v", err)
	}
	if compacted != 0 {
		t.Fatalf("a refused fold flagged %d rows — a refusal must write NOTHING", compacted)
	}
	after, err := GetConversationByIdInternal(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	for _, m := range after.Messages {
		if IsCheckpointMessage(m) {
			t.Fatal("a refused fold minted a checkpoint pair — a refusal must write NOTHING")
		}
	}
}
