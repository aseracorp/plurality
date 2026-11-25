package ai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// checkpointMsg builds the assistant half of an eco checkpoint pair — a
// synthetic assistant message whose only tool call is the checkpoint tool.
func checkpointMsg() utils.Message {
	return utils.Message{
		Role:    "assistant",
		Content: utils.MessageContent{},
		ToolCalls: []utils.ToolCall{{
			ID:   "eco_regression",
			Type: "function",
			Function: utils.FunctionCall{
				Name:      db.EcoCheckpointToolName,
				Arguments: "{}",
			},
		}},
	}
}

// toolMsg builds the tool half of the checkpoint pair.
func toolMsg() utils.Message {
	return utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent("checkpoint summary"),
		ToolCallID: "eco_regression",
		Name:       db.EcoCheckpointToolName,
	}
}

// renderRequestView flattens the provider-facing view to a single string so
// prefix/append-only properties can be asserted byte-for-byte.
func renderRequestView(msgs []utils.Message) string {
	var b strings.Builder
	for _, m := range filterCheckpointsForRequest(msgs, true) {
		b.WriteString(m.Role)
		b.WriteByte(0)
		b.WriteString(m.TextContent())
		b.WriteByte(0)
	}
	return b.String()
}

// TestFilterCheckpointsForRequest_LastElementOversizedKeepsEverything pins the
// shape that used to panic in the budget walker (index out of range [N] with
// length N, eco.go:89). The walker is gone; the contract that remains is the
// append-only one: every row from the checkpoint forward is kept, and the
// oversized user row is bounded by the per-message cap, never dropped.
func TestFilterCheckpointsForRequest_LastElementOversizedKeepsEverything(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}

	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != 3 {
		t.Fatalf("expected all 3 rows kept (checkpoint + summary + user turn), got %d", len(got))
	}
	if got[0].Role != "assistant" || !db.IsCheckpointMessage(got[0]) {
		t.Fatalf("expected the view to start at the checkpoint, got %q", got[0].Role)
	}
	text := got[2].TextContent()
	if len(text) >= 150_000 {
		t.Fatalf("expected the oversized user row to be bounded, got %d chars", len(text))
	}
	if !strings.Contains(text, "truncated by Plurality") {
		t.Fatalf("expected truncation banner in the bounded user row")
	}
}

// TestFilterCheckpointsForRequest_IsAppendOnlyAcrossTurns is the load-bearing
// contract: the provider-facing view must be byte-stable and ONLY EVER
// APPENDED TO. Appending turns may never change a byte before the append
// point, because a KV prefix cache matches the longest COMMON PREFIX — a view
// whose head moves every turn (a sliding tail cap) forces a full re-prefill
// every turn and destroys prompt caching.
//
// Mutation check: restore any per-request window cap that walks back from the
// newest row and this test fails, because the earlier render stops being a
// prefix of the later one.
func TestFilterCheckpointsForRequest_IsAppendOnlyAcrossTurns(t *testing.T) {
	var msgs []utils.Message
	msgs = append(msgs, checkpointMsg(), toolMsg())
	msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("start")})

	// Grow the conversation well past any plausible tail budget: 60 turns of
	// ~5k chars each ≈ 300k chars, i.e. 3x the 100k budget the old cap used.
	prev := renderRequestView(msgs)
	for i := 0; i < 60; i++ {
		msgs = append(msgs,
			utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("user %d %s", i, strings.Repeat("a", 5_000)))},
			utils.Message{Role: "assistant", Content: utils.NewTextContent(fmt.Sprintf("assistant %d %s", i, strings.Repeat("b", 5_000)))},
		)
		cur := renderRequestView(msgs)
		if !strings.HasPrefix(cur, prev) {
			t.Fatalf("turn %d: the provider view is not append-only — the render of the "+
				"previous turn is not a byte prefix of this one, so the KV prefix cache "+
				"diverges and the whole prompt is re-prefilled", i)
		}
		prev = cur
	}

	// Nothing may be hidden from the model: every row from the checkpoint
	// forward is in the view.
	final := filterCheckpointsForRequest(msgs, true)
	if len(final) != len(msgs)-0 {
		t.Fatalf("expected every row from the checkpoint forward (%d), got %d", len(msgs), len(final))
	}
}

