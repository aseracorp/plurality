package ai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// Attachment delivery must be MONOTONE: the in-place bytes of a stored
// attachment row may never change between turns. The real content is confined
// to an append-only trailing row (the clock-tail pattern, see clock_tail.go),
// so the prompt prefix — system prompt + every history row — stays
// byte-identical and KV prompt-cache reuse survives. The old rule decided
// real-content-vs-placeholder from userMessagesSince (user messages counted
// FROM THE END), so an old attachment flipped to a placeholder mid-history and
// every byte from there on diverged.

// attActionModel registers a chat model with function calling, so delivery
// takes the action-model branch (placeholders + trailing row). Registered via
// ucSeedActionModel, which cleans up, so the registry singleton stays clean.
func attActionModel(t *testing.T, name string) utils.Model {
	t.Helper()
	return ucSeedActionModel(t, name, 128000, map[string]string{"sh": "shell"})
}

// attImage builds an inline data-URI image part big enough to be indexed
// (BuildAttachmentIndex skips inline content under 3KB).
func attImage(id int) utils.ContentPart {
	return utils.ContentPart{
		Type:     "image_url",
		ImageURL: &utils.ContentImageURL{URL: "data:image/png;base64," + strings.Repeat(fmt.Sprintf("IMG%04d", id), 600)},
	}
}

// attSnippet builds an inline text attachment above the 3KB indexing floor.
func attSnippet(id, size int) utils.ContentPart {
	return utils.ContentPart{
		Type:     "text",
		Text:     strings.Repeat(fmt.Sprintf("S%04d", id), size/6),
		Filename: fmt.Sprintf("notes_%d.txt", id),
	}
}

func attUserText(s string) utils.Message {
	return utils.Message{Role: "user", Content: utils.NewTextContent(s)}
}

func attAssistantText(s string) utils.Message {
	return utils.Message{Role: "assistant", Content: utils.NewTextContent(s)}
}

func attBytes(t *testing.T, m utils.Message) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// attRender renders the outgoing slice plus the "has attachments" flag.
func attRender(msgs []utils.Message, model utils.Model) ([]utils.Message, bool) {
	out, has, _ := PrepareMessagesForAI(msgs, model)
	return out, has
}

// attConvWithImage builds a conversation whose FIRST user row (index 1) carries
// one image attachment, followed by `turns` plain assistant/user exchanges.
func attConvWithImage(turns int) []utils.Message {
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "look at this screenshot"},
			attImage(1),
		})},
	}
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			attAssistantText(fmt.Sprintf("answer %d", i)),
			attUserText(fmt.Sprintf("follow-up %d", i)),
		)
	}
	return msgs
}

// trailingAttachmentRow returns the synthetic appended row (the LAST message,
// carrying the attachment markers), nil when there is none.
func trailingAttachmentRow(msgs []utils.Message) *utils.Message {
	if len(msgs) == 0 {
		return nil
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		return nil
	}
	if !strings.HasPrefix(last.TextContent(), "[Attachments attached for this turn") {
		return nil
	}
	return &last
}

// stripTrailingAttachmentRow returns the history rows (everything except a
// trailing synthetic attachment row).
func stripTrailingAttachmentRow(msgs []utils.Message) []utils.Message {
	if trailingAttachmentRow(msgs) == nil {
		return msgs
	}
	return msgs[:len(msgs)-1]
}

