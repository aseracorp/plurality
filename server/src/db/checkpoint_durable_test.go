package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// TestCompactConversationDurable verifies the durable context-compaction
// path: old messages are folded into the rolling checkpoint summary, the
// rows are physically deleted, and the summary survives a fresh load —
// so a context-overflow retry cannot be undone by the next DB reload
// (the bug that kept conversation 319ce9283cac61666757b88a re-hitting
// context_length_exceeded on every turn).
func TestCompactConversationDurable(t *testing.T) {
	db := openTestDB(t)

	ctx := context.WithValue(context.Background(), "userID", "sam")
	if _, err := db.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES ('c_durable','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}
	// 10 turns of user/assistant + 2 final "in-flight" messages. The last
	// user message is the floor that must always survive.
	var msgs []utils.Message
	seq := 1
	for i := 0; i < 10; i++ {
		for _, m := range []utils.Message{
			{Role: "user", Content: utils.NewTextContent(strings.Repeat("u", 100) + string(rune('0'+i)))},
			{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("a", 100))},
		} {
			contentJSON, _ := json.Marshal(m.Content)
			if _, err := db.Exec(`INSERT INTO messages (conversation_id, seq, role, content) VALUES ('c_durable', ?, ?, ?)`, seq, m.Role, string(contentJSON)); err != nil {
				t.Fatalf("insert: %v", err)
			}
			msgs = append(msgs, m)
			seq++
		}
	}
	// Final in-flight user message (the newest, floor).
	final := utils.Message{Role: "user", Content: utils.NewTextContent("continue")}
	finalJSON, _ := json.Marshal(final.Content)
	if _, err := db.Exec(`INSERT INTO messages (conversation_id, seq, role, content) VALUES ('c_durable', ?, 'user', ?)`, seq, string(finalJSON)); err != nil {
		t.Fatalf("final: %v", err)
	}
	msgs = append(msgs, final)

	// Reload as the app does, to get seq-ordered messages incl. any ids.
	full, err := loadRawConversationMessagesForTest(ctx, "c_durable")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(full) != 21 {
		t.Fatalf("expected 21 messages before compaction, got %d", len(full))
	}

	// Compact: keep the newest 24 (which here is everything except old rows
	// get folded — with keep=24 > 21 nothing should be removed).
	removed, err := CompactConversationDurable(ctx, "c_durable", full, 24, func(text string) (string, error) {
		return "SUMMARY: " + text[:min(len(text), 50)], nil
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed with keep=24 (all fit), got %d", removed)
	}

	// Now keep only 5: 21-5 = 16 rows should be folded+deleted.
	removed, err = CompactConversationDurable(ctx, "c_durable", full, 5, func(text string) (string, error) {
		return "SUMMARY: " + text[:min(len(text), 50)], nil
	})
	if err != nil {
		t.Fatalf("compact keep=5: %v", err)
	}
	if removed < 5 {
		t.Fatalf("expected >=5 removed with keep=5, got %d", removed)
	}

	// Reload: the summary should now appear as a checkpoint pair, and the
	// newest user message must still be present.
	after, err := loadRawConversationMessagesForTest(ctx, "c_durable")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var hasCheckpoint bool
	var hasNewest bool
	total := 0
	for _, m := range after {
		total += m.Content.TotalContentChars()
		if IsCheckpointMessage(m) {
			hasCheckpoint = true
		}
		if m.Role == "user" && m.TextContent() == "continue" {
			hasNewest = true
		}
	}
	if !hasCheckpoint {
		t.Fatal("expected a checkpoint pair after durable compaction")
	}
	if !hasNewest {
		t.Fatal("expected the newest user message to survive")
	}
	if len(after) > 7 { // pair + ~5 kept + floor
		t.Fatalf("expected a small remaining history, got %d messages", len(after))
	}
}

// loadRawConversationMessagesForTest returns a conversation's raw
// seq-ordered messages (including any checkpoint pair) — the same shape
// CompactConversationDurable consumes via GetConversationByIdInternal.
func loadRawConversationMessagesForTest(ctx context.Context, id string) ([]utils.Message, error) {
	conv, err := GetConversationByIdInternal(ctx, id)
	if err != nil {
		return nil, err
	}
	return conv.Messages, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestTurnAnchoredFirstKeptIdx pins the turn-anchored keep boundary:
// the kept window must always start on a user message (newest user turn is
// the floor; previous user turn included when it fits), and a conversation
// with NO user messages (pure background-agent tool loop) must never start
// on a tool row.
func TestTurnAnchoredFirstKeptIdx(t *testing.T) {
	// Helper to build a message list: turns of user+assistant+tool, then a
	// trailing tool chain with no user (the "in-flight turn").
	mk := func(roles []string) []utils.Message {
		var msgs []utils.Message
		for _, r := range roles {
			m := utils.Message{Role: r}
			if r == "tool" {
				m.Content = utils.NewTextContent("tool result")
				m.Name = "system_tools__shell_exec"
			} else {
				m.Content = utils.NewTextContent("content")
			}
			msgs = append(msgs, m)
		}
		return msgs
	}

	// 1. Normal: user/assistant turns, newest user turn is the floor.
	msgs := mk([]string{"user", "assistant", "user", "assistant", "user", "assistant"})
	idx := turnAnchoredFirstKeptIdx(msgs, 4)
	if msgs[idx].Role != "user" {
		t.Fatalf("expected kept window to start on a user message, got %q at idx %d", msgs[idx].Role, idx)
	}
	// The newest user turn (index 4) must be inside the kept window.
	if idx > 4 {
		t.Fatalf("expected newest user turn (idx 4) to be kept, got start idx %d", idx)
	}

	// 2. Long tool chain after the newest user: the whole in-flight turn is
	// kept even when it exceeds `keep` messages.
	msgs = mk([]string{"user", "assistant", "tool", "tool", "tool", "tool", "tool"})
	idx = turnAnchoredFirstKeptIdx(msgs, 3)
	if idx != 0 {
		t.Fatalf("expected floor at the user message (idx 0), got %d", idx)
	}

	// 3. No user messages at all: fall back to newest `keep` but never start
	// on a tool row (would orphan its parent assistant call).
	msgs = mk([]string{"assistant", "tool", "assistant", "tool", "assistant", "tool", "tool"})
	idx = turnAnchoredFirstKeptIdx(msgs, 4)
	if msgs[idx].Role == "tool" {
		t.Fatalf("expected kept window to never start on a tool row, got idx %d role %q", idx, msgs[idx].Role)
	}
	if idx < len(msgs)-4 {
		t.Fatalf("expected fallback window within newest %d, got start idx %d", 4, idx)
	}

	// 4. Empty slice.
	if idx := turnAnchoredFirstKeptIdx(nil, 5); idx != 0 {
		t.Fatalf("expected 0 for empty slice, got %d", idx)
	}
}

// TestCompactConversationDurable_MovesCheckpointForward pins the fix for the
// "context lost on restart" bug: durable compaction must MOVE the checkpoint
// forward (delete the old pair, insert a fresh pair at the new boundary)
// instead of updating the old tool row in place. The old behaviour froze the
// checkpoint at its original seq while the live tail grew behind it — after
// a restart the tail had no user anchor and the model lost the thread
// (observed: conversation e1c3b48c63eb077f0985b83a, checkpoint stuck at seq
// 119/120 with 100+ pure tool/assistant messages behind it).
func TestCompactConversationDurable_MovesCheckpointForward(t *testing.T) {
	db := openTestDB(t)
	ctx := context.WithValue(context.Background(), "userID", "sam")
	if _, err := db.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES ('c_move','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}

	// Seed a checkpoint pair at seq 1/2 (assistant half + tool half).
	toolCallID := "eco_seed"
	asstJSON, _ := json.Marshal([]map[string]interface{}{{
		"id": toolCallID, "type": "function",
		"function": map[string]interface{}{"name": EcoCheckpointToolName, "arguments": "{}"},
	}})
	if _, err := db.Exec(`INSERT INTO messages (conversation_id, seq, role, tool_calls, content) VALUES ('c_move', 1, 'assistant', ?, 'null')`, string(asstJSON)); err != nil {
		t.Fatalf("seed asst: %v", err)
	}
	seedSummary, _ := json.Marshal("OLD SEED SUMMARY")
	if _, err := db.Exec(`INSERT INTO messages (conversation_id, seq, role, name, tool_call_id, content) VALUES ('c_move', 2, 'tool', ?, ?, ?)`, EcoCheckpointToolName, toolCallID, string(seedSummary)); err != nil {
		t.Fatalf("seed tool: %v", err)
	}

	// Live tail: NO user messages — pure assistant/tool pairs (the exact
	// shape that broke budgeting: no user anchor).
	var msgs []utils.Message
	seq := 3
	for i := 0; i < 10; i++ {
		for _, m := range []utils.Message{
			{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("a", 50))},
			{Role: "tool", Content: utils.NewTextContent(strings.Repeat("t", 50)), Name: "system_tools__shell_exec"},
		} {
			contentJSON, _ := json.Marshal(m.Content)
			if _, err := db.Exec(`INSERT INTO messages (conversation_id, seq, role, content, name) VALUES ('c_move', ?, ?, ?, ?)`, seq, m.Role, string(contentJSON), m.Name); err != nil {
				t.Fatalf("insert: %v", err)
			}
			msgs = append(msgs, m)
			seq++
		}
	}

	// Reload full history (incl. the checkpoint pair) as the app does.
	full, err := loadRawConversationMessagesForTest(ctx, "c_move")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(full) != 22 { // pair + 20 tail
		t.Fatalf("expected 22 messages before compaction, got %d", len(full))
	}

	removed, err := CompactConversationDurable(ctx, "c_move", full, 6, func(text string) (string, error) {
		return "NEW SUMMARY " + text[:min(len(text), 30)], nil
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if removed <= 0 {
		t.Fatalf("expected rows removed, got %d", removed)
	}

	// Reload: the checkpoint must now sit at the FRONTIER (after the folded
	// rows), the old seed pair must be gone, and the summary must be the NEW
	// one, not the old one updated in place.
	after, err := loadRawConversationMessagesForTest(ctx, "c_move")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var pairSeqs []int64
	var summary string
	for i, m := range after {
		if IsCheckpointMessage(m) {
			pairSeqs = append(pairSeqs, int64(i))
			if m.Role == "tool" {
				summary = m.TextContent()
			}
		}
	}
	if len(pairSeqs) != 2 {
		t.Fatalf("expected exactly one checkpoint pair (2 messages), got %d", len(pairSeqs))
	}
	if pairSeqs[0]+1 != pairSeqs[1] {
		t.Fatalf("expected checkpoint pair to be adjacent, got %v", pairSeqs)
	}
	// The pair must sit AFTER the oldest kept rows — i.e. not at position 0
	// (the old seed pair was at the very start). The new pair is inserted at
	// the boundary, so it must not be the first two messages.
	if pairSeqs[0] == 0 {
		t.Fatalf("checkpoint pair did not move forward: still at the start (old pair updated in place?)")
	}
	if !strings.Contains(summary, "NEW SUMMARY") {
		t.Fatalf("expected the NEW summary, got: %q", summary)
	}
	if strings.Contains(summary, "OLD SEED") {
		t.Fatalf("old seed summary leaked into the new pair: %q", summary)
	}
	// The kept tail must start on a non-tool boundary (turn-anchored).
	// After compaction the first kept message should be an assistant or user,
	// never a tool row.
	if len(after) > 0 {
		// Skip the checkpoint pair if it happens to be first.
		start := 0
		if IsCheckpointMessage(after[0]) && len(after) > 1 {
			start = 1
			if IsCheckpointMessage(after[1]) {
				start = 2
			}
		}
		if start < len(after) && after[start].Role == "tool" {
			t.Fatalf("kept tail starts on a tool row (orphaned parent call): %q", after[start].Role)
		}
	}
}
