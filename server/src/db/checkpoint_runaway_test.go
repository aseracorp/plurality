package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The rolling checkpoint summary is the HEAD of the LLM view:
// GetConversationByIdInternal returns compacted=0 rows in seq order, and the
// checkpoint pair is the first thing in it. It used to grow and NEST on every
// compaction: the previous summary is pasted under a "PRIOR CHECKPOINT:"
// banner, and the mechanical fallback stores banner+text where text already
// carries the previous banner — so each generation embeds the entire previous
// summary, nesting the banner one level deeper per turn.
//
// Live evidence (conversation 08611c509032e6831ba6031f, "System Status Check",
// after a 991440-char logfile was pasted): the live checkpoint tool row at the
// head of every prompt measured seq2029 65243 -> 2031 65348 -> 2033 65453 ->
// 2035 65558 -> 2037 65663 chars, and its content was
// "PRIOR CHECKPOINT:" repeated 15 times followed by
// "[compacted by Plurality — summary unavailable; raw excerpt follows]".
// The head bytes of the prompt therefore moved on EVERY turn — exactly what
// collapses backend prefix reuse down to the stable system block, i.e. the
// "prefill caching only caches ~24K tokens" symptom.

// liveToolSummary returns the content of the live (compacted=0) checkpoint
// tool row of a conversation — the string that sits at the head of every
// prompt sent to the model.
func liveToolSummary(t *testing.T, ctx context.Context, convID string) string {
	t.Helper()
	userID, _ := ctx.Value("userID").(string)
	d, err := GetUserDB(userID)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	var content *string
	err = d.QueryRowContext(ctx,
		`SELECT content FROM messages
		 WHERE conversation_id = ? AND role = 'tool' AND name = ? AND compacted = 0
		 ORDER BY seq DESC LIMIT 1`, convID, EcoCheckpointToolName).Scan(&content)
	if err != nil {
		t.Fatalf("no live checkpoint tool row: %v", err)
	}
	if content == nil {
		return ""
	}
	return unmarshalMessageContent(content).TextContent()
}

// seedCheckpointPair writes a live checkpoint pair at seq 1/2 carrying
// summary. It mirrors ReplaceCheckpoint's row shape (assistant half carries the
// eco_checkpoint tool call, tool half carries the summary).
func seedCheckpointPair(t *testing.T, ctx context.Context, convID, summary string) {
	t.Helper()
	userID, _ := ctx.Value("userID").(string)
	d, err := GetUserDB(userID)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	toolCallID := "eco_seed"
	asstJSON, _ := json.Marshal([]map[string]interface{}{{
		"id":   toolCallID,
		"type": "function",
		"function": map[string]interface{}{
			"name":      EcoCheckpointToolName,
			"arguments": "{}",
		},
	}})
	if _, err := d.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, seq, role, tool_calls, content)
		 VALUES (?, 1, 'assistant', ?, 'null')`, convID, string(asstJSON)); err != nil {
		t.Fatalf("seed asst: %v", err)
	}
	toolJSON, _ := json.Marshal(utils.NewTextContent(summary))
	if _, err := d.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, seq, role, name, tool_call_id, content)
		 VALUES (?, 2, 'tool', ?, ?, ?)`, convID, EcoCheckpointToolName, toolCallID, string(toolJSON)); err != nil {
		t.Fatalf("seed tool: %v", err)
	}
}

// mkRaw inserts a plain text message at an explicit seq.
func mkRaw(t *testing.T, ctx context.Context, convID string, seq int, role, text string) {
	t.Helper()
	userID, _ := ctx.Value("userID").(string)
	d, err := GetUserDB(userID)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	cj, _ := json.Marshal(utils.NewTextContent(text))
	if _, err := d.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, seq, role, content) VALUES (?, ?, ?, ?)`,
		convID, seq, role, string(cj)); err != nil {
		t.Fatalf("insert seq %d: %v", seq, err)
	}
}

// insertConversation creates the conversation row every test needs.
func insertConversation(t *testing.T, ctx context.Context, convID string) {
	t.Helper()
	userID, _ := ctx.Value("userID").(string)
	d, err := GetUserDB(userID)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO conversations (id, title, last_message_at) VALUES (?, 't', '2026-01-01T00:00:00Z')`,
		convID); err != nil {
		t.Fatalf("conv: %v", err)
	}
}