// THE DEFECT THIS FIXES. Render at the attachment's own turn (k) and five turns
// later (k+5): every history row at/below the attachment's index must be
// byte-identical (the in-place placeholder is stable from the FIRST request),
// and the SHORT render's history must be a byte-exact PREFIX of the longer one
// — nothing in the middle ever moves. The attachment's real content was
// available at the early render, in the ONE appended trailing row.
func TestAttachmentPlaceholderIsStableAcrossTurns(t *testing.T) {
	model := attActionModel(t, "att-stability-model")

	early, earlyHas := attRender(attConvWithImage(0), model)
	late, lateHas := attRender(attConvWithImage(5), model)

	if !earlyHas || !lateHas {
		t.Fatalf("attachments must be detected: early=%v late=%v", earlyHas, lateHas)
	}
	earlyHist, lateHist := stripTrailingAttachmentRow(early), stripTrailingAttachmentRow(late)

	// The attachment row is index 1 (right after the system prompt).
	const attIdx = 1
	if !strings.Contains(attBytes(t, earlyHist[attIdx]), "Attachment omitted") {
		t.Fatalf("attachment row must carry the placeholder at the FIRST render, not the image:\n%s", attBytes(t, earlyHist[attIdx]))
	}
	if strings.Contains(attBytes(t, earlyHist[attIdx]), "data:image/png") {
		t.Fatalf("attachment row still ships the image in place at the first render")
	}

	// Monotone history: the short render is a byte-exact prefix of the long one.
	if len(earlyHist) > len(lateHist) {
		t.Fatalf("history shrank %d -> %d", len(earlyHist), len(lateHist))
	}
	for i := range earlyHist {
		if attBytes(t, earlyHist[i]) != attBytes(t, lateHist[i]) {
			t.Fatalf("prefix break at history row %d (the attachment's index is %d): the in-place bytes are not monotone\nearly: %s\nlate:  %s",
				i, attIdx, attBytes(t, earlyHist[i]), attBytes(t, lateHist[i]))
		}
	}

	// The real content WAS available at the early render, as ONE appended row.
	earlyTail := trailingAttachmentRow(early)
	if earlyTail == nil {
		t.Fatalf("no trailing attachment row at the early render: the real content is missing")
	}
	if !strings.Contains(attBytes(t, *earlyTail), "data:image/png") {
		t.Fatalf("trailing row must carry the real image bytes: %s", attBytes(t, *earlyTail))
	}
	if earlyTail.Role != "user" {
		t.Fatalf("trailing row role is %q, expected user", earlyTail.Role)
	}
	if !strings.Contains(attBytes(t, *earlyTail), "att_0") {
		t.Fatalf("trailing row must reference the attachment ID: %s", attBytes(t, *earlyTail))
	}
	if len(early) != len(earlyHist)+1 {
		t.Fatalf("the trailing row must be exactly ONE appended row: %d vs %d", len(early), len(earlyHist)+1)
	}

	// Aged out: at the late render the content drops out of the trailing row
	// (the trailing region is tail bytes, expected to change) and the
	// attachment's placeholder is untouched.
	if tail := trailingAttachmentRow(late); tail != nil && strings.Contains(attBytes(t, *tail), "data:image/png") {
		t.Fatalf("aged-out attachment must drop out of the trailing row")
	}
	if !strings.Contains(attBytes(t, lateHist[attIdx]), "Attachment omitted") {
		t.Fatalf("placeholder must be identical at the late render: %s", attBytes(t, lateHist[attIdx]))
	}
}

// The placeholder bytes are metadata only: bounded per attachment, independent
// of the attachment's size. A 200KB snippet must not put 200KB of history bytes
// on the wire in place.
func TestAttachmentPlaceholderBytesAreBounded(t *testing.T) {
	model := attActionModel(t, "att-bound-model")

	msgs := []utils.Message{{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")}}
	const n = 6
	for i := 0; i < n; i++ {
		msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			attSnippet(i, 200*1024),
		})})
	}

	out, has, _ := PrepareMessagesForAI(msgs, model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	hist := stripTrailingAttachmentRow(out)
	total, count := 0, 0
	for _, m := range hist {
		for _, p := range m.ContentParts() {
			if p.Type == "text" && strings.Contains(p.Text, "Attachment omitted") {
				total += len(p.Text)
				count++
			}
		}
	}
	if count < n-1 {
		t.Fatalf("expected at least %d placeholders in the history, found %d", n-1, count)
	}
	if bound := count * 200; total > bound {
		t.Fatalf("placeholder bytes %d exceed the bound %d (%d bytes per placeholder)", total, bound, 200)
	}
}

