package ai

import (
	"strings"
	"testing"
)

// The prefix-replay shape, asserted directly on the request buildSummaryRequestFromView
// posts. The end-to-end test above drives two folds and lands on the megacave-body
// fallback (its fold set is far bigger than the window, so the replay is refused);
// this test pins the PRIMARY path, the one that ships the conversation with the
// checkpoint pair at the head of the history.
//
// Two things are asserted, and both are load-bearing for the owner's cost rule:
//
//	a. the brief POINTS at the chain ("the archive in the tool result at the head
//	   of the history above") and does NOT re-quote it. Quoting the roll-up into
//	   the brief of a request that already ships it is prefill paid for a rule the
//	   model can read;
//	b. the output cap on the wire is the DELTA budget (bounded by the folded
//	   material), not the window-derived budget. At ~40 tokens/second that number
//	   is the wall-clock, and it is what makes re-emitting the chain unaffordable.
func TestFoldPrefixReplayPointsAtTheChainAndCapsTheAnswer(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-replay-shape", 262_144, nil)

	var posts []StandardChatRequest
	echo := &appendEchoRows{}
	srv := newAppendProbeProxy(t, &posts, "REPLAY-PRIOR-3a11", echo)
	oldBase, prevReady := LiteLLMBaseURL, litellmReady.Load()
	LiteLLMBaseURL = srv.URL
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	// Fold once so the conversation MINTS a checkpoint pair carrying a prior
	// roll-up: that is the state in which "skip the previous summaries" bites.
	gen := ucChainHistory("replay generation one", 40, 1_500)
	echo.rows = ucUserTexts(gen)
	ucPush(t, ctx, convID, gen...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	if _, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 4, ucTarget(conv, model)); err != nil {
		t.Fatalf("fold: %v", err)
	}
	prior := PriorSummaryFromView(ucLLMView(t, ctx, convID))
	if prior == "" {
		t.Fatal("no prior roll-up after the first fold: the fixture has nothing to skip")
	}
	if !strings.Contains(prior, "REPLAY-PRIOR-3a11") {
		t.Fatal("the prior is not the model's own checkpoint")
	}

	// Build the replay request for the CURRENT view (pair included).
	view = ucLLMView(t, ctx, convID)
	conv = ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}
	const ceiling, folded = 60_000, 9_000
	req, err := buildSummaryRequestFromView(ctx, conv, model, payload, foldStageCompress, ceiling, folded)
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}

	// Shape: more than head+1 row means the conversation is shipped, so this IS
	// the replay, and the model can read the stored chain in it.
	head := leadingSystemRows(req.Messages)
	if len(req.Messages) <= head+1 {
		t.Fatalf("not the replay shape (%d rows, head %d)", len(req.Messages), head)
	}
	brief := wireRowText(req.Messages[len(req.Messages)-1])
	if !strings.Contains(brief, "APPEND-ONLY CONTRACT") {
		t.Fatal("the replay brief lost the append-only contract")
	}
	if !strings.Contains(brief, "head of the history") {
		t.Fatalf("the replay brief must point at the chain it ships:\n%s", headRunes([]byte(brief), 500))
	}
	probe := prior
	if len(probe) > 160 {
		probe = probe[:160]
	}
	if strings.Contains(brief, probe) {
		t.Fatal("the replay brief re-quotes the prior: the request already ships it - pure prefill cost")
	}
	if !strings.Contains(string(marshalNoEscape(req.Messages)), "REPLAY-PRIOR-3a11") {
		t.Fatal("the replay does not ship the checkpoint pair: the model cannot see the chain, so the brief must quote it")
	}

	// The cap: the delta budget, bounded to the folded material, not the window.
	if req.MaxTokens == nil {
		t.Fatal("the replay request has no output cap")
	}
	want := deltaOutputTokens(folded, ceiling)
	if *req.MaxTokens > want {
		t.Fatalf("max_tokens %d exceeds the delta budget %d: a window-sized budget is an open invitation to replay the chain",
			*req.MaxTokens, want)
	}
	if *req.MaxTokens*summaryCharsPerToken >= folded+ceiling {
		t.Fatalf("the answer budget (%d chars) can write prior+new (%d chars): a 1:1 replay is affordable",
			*req.MaxTokens*summaryCharsPerToken, folded+ceiling)
	}
	// And it is a real budget, not a collapse to the floor: a fold that cannot
	// answer is a fold that goes machine-only.
	if *req.MaxTokens <= minSummaryOutputTokens {
		t.Fatalf("the delta budget collapsed to %d: the fold would refuse the model", *req.MaxTokens)
	}
	t.Logf("replay shape: %d rows shipped, prior %d chars visible, answer budget %d tokens (delta cap %d)",
		len(req.Messages), len(prior), *req.MaxTokens, want)
}
