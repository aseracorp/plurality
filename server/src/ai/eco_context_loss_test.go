package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// Regression tests for "aseracorp/Plurality loses context on compaction".
//
// Production evidence (conversation 9aa5f25c582ecd78f7991138, "context
// lost", server.log 2026-11-03T18:20:25):
//
//	[Eco] capping tail by budget: kept 22 msgs / 101122 chars (start idx 1)
//	[Eco] budget re-trim: dropping 21 oldest msgs / 103822 chars (kept 1 msgs / 1 chars)
//
// The model received a system prompt plus ONE user message containing ".",
// and answered "Hey — I got just a '.'". trimTailToFitBudget jumped
// user-boundary to user-boundary; that conversation had only three user
// messages, so a 2.7% budget overshoot deleted 99.99% of the context.

// contextLossShape rebuilds the exact message shape of the "context lost"
// conversation: a long task in the first user turn, one giant tool result
// wall, and bare "." user pings. Total content lands just over `budget`.
func contextLossShape(budget int) []utils.Message {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("TASK: prepare the remaining btrfs work on pve01 " + strings.Repeat("x", 1046))},
		{Role: "user", Content: utils.NewTextContent(".")},
	}
	// One giant tool dump (the 77k pnpm-store listing) + a tool wall,
	// mirroring seq 7..21 of the real conversation.
	msgs = append(msgs, utils.Message{Role: "assistant", Content: utils.NewTextContent("let me look")})
	msgs = append(msgs, utils.Message{Role: "tool", Name: "system_tools__shell_exec", Content: utils.NewTextContent(strings.Repeat("L", 77_066))})
	for i := 0; i < 13; i++ {
		msgs = append(msgs, utils.Message{Role: "tool", Name: "system_tools__shell_exec", Content: utils.NewTextContent(strings.Repeat("o", 2_050))})
	}
	// The bare ping that survived the old trim.
	msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent(".")})
	return msgs
}

func totalChars(msgs []utils.Message) int {
	n := 0
	for _, m := range msgs {
		n += m.Content.TotalContentChars()
	}
	return n
}

// TestTrimTailToFitBudget_ContextLoss is the direct repro of the "context
// lost" bug. The old loop dropped whole turns until the budget fit; with
// only three user turns spread across a 103k-char tail, the last legal drop
// left ONE message (1 char). The fix must keep the window near the budget
// and never gut the context.
func TestTrimTailToFitBudget_ContextLoss(t *testing.T) {
	budget := 100_000
	msgs := contextLossShape(budget)
	before := totalChars(msgs)
	if before <= budget {
		t.Fatalf("test shape must exceed the budget to exercise the trim (got %d)", before)
	}

	got := trimTailToFitBudget(msgs, budget)

	if len(got) <= 1 {
		t.Fatalf("context gutted: kept %d msgs / %d chars (the 'context lost' bug)", len(got), totalChars(got))
	}
	kept := totalChars(got)
	if kept < budget/2 {
		t.Fatalf("kept only %d chars of a %d-char budget: window starved the model", kept, budget)
	}
	if kept > budget+500 {
		t.Fatalf("kept %d chars exceeds budget %d: shrink-to-fit did not bound the window", kept, budget)
	}
	// The newest user turn (the request the model must answer) must survive.
	last := msgs[len(msgs)-1]
	found := false
	for _, m := range got {
		if m.Role == last.Role && m.Content.TextContent() == last.Content.TextContent() {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the newest user message must always be kept")
	}
	// Every kept row must be a real row: rows are shrunk, never dropped, so
	// the count can only shrink via whole-turn drops in phase A.
	if len(got) > len(msgs) {
		t.Fatalf("message count grew: %d -> %d", len(msgs), len(got))
	}
}

// TestTrimTailToFitBudget_DoesNotMutateInput pins the aliasing contract:
// trimTailToFitBudget receives tail[start:] of the caller's slice, and
// shrink-to-fit must not rewrite the caller's backing array (a request-time
// trim that permanently shrank the in-memory conversation would leak the
// elision into every later turn).
func TestTrimTailToFitBudget_DoesNotMutateInput(t *testing.T) {
	budget := 100_000
	msgs := contextLossShape(budget)

	// Give the trim a sub-slice, exactly like capTailBudget does.
	sub := msgs[1:]
	orig := make([]utils.Message, len(sub))
	copy(orig, sub)

	_ = trimTailToFitBudget(sub, budget)

	for i := range orig {
		if orig[i].Content.TotalContentChars() != sub[i].Content.TotalContentChars() {
			t.Fatalf("input row %d mutated by trim: %d -> %d chars",
				i, orig[i].Content.TotalContentChars(), sub[i].Content.TotalContentChars())
		}
		if orig[i].Content.TextContent() != sub[i].Content.TextContent() {
			t.Fatalf("input row %d content mutated by trim", i)
		}
	}
}

// TestShrinkTailToFitBudget_ProtectsLiveTurn asserts the newest user turn is
// never shrunk (it is the request the model must answer) and that rows are
// never removed, keeping assistant tool_calls ↔ tool result pairing valid.
func TestShrinkTailToFitBudget_ProtectsLiveTurn(t *testing.T) {
	toolCallID := "call_1"
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("do the thing")},
		{Role: "assistant", ToolCalls: []utils.ToolCall{{ID: toolCallID, Function: utils.FunctionCall{Name: "system_tools__shell_exec"}}}},
		{Role: "tool", ToolCallID: toolCallID, Name: "system_tools__shell_exec", Content: utils.NewTextContent(strings.Repeat("R", 200_000))},
		{Role: "user", Content: utils.NewTextContent("continue")},
	}
	protectedFrom := 3
	out := shrinkTailToFitBudget(msgs, 50_000, protectedFrom)

	if len(out) != len(msgs) {
		t.Fatalf("shrink-to-fit must never drop rows: %d -> %d", len(msgs), len(out))
	}
	if out[protectedFrom].Content.TextContent() != "continue" {
		t.Fatalf("the live user turn was modified: %q", out[protectedFrom].Content.TextContent())
	}
	if out[1].ToolCalls[0].ID != toolCallID {
		t.Fatal("assistant tool_calls lost during shrink")
	}
	if out[2].ToolCallID != toolCallID {
		t.Fatal("tool result pairing lost during shrink")
	}
	total := totalChars(out)
	if total > 50_000+200 {
		t.Fatalf("window not bounded: %d chars vs 50000 budget", total)
	}
	// The shrunk tool result keeps its per-message floor, not nothing.
	if got := out[2].Content.TotalContentChars(); got < minShrinkableMsgChars {
		t.Fatalf("tool result shrunk below the floor: %d chars", got)
	}
}

