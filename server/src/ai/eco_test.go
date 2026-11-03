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

// TestFilterCheckpointsForRequest_BudgetCapLastElementIsPanicRegression
// reproduces the crash that killed the server whenever "Debug WordPress
// Slowness" hit the eco tail-cap path:
//
//	panic: runtime error: index out of range [N] with length N
//	github.com/azukaar/plurality/src/ai.filterCheckpointsForRequest
//	    server/src/ai/eco.go:89
//
// The budget walker runs backwards from the end and, when the LAST message
// alone tips the budget over maxTailChars, increments start one past the
// end (== len(tail)). That is a valid *slice* start but an invalid index,
// and the subsequent "back up to a user boundary" loop dereferenced
// tail[start] before the fix. This test pins the scenario: tail length is
// not huge (the bug is about position, not size).
func TestFilterCheckpointsForRequest_BudgetCapLastElementIsPanicRegression(t *testing.T) {
	msgs := []utils.Message{
		// One checkpoint pair (assistant + tool half).
		checkpointMsg(),
		toolMsg(),
		// One live user turn that alone exceeds the 100k-char budget.
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}

	// Must not panic. With eco ON the tail after the checkpoint is the
	// single oversized user message: the walker breaks at start==1, then
	// increments to start==2 == len(tail) (one-past-end), clamps down to
	// the last element, stays on the user boundary, and keeps the message.
	got := filterCheckpointsForRequest(msgs, true, 131072)
	if len(got) != 1 {
		t.Fatalf("expected 1 kept message (the oversized user turn), got %d", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("expected kept role user, got %q", got[0].Role)
	}
}

// TestFilterCheckpointsForRequest_BudgetCapLongTail exercises the normal
// capping path: a long tail where the budget is exceeded somewhere in the
// middle must back up to a user boundary and keep the suffix.
func TestFilterCheckpointsForRequest_BudgetCapLongTail(t *testing.T) {
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

	got := filterCheckpointsForRequest(msgs, true, 131072)
	if len(got) == 0 {
		t.Fatal("expected a non-empty tail")
	}
	// The kept tail must start exactly on a user boundary so tool results
	// never get orphaned.
	if got[0].Role != "user" {
		t.Fatalf("expected kept tail to start with a user message, got %q", got[0].Role)
	}
	// Total kept chars must respect the budget (plus up to one boundary
	// message of slack).
	total := 0
	for _, m := range got {
		total += len(m.TextContent())
	}
	if total > maxTailChars(131072)+5_100 {
		t.Fatalf("kept tail %d chars exceeds budget", total)
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
	got := filterCheckpointsForRequest(msgs, false, 131072)
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
func TestFilterCheckpointsForRequest_TruncatesOversizedSingleMessage(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}
	got := filterCheckpointsForRequest(msgs, true, 131072)
	if len(got) != 1 {
		t.Fatalf("expected 1 kept message, got %d", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("expected kept role user, got %q", got[0].Role)
	}
	text := got[0].TextContent()
	// Window-aware bound: 131072/2 = 65536 chars + banner overhead.
	if len(text) < 60_000 || len(text) >= 70_000 {
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
	got := filterCheckpointsForRequest(msgs, true, 131072)
	if len(got) == 0 {
		t.Fatalf("expected a non-empty tail")
	}
	for _, m := range got {
		if m.Role == "tool" && (len(m.TextContent()) < 60_000 || len(m.TextContent()) >= 70_000) {
			t.Fatalf("expected oversized tool result truncated (head kept), got %d chars", len(m.TextContent()))
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
	got := filterCheckpointsForRequest(msgs, true, 131072)
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
	got := filterCheckpointsForRequest(msgs, true, 131072)
	for _, m := range got {
		if m.Role != "tool" || m.ToolCallID != "call_huge" {
			continue
		}
		// The giant later part must be gone: total retained text stays <= bound+a little.
		total := m.Content.TotalContentChars()
		if total >= maxKeepTailChars(131072)+200 {
			t.Fatalf("expected multi-part oversized tool result truncated, got %d total chars", total)
		}
		if !strings.Contains(m.TextContent(), "truncated by Plurality") {
			t.Fatalf("expected truncation banner in multi-part result")
		}
	}
}

// buildTurn appends a complete user→assistant turn (user message, assistant
// reply) to msgs, using a deterministic pattern that also yields a stable
// per-message char count. size chars per message, incl. the 2-char separators.
func buildTurn(msgs []utils.Message, i, size int) []utils.Message {
	return append(msgs,
		utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("user %d %s", i, strings.Repeat("a", size)))},
		utils.Message{Role: "assistant", Content: utils.NewTextContent(fmt.Sprintf("assistant %d %s", i, strings.Repeat("b", size)))},
	)
}

// TestTrimTailToFitBudget_HardCap regression test for the context-guard loop
// "conversation 319ce9283cac61666757b88a" bug. filterCheckpointsForRequest
// (and the durable LLM-loop compaction) kept 320 msgs / 215888 chars against
// a 100000-char budget — the coarse backward walk in capTailBudget backs up
// to a user boundary and its single re-derivation in trimTailToFitBudget
// re-derived the SAME cut, so the overshoot was never corrected and the
// preemptive context guard re-fired on every turn. trimTailToFitBudget must
// now keep dropping the oldest complete turns until the kept suffix actually
// fits the budget.
func TestTrimTailToFitBudget_HardCap(t *testing.T) {
	// 40 turns x 2 msgs x ~10k chars = ~800k chars total — far over any
	// budget, so the cap MUST trigger and must stop only when fitting.
	var msgs []utils.Message
	for i := 0; i < 40; i++ {
		msgs = buildTurn(msgs, i, 10_000)
	}
	budget := 100_000

	got := trimTailToFitBudget(msgs, budget)
	if len(got) == 0 {
		t.Fatal("expected a non-empty tail (newest user turn is the floor)")
	}
	// The newest user message must always survive.
	foundNewest := false
	for _, m := range got {
		if m.Role == "user" && strings.HasPrefix(m.TextContent(), "user 39 ") {
			foundNewest = true
		}
	}
	if !foundNewest {
		t.Fatal("expected the newest user message to be preserved")
	}
	// Total kept size must respect the budget (no slack allowance: this is
	// the hard-cap function itself).
	total := 0
	for _, m := range got {
		total += m.Content.TotalContentChars()
	}
	if total > budget {
		t.Fatalf("kept tail %d chars exceeds budget %d", total, budget)
	}
	// Kept tail must start on a user boundary (no orphaned tool results).
	if got[0].Role != "user" {
		t.Fatalf("expected kept tail to start with a user message, got %q", got[0].Role)
	}
}

// TestTrimTailToFitBudget_SingleTurnOverflow pins the floor behaviour: when
// the single newest turn already exceeds the whole budget, the function must
// still keep it (it cannot be dropped without splitting a turn) and return
// exactly the last complete turn, never nothing and never a partial turn.
func TestTrimTailToFitBudget_SingleTurnOverflow(t *testing.T) {
	// Newest turn alone: ~210k chars > 100k budget.
	var msgs []utils.Message
	for i := 0; i < 3; i++ {
		msgs = buildTurn(msgs, i, 10_000) // ~60k
	}
	msgs = append(msgs,
		utils.Message{Role: "user", Content: utils.NewTextContent(strings.Repeat("u", 100_000))},
		utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("a", 110_000))},
	)
	got := trimTailToFitBudget(msgs, 100_000)
	if len(got) < 2 {
		t.Fatalf("expected at least the full newest turn, got %d msgs", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("expected kept tail to start on the newest user message, got %q", got[0].Role)
	}
	total := 0
	for _, m := range got {
		total += m.Content.TotalContentChars()
	}
	if total <= 100_000-10_000 {
		t.Fatalf("expected the oversized newest turn to be preserved whole (got %d chars)", total)
	}
}

// TestTrimTailToFitBudget_NoUserTailAfterCheckpoint is the regression test
// for the "context lost on restart" shape: a conversation tail that
// contains ONLY assistant/tool messages after the last checkpoint (a
// background-agent tool loop with the user ask already folded away). The
// budget walker must NEVER hand the model a window that starts mid-turn on
// a tool row — otherwise the tool_calls/result pairing is broken and the
// model has no request to answer. When there is no user message at all the
// floor is index 0, so the function must keep the whole tail (nothing can
// be dropped without orphaning the first turn) rather than emit a dangling
// tool result.
func TestTrimTailToFitBudget_NoUserTailAfterCheckpoint(t *testing.T) {
	// 10 assistant/tool pairs, no user anywhere.
	var msgs []utils.Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, mkMsg("assistant", fmt.Sprintf("call %d", i), ""))
		msgs = append(msgs, mkMsg("tool", strings.Repeat("t", 500), "system_tools__shell_exec"))
	}
	// Tiny budget: nothing can be dropped without orphaning the head turn.
	out := trimTailToFitBudget(msgs, 1_000)
	if len(out) == 0 {
		t.Fatal("expected a non-empty tail")
	}
	if out[0].Role == "tool" {
		t.Fatalf("kept tail starts on a tool row (orphaned parent assistant call): %q", out[0].Role)
	}
	// With no user message the floor is the very start: the whole tail must
	// survive (dropping would break the first assistant/tool pair).
	if len(out) != len(msgs) {
		t.Fatalf("expected the whole no-user tail to be kept (floor=0), got %d of %d", len(out), len(msgs))
	}
}

// TestFilterCheckpointsForRequest_NoUserTailAfterCheckpoint pins that the
// eco in-memory filter also survives a checkpoint followed only by
// assistant/tool messages (no user turn): it must return a non-empty,
// non-tool-headed window and never panic (this is the shape that used to
// feed the durable-compaction loop on conversation e1c3b48c63eb077f0985b83a).
func TestFilterCheckpointsForRequest_NoUserTailAfterCheckpoint(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
	}
	for i := 0; i < 12; i++ {
		msgs = append(msgs, mkMsg("assistant", fmt.Sprintf("call %d", i), ""))
		msgs = append(msgs, mkMsg("tool", strings.Repeat("y", 2_000), "system_tools__shell_exec"))
	}
	out := filterCheckpointsForRequest(msgs, true, 65_536)
	if len(out) == 0 {
		t.Fatal("expected a non-empty window")
	}
	if out[0].Role == "tool" {
		t.Fatalf("window starts on a tool row (orphaned parent call): %q", out[0].Role)
	}
}

