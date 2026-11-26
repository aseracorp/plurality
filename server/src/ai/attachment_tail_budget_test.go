package ai

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// Text attachment content belongs at the row that produced it.
//
// WHY THIS FILE EXISTS. #217 bounded the trailing attachment row to 32 KB.
// Measured on the deployed build after it shipped: the row was 32,521 bytes and
// the uncached cost sat at 10693-12361 tokens per turn, while each turn added
// 149-1715 tokens. 32521 / 3.0 bytes-per-token = ~10.8K tokens: the row itself,
// re-sent every turn. A row appended at the END of the array sits behind the
// append point, so bounding it capped the damage without ever moving the
// content inside the cached prefix.
//
// Upstream (azukaar/Plurality) has no trailing row: content ships in place at
// its own row. The fork added the row because upstream's staleness rule is an
// AGE term (user messages since, counted from the end), which flips a history
// row from content to placeholder mid-prefix and discards the whole
// conversation's cached KV. Dropping the age term is what makes in-place safe:
// size decides delivery, size is immutable, so a row renders identically the
// turn it is written and every turn after. The prefix only extends, and the
// tail carries only what the newest turn adds.
//
// What the trailing row keeps: IMAGES. Providers accept image parts in a user
// row and not in a tool row, so this row is the only legal carrier for them —
// and images are the vision contract, since a text retrieval cannot reproduce
// pixels.

// attAgenticConv builds the deployed shape: one user request, then `tools`
// assistant/tool pairs, each tool result carrying one text attachment of
// `size` bytes.
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

// TestTextAttachmentsNeverRideTheTrailingRow pins the fix for the deployed
// measurement above: text content in place, so the row cannot exist and the
// per-turn re-prefill with it.
func TestTextAttachmentsNeverRideTheTrailingRow(t *testing.T) {
	model := attActionModel(t, "att-inplace-tail")

	out, has, _ := PrepareMessagesForAI(attAgenticConv(10, 20*1024), model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	if bytes := trailingRowBytes(t, out); bytes > 0 {
		t.Errorf("a trailing row of %d bytes exists for a run of text attachments: that is the "+
			"~10K-token per-turn re-prefill this change removes (deployed: uncached 10693-12361, "+
			"with 149-1715 added by the turn itself)", bytes)
	}

	// Every attachment's content sits in the tool row that produced it.
	hist := stripTrailingAttachmentRow(out)
	for i := 0; i < 10; i++ {
		needle := fmt.Sprintf("S%04d", i)
		row := hist[3+2*i] // assistant at 2+2i, its tool result next
		if !strings.Contains(attBytes(t, row), needle) {
			t.Errorf("attachment %d content is not in its own tool row: the content is in flight, "+
				"not in place (row %d: %s)", i, 3+2*i, attSnip(attBytes(t, row), 0))
		}
	}
}

// TestAttachmentTailKeepsImages covers what the trailing row is for. Images are
// exempt from the text rules: a text retrieval cannot recover pixels, and a tool
// row cannot legally carry an image part.
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
		t.Fatalf("expected a trailing row carrying the fresh image")
	}
	if !strings.Contains(attBytes(t, *row), "data:image/png") {
		t.Errorf("the fresh image was evicted; images are the vision contract and must ride")
	}
	for i, m := range stripTrailingAttachmentRow(out) {
		if m.HasImages() {
			t.Errorf("history row %d (%s) carries an image in place", i, m.Role)
		}
	}
}

