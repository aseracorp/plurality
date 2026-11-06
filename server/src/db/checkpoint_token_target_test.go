package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// toolWallConversation builds the production shape that defeated count-based
// folding (conversation 4aa674fc88d9de92b6f8af15): a few cheap old turns, then
// ONE user ask followed by a wall of assistant(tool_calls)/tool rows, each
// carrying a large dump — and NO user row after the ask, because the agent is
// mid-loop. The turn-anchored floor keeps that whole turn, so the fold boundary
// can only fall back on the `keep` message count, and 24 messages of 3000-char
// tool dumps is ~24k tokens the backend cannot cache.
func toolWallConversation(turns, wallRows, dumpChars int) []utils.Message {
	var msgs []utils.Message
	add := func(m utils.Message) { msgs = append(msgs, m) }
	for i := 0; i < turns; i++ {
		add(utils.Message{Role: "user", Content: utils.NewTextContent(strings.Repeat("u", 40))})
		add(utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("a", 40))})
	}
	add(utils.Message{Role: "user", Content: utils.NewTextContent("start the long task")})
	for i := 0; i < wallRows; i++ {
		id := "call_" + itoa(i)
		add(utils.Message{
			Role:    "assistant",
			Content: utils.NewTextContent(""),
			ToolCalls: []utils.ToolCall{{
				ID:       id,
				Type:     "function",
				Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{\"command\":\"scan\"}"},
			}},
		})
		add(utils.Message{
			Role:       "tool",
			Name:       "system_tools__shell_exec",
			ToolCallID: id,
			Content:    utils.NewTextContent(strings.Repeat("d", dumpChars)),
		})
	}
	return msgs
}

func seedConv(t *testing.T, cid, userID string, msgs []utils.Message) {
	t.Helper()
	testDB := openTestUserDB(t, userID)
	if _, err := testDB.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES (?, 't', '2026-01-01T00:00:00Z')`, cid); err != nil {
		t.Fatalf("conv: %v", err)
	}
	seq := 1
	for _, m := range msgs {
		contentJSON, _ := json.Marshal(m.Content)
		toolCalls := "null"
		if len(m.ToolCalls) > 0 {
			b, _ := json.Marshal(m.ToolCalls)
			toolCalls = string(b)
		}
		if _, err := testDB.Exec(`INSERT INTO messages (conversation_id, seq, role, name, tool_call_id, content, tool_calls)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, cid, seq, m.Role, m.Name, m.ToolCallID, string(contentJSON), toolCalls); err != nil {
			t.Fatalf("insert: %v", err)
		}
		seq++
	}
}

func loadLive(t *testing.T, ctx context.Context, cid string) []utils.Message {
	t.Helper()
	msgs, err := loadRawConversationMessagesForTest(ctx, cid)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	out := make([]utils.Message, 0, len(msgs))
	for _, m := range msgs {
		if !m.Compacted && !IsCheckpointMessage(m) {
			out = append(out, m)
		}
	}
	return out
}

func windowChars(msgs []utils.Message, from int) int {
	c := 0
	for i := from; i < len(msgs); i++ {
		c += msgs[i].Content.TotalContentChars()
	}
	return c
}

func totalChars(msgs []utils.Message) int { return windowChars(msgs, 0) }

const tokUserID, tokCID = "tok", "c_tok"