func TestFindCharCutoffIndex_AdvancesOnNeverCompacted(t *testing.T) {
	// lastPT <= target previously made findCutoffIndex return -1 forever, so
	// eco ticks logged "no satisfying cutoff ... skip" and never compacted a
	// giant never-compacted conversation. The char fallback must return a
	// real cutoff that advances past the existing checkpoint while always
	// preserving the newest user turn.
	mk := func(roles []string) []utils.Message {
		var msgs []utils.Message
		for _, r := range roles {
			msgs = append(msgs, utils.Message{Role: r, Content: utils.NewTextContent("x")})
		}
		return msgs
	}
	// Shape: checkpoint pair at 0/1, then several OLD turns whose tool walls
	// make the raw history dwarf the target budget, then a small newest user
	// turn at the very end (the realistic "giant never-compacted" shape).
	msgs := mk([]string{"assistant", "tool"}) // tidx 0,1 (existing checkpoint)
	for turn := 0; turn < 8; turn++ {
		msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("old ask %d", turn))})
		for i := 0; i < 30; i++ {
			msgs = append(msgs,
				utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("A", 500))},
				utils.Message{Role: "tool", Content: utils.NewTextContent(strings.Repeat("B", 500)), ToolCallID: fmt.Sprintf("t%d_%d", turn, i)},
			)
		}
	}
	newestUser := len(msgs)
	msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent("NEWEST ASK")})

	// lastPT (100) <= target (5000 → 20k chars) while the raw history is
	// ~240k chars — the exact "no satisfying cutoff ... skip" every-tick
	// shape (observed live: raw history ≈1.75M chars vs a ~100k-token
	// target). The char fallback must return a real cutoff.
	cut := findCutoffIndex(msgs, 100, 5000, 1)
	if cut <= 1 {
		t.Fatalf("expected char-fallback cutoff to advance past checkpoint (idx 1), got %d (len=%d)", cut, len(msgs))
	}
	if cut <= 1 || cut >= len(msgs) {
		t.Fatalf("cutoff must leave live messages, got %d (len=%d)", cut, len(msgs))
	}
	if cut > newestUser {
		t.Fatalf("cutoff %d must never fold the newest user turn (idx %d)", cut, newestUser)
	}
	if msgs[cut].Role == "tool" {
		t.Fatalf("kept tail must not start on a tool row, got index %d role %q", cut, msgs[cut].Role)
	}
}

