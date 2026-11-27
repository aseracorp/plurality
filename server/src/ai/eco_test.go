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

// TestFilterCheckpointsForRequest_OversizedLastRowNoPanic: the walker that
// used to cap the tail could compute a one-past-end index and panic. The
// cap is deleted, so the panic path is gone with it; the test stays as the
// regression pin for "a single oversized live row after the pair must not
// panic and must not be dropped".
func TestFilterCheckpointsForRequest_OversizedLastRowNoPanic(t *testing.T) {
	msgs := []utils.Message{
		// One checkpoint pair (assistant + tool half).
		checkpointMsg(),
		toolMsg(),
		// One live user turn that alone exceeds the budget the deleted cap used.
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}

	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages (checkpoint pair + the live user turn), got %d", len(got))
	}
	last := got[len(got)-1]
	if last.Role != "user" {
		t.Fatalf("expected last role user, got %q", last.Role)
	}
	// Ships at its stored size: the request path rewrites nothing. Bounding
	// this view is the preemptive guard's job (it fires on this measure), and
	// the durable fold is what shrinks it.
	if n := last.Content.TotalContentChars(); n != 150_000 {
		t.Fatalf("oversized user row shipped %d chars, want the stored 150000 — "+
			"a rewritten row changes bytes between turns and drops the KV prefix", n)
	}
}

// TestFilterCheckpointsForRequest_LongTailKeepsEveryRow: a long tail (30 turns
// of ~5k chars, past the budget the deleted cap enforced) ships every row. It
// used to assert "the kept tail starts on a user message and respects
// maxTailChars" — that sliding window re-picked its start index on almost every
// turn, so the first bytes of the prompt moved every turn and the provider's
// prefix cache was invalidated from the head.
func TestFilterCheckpointsForRequest_LongTailKeepsEveryRow(t *testing.T) {
	var msgs []utils.Message
	msgs = append(msgs, checkpointMsg(), toolMsg())
	// 30 alternating user/assistant turns of ~5k chars each (~150k total),
	// so the cap triggers mid-tail and must resync to a user boundary.
	for i := 0; i < 30; i++ {
		msgs = append(msgs,
			utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("user %d %s", i, strings.Repeat("a", 5_000)))},
			utils.Message{Role: "assistant", Content: utils.NewTextContent(fmt.Sprintf("assistant %d %s", i, strings.Repeat("b", 5_000)))},
		)
	}

	got := filterCheckpointsForRequest(msgs, true)
	if len(got) == 0 {
		t.Fatal("expected a non-empty tail")
	}
	// FROZEN PREFIX contract: the sent view starts at the checkpoint pair and
	// carries EVERY live row after it. It used to assert "kept tail starts on a
	// user message and respects maxTailChars" — that sliding window re-picked
	// its start index on almost every turn, so the first bytes of the prompt
	// moved every turn and the provider's prefix cache was invalidated from the
	// head. Bounding the view is now durable compaction's job, not a
	// per-request window's. The per-message bound and the bootstrap cap are
	// pinned by ultracave_safety_test.go.
	if !db.IsCheckpointMessage(got[0]) {
		t.Fatalf("expected the sent view to start at the checkpoint pair, got role %q", got[0].Role)
	}
	if len(got) != 2+60 {
		t.Fatalf("expected the pair + all 60 live rows, got %d", len(got))
	}
}

// TestFilterCheckpointsForRequest_EcoOff drops checkpoint pairs entirely.
// TestFilterCheckpointsForRequest_EcoOff: auto-compaction OFF means the user
// asked for the raw history, so the pair is dropped and every stored row ships.
// Here the fixture carries a pair, and that case has its own rule (see
// TestFilterCheckpointsForRequest_EcoOffWithFoldedRows): dropping a pair whose
// rows the loader has already excluded would delete the folded history with
// nothing to replace it.
func TestFilterCheckpointsForRequest_EcoOffNoPair(t *testing.T) {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("hi")},
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