func compactToolWall(t *testing.T, tokenTarget int) (removed int, live []utils.Message, ctxRaw []utils.Message) {
	t.Helper()
	seedConv(t, tokCID, tokUserID, toolWallConversation(20, 60, 3000))
	ctx := context.WithValue(context.Background(), "userID", tokUserID)
	full, err := loadRawConversationMessagesForTest(ctx, tokCID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	removed, err = CompactConversationDurable(ctx, tokCID, full, 24, tokenTarget,
		func(msgs []utils.Message) string { return mechanicalExcerpt(msgs) })
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	return removed, loadLive(t, ctx, tokCID), full
}

// TestCompactConversationDurable_TokenTargetShrinksPastMessageCount is the
// regression for the live strata failure. Count-based folding can only say
// "keep 24 messages"; 24 messages of 3000-char tool dumps is ~24k tokens, and
// on strata a 200k+ token request collapses prompt-cache reuse from 89.7% to
// 25.3% (15s -> 154s prefill per turn). A token target must leave a strictly
// smaller live window than the message count does.
func TestCompactConversationDurable_TokenTargetShrinksPastMessageCount(t *testing.T) {
	_, countOnlyLive, _ := compactToolWall(t, 0)
	countChars := totalChars(countOnlyLive)
	if countChars == 0 {
		t.Fatal("setup: count-only folding left an empty live window")
	}

	removed, live, _ := compactToolWall(t, 2000)
	got := totalChars(live)

	if removed == 0 {
		t.Fatal("tokenTarget set but durable compaction folded 0 rows")
	}
	if got >= countChars {
		t.Fatalf("token target had no teeth: live %d chars, count-only %d chars", got, countChars)
	}
	if len(live) == 0 {
		t.Fatal("token target folded the entire window — caller would ship the full uncompressed history")
	}
	if live[0].Role == "tool" {
		t.Fatal("live window starts on a tool row — orphaned tool_call, hard API error")
	}
	// 2000 est tokens ~ 6000 chars; allow slack for the boundary walk.
	if got > 2000*3*4 {
		t.Fatalf("live window far above target: %d chars (~%d tokens)", got, got/3)
	}
}

// TestCompactConversationDurable_AbsurdTargetNeverEmptiesWindow pins the
// invariant #161 was fixing: a resolver that returns nothing makes the caller
// ship the FULL uncompressed history. Even a target of 1 token must leave a
// non-empty, tool-orphan-free window.
func TestCompactConversationDurable_AbsurdTargetNeverEmptiesWindow(t *testing.T) {
	removed, live, _ := compactToolWall(t, 1)
	if len(live) == 0 {
		t.Fatal("absurd target emptied the window")
	}
	if live[0].Role == "tool" {
		t.Fatal("live window starts on a tool row")
	}
	if removed == 0 {
		t.Fatal("absurd target folded nothing")
	}
}

// TestCompactConversationDurable_GenerousTargetIsCountOnly proves the parameter
// is strictly additive: a target the count-based window already satisfies must
// reproduce the pre-existing behaviour exactly.
func TestCompactConversationDurable_GenerousTargetIsCountOnly(t *testing.T) {
	removedOff, off, _ := compactToolWall(t, 0)
	removedGen, gen, _ := compactToolWall(t, 10_000_000)
	if totalChars(gen) != totalChars(off) {
		t.Fatalf("generous target changed the window: %d chars vs count-only %d chars", totalChars(gen), totalChars(off))
	}
	if removedGen != removedOff {
		t.Fatalf("generous target folded %d, count-only folded %d", removedGen, removedOff)
	}
	if len(off) == 0 {
		t.Fatal("setup: count-only folding left an empty live window")
	}
}

// TestCompactConversationDurable_TokenTargetIsNonDestructive pins the standing
// guarantee under the new shrink path: rows are marked, never deleted, so the
// client view keeps the whole history.
func TestCompactConversationDurable_TokenTargetIsNonDestructive(t *testing.T) {
	seedConv(t, tokCID, tokUserID, toolWallConversation(20, 60, 3000))
	ctx := context.WithValue(context.Background(), "userID", tokUserID)
	full, err := loadRawConversationMessagesForTest(ctx, tokCID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := CompactConversationDurable(ctx, tokCID, full, 24, 2000,
		func(msgs []utils.Message) string { return mechanicalExcerpt(msgs) }); err != nil {
		t.Fatalf("compact: %v", err)
	}
	clientView, err := GetConversationById(ctx, tokCID)
	if err != nil {
		t.Fatalf("client view: %v", err)
	}
	if len(clientView.Messages) < len(full) {
		t.Fatalf("non-destructive guarantee violated: %d < %d", len(clientView.Messages), len(full))
	}
}

func TestNextKeptBoundaryIdx(t *testing.T) {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("ask")},
		{Role: "tool", Name: "t", ToolCallID: "c1", Content: utils.NewTextContent("dump")},
		{Role: "assistant", Content: utils.NewTextContent("note")},
		{Role: "tool", Name: "t", ToolCallID: "c2", Content: utils.NewTextContent("dump")},
	}
	// A tool row is never a valid window start.
	if i := nextKeptBoundaryIdx(msgs, 1); msgs[i].Role != "assistant" {
		t.Fatalf("nextKeptBoundaryIdx landed on %q, want assistant", msgs[i].Role)
	}
	if i := nextKeptBoundaryIdx(msgs, 3); i != -1 {
		t.Fatalf("nextKeptBoundaryIdx = %d, want -1 (only a tool row left)", i)
	}
	allTools := []utils.Message{{Role: "tool", Name: "t", ToolCallID: "c", Content: utils.NewTextContent("d")}}
	if i := nextKeptBoundaryIdx(allTools, 0); i != -1 {
		t.Fatalf("nextKeptBoundaryIdx = %d, want -1", i)
	}
}

func TestKeptWindowTokens(t *testing.T) {
	msgs := toolWallConversation(2, 3, 300)
	if n := keptWindowTokens(msgs, 0); n != totalChars(msgs)/3 {
		t.Fatalf("keptWindowTokens = %d, want %d", n, totalChars(msgs)/3)
	}
	if n := keptWindowTokens(msgs, len(msgs)); n != 0 {
		t.Fatalf("keptWindowTokens past the end = %d, want 0", n)
	}
	// Shrinking the window from the front must never increase its width.
	prev := keptWindowTokens(msgs, 0)
	for i := 1; i < len(msgs); i++ {
		n := keptWindowTokens(msgs, i)
		if n > prev {
			t.Fatalf("keptWindowTokens grew when shrinking: %d > %d at i=%d", n, prev, i)
		}
		prev = n
	}
}
