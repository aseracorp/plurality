package ai

import (
	"bytes"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// eco_summarize_strip_test.go — the summariser request contract, per the owner:
//
//	No request, including the summariser, may modify the request head without
//	user interaction. Only strip the live tail. The summariser summarises the
//	OLDEST context and keeps the rest out of the request. It must never be
//	handed more history than fits the context window.
//
// Helpers are prefixed st_ to avoid the package's existing mkMsg / checkpointMsg.

// stRows: n turns, each ~chars, tagged so position is recoverable.
func stRows(turns, chars int) []utils.Message {
	out := []utils.Message{{Role: "user", Content: utils.NewTextContent("st start")}}
	for i := 0; i < turns; i++ {
		out = append(out, utils.Message{Role: "user",
			Content: utils.NewTextContent("st ask " + itoa(i) + " " + strings.Repeat("q", chars))})
		out = append(out, ucTurn(i, "system_tools__shell_exec",
			"st file=/srv/app/module_"+itoa(i)+".go "+strings.Repeat("w", chars))...)
	}
	return out
}

// TestSummariserRequestHeadIsByteIdenticalToChatRequest: the system rows and the
// tool schemas of the summariser request are the chat request's bytes. Dropping
// the schemas (as an earlier draft of this fix did) moves the tool block, which
// renders at the START of the prompt — that is the head, and it starts a new KV
// cache entry.
func TestSummariserRequestHeadIsByteIdenticalToChatRequest(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "st-head-model", window262k, map[string]string{"system_tools__shell_exec": "on"})
	hist := stRows(12, 900)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}
	evicted := view[:len(view)-4]
	req, err := buildSummaryRequestFromView(ctx, conv, model, evicted, payload)
	if err != nil {
		t.Fatalf("summary request: %v", err)
	}

	// The schemas are the chat request's array, byte for byte.
	if !bytes.Equal(marshalNoEscape(chat.request.Tools), marshalNoEscape(req.Tools)) {
		t.Fatal("the summariser request changed the tool schemas — the tool block renders at the head of the prompt")
	}
	if len(req.Tools) == 0 {
		t.Fatal("the summariser request ships no tools; the chat request does — that moves the prompt head")
	}

	// The system rows are the chat request's, byte for byte.
	chatFull := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))
	n := leadingSystemRows(chatFull)
	if n == 0 {
		t.Fatal("the chat request has no leading system rows — the fixture is not the production shape")
	}
	if !bytes.Equal(ucRowsBytes(t, req.Messages[:n]), ucRowsBytes(t, chatFull[:n])) {
		t.Fatalf("the summariser's system rows differ from the chat request's (divergence at %d) — the head moved",
			firstDivergence(ucRowsBytes(t, req.Messages[:n]), ucRowsBytes(t, chatFull[:n])))
	}
}

// TestSummariserStripsTheLiveTail: the request ships a PREFIX of the chat
// request's rows — the oldest, the evicted range — and NOT the live tail. The
// deployed failure was carrying the whole view: the window filled, the output
// budget floored, and every fold went machine-only without asking the model.
func TestSummariserStripsTheLiveTail(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "st-tail-model", window262k, map[string]string{"system_tools__shell_exec": "on"})
	hist := stRows(14, 900)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}
	chatFull := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))

	evicted := view[:len(view)-6]
	req, err := buildSummaryRequestFromView(ctx, conv, model, evicted, payload)
	if err != nil {
		t.Fatalf("summary request: %v", err)
	}
	shipped := len(req.Messages) - 1 // the instruction is the last row
	if shipped >= len(chatFull) {
		t.Fatalf("the request ships all %d chat rows — the live tail was not stripped", shipped)
	}
	// What it ships is a byte-identical PREFIX of the chat request.
	if !bytes.Equal(ucRowsBytes(t, req.Messages[:shipped]), ucRowsBytes(t, chatFull[:shipped])) {
		t.Fatalf("the shipped rows are not a prefix of the chat request (divergence at %d)",
			firstDivergence(ucRowsBytes(t, req.Messages[:shipped]), ucRowsBytes(t, chatFull[:shipped])))
	}
	// The tail is what is gone: the chat request's NEWEST history row must not
	// appear in the summariser request.
	newest, _ := chatFull[len(chatFull)-1].Content.(string)
	if newest == "" {
		t.Fatal("the chat request's newest history row has no text body — the fixture is not the production shape")
	}
	if strings.Contains(string(marshalNoEscape(req.Messages[:shipped])), strings.TrimSpace(newest)) {
		t.Fatal("the live tail is present in the summariser request")
	}
}