// TestFilterCheckpointsForRequest_EcoOffWithFoldedRows: eco OFF, and a pair
// EXISTS (it was minted by the context-overflow fallback, which fires
// regardless of the setting because the alternative is a provider 400 on every
// turn). The LLM loader excludes rows flagged compacted, so dropping the pair
// here removes the ONLY record of those rows: the model answers with the
// folded history simply gone. Silent context loss, exactly what this subsystem
// exists to prevent. So the pair ships, and the view is lossless.
func TestFilterCheckpointsForRequest_EcoOffWithFoldedRows(t *testing.T) {
	// Realistic layout: durable compaction mints the pair at the NEWEST seq, so
	// every live row follows it. Rows before it are the folded ones, which the
	// LLM loader already excludes.
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("ask")},
		{Role: "assistant", Content: utils.NewTextContent("hello")},
	}
	got := filterCheckpointsForRequest(msgs, false)
	pair := 0
	for _, m := range got {
		if db.IsCheckpointMessage(m) {
			pair++
		}
	}
	if pair != 2 {
		t.Fatalf("eco off with an existing fold shipped %d pair rows, want 2: "+
			"the pair is the only representation of the compacted rows, and the "+
			"loader already excluded those rows", pair)
	}
	if len(got) != len(msgs) {
		t.Fatalf("eco off with a fold shipped %d rows, want all %d", len(got), len(msgs))
	}
}

// The size caps this file used to test (a per-message truncation to
// maxKeepTailChars and a drop rule for oversized orphan tool results) are
// DELETED: a truncated row changes bytes between passes (the banner made the
// row oversized on its own output: 100000 + a 104-char banner = 100104, over
// its own bound), so every later pass re-truncated it and rewrote the banner —
// 332 stored rows carried the banner and 6 carried it nested twice. The tests
// below pin the replacement contract: rows ship as stored.

// TestFilterCheckpointsForRequest_ShipsOversizedSingleMessage: the per-message
// truncation this test used to pin is DELETED. It was the prefix-killer: the
// banner was appended AFTER cutting to the bound (100000 + a 104-char banner =
// 100104, over the bound the function claimed to enforce), so the row was
// oversized on its own output and every later pass re-truncated it and rewrote
// the banner's "original length". Live evidence: 332 stored rows carried the
// banner, 6 carried it nested twice, the inner length equal to the outer cut.
//
// The contract now: rows ship as stored, the guard bounds the view, and a
// durable fold is what makes the view smaller.
func TestFilterCheckpointsForRequest_ShipsOversizedSingleMessage(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages (pair + live user turn), got %d", len(got))
	}
	if got[2].Role != "user" {
		t.Fatalf("expected kept role user, got %q", got[2].Role)
	}
	text := got[2].TextContent()
	if len(text) != 150_000 {
		t.Fatalf("oversized user row shipped %d chars, want the stored 150000", len(text))
	}
	if strings.Contains(text, "truncated by Plurality") {
		t.Fatal("a truncation banner is present in the sent view: the request path must not rewrite rows")
	}
	// 150k chars ~ 50k tokens, which FITS a 128k window, so the guard must NOT
	// fire here: firing on a view that fits is the ~20x false positive that
	// forced a fold every turn (conv c363b2b3, live rows 1830 -> 1855).
	if err := preemptiveContextGuard("gpt-4o", SentViewTokens(got), 0, nil, 0, 128_000, len(got)); err != nil {
		t.Fatalf("guard fired on a %d-token view that fits a 128k window (false positive forces "+
			"a fold every turn): %v", SentViewTokens(got), err)
	}
	// Give it a window the view genuinely exceeds and the refusal must appear:
	// with no per-message cap, the guard is the ONLY bound on this path.
	if err := preemptiveContextGuard("gpt-4o", SentViewTokens(got), 0, nil, 0, 32_000, len(got)); err == nil {
		t.Fatalf("guard accepted a %d-token view on a 32k window", SentViewTokens(got))
	}
}

