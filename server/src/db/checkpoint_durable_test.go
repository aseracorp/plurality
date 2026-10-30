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