// The 3KB-tiny-attachment keep-inline rule is pure and stable: the stored bytes
// ship verbatim in place, on every turn, and the row never churns.
func TestTinyAttachmentStaysInline(t *testing.T) {
	model := attActionModel(t, "att-tiny-model")

	tiny := utils.ContentPart{Type: "text", Text: strings.Repeat("t", 100), Filename: "tiny.txt"}
	early, _ := attRender(attConvTiny(0, tiny), model)
	late, _ := attRender(attConvTiny(5, tiny), model)

	for _, msgs := range [2][]utils.Message{early, late} {
		if !strings.Contains(attBytes(t, msgs[1]), strings.Repeat("t", 100)) {
			t.Fatalf("tiny attachment must stay inline: %s", attBytes(t, msgs[1]))
		}
	}
	if attBytes(t, early[1]) != attBytes(t, late[1]) {
		t.Fatalf("tiny attachment row churned across turns")
	}
	if trailingAttachmentRow(early) != nil {
		t.Fatalf("a sub-3KB attachment must not add a trailing row")
	}
}

func attConvTiny(turns int, tiny utils.ContentPart) []utils.Message {
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{tiny})},
	}
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			attAssistantText(fmt.Sprintf("a%d", i)),
			attUserText(fmt.Sprintf("q%d", i)),
		)
	}
	return msgs
}

// Assistant-side images are placeholders in place IMMEDIATELY, and their real
// content rides the trailing row on the turn that produced them.
func TestAssistantSideImageIsPlaceholderPlusTrailingRow(t *testing.T) {
	model := attActionModel(t, "att-assistant-image-model")

	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		attUserText("generate an image"),
		{Role: "assistant", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "here is the image"},
			attImage(2),
		})},
	}

	out, has, _ := PrepareMessagesForAI(msgs, model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	hist := stripTrailingAttachmentRow(out)
	row := attBytes(t, hist[2])
	if !strings.Contains(row, "Attachment omitted") {
		t.Fatalf("assistant-side image must be a placeholder in place: %s", row)
	}
	if strings.Contains(row, "data:image/png") {
		t.Fatalf("assistant-side image must not ship in place: %s", row)
	}
	tail := trailingAttachmentRow(out)
	if tail == nil || !strings.Contains(attBytes(t, *tail), "data:image/png") {
		t.Fatalf("assistant-side image must ride the trailing row on its own turn")
	}
	// Images never sit in an assistant/tool row on the wire: the trailing user
	// row is the only image carrier.
	for i, m := range hist {
		if m.HasImages() {
			t.Fatalf("history row %d (%s) still carries an image in place", i, m.Role)
		}
	}
}