// TestShrinkMessageContent_KeepsHeadAndTail: for tool output the command
// (head) and the result/error (tail) carry the meaning; a pure head
// truncation loses the outcome, so both ends must survive.
func TestShrinkMessageContent_KeepsHeadAndTail(t *testing.T) {
	head := "COMMAND: ls -la /important"
	tail := "EXIT 2: no space left on device"
	m := utils.Message{Role: "tool", Name: "system_tools__shell_exec",
		Content: utils.NewTextContent(head + strings.Repeat("p", 100_000) + tail)}

	out, removed := shrinkMessageContent(m, 5_000)
	txt := out.Content.TextContent()
	if removed <= 0 {
		t.Fatal("expected characters to be elided")
	}
	if !strings.HasPrefix(txt, head) {
		t.Fatal("head of the message was not preserved")
	}
	if !strings.HasSuffix(txt, tail) {
		t.Fatal("tail of the message was not preserved (the tool outcome is the point)")
	}
	if len(txt) > 5_000 {
		t.Fatalf("shrink exceeded the keep budget: %d > 5000", len(txt))
	}
}

// TestShrinkMessageContent_PreservesNonTextParts: an image part must survive
// the shrink (rebuilding parts is easy to get wrong).
func TestShrinkMessageContent_PreservesNonTextParts(t *testing.T) {
	m := utils.Message{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
		{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: "data:image/png;base64,AAA"}},
		{Type: "text", Text: strings.Repeat("d", 50_000)},
	})}
	out, removed := shrinkMessageContent(m, 3_000)
	if removed <= 0 {
		t.Fatal("expected shrink")
	}
	if !out.HasImages() {
		t.Fatal("image part was dropped while shrinking the text")
	}
}

// TestFilterCheckpointsForRequestBudget_NeverGutsContext drives the real
// request path (the function SendChatCompletion calls) with the "context
// lost" shape and a small window: the emitted window must stay near the
// tail budget instead of collapsing to the newest ping.
func TestFilterCheckpointsForRequestBudget_NeverGutsContext(t *testing.T) {
	budget := 100_000
	msgs := contextLossShape(budget)

	out := filterCheckpointsForRequestBudget(msgs, true, 262_144, budget)
	if len(out) <= 1 {
		t.Fatalf("request window gutted to %d msgs / %d chars", len(out), totalChars(out))
	}
	kept := totalChars(out)
	if kept < budget/2 {
		t.Fatalf("window carries only %d chars against a %d-char budget", kept, budget)
	}
	if out[len(out)-1].Role != "user" {
		t.Fatalf("window must end on the live user turn, got %q", out[len(out)-1].Role)
	}
}
