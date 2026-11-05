package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// TestCompactConversationDurable verifies the NON-DESTRUCTIVE durable
// context-compaction path: old messages are folded into the rolling
// checkpoint summary and marked compacted (NOT deleted), the summary
// survives a fresh load, and the full history remains available to the
// client — so a context-overflow retry cannot be undone by the next DB
// reload, while no conversation data is ever destroyed (the old behaviour
// physically deleted rows, which is the "conversations lose messages" bug).
// openTestUserDB creates a temp user database and wires it into the package
// cache (userDataPath + userDBs) so GetUserDB / GetConversationByIdInternal
// resolve to the SAME sql.DB the test writes to. The checkpoint tests load
// through GetConversationByIdInternal (the real app path), so the temp db
// from openTestDB alone would never be found and the conversation lookups
// would hit the default users-data dir instead.
func openTestUserDB(t *testing.T, userID string) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	oldPath := userDataPath
	userDataPath = dir
	t.Cleanup(func() { userDataPath = oldPath })

	if err := os.MkdirAll(filepath.Join(dir, userID), 0755); err != nil {
		t.Fatalf("user dir: %v", err)
	}
	testDB, err := sql.Open("sqlite3", filepath.Join(dir, userID, "conversations.db")+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	if _, err := testDB.Exec(schema); err != nil {
		if strings.Contains(err.Error(), "no such module") {
			t.Skipf("sqlite lacks FTS5/vec0 modules: %v", err)
		}
		t.Fatalf("schema: %v", err)
	}
	// Transactions: ensureColumn etc. run DDL; keep it simple, no tx here.
	userDBs.Store(userID, testDB)
	t.Cleanup(func() { userDBs.Delete(userID) })
	return testDB
}

// mechanicalExcerpt is a pure (no-LLM) machine summary for tests: it renders
// each folded message on its own line. Compaction is machine-only (#147), so
// durable compaction needs a producer of this shape — the old tests passed an
// LLM-style summariser to the (never-invoked) summaryFn parameter instead.
func mechanicalExcerpt(msgs []utils.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role + ": " + strings.TrimSpace(m.TextContent()) + "\n")
	}
	return strings.TrimSpace(b.String())
}

func TestCompactConversationDurable(t *testing.T) {
	db := openTestUserDB(t, "sam")

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
	removed, err := CompactConversationDurable(ctx, "c_durable", full, 24,
		func(msgs []utils.Message) string { return mechanicalExcerpt(msgs) })
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed with keep=24 (all fit), got %d", removed)
	}

	// Now keep only 5: the older rows should be folded and MARKED compacted
	// (never deleted).
	removed, err = CompactConversationDurable(ctx, "c_durable", full, 5,
		func(msgs []utils.Message) string { return mechanicalExcerpt(msgs) })
	if err != nil {
		t.Fatalf("compact keep=5: %v", err)
	}
	if removed < 5 {
		t.Fatalf("expected >=5 rows marked compacted with keep=5, got %d", removed)
	}

	// The LLM-facing view must show the checkpoint pair (summary), the
	// newest user message, and only the live tail — the compacted rows are
	// hidden from the model (the summary stands in for them).
	after, err := loadRawConversationMessagesForTest(ctx, "c_durable")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var hasCheckpoint bool
	var hasNewest bool
	for _, m := range after {
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
	if len(after) > 9 { // pair + ~5 kept + floor + slack
		t.Fatalf("expected a small LLM-side history, got %d messages", len(after))
	}

	// THE NON-DESTRUCTIVE GUARANTEE: the client-facing view must still
	// contain EVERY message that existed before compaction — nothing was
	// deleted, only marked.
	clientView, err := GetConversationById(ctx, "c_durable")
	if err != nil {
		t.Fatalf("client view: %v", err)
	}
	if len(clientView.Messages) != len(full)+2 {
		t.Fatalf("non-destructive guarantee violated: client view has %d messages, expected %d (original %d + checkpoint pair 2)", len(clientView.Messages), len(full)+2, len(full))
	}
	// Every original message must still be present in the client view.
	clientSet := map[string]bool{}
	for _, m := range clientView.Messages {
		clientSet[m.Role+"|"+m.TextContent()] = true
	}
	for _, m := range full {
		if !clientSet[m.Role+"|"+m.TextContent()] {
			t.Fatalf("message lost from client view: role=%s content=%q", m.Role, m.TextContent())
		}
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

// liveCheckpointSeqs returns the persisted `seq` values of the live
// (compacted = 0) checkpoint rows of a conversation, ascending. Durable
// compaction marks folded rows compacted=1, so this is the only faithful way
// to observe where the rolling checkpoint actually sits in the history.
func liveCheckpointSeqs(ctx context.Context, conversationID string) ([]int64, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx,
		`SELECT seq FROM messages
		 WHERE conversation_id = ? AND compacted = 0
		   AND (name = ? OR tool_calls LIKE ?)
		 ORDER BY seq ASC`,
		conversationID, EcoCheckpointToolName, "%"+EcoCheckpointToolName+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		seqs = append(seqs, seq)
	}
	return seqs, rows.Err()
}

// itoa is a tiny strconv-free int formatter for test messages.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
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
	db := openTestUserDB(t, "sam")
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

	// The stub must NOT echo its input: the durable path prefixes the previous
	// summary as "PRIOR CHECKPOINT:\n<old summary>", and the assertion below
	// checks the seed summary did not leak verbatim into the new pair. A stub
	// that copies raw input bytes would echo "OLD SEED SUMMARY" and fail on its
	// own echo, not on a bug.
	//
	// This is the test that exposed the dead `summaryFn` parameter: #147 made
	// compaction machine-only, so the summariser passed here was accepted and
	// NEVER INVOKED. The stored summary was the mechanical fallback
	// ("[compacted by Plurality — summary unavailable...]"), which is why the
	// "expected the NEW summary" assertion could never pass.
	removed, err := CompactConversationDurable(ctx, "c_move", full, 6,
		func(msgs []utils.Message) string { return "NEW SUMMARY (folded " + itoa(len(msgs)) + " messages)" })
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
	var summary string
	for _, m := range after {
		if IsCheckpointMessage(m) && m.Role == "tool" {
			summary = m.TextContent()
		}
	}
	// The moved-forward proof has to come from the PERSISTED seq, not from
	// the position inside `after`: GetConversationByIdInternal selects
	// `compacted = 0` rows only, and durable compaction marks the folded
	// rows (including the superseded pair) compacted=1. The live checkpoint
	// pair is therefore ALWAYS the first two rows of that view, at any
	// implementation, so an index-in-slice assertion here is unsatisfiable
	// (it has failed ever since it was added in #103 — CI runs no `go test`
	// step, so it was never observed). The real invariant: the pair's seqs
	// moved past the seed pair's seqs (1/2) to the frontier, and exactly one
	// pair remains live.
	livePair, err := liveCheckpointSeqs(ctx, "c_move")
	if err != nil {
		t.Fatalf("query live pair: %v", err)
	}
	if len(livePair) != 2 {
		t.Fatalf("expected exactly one live checkpoint pair (2 rows), got %v", livePair)
	}
	if livePair[0]+1 != livePair[1] {
		t.Fatalf("expected the checkpoint pair to be adjacent in seq, got %v", livePair)
	}
	if livePair[0] <= 2 {
		t.Fatalf("checkpoint pair did not move forward: still at the seed seqs (old pair updated in place?) %v", livePair)
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
