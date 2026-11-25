package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The trailing attachment row must be BOUNDED.
//
// WHY THIS TEST EXISTS. The trailing row carries the real content of every
// attachment the delivery rule calls fresh, and it is appended at the END of
// the outgoing slice. In an agentic run it never shrinks: the freshness window
// is measured in USER messages, and an agentic run is one user message with
// dozens of assistant/tool rows behind it, so a tool row's userMessagesSince is
// 0 for the entire run and every window (1, 2, 3) passes. Measured on the
// deployed build, every turn of this conversation logged
//
//	[Attachments] Found 29 attachment(s) in conversation
//	[Attachments] Indexed and removed 29 attachment(s) (254 KB saved)
//
// and the row this turn shipped carries 28 attachments / ~157 KB of prior tool
// output. That is ~45K tokens re-sent on every turn of the loop, for content
// the model can always re-read through conversation_attachments.
//
// This is a TOKEN cost, not a cache-bust claim: cache_read advances on a
// 32768-token block ladder (measured: 21541 -> 54309, 33213 -> 65981 -> 98749,
// every step exactly 32768), so a flat cache_read across turns is quantization,
// not churn. The row's cost stands on its own size.
//
// The bound keeps the NEWEST content (what the loop just produced) and evicts
// the oldest, so the row degrades to a retrieval, never to missing information.

// attAgenticConv builds the deployed shape: one user request, then `tools`
// assistant/tool pairs, each tool result carrying one text attachment of
// `size` bytes. All the attachments sit in the last `tools` rows, so all of
// them are fresh by the row window.
func attAgenticConv(tools, size int) []utils.Message {
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		attUserText("run the task"),
	}
	for i := 0; i < tools; i++ {
		msgs = append(msgs,
			attAssistantCall("c"+itoaTest(i), "system_tools__shell_exec"),
			attToolResult("c"+itoaTest(i), "system_tools__shell_exec", attSnippet(i, size)),
		)
	}
	return msgs
}

func attAssistantCall(id, tool string) utils.Message {
	return utils.Message{
		Role: "assistant",
		ToolCalls: []utils.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: utils.FunctionCall{Name: tool, Arguments: "{}"},
		}},
	}
}

func attToolResult(id, name string, part utils.ContentPart) utils.Message {
	return utils.Message{
		Role:       "tool",
		Name:       name,
		ToolCallID: id,
		Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "output " + id},
			part,
		}),
	}
}

// trailingRowBytes is the shipped size of the trailing attachment row.
func trailingRowBytes(t *testing.T, msgs []utils.Message) int {
	t.Helper()
	row := trailingAttachmentRow(msgs)
	if row == nil {
		return 0
	}
	return len(attBytes(t, *row))
}

// TestAttachmentTailIsBounded renders the deployed shape (10 tool results,
// 20 KB of attachment content each = 200 KB fresh) and requires the trailing
// row to stay near the budget instead of shipping all of it.
func TestAttachmentTailIsBounded(t *testing.T) {
	model := attActionModel(t, "att-budget-model")

	out, has, _ := PrepareMessagesForAI(attAgenticConv(10, 20*1024), model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	row := trailingAttachmentRow(out)
	if row == nil {
		t.Fatalf("expected a trailing attachment row")
	}

	got := trailingRowBytes(t, out)
	// Budget + the per-attachment labels + the head banner. The bound is on
	// content; labels are metadata and stay.
	limit := attachmentTailBudgetChars + 8*1024
	if got > limit {
		t.Errorf("trailing row shipped %d bytes, over the %d-byte bound: the row re-ships "+
			"prior tool output every turn of the loop (deployed: 29 attachments / 254 KB)", got, limit)
	}

	// The newest attachment must survive: the bound evicts the OLDEST, so the
	// content the loop just produced is always present.
	body := attBytes(t, *row)
	if !strings.Contains(body, "S0009") {
		t.Errorf("the newest attachment was evicted; eviction must be oldest-first, "+
			"the loop's latest output is the content that matters:\n%s", body[:attMin(len(body), 400)])
	}
	if strings.Contains(body, "S0000") {
		t.Errorf("the oldest attachment survived while the row is over budget: "+
			"eviction is oldest-first, so it should be the first to go (row=%d bytes)", got)
	}

	// The in-place history is untouched by the bound: every attachment row
	// keeps its stable placeholder, never the content.
	for _, m := range stripTrailingAttachmentRow(out) {
		if strings.Contains(attBytes(t, m), "S0000S0000S0000") {
			t.Errorf("an attachment's content shipped IN PLACE: the bound must apply to the "+
				"trailing row only, the in-place view is the prompt prefix: %s", attBytes(t, m)[:attMin(300, len(attBytes(t, m)))])
		}
	}
}

// TestAttachmentTailKeepsImages covers the exemption: images are the vision
// contract (a text-only retrieval cannot recover them), so the content budget
// must not evict a fresh image.
func TestAttachmentTailKeepsImages(t *testing.T) {
	model := attActionModel(t, "att-budget-images")

	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "look at this"},
			attImage(1),
		})},
	}
	out, has, _ := PrepareMessagesForAI(msgs, model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	row := trailingAttachmentRow(out)
	if row == nil {
		t.Fatalf("expected a trailing attachment row carrying the image")
	}
	if !strings.Contains(attBytes(t, *row), "data:image/png") {
		t.Errorf("the content budget evicted a fresh image; images are exempt because a "+
			"text-only model cannot retrieve them later")
	}
}

// TestAttachmentTailBudgetIsMonotone renders the same conversation at turn k
// and k+5: the history rows must stay byte-identical (the bound touches only
// the tail), which is the property #216 relies on.
func TestAttachmentTailBudgetIsMonotone(t *testing.T) {
	model := attActionModel(t, "att-budget-monotone")

	short, _, _ := PrepareMessagesForAI(attAgenticConv(4, 20*1024), model)
	long, _, _ := PrepareMessagesForAI(attAgenticConv(12, 20*1024), model)

	shortHist, longHist := stripTrailingAttachmentRow(short), stripTrailingAttachmentRow(long)
	if len(shortHist) > len(longHist) {
		t.Fatalf("history shrank %d -> %d rows", len(shortHist), len(longHist))
	}
	for i := range shortHist {
		if attBytes(t, shortHist[i]) != attBytes(t, longHist[i]) {
			t.Fatalf("prefix break at history row %d: the tail bound changed an in-place row", i)
		}
	}
}

func attMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// itoaTest avoids pulling strconv into a fixture helper.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
