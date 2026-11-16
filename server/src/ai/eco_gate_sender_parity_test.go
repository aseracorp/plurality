package ai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The end-to-end signature of the bug, in the two numbers the server logged on
// the same turn:
//
//	[Eco] content ~114829 tokens <= trigger 150000 -- sending unchanged
//	[Eco] compression-aware guard: est 36459 tokens / factor 1.08 = ...
//
// The eco gate (SentViewTokens + overhead) measures the conversation, including
// the checkpoint roll-up at full size — boundedWireChars exempts it from the
// per-message cap precisely because the sender does. The guard measures the
// marshalled request AFTER PrepareMessagesForAI. When the indexer treats the
// roll-up as a stale attachment, the two measure different requests and the gap
// is the summary: 114829 - 36459 = 78370 tokens, and 235059/3 = 78353.
//
// The provider prompt for that turn was 27739 tokens, i.e. the request that
// reached the model carried the system block and the live tail and NOTHING
// representing 102 folded messages plus every earlier fold.
//
// This test pins the invariant the whole eco design rests on: the size the gate
// budgets against and the size that actually leaves must be the same request.
func TestEcoGateAndSenderMeasureTheSameRequest(t *testing.T) {
	registerActionModelForAttachmentsTest(t, "test-action-model")
	model := utils.Model{Name: "test-action-model"}

	summaryChars := 235_059
	conv := checkpointPairMessages(summaryChars)

	// --- gate side: exactly what index.go computes for the trigger ---
	gate := SentViewTokens(conv) + 512 // overhead without tools/system, as index.go does

	// --- sender side: what leaves, measured as the preemptive guard does ---
	all := append([]utils.Message{{Role: "system", Content: utils.NewTextContent(strings.Repeat("S", 4000))}}, conv...)
	prepared, _, _ := PrepareMessagesForAI(all, model)
	prepared = stripImagesForNonVisionModel(prepared)
	converted, _ := convertMessagesToOpenAI(prepared, model)
	req := StandardChatRequest{Model: model.Name, Messages: converted, Stream: true}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	guard := buf.Len() / 3

	// The roll-up must still be on the wire.
	if !strings.Contains(buf.String(), "CHECKPOINT-ROLLUP") {
		t.Fatalf("the checkpoint roll-up is not in the marshalled request — the model receives no representation of folded history (wire %d bytes, gate %d, guard %d)",
			buf.Len(), gate, guard)
	}

	// Gate and guard must agree within the system block + JSON plumbing. A gap
	// the size of the summary means one of the two is measuring a request that
	// never leaves.
	drift := gate - guard
	if drift > summaryChars/3/4 {
		t.Errorf("eco gate and sender disagree by %d tokens (gate %d, guard %d, summary %d chars = %d tokens): the gate folds history to fit a request the sender then shrinks by deleting the summary",
			drift, gate, guard, summaryChars, summaryChars/3)
	}
	t.Logf("gate %d tokens, guard %d tokens, drift %d (summary %d chars = %d tokens)",
		gate, guard, drift, summaryChars, summaryChars/3)
}
