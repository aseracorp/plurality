package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// pairFixture builds the live slice shape of conv 9f0ab2352bf2bdc40e77384b on
// 2026-11-26: TWO checkpoint pairs in one conversation.
//
// The first is the DURABLE FOLD's: an assistant row whose tool call id starts
// "eco_" (insertCheckpointPairInTx mints it), carrying the real roll-up
// (228292 chars stored, ~76000 tokens).
//
// The second was issued by the MODEL: the model called eco_checkpoint, the
// backend answered with its "tool not advertised in this request" error string
// (319 chars), and that [assistant, tool] exchange satisfies
// IsCheckpointMessage — its tool_call_id is backend-generated
// ("call_6125_call_af1c62b4c8284e118e450f88").
//
// Both halves sit at compacted = 0, and the model pair is NEWER, so a
// "last pair wins" selection picks the 319-char error and drops the 228292-char
// roll-up from the request. That is the live 24K-token prompt: the provider
// reported prompt_tokens=24531 while the fold's own log claimed a ~71904-token
// view, because ~107 tokens of archived history reached the model — 0.05 % of
// the 204166 tokens the fold had compressed.
func pairFixture(t *testing.T) []utils.Message {
	t.Helper()
	rollup := strings.Repeat("R", 60_000) // the fold's record of 5372 archived rows
	modelErr := "Error: tool 'eco_checkpoint' cannot be executed by this client: " +
		"it was not advertised in this request (the model may have invented the name)."
	mk := func(role, callID, name, content string, calls ...utils.ToolCall) utils.Message {
		m := utils.Message{Role: role, Content: utils.NewTextContent(content),
			ToolCallID: callID, Name: name}
		m.ToolCalls = calls
		return m
	}
	toolCall := func(id string) utils.ToolCall {
		return utils.ToolCall{ID: id, Type: "function",
			Function: utils.FunctionCall{Name: db.EcoCheckpointToolName, Arguments: "{}"}}
	}
	msgs := []utils.Message{
		mk("assistant", "", "", "", toolCall("eco_aaaa")),
		mk("tool", "eco_aaaa", db.EcoCheckpointToolName, rollup),
		mk("assistant", "", "", "", toolCall("call_6125_call_af1c62b4")),
		mk("tool", "call_6125_call_af1c62b4", db.EcoCheckpointToolName, modelErr),
	}
	// The live tail after the newest pair: real content the fold left unarchived.
	for i := 0; i < 40; i++ {
		msgs = append(msgs,
			mk("assistant", "", "", "working on the task",
				utils.ToolCall{ID: "c", Type: "function", Function: utils.FunctionCall{Name: "x"}}),
			mk("tool", "c", "x", strings.Repeat("t", 3000)))
	}
	return msgs
}

// The predicate itself: fold pair yes, model pair no, on both halves.
func TestFoldPairPredicateSeparatesFoldPairFromModelPair(t *testing.T) {
	msgs := pairFixture(t)
	if !db.IsFoldCheckpointMessage(msgs[0]) || !db.IsFoldCheckpointMessage(msgs[1]) {
		t.Fatalf("the fold's pair (call id prefix %q) must satisfy IsFoldCheckpointMessage",
			db.FoldCheckpointCallIDPrefix)
	}
	if !db.IsCheckpointMessage(msgs[2]) || !db.IsCheckpointMessage(msgs[3]) {
		t.Fatalf("a model-issued pair must still be a checkpoint pair — the client must never see it")
	}
	if db.IsFoldCheckpointMessage(msgs[2]) || db.IsFoldCheckpointMessage(msgs[3]) {
		t.Fatalf("a model-issued eco_checkpoint exchange is NOT the fold's pair: rewriting it would " +
			"discard the roll-up that is the only record of every compacted row")
	}
}

// The request path: the frozen head anchors on the FOLD's pair, so the roll-up
// ships. Pre-fix, slicing from the newest pair shipped the 319-char error and
// the model answered from thin air while the client showed the full transcript.
func TestRequestPathShipsTheFoldsRollUp(t *testing.T) {
	msgs := pairFixture(t)
	rollupChars := msgs[1].Content.TotalContentChars()

	for _, ecoOn := range []bool{true, false} {
		view := filterCheckpointsForRequest(msgs, ecoOn, 262_144)
		shipped := 0
		for _, m := range view {
			if m.Role == "tool" && db.IsFoldCheckpointMessage(m) {
				shipped += m.Content.TotalContentChars()
			}
		}
		if shipped != rollupChars {
			t.Fatalf("ecoOn=%v: the fold's roll-up shipped %d chars, want %d — the archived history is "+
				"the ONLY representation of the compacted rows, so dropping it is silent context loss "+
				"(live: prompt_tokens=24531 against a fold that logged a ~71904-token view)",
				ecoOn, shipped, rollupChars)
		}
		if len(view) < 20 {
			t.Fatalf("ecoOn=%v: shipped view is %d rows — the live tail after the fold pair must ride along",
				ecoOn, len(view))
		}
	}
	// The prior summary the fold feeds forward is the ROLL-UP, not the error.
	if got := PriorSummaryFromView(msgs); len(got) != rollupChars {
		t.Fatalf("PriorSummaryFromView returned %d chars, want the roll-up's %d — the next fold would "+
			"compress the whole tail into the 319-char error string", len(got), rollupChars)
	}
	if !hasCheckpointPair(msgs) {
		t.Fatalf("a conversation with the fold's pair has a pair")
	}
}

// A model-issued pair is conversation content, not a checkpoint, on the request
// path: it stays in the live rows (and FilterOutCheckpoints still hides it from
// the client). Dropping it is what made the 24K prompt: the archived history
// sat BEFORE it in the slice.
func TestModelIssuedPairDoesNotTruncateTheView(t *testing.T) {
	msgs := pairFixture(t)
	view := filterCheckpointsForRequest(msgs, true, 262_144)
	// The view STARTS at the fold's pair — the archived history is the head.
	if view[0].Role != "assistant" || !db.IsFoldCheckpointMessage(view[0]) {
		t.Fatalf("the shipped view starts at %q, want the fold's assistant half", view[0].Role)
	}
	if view[1].Content.TotalContentChars() != msgs[1].Content.TotalContentChars() {
		t.Fatalf("the view's second row is not the roll-up")
	}
	// The model-issued exchange is kept as content: it sits AFTER the roll-up, so
	// treating it as the head is what truncated 76000 tokens of archived history.
	if view[2].Role != "assistant" || !db.IsCheckpointMessage(view[2]) || db.IsFoldCheckpointMessage(view[2]) {
		t.Fatalf("the model-issued pair must ride in the live rows after the fold's roll-up")
	}
	// FilterOutCheckpoints (the client view) hides BOTH kinds.
	if n := len(db.FilterOutCheckpoints(msgs)); n != len(msgs)-4 {
		t.Fatalf("FilterOutCheckpoints removed %d rows, want 4 (both pairs hidden from the client)", len(msgs)-n)
	}
}