// TestFilterCheckpointsForRequest_OversizedCheckpointSummary pins the
// "eco compaction makes conversation lose ALL context" bug. When the
// checkpoint summary (carried in the checkpoint pair's tool-role message)
// is far larger than the model's context window — e.g. after durable
// compaction folded a huge history whose summarisation output exceeded the
// window — the very next request dies with context_length_exceeded, the
// LLM loop then compact-durably folds the live tail into a NEW summary, and
// a restarted session reloads only the checkpoint: the summary is the ONLY
// surviving context. With a 150k-char summary on a 65k-token window (max
// tail ≈16k chars), the filter must truncate the summary to the budget so
// the (much smaller) live tail still fits the window.
func TestFilterCheckpointsForRequest_OversizedCheckpointSummary(t *testing.T) {
	hugeSummary := strings.Repeat("S", 150_000)
	msgs := []utils.Message{
		checkpointMsg(),
		{Role: "tool", Content: utils.NewTextContent(hugeSummary), ToolCallID: "eco_regression", Name: db.EcoCheckpointToolName},
		{Role: "user", Content: utils.NewTextContent("continue the work")},
	}
	got := filterCheckpointsForRequest(msgs, true, 65_536)
	if len(got) == 0 {
		t.Fatal("expected a non-empty request")
	}
	// The summary tool message must be truncated to the budget, not shipped
	// whole (150k chars >> 16k budget for a 65k-token window).
	for _, m := range got {
		if m.Role == "tool" && m.Name == db.EcoCheckpointToolName {
			if len(m.TextContent()) > maxTailChars(65_536)+200 {
				t.Fatalf("checkpoint summary not truncated to budget: %d chars (budget %d)", len(m.TextContent()), maxTailChars(65_536))
			}
			if !strings.Contains(m.TextContent(), "truncated by Plurality") {
				t.Fatal("expected truncation banner on oversized checkpoint summary")
			}
			found := false
			for _, tailM := range got {
				if tailM.Role == "user" && tailM.TextContent() == "continue the work" {
					found = true
				}
			}
			if !found {
				t.Fatal("live tail (user message) must survive alongside the truncated summary")
			}
		}
	}
}

// TestCapCheckpointSummaryForWindow_SmallWindow pins the helper directly on
// a small-window model: the summary must be bounded to the model's tail
// budget even when the caller passes no explicit budget.
func TestCapCheckpointSummaryForWindow_SmallWindow(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		{Role: "tool", Content: utils.NewTextContent(strings.Repeat("S", 200_000)), ToolCallID: "eco_regression", Name: db.EcoCheckpointToolName},
	}
	out := capCheckpointSummaryForWindow(msgs, 65_536, -1)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
	for _, m := range out {
		if m.Role == "tool" && m.Name == db.EcoCheckpointToolName {
			if len(m.TextContent()) > maxTailChars(65_536)+200 {
				t.Fatalf("summary not bounded for 65k window: %d chars", len(m.TextContent()))
			}
		}
	}
}