// The conversation_attachments tool-result strip stays (its placeholder is
// fixed), and a FRESH recall result's images move to the trailing row — images
// cannot ship inside a tool row.
func TestRecallResultStripIsStable(t *testing.T) {
	model := attActionModel(t, "att-recall-model")

	build := func(turns int) []utils.Message {
		msgs := []utils.Message{
			{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
			{Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
				{Type: "text", Text: "read the report"},
				attSnippet(0, 40*1024),
			})},
			attAssistantText("I will fetch it"),
			{Role: "tool", Name: "conversation_attachments", ToolCallID: "call_1",
				Content: utils.NewPartsContent([]utils.ContentPart{
					{Type: "text", Text: "[Attachment att_0]:\n" + strings.Repeat("R", 5000)},
					attImage(3),
				})},
		}
		for i := 0; i < turns; i++ {
			msgs = append(msgs,
				attAssistantText(fmt.Sprintf("a%d", i)),
				attUserText(fmt.Sprintf("q%d", i)),
			)
		}
		return msgs
	}

	const stripText = "Attachment tool result omitted. Call conversation_attachments again if needed."

	// Fresh recall (no user message after it): the text body stays in the tool
	// row, the image moves to the trailing row.
	fresh, _ := attRender(build(0), model)
	freshHist := stripTrailingAttachmentRow(fresh)
	freshTool := attBytes(t, freshHist[3])
	if !strings.Contains(freshTool, strings.Repeat("R", 200)) {
		t.Fatalf("fresh recall text body must stay in the tool row: %.200s", freshTool)
	}
	if strings.Contains(freshTool, "data:image/png") {
		t.Fatalf("recall image must not ship in the tool row")
	}
	tail := trailingAttachmentRow(fresh)
	if tail == nil || !strings.Contains(attBytes(t, *tail), "data:image/png") {
		t.Fatalf("fresh recall image must ride the trailing row")
	}
	if !strings.Contains(attBytes(t, *tail), "att_0") {
		t.Fatalf("trailing row must reference the fresh snippet: %.300s", attBytes(t, *tail))
	}

	// Aged: the recall result carries the FIXED strip placeholder, the 40KB
	// snippet has dropped out of the trailing row (its window is 2), and the
	// strip bytes never move again — compare two aged renders.
	aged3, _ := attRender(build(3), model)
	aged6, _ := attRender(build(6), model)
	agedHist3, agedHist6 := stripTrailingAttachmentRow(aged3), stripTrailingAttachmentRow(aged6)
	if !strings.Contains(attBytes(t, agedHist3[3]), stripText) {
		t.Fatalf("aged recall result must carry the fixed strip placeholder: %.200s", attBytes(t, agedHist3[3]))
	}
	for i := 0; i <= 3; i++ {
		if attBytes(t, agedHist3[i]) != attBytes(t, agedHist6[i]) {
			t.Fatalf("prefix break at row %d between two aged renders: the strip/placeholder churned", i)
		}
	}
	// The 40KB snippet is a placeholder in place from the FIRST render, and its
	// row is byte-identical in all three renders.
	for i, hist := range [3][]utils.Message{freshHist, agedHist3, agedHist6} {
		if !strings.Contains(attBytes(t, hist[1]), "Attachment omitted") {
			t.Fatalf("render %d: the 40KB snippet must be a placeholder in place, not inline", i)
		}
		if attBytes(t, hist[1]) != attBytes(t, freshHist[1]) {
			t.Fatalf("render %d: the attachment row churned", i)
		}
	}
}

// Non-action models have no retrieval tool: images are stripped, text-based
// attachments stay inline. The in-place view is pure, so it stays stable.
func TestNonActionModelKeepsTextInlineStripsImages(t *testing.T) {
	model := utils.Model{Name: "att-non-action-model"} // not registered -> not an action model

	early, _ := attRender(attConvWithImage(1), model)
	late, _ := attRender(attConvWithImage(6), model)
	earlyHist, lateHist := stripTrailingAttachmentRow(early), stripTrailingAttachmentRow(late)

	for i := range earlyHist {
		if attBytes(t, earlyHist[i]) != attBytes(t, lateHist[i]) {
			t.Fatalf("non-action prefix break at row %d", i)
		}
	}
	if strings.Contains(attBytes(t, earlyHist[1]), "data:image/png") {
		t.Fatalf("non-action model must not receive an image in place")
	}
}

// estimateImageTokens must account for the images in the trailing row: they are
// the only images in the request, and a screenshot's base64 is the biggest
// single line item in the eco tail budget.
func TestEstimateImageTokensSeesTheTrailingRow(t *testing.T) {
	model := attActionModel(t, "att-estimate-model")

	out, has, _ := PrepareMessagesForAI(attConvWithImage(0), model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	if tokens := estimateImageTokens(out); tokens <= 0 {
		t.Fatalf("the trailing row's image must be counted, got %d", tokens)
	}
}
