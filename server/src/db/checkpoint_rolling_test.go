package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// TestCompactConversationDurable_RollingViaLLMView reproduces the wedge that
// made durable compaction "succeed" while folding nothing, observed live on
// conversation 6ed16d2e862c8408c1d1dc26 (1076 rows, 1067 compacted, 9 live):
//
//	folding 1035 of 1036 -> durable removed 24
//	folding 1037 of 1062 -> durable removed 0
//	folding 1065 of 1070 -> durable removed 26
//	folding 1067 of 1074 -> durable removed 0   (turn dies at the window)
//
// The production durable path (ai/tool_loop.go) loads the conversation with
// GetConversationByIdInternal — the LLM-FACING view, which EXCLUDES rows with
// compacted = 1 and INJECTS the synthetic checkpoint pair — and passes that
// slice to CompactConversationDurable. The boundary was resolved POSITIONALLY
// (MessageSeqAt: `ORDER BY seq LIMIT 1 OFFSET sliceIdx`) over the FULL
// history. Once prior compactions have marked rows compacted, the slice index
// and the SQL OFFSET count different row sets: the offset lands INSIDE the
// already-compacted backlog, so the UPDATE matches 0 rows. The live tail —
// the only part that reaches the outgoing request — therefore never shrinks,
// and the preemptive guard estimate climbs 325435 -> 335993 tokens vs a
// 262144 window until the turn dies.
//
// The existing durable tests pass the FULL slice (they use a raw loader),
// which is exactly the contract MessageSeqAt documents — so they never
// exercised the view the app actually uses, and the wedge shipped.
//
// Contract under test (the user's requirement): compaction keeps working on
// every following turn — the fold boundary strictly advances, the checkpoint
// pair moves up to it, the LLM-facing view shrinks, and the next compaction
// folds the history that appeared since the last one. Compaction stays
// NON-DESTRUCTIVE: the client keeps the entire history.
func TestCompactConversationDurable_RollingViaLLMView(t *testing.T) {
	db := openTestUserDB(t, "sam")
	ctx := context.WithValue(context.Background(), "userID", "sam")
	const convID = "c_rolling"
	if _, err := db.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES (?, 't', '2026-01-01T00:00:00Z')`, convID); err != nil {
		t.Fatalf("conv: %v", err)
	}

	// seq is derived from the DB, not a local counter: CompactConversationDurable
	// shifts live rows by +2 when it inserts a checkpoint pair.
	insert := func(m utils.Message) {
		t.Helper()
		var next int64
		if err := db.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE conversation_id = ?`, convID).Scan(&next); err != nil {
			t.Fatalf("next seq: %v", err)
		}
		contentJSON, err := json.Marshal(m.Content)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var toolCalls *string
		if len(m.ToolCalls) > 0 {
			b, _ := json.Marshal(m.ToolCalls)
			s := string(b)
			toolCalls = &s
		}
		if _, err := db.Exec(
			`INSERT INTO messages (conversation_id, seq, role, content, name, tool_call_id, tool_calls)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			convID, next, m.Role, string(contentJSON), m.Name, m.ToolCallID, toolCalls,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// user + assistant + tool result, the shape a tool loop produces.
	addTurns := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			insert(utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("request %d %s", i, strings.Repeat("u", 40)))})
			insert(utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("a", 40))})
			insert(utils.Message{Role: "tool", Name: "system_tools__shell_exec", Content: utils.NewTextContent(strings.Repeat("t", 60))})
		}
	}

	excerpt := func(msgs []utils.Message) string {
		return fmt.Sprintf("MACHINE SUMMARY of %d messages", len(msgs))
	}

	// liveSlice is what the production durable path sees: the LLM-facing view.
	liveSlice := func() []utils.Message {
		t.Helper()
		conv, err := GetConversationByIdInternal(ctx, convID)
		if err != nil {
			t.Fatalf("load LLM view: %v", err)
		}
		return conv.Messages
	}

	// boundary picks a legal live-window start: skip tool rows (a live window
	// may not start on one) and keep a non-empty tail.
	boundary := func(msgs []utils.Message) int {
		t.Helper()
		idx := len(msgs) - 3
		for idx < len(msgs) && msgs[idx].Role == "tool" {
			idx++
		}
		if idx <= 0 || idx >= len(msgs) {
			t.Fatalf("no legal boundary in %d messages (picked %d)", len(msgs), idx)
		}
		return idx
	}

	checkpointSeq := func() int64 {
		t.Helper()
		var s int64
		if err := db.QueryRow(`SELECT seq FROM messages WHERE conversation_id = ? AND role = 'tool' AND name = ? AND compacted = 0 ORDER BY seq DESC LIMIT 1`, convID, EcoCheckpointToolName).Scan(&s); err != nil {
			t.Fatalf("no live checkpoint pair: %v", err)
		}
		return s
	}
	compactedCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id = ? AND compacted = 1`, convID).Scan(&n); err != nil {
			t.Fatalf("count compacted: %v", err)
		}
		return n
	}

	addTurns(40)

	// Seed the state the wedge needs: a large ALREADY-COMPACTED backlog below
	// the live tail — the state compaction leaves behind. This is what makes a
	// slice index of the LLM view diverge from an OFFSET into the full history:
	// the offset lands inside the backlog.
	addTurns(40)
	if _, err := db.Exec(`UPDATE messages SET compacted = 1 WHERE conversation_id = ? AND seq <= (SELECT MAX(seq) - 12 FROM messages WHERE conversation_id = ?)`, convID, convID); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if n := compactedCount(); n < 100 {
		t.Fatalf("expected a compacted backlog, got %d rows", n)
	}

	// ---- pass 1 ------------------------------------------------------------
	slice := liveSlice()
	before1 := len(slice)
	idx1 := boundary(slice)
	removed1, err := CompactConversationDurable(ctx, convID, slice, idx1, excerpt)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if removed1 <= 0 {
		t.Fatalf("pass 1: compaction folded 0 rows (boundary=%d sliceLen=%d) — the fold is wedged", idx1, len(slice))
	}
	after1 := liveSlice()
	if len(after1) >= before1 {
		t.Fatalf("pass 1: LLM view did not shrink: %d -> %d", before1, len(after1))
	}
	seq1 := checkpointSeq()

	// ---- new history arrives: compaction must fire AGAIN -------------------
	addTurns(10)
	slice2 := liveSlice()
	idx2 := boundary(slice2)
	removed2, err := CompactConversationDurable(ctx, convID, slice2, idx2, excerpt)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if removed2 <= 0 {
		t.Fatalf("pass 2: compaction folded 0 rows (boundary=%d sliceLen=%d) — rolling fold wedged", idx2, len(slice2))
	}
	seq2 := checkpointSeq()
	if seq2 <= seq1 {
		t.Fatalf("checkpoint did not move up: seq %d -> %d", seq1, seq2)
	}
	after2 := liveSlice()
	if len(after2) >= len(slice2) {
		t.Fatalf("pass 2: LLM view did not shrink: %d -> %d", len(slice2), len(after2))
	}

	// ---- pass 3: "and so on", indefinitely ---------------------------------
	addTurns(10)
	slice3 := liveSlice()
	idx3 := boundary(slice3)
	removed3, err := CompactConversationDurable(ctx, convID, slice3, idx3, excerpt)
	if err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if removed3 <= 0 {
		t.Fatalf("pass 3: compaction folded 0 rows (boundary=%d sliceLen=%d)", idx3, len(slice3))
	}
	seq3 := checkpointSeq()
	if seq3 <= seq2 {
		t.Fatalf("checkpoint did not advance on pass 3: %d -> %d", seq2, seq3)
	}

	// Non-destructive: the client view must contain the ENTIRE history.
	conv, err := getConversationByIdIncludeCompacted(ctx, convID)
	if err != nil {
		t.Fatalf("client load: %v", err)
	}
	var want int
	if err := db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&want); err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(conv.Messages) < want {
		t.Fatalf("client view lost history: %d messages, DB has %d (compaction must be non-destructive)", len(conv.Messages), want)
	}
}