// TestCompactConversationDurable_PriorSummaryDoesNotNest drives durable
// compaction repeatedly with machineSummary = nil — the mechanical fallback
// ("[compacted by Plurality — summary unavailable...]") is the producer that
// nested in production. The summary carried forward must stay bounded and must
// never accumulate generations of "PRIOR CHECKPOINT:" banners.
func TestCompactConversationDurable_PriorSummaryDoesNotNest(t *testing.T) {
	openTestUserDB(t, "sam")
	ctx := context.WithValue(context.Background(), "userID", "sam")
	const cid = "c_nest"
	insertConversation(t, ctx, cid)

	// 40 user/assistant turns; every round folds a few of the oldest, so the
	// rolling summary is rewritten many times — the shape that reached 15
	// nested banners and 65k chars live.
	seq := 1
	for i := 0; i < 40; i++ {
		mkRaw(t, ctx, cid, seq, "user", "ask "+itoa(i)+" "+strings.Repeat("u", 400))
		seq++
		mkRaw(t, ctx, cid, seq, "assistant", "reply "+itoa(i)+" "+strings.Repeat("a", 400))
		seq++
	}

	// Each round folds up to the 4th live user row, leaving a long live tail.
	for round := 0; round < 12; round++ {
		full, err := loadRawConversationMessagesForTest(ctx, cid)
		if err != nil {
			t.Fatalf("round %d load: %v", round, err)
		}
		foldFrom := -1
		users := 0
		for i := range full {
			if full[i].Role == "user" && !IsCheckpointMessage(full[i]) {
				users++
				if users == 4 {
					foldFrom = i
					break
				}
			}
		}
		if foldFrom <= 0 {
			break
		}
		if _, err := CompactConversationDurable(ctx, cid, full, foldFrom, nil); err != nil {
			t.Fatalf("round %d compact: %v", round, err)
		}

		summary := liveToolSummary(t, ctx, cid)
		if n := strings.Count(summary, priorSummaryBanner); n > 1 {
			head := summary
			if len(head) > 320 {
				head = head[:320]
			}
			t.Fatalf("round %d: the stored summary nests %d %q banners (len=%d) — the head bytes of every prompt move, which collapses backend prefix reuse\nhead: %q",
				round, n, priorSummaryBanner, len(summary), head)
		}
		if len(summary) > maxDurableSummaryChars {
			t.Fatalf("round %d: stored summary %d chars exceeds the %d bound", round, len(summary), maxDurableSummaryChars)
		}
	}
}

// TestCompactConversationDurable_ZeroFoldLeavesCheckpointHeadAlone: a boundary
// that folds nothing must NOT rewrite the checkpoint pair.
//
// The pair is the first content of the prompt after the system block, so
// re-inserting it (mark the old pair compacted, shift seqs +2, insert a fresh
// pair carrying a newly-rendered summary) rewrites the head of the prompt and
// invalidates the entire KV prefix. This fired live on EVERY message of
// conversation 08611c509032e6831ba6031f — "durable compaction folded 0
// messages at boundary 2" — with prefill reuse pinned at ~24k of a ~95k
// prompt, i.e. the whole history re-prefilled each turn.
func TestCompactConversationDurable_ZeroFoldLeavesCheckpointHeadAlone(t *testing.T) {
	openTestUserDB(t, "sam")
	ctx := context.WithValue(context.Background(), "userID", "sam")
	const cid = "c_zero"
	insertConversation(t, ctx, cid)
	seedCheckpointPair(t, ctx, cid, "SEED SUMMARY")
	// Live tail: nothing older than the pair exists, so the only legal
	// boundary is the newest user row and nothing can fold.
	mkRaw(t, ctx, cid, 3, "user", "the live ask")
	mkRaw(t, ctx, cid, 4, "assistant", "the live reply")

	// The slice the resolver hands over: the injected checkpoint pair (no DB
	// id — the documented synthetic-pair positional path) followed by the live
	// tail. foldFrom points at the live tail, so the boundary resolves to the
	// tool row's seq: exactly the "boundary 2" shape from the live log.
	synth := []utils.Message{
		{Role: "assistant", ToolCalls: []utils.ToolCall{{
			ID: "eco_x", Type: "function",
			Function: utils.FunctionCall{Name: EcoCheckpointToolName, Arguments: "{}"},
		}}},
		{Role: "tool", Name: EcoCheckpointToolName, ToolCallID: "eco_x",
			Content: utils.NewTextContent("SEED SUMMARY")},
		{Role: "user", Content: utils.NewTextContent("the live ask")},
		{Role: "assistant", Content: utils.NewTextContent("the live reply")},
	}

	before, err := liveCheckpointSeqs(ctx, cid)
	if err != nil {
		t.Fatalf("seqs before: %v", err)
	}
	if len(before) != 2 || before[0] != 1 || before[1] != 2 {
		t.Fatalf("expected the seed pair at seq 1/2, got %v", before)
	}

	removed, err := CompactConversationDurable(ctx, cid, synth, 2,
		func(msgs []utils.Message) string { return "SHOULD NOT BE WRITTEN (" + itoa(len(msgs)) + ")" })
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected a zero fold, got %d", removed)
	}

	after, err := liveCheckpointSeqs(ctx, cid)
	if err != nil {
		t.Fatalf("seqs after: %v", err)
	}
	if len(after) != 2 || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("a zero fold moved the checkpoint pair: before=%v after=%v — the prompt head churns every turn and prefix reuse collapses", before, after)
	}
	if s := liveToolSummary(t, ctx, cid); s != "SEED SUMMARY" {
		t.Fatalf("a zero fold rewrote the checkpoint summary: %q", s)
	}
}
