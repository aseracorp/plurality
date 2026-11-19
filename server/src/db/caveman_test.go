package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// ctxWithUser builds the context the db package expects for user resolution.
func ctxWithUser(userID string) context.Context {
	return context.WithValue(context.Background(), "userID", userID)
}

// bigToolResult returns a realistic shell-style tool result: a header, a long
// run of IDENTICAL progress lines (the spam shape), a blank run, and a signal
// line at the end. Adjacent-identical lines are what run-length collapse
// targets; a fixture of all-unique lines has nothing to gain and the gain
// guard correctly refuses to compress it.
func bigToolResult(repeats int) string {
	var b strings.Builder
	b.WriteString("2026-11-19 12:00:00 [INFO] starting scan of /usr/simplify\n")
	for i := 0; i < repeats; i++ {
		b.WriteString("2026-11-19 12:00:03 [INFO] scanning /usr/simplify/item 100% done\n")
	}
	b.WriteString("\n\n\n")
	b.WriteString("2026-11-19 12:05:00 [ERROR] build failed: exit status 2\n")
	return b.String()
}

// The whole point of write-time compression: the projection is stored, so the
// LLM rendering of a stored message is byte-identical on every later turn.
func TestInsertMessageStoresCompressionOnce(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES ('c1','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}

	body := bigToolResult(200)
	msg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(body),
		ToolCallID: "call_1",
		Name:       "system_tools__shell_exec",
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := insertMessage(tx, "c1", 1, msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var storedContent string
	var storedCompressed *string
	if err := db.QueryRow(`SELECT content, compressed_content FROM messages WHERE seq=1`).Scan(&storedContent, &storedCompressed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if storedCompressed == nil {
		t.Fatal("expected a stored projection, got NULL")
	}
	compressed := *storedCompressed

	// Non-destructive: the original body must survive verbatim in content.
	decoded, _ := json.Marshal(body)
	if storedContent != string(decoded) {
		t.Fatal("compression mutated the stored original")
	}
	if compressed == "" {
		t.Fatal("expected a non-empty projection")
	}
	if len(compressed) >= len(body) {
		t.Fatalf("projection not smaller: %d -> %d", len(body), len(compressed))
	}
	if !strings.Contains(compressed, "caveman_retrieve") {
		t.Fatal("projection missing the recovery pointer")
	}

	// Byte-stability: rendering the same stored row again must be identical.
	var again *string
	if err := db.QueryRow(`SELECT compressed_content FROM messages WHERE seq=1`).Scan(&again); err != nil {
		t.Fatalf("read2: %v", err)
	}
	if again == nil || *again != compressed {
		t.Fatal("projection changed between reads")
	}
}

// Small results, checkpoint summaries, and base64 payloads must be left alone.
func TestInsertMessageSkipsCompressionWhereUnsuitable(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES ('c2','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}

	cases := []struct {
		name string
		msg  utils.Message
	}{
		{"small", utils.Message{Role: "tool", Name: "x", ToolCallID: "a", Content: utils.NewTextContent(`{"ok":true}`)}},
		{"base64", utils.Message{Role: "tool", Name: "x", ToolCallID: "b", Content: utils.NewTextContent("base64," + strings.Repeat("Q", 5000))}},
		{
			"checkpoint",
			utils.Message{
				Role:       "tool",
				Name:       EcoCheckpointToolName,
				ToolCallID: "eco_1",
				Content:    utils.NewTextContent(strings.Repeat("summary text. ", 500)),
			},
		},
	}

	tx, _ := db.Begin()
	for i, tc := range cases {
		if _, err := insertMessage(tx, "c2", int64(i+1), tc.msg); err != nil {
			t.Fatalf("%s insert: %v", tc.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for i, tc := range cases {
		var compressed *string
		if err := db.QueryRow(`SELECT compressed_content FROM messages WHERE seq=?`, i+1).Scan(&compressed); err != nil {
			t.Fatalf("%s read: %v", tc.name, err)
		}
		if compressed != nil && *compressed != "" {
			t.Fatalf("%s: expected no projection, got %d chars", tc.name, len(*compressed))
		}
	}
}

// User/assistant rows never get a projection — only tool results do.
func TestInsertMessageDoesNotCompressNonToolRoles(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES ('c3','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}
	tx, _ := db.Begin()
	if _, err := insertMessage(tx, "c3", 1, utils.Message{Role: "user", Content: utils.NewTextContent(bigToolResult(200))}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var compressed *string
	if err := db.QueryRow(`SELECT compressed_content FROM messages WHERE seq=1`).Scan(&compressed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if compressed != nil && *compressed != "" {
		t.Fatal("user message must not be compressed")
	}
}

func TestToolResultByCallIDRecoversFullOriginal(t *testing.T) {
	db := openTestDB(t)
	userID := "seq-recover-user"
	// Put this DB into the package cache so GetUserDB resolves it (same
	// pattern as recovery_test.go).
	userDBs.Store(userID, db)
	defer userDBs.Delete(userID)

	if _, err := db.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES ('c4','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}

	body := bigToolResult(300)
	msg := utils.Message{Role: "tool", Name: "system_tools__shell_exec", ToolCallID: "call_big", Content: utils.NewTextContent(body)}
	tx, _ := db.Begin()
	if _, err := insertMessage(tx, "c4", 7, msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, name, found, err := ToolResultByCallID(ctxWithUser(userID), "c4", "call_big")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !found {
		t.Fatal("expected to find the tool row")
	}
	if name != "system_tools__shell_exec" {
		t.Fatalf("wrong tool name: %q", name)
	}
	// Recovery must be byte-exact — that is the entire non-destructive promise.
	if got != body {
		t.Fatalf("recovery not exact: got %d chars, want %d", len(got), len(body))
	}

	// Non-tool seq and unknown seq must report not-found, not error.
	if _, _, f, _ := ToolResultByCallID(ctxWithUser(userID), "c4", "no-such-call"); f {
		t.Fatal("expected not-found for missing tool_call_id")
	}
	// Empty id must be not-found, never a full-table scan.
	if _, _, f, _ := ToolResultByCallID(ctxWithUser(userID), "c4", ""); f {
		t.Fatal("empty tool_call_id must not match anything")
	}
}
