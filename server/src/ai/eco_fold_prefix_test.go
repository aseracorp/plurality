package ai

import (
	"bytes"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// eco_fold_prefix_test.go — the contract of the fold's STAGE 1 request shape
// (buildSummaryRequestFromView), per the owner: the LLM call must not rewrite
// the context, it APPENDS its instructions, so the prefill cache is reused one
// last time before the compacted answer replaces the history.
//
// The tests here are the positive proof of that: the summary request's messages
// are the CHAT request's messages, byte for byte, with exactly ONE row added at
// the END. Anything removed, reordered or rewritten in the middle voids the
// cache the shape exists to hit.

func fpRows(turns, chars int) []utils.Message {
	out := []utils.Message{{Role: "user", Content: utils.NewTextContent("fp start")}}
	for i := 0; i < turns; i++ {
		out = append(out, utils.Message{Role: "user",
			Content: utils.NewTextContent("fp ask " + itoa(i) + " " + strings.Repeat("q", chars))})
		out = append(out, ucTurn(i, "system_tools__shell_exec",
			"fp file=/srv/app/module_"+itoa(i)+".go "+strings.Repeat("w", chars))...)
	}
	return out
}

// TestFoldPrefixReplayIsTheChatRequestPlusOneRow: the whole point of stage 1.
func TestFoldPrefixReplayIsTheChatRequestPlusOneRow(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "fp-prefix-model", window262k, map[string]string{"system_tools__shell_exec": "on"})
	hist := fpRows(10, 700)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}
	chatNoClock := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))

	req, err := buildSummaryRequestFromView(ctx, conv, model, payload, foldStageCompress)
	if err != nil {
		t.Fatalf("stage 1 request: %v", err)
	}

	// APPEND-ONLY: same rows, in the same order, plus exactly one at the end.
	if len(req.Messages) != len(chatNoClock)+1 {
		t.Fatalf("stage 1 ships %d rows, want the chat request's %d plus exactly one appended",
			len(req.Messages), len(chatNoClock))
	}
	got := ucRowsBytes(t, req.Messages[:len(chatNoClock)])
	want := ucRowsBytes(t, chatNoClock)
	if !bytes.Equal(got, want) {
		t.Fatalf("stage 1 rewrote the prefix (summary %d bytes, chat %d bytes): first divergence at byte %d\n--- summary ---\n%s\n--- chat ---\n%s",
			len(got), len(want), firstDivergence(got, want), headRunes(got, 300), headRunes(want, 300))
	}
	// The tool schemas ride at the head of the prompt: changing them is a
	// head move, and a head move is a new cache entry.
	if !bytes.Equal(marshalNoEscape(chat.request.Tools), marshalNoEscape(req.Tools)) {
		t.Fatal("stage 1 changed the tool schemas — the tool block renders at the head of the prompt")
	}
	// The appended row: a user turn carrying the brief, and nothing else.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("the appended brief row is %q, want user", last.Role)
	}
	brief, _ := last.Content.(string)
	for _, want := range []string{"EVICTED", "COMPRESS", "PAYLOAD FLOOR", "COMPRESSION STYLE"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the stage-1 brief is missing %q:\n%s", want, headRunes([]byte(brief), 400))
		}
	}
	// Stage 1 must NOT be told to drop anything: dropping is stage 2's job.
	if strings.Contains(brief, "NO LONGER IN ACTIVE USE") {
		t.Fatal("the stage-1 brief tells the model to drop context: that instruction belongs to stage 2")
	}
}

// TestFoldStage2BriefHasTheTwoInstructions: the owner's two extra instructions,
// stated on the wire.
func TestFoldStage2BriefHasTheTwoInstructions(t *testing.T) {
	brief := stage2SummarizeInstruction("PRIOR-ROLLUP-MARKER", 42)
	for _, want := range []string{"SUMMARISE", "NO LONGER IN ACTIVE USE", "PAYLOAD FLOOR", "PRIOR-ROLLUP-MARKER", "42"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the stage-2 brief is missing %q:\n%s", want, headRunes([]byte(brief), 400))
		}
	}
	// The floor must outrank terseness, and it says so.
	if !strings.Contains(brief, "Never drop") {
		t.Fatal("the stage-2 brief has no anti-drop guard")
	}
}

// TestFoldStage1BriefForbidsJudgement: stage 1 compresses, it does not rank.
func TestFoldStage1BriefForbidsJudgement(t *testing.T) {
	brief := stage1CompressInstruction("", 7)
	for _, want := range []string{"COMPRESS", "Do NOT summarise", "PAYLOAD FLOOR", "last reader"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the stage-1 brief is missing %q:\n%s", want, headRunes([]byte(brief), 400))
		}
	}
}

// TestFoldPrefixReplayRefusesWhenThePrefixDoesNotFit: the shape is a cache hit
// or nothing. Truncating history to squeeze the call into the window would
// break the cache AND hide rows from the model asked to record them, so the
// builder refuses and the caller takes the budgeted shape instead.
func TestFoldPrefixReplayRefusesWhenThePrefixDoesNotFit(t *testing.T) {
	ctx, _, convID := ucDB(t)
	// A window the fixture cannot fit: the prefix replay must say so.
	model := ucSeedActionModel(t, "fp-narrow-model", 2048, map[string]string{"system_tools__shell_exec": "on"})
	hist := fpRows(10, 700)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	_, err := buildSummaryRequestFromView(ctx, conv, model, payload, foldStageCompress)
	if err == nil {
		t.Fatal("the prefix replay accepted a prefix that cannot fit the declared window")
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("expected a fit refusal, got: %v", err)
	}
}