// TestAttachmentTailIsAppendOnly renders the same conversation at turn k and
// k+tools: history rows must stay byte-identical, the shorter render a byte
// prefix of the longer. That is the KV-cache property the design exists to get,
// and the one a trailing text row breaks.
func TestAttachmentTailIsAppendOnly(t *testing.T) {
	model := attActionModel(t, "att-budget-monotone")

	short, _, _ := PrepareMessagesForAI(attAgenticConv(4, 20*1024), model)
	long, _, _ := PrepareMessagesForAI(attAgenticConv(12, 20*1024), model)

	shortHist, longHist := stripTrailingAttachmentRow(short), stripTrailingAttachmentRow(long)
	if len(shortHist) > len(longHist) {
		t.Fatalf("history shrank %d -> %d rows", len(shortHist), len(longHist))
	}
	for i := range shortHist {
		if attBytes(t, shortHist[i]) != attBytes(t, longHist[i]) {
			t.Errorf("prefix break at history row %d: the in-place rendering is not a pure function "+
				"of stored state (an age term): %q -> %q",
				i, attSnip(attBytes(t, shortHist[i]), 0), attSnip(attBytes(t, longHist[i]), 0))
			return
		}
	}
}

// TestLargeAttachmentShipsInPlaceAtItsTurn pins the floor #217 established, in
// the shape the in-place design gives it. #217 had to exempt the newest
// attachment from the row's aggregate budget, because evicting it left the model
// with "a placeholder for the file it was just given". In place the same floor
// is a per-part cap: under the cap the content is in its row from its very
// first turn, and the cap applies only to pathological bulk — which is what the
// retrieval tools exist for.
func TestLargeAttachmentShipsInPlaceAtItsTurn(t *testing.T) {
	model := attActionModel(t, "att-inplace-large")

	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "read the report"},
			attSnippet(0, 40*1024),
		})},
	}
	out, has, _ := PrepareMessagesForAI(msgs, model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	body := attBytes(t, out[1])
	if !strings.Contains(body, "S0000S0000S0000") {
		t.Errorf("the attachment is not readable at the turn it was given: the model is left with "+
			"a placeholder for the file it was just handed: %s", attSnip(body, 0))
	}
	if bytes := trailingRowBytes(t, out); bytes > 0 {
		t.Errorf("a 40 KB text attachment moved into a trailing row (%d bytes): in place the bytes "+
			"are prefix and cost once; in a row appended at the end they cost every turn", bytes)
	}
}

// TestOversizedAttachmentIsAFetchPointer covers the cap side: a part above
// attachmentInlineCapBytes is a fetch placeholder from its FIRST turn, so the
// decision is stable forever and never flips mid-history.
func TestOversizedAttachmentIsAFetchPointer(t *testing.T) {
	model := attActionModel(t, "att-inplace-oversize")

	// Exact size: attSnippet(id, size) emits 5 chars per 6 requested, so ask
	// for the size in units of its own arithmetic and land above the cap.
	part := utils.ContentPart{
		Type:     "text",
		Text:     strings.Repeat("S0000", (attachmentInlineCapBytes+8*1024)/5),
		Filename: "config_dump.txt",
	}
	if len(part.Text) <= attachmentInlineCapBytes {
		t.Fatalf("fixture below the cap: %d bytes", len(part.Text))
	}
	build := func(turns int) []utils.Message {
		msgs := []utils.Message{
			{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
			{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
				{Type: "text", Text: "the whole config dump"}, part,
			})},
		}
		for i := 0; i < turns; i++ {
			msgs = append(msgs, attAssistantText("a"+itoaTest(i)), attUserText("q"+itoaTest(i)))
		}
		return msgs
	}

	now, _, _ := PrepareMessagesForAI(build(0), model)
	later, _, _ := PrepareMessagesForAI(build(9), model)
	if attBytes(t, now[1]) != attBytes(t, later[1]) {
		t.Errorf("the oversized row churns between turns: a size cap that moves with age is the "+
			"age-window defect in a different shape (%q -> %q)",
			attSnip(attBytes(t, now[1]), 0), attSnip(attBytes(t, later[1]), 0))
	}
	row := attBytes(t, now[1])
	if !strings.Contains(row, "conversation_attachments") {
		t.Errorf("the oversized row must name the tool that can read it: %.300s", row)
	}
}

func itoaTest(n int) string { return strconv.Itoa(n) }