// TestFilterCheckpointsForRequest_EcoOff drops checkpoint pairs entirely.
func TestFilterCheckpointsForRequest_EcoOff(t *testing.T) {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("hi")},
		checkpointMsg(),
		toolMsg(),
		{Role: "assistant", Content: utils.NewTextContent("hello")},
	}
	got := filterCheckpointsForRequest(msgs, false)
	for _, m := range got {
		if db.IsCheckpointMessage(m) {
			t.Fatalf("checkpoint message leaked through with eco off: %+v", m)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
}

// TestFilterCheckpointsForRequest_TruncatesOversizedSingleMessage ensures a
// single oversized message in the tail (e.g. a multi-MB pasted blob or a
// giant inline attachment dump) is truncated to the per-message bound
// instead of being shipped in full. Before this fix, the tail-cap walker
// re-included the whole message via the user-boundary back-up, so the
// "WordPress Security Review" conversation (1858 msgs, ~4.2MB Cosmos dump
// inline) shipped the blob on every resume and dead-ended with
// context_length_exceeded.
//
// Append-only: the checkpoint rows stay in the view (the old sliding cap
// dropped them), so all three rows are kept and the oversized user row is
// bounded in place.
func TestFilterCheckpointsForRequest_TruncatesOversizedSingleMessage(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != 3 {
		t.Fatalf("expected 3 kept messages, got %d", len(got))
	}
	if got[2].Role != "user" {
		t.Fatalf("expected kept role user, got %q", got[2].Role)
	}
	text := got[2].TextContent()
	if len(text) < 90_000 || len(text) >= 150_000 {
		t.Fatalf("expected oversized user message truncated (head kept), got %d chars", len(text))
	}
	if !strings.Contains(text, "truncated by Plurality") {
		t.Fatalf("expected truncation banner in retained content")
	}
}

// TestFilterCheckpointsForRequest_TruncatesOversizedToolResult ensures a
// huge tool result whose parent assistant call is STILL present in the tail
// is truncated (not dropped), so the OpenAI tool_calls/result pairing stays
// valid. Before this fix the giant result was shipped whole and blew the
// context window.
func TestFilterCheckpointsForRequest_TruncatesOversizedToolResult(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("hi")},
		{Role: "assistant", Content: utils.NewTextContent("calling tool"),
			ToolCalls: []utils.ToolCall{{ID: "call_big", Type: "function", Function: utils.FunctionCall{Name: "some_tool", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_big", Name: "some_tool", Content: utils.NewTextContent(strings.Repeat("y", 120_000))},
		{Role: "assistant", Content: utils.NewTextContent("done")},
	}
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) == 0 {
		t.Fatalf("expected a non-empty tail")
	}
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "call_big" {
			if len(m.TextContent()) < 90_000 || len(m.TextContent()) >= 150_000 {
				t.Fatalf("expected oversized tool result truncated (head kept), got %d chars", len(m.TextContent()))
			}
		}
	}
	// The tool call must still have its result (truncated, never dropped).
	foundCall := false
	foundResult := false
	for _, m := range got {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID == "call_big" {
					foundCall = true
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID == "call_big" {
			foundResult = true
		}
	}
	if !foundCall || !foundResult {
		t.Fatalf("expected kept tool call call_big AND its truncated result (call=%v result=%v)", foundCall, foundResult)
	}
}

// TestFilterCheckpointsForRequest_DropsDanglingOversizedToolResult ensures
// a huge tool result whose parent call was already dropped from the tail is
// removed entirely (pure context waste).
func TestFilterCheckpointsForRequest_DropsDanglingOversizedToolResult(t *testing.T) {
	// A dangling oversized tool result (no matching assistant tool_call in
	// the tail — e.g. left behind by an interrupted stream) must be removed
	// entirely: it has no parent call, so it is pure context waste.
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("hi")},
		{Role: "tool", ToolCallID: "call_orphan", Name: "other_tool", Content: utils.NewTextContent(strings.Repeat("z", 120_000))},
	}
	got := filterCheckpointsForRequest(msgs, true)
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "call_orphan" {
			t.Fatalf("expected dangling oversized tool result call_orphan to be dropped")
		}
	}
}

// TestCompactConversationForContext_TruncatesOversizedMessage ensures the
// context-overflow recovery path (CompactConversationForContext +
// TruncateOversizedMessages) also bounds a single oversized message that
// sits inside the newest keep-window, so the retry cannot fail again with
// context_length_exceeded on the same giant blob.
func TestCompactConversationForContext_TruncatesOversizedMessage(t *testing.T) {
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("sys")},
		{Role: "user", Content: utils.NewTextContent("hi")},
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("q", 250_000))}, // monster inline blob
		{Role: "assistant", Content: utils.NewTextContent("ok")},
	}
	got := CompactConversationForContext(msgs, 30)
	if len(got) != 4 {
		t.Fatalf("expected all 4 messages preserved (truncated, not dropped), got %d", len(got))
	}
	for _, m := range got {
		if m.Role == "user" && len(m.TextContent()) >= 250_000 {
			t.Fatalf("expected oversized user message truncated, got %d chars", len(m.TextContent()))
		}
		if len(m.TextContent()) > 150_000 {
			t.Fatalf("message still oversized after compaction: %d chars", len(m.TextContent()))
		}
	}
}

// TestFilterCheckpointsForRequest_TruncatesOversizedMultipartMessage guards
// the case TextContent() misses: a multi-part tool result whose FIRST part
// is a short preamble and whose LATER text part carries a multi-MB blob.
// Before TotalContentChars existed, truncation sized against the first part
// only and shipped the hidden megabyte payload (the actual "New Chat stops
// working" bug). It must now truncate to the bound and drop the blob part.
func TestFilterCheckpointsForRequest_TruncatesOversizedMultipartMessage(t *testing.T) {
	preamble := "[Attachment att_99, bytes 0-50000 of 4901042]"
	blob := strings.Repeat("B", 4_900_000)
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("continue")},
		{Role: "tool", ToolCallID: "call_huge", Name: "conversations__retrieve_conversation",
			Content: utils.NewPartsContent([]utils.ContentPart{
				{Type: "text", Text: preamble},
				{Type: "text", Text: blob},
			})},
	}
	got := filterCheckpointsForRequest(msgs, true)
	for _, m := range got {
		if m.Role != "tool" || m.ToolCallID != "call_huge" {
			continue
		}
		// The giant later part must be gone: total retained text stays <= bound+a little.
		total := m.Content.TotalContentChars()
		if total >= 100_000+200 {
			t.Fatalf("expected multi-part oversized tool result truncated, got %d total chars", total)
		}
		if !strings.Contains(m.TextContent(), "truncated by Plurality") {
			t.Fatalf("expected truncation banner in multi-part result")
		}
	}
}