// TestFilterCheckpointsForRequest_KeepsOversizedToolResultWithItsCall: a huge
// tool result whose parent assistant call is present ships WHOLE. It is not
// dropped (the OpenAI tool_calls/result pairing must stay legal: dropping the
// result of a surviving call is a 400 on every later request) and it is not
// truncated (a truncated row changes bytes between passes, which discards the
// KV prefix behind it).
func TestFilterCheckpointsForRequest_KeepsOversizedToolResultWithItsCall(t *testing.T) {
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
	if len(got) != len(msgs) {
		t.Fatalf("sent view dropped %d of %d rows", len(msgs)-len(got), len(msgs))
	}
	var shipped int
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "call_big" {
			shipped = m.Content.TotalContentChars()
		}
	}
	if shipped != 120_000 {
		t.Fatalf("tool result shipped %d chars, want the stored 120000", shipped)
	}
	// Pairing intact: the call and its result are both present.
	var foundCall, foundResult bool
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
		t.Fatalf("tool call/result pairing must survive (call=%v result=%v)", foundCall, foundResult)
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

// The compacting-then-truncating recovery path this file used to test
// (CompactConversationForContext + TruncateOversizedMessages) is DELETED: it
// dropped whole live turns from the request and truncated rows the sender
// stores, i.e. history loss and a churning KV prefix. Its replacement is the
// durable fold (stage-1 render + the model's checkpoint) and, when nothing
// can be folded, the durable roll-up refine. See
// TestRefinedRollupShrinksTheArchive in eco_rollup_refine_test.go.

// TestFilterCheckpointsForRequest_ShipsOversizedMultipartMessage guards the
// case TextContent() used to miss: a multi-part tool result whose FIRST part
// is a short preamble and whose LATER text part carries a multi-MB blob. The
// estimator must size it by TotalContentChars (the whole payload), so the
// guard sees the hidden megabyte and refuses the request instead of shipping
// it. The row ships whole — nothing on this path rewrites stored bytes.
func TestFilterCheckpointsForRequest_ShipsOversizedMultipartMessage(t *testing.T) {
	preamble := "[Attachment att_99, bytes 0-50000 of 4901042]"
	blob := strings.Repeat("B", 4_900_000)
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("continue")},
		{Role: "assistant", Content: utils.NewTextContent("reading"),
			ToolCalls: []utils.ToolCall{{ID: "call_huge", Type: "function", Function: utils.FunctionCall{Name: "conversations__retrieve_conversation", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_huge", Name: "conversations__retrieve_conversation",
			Content: utils.NewPartsContent([]utils.ContentPart{
				{Type: "text", Text: preamble},
				{Type: "text", Text: blob},
			})},
	}
	got := filterCheckpointsForRequest(msgs, true)
	var total int
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "call_huge" {
			total = m.Content.TotalContentChars()
		}
	}
	if total != len(preamble)+len(blob) {
		t.Fatalf("multi-part tool result shipped %d chars, want the stored %d "+
			"(the hidden later part must be counted AND shipped, not hidden by TextContent)",
			total, len(preamble)+len(blob))
	}
	if strings.Contains(got[len(got)-1].TextContent(), "truncated by Plurality") {
		t.Fatal("a truncation banner is present: the request path must not rewrite rows")
	}
	// The estimator sees the megabyte, so the guard fires: that refusal is the
	// durable fold, which bounds this row inside the fold render.
	est := SentViewTokens(got)
	if est < 1_600_000 {
		t.Fatalf("estimator counted %d tokens for a 4.9MB row: it sized only the first part", est)
	}
	if err := preemptiveContextGuard("gpt-4o", est, 0, nil, 0, 128_000, len(got)); err == nil {
		t.Fatal("guard accepted a 4.9MB view on a 128k window")
	}
}