// TestSummariserNeverExceedsTheWindow: the evicted range is unbounded by
// construction (a deployed fold carried 1240 rows / 2824666 chars), so the
// summariser's INPUT is capped to the model's window. This is what makes the
// output budget non-collapsing.
func TestSummariserNeverExceedsTheWindow(t *testing.T) {
	const window = 16_384
	model := ucSeedActionModel(t, "st-cap-model", window, map[string]string{"system_tools__shell_exec": "on"})
	ctx, _, convID := ucDB(t)
	// Rows far past the window: ~40 turns x ~3000 chars ≈ 120k chars ≈ 40k tokens.
	hist := stRows(40, 1_400)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	evicted := view[:len(view)-2]
	if SentViewTokens(evicted) <= window {
		t.Fatalf("fixture is too small to exercise the cap: evicted %d tokens, window %d", SentViewTokens(evicted), window)
	}
	req, err := buildSummaryRequestFromView(ctx, conv, model, evicted, payload)
	if err != nil {
		t.Fatalf("summary request: %v", err)
	}

	input := renderedRowsTokens(req.Messages) + len(marshalNoEscape(req.Tools))/summaryCharsPerToken
	if input > window {
		t.Fatalf("the summariser request ships %d input tokens against a %d-token window — the cap did not apply", input, window)
	}
	// The budget must not collapse: that is the deployed defect.
	if req.MaxTokens == nil || *req.MaxTokens <= minSummaryOutputTokens {
		t.Fatalf("the summary budget collapsed (max_tokens=%v) for a %d-token window — the fold would go machine-only without asking the model",
			req.MaxTokens, window)
	}
	// The cap strips from the NEWEST end, so the head is untouched and the
	// request is still a prefix of the chat request.
	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}
	chatFull := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))
	shipped := len(req.Messages) - 1
	if shipped >= len(chatFull) {
		t.Fatalf("shipped %d rows, want fewer than the chat request's %d", shipped, len(chatFull))
	}
	if !bytes.Equal(ucRowsBytes(t, req.Messages[:shipped]), ucRowsBytes(t, chatFull[:shipped])) {
		t.Fatalf("the capped request is not a prefix of the chat request (divergence at %d) — the cap moved the head",
			firstDivergence(ucRowsBytes(t, req.Messages[:shipped]), ucRowsBytes(t, chatFull[:shipped])))
	}
}

// TestSummariserRefusesWhenTheHeadCannotFit: with a window too small for the
// system rows + schemas + instruction + reserves, NO history can be sent, so the
// builder must REFUSE rather than ship an over-window request. The fold then
// falls back to the machine-only render — a decision, not a provider 500.
func TestSummariserRefusesWhenTheHeadCannotFit(t *testing.T) {
	ctx, _, convID := ucDB(t)
	// A window smaller than the system prompt alone.
	model := ucSeedActionModel(t, "st-headrefuse-model", 512, map[string]string{"system_tools__shell_exec": "on"})
	hist := stRows(6, 900)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	_, err := buildSummaryRequestFromView(ctx, conv, model, view[:len(view)-2], payload)
	if err == nil {
		t.Fatal("the builder shipped a request whose own head exceeds a 512-token window")
	}
	if !strings.Contains(err.Error(), "does not fit the window") {
		t.Fatalf("want the head-does-not-fit refusal, got: %v", err)
	}
}
