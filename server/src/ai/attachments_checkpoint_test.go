package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The eco checkpoint pair is the ONLY representation a conversation keeps of
// the history durable compaction folded away (db.CompactConversationDurable
// marks those rows compacted = 1). PrepareMessagesForAI runs on every request
// and replaces "stale attachments" with a short placeholder. The checkpoint
// summary is a tool-role message whose content is one big text part, so the
// attachment indexer classified it as an attachment and the placeholder
// replaced the whole roll-up.
//
// Measured live, conversation 141c6b2d33616fed6f2dd8dc, 2026-11-16 15:35:15:
//
//	[Attachments] Found 1 attachment(s) in conversation
//	[Attachments] Indexed and removed 1 attachment(s) (230 KB saved)
//	[Eco] content ~114829 tokens <= trigger 150000 -- sending unchanged
//	[Eco] compression-aware guard: est 36459 tokens ...
//
// The "1 attachment / 230 KB" IS the checkpoint row: its content is 235059
// chars = 229.5 KB. The gate measures the summary in full (boundedWireChars
// deliberately exempts it from the per-message cap, mirroring the sender); the
// guard measures the marshalled request AFTER the replacement. Their gap is the
// summary: 114829 - 36459 = 78370 tokens, and 235059/3 = 78353. The provider
// answered that turn with prompt_tokens = 27739 -- the roll-up of 102 folded
// messages never reached the model.
func registerActionModelForAttachmentsTest(t *testing.T, name string) {
	t.Helper()
	Models.mu.Lock()
	defer Models.mu.Unlock()
	prev, had := Models.byName[name]
	Models.byName[name] = ModelEntry{
		Name:                    name,
		Mode:                    "chat",
		SupportsFunctionCalling: true,
		MaxContextTokens:        262144,
	}
	t.Cleanup(func() {
		Models.mu.Lock()
		defer Models.mu.Unlock()
		if had {
			Models.byName[name] = prev
		} else {
			delete(Models.byName, name)
		}
	})
}

// checkpointPairMessages builds the shape the request path actually loads:
// older turns, the checkpoint pair (assistant tool_call + tool summary), and a
// newest user message. The summary is sized above the 50 KB staleness
// threshold so the indexer treats it as a stale attachment.
func checkpointPairMessages(summaryChars int) []utils.Message {
	unit := "CHECKPOINT-ROLLUP "
	n := summaryChars/len(unit) + 1
	return []utils.Message{
		{Role: "user", Content: utils.NewTextContent("the original ask")},
		{Role: "assistant", Content: utils.NewTextContent("an answer")},
		{Role: "assistant", ToolCalls: []utils.ToolCall{{
			ID:       "eco_test",
			Type:     "function",
			Function: utils.FunctionCall{Name: "eco_checkpoint", Arguments: "{}"},
		}}},
		{Role: "tool", Name: "eco_checkpoint", ToolCallID: "eco_test",
			Content: utils.NewTextContent(strings.Repeat(unit, n)[:summaryChars])},
		{Role: "user", Content: utils.NewTextContent("the newest ask")},
	}
}

// TestPrepareMessagesForAI_KeepsCheckpointSummaryVerbatim: the roll-up must
// reach the provider intact. "The summary text survives" is the whole contract
// -- everything the fold loop preserves is carried by this one string.
func TestPrepareMessagesForAI_KeepsCheckpointSummaryVerbatim(t *testing.T) {
	registerActionModelForAttachmentsTest(t, "test-action-model")
	in := checkpointPairMessages(235_059)
	// The newest user message makes the summary stale by the attachment
	// staleness rule (size > 50 KB => threshold 1 user message).
	if n := countUserAfterTest(in, 3); n < 1 {
		t.Fatalf("fixture must put at least one user message after the checkpoint row, got %d", n)
	}
	out, _, _ := PrepareMessagesForAI(in, utils.Model{Name: "test-action-model"})

	var got string
	for _, m := range out {
		if m.Role == "tool" && m.Name == "eco_checkpoint" {
			got = m.TextContent()
		}
	}
	if got == "" {
		t.Fatal("the eco_checkpoint summary row vanished from the request: the model now has NO representation of the folded history")
	}
	if len(got) < 100_000 {
		t.Errorf("checkpoint summary replaced by a %d-char stub instead of the %d-char roll-up: %q",
			len(got), 235_059, first(got, 200))
	}
}

// TestPrepareMessagesForAI_KeepsStagedMegacaveArtifact: the staged resolver's
// in-memory view carries the same roll-up under a "megacave_<stage>" name, and
// index.go runs PrepareMessagesForAI over that view too. If the artifact is
// replaced there, the resolver has folded history durably AND deleted its own
// summary in the same turn -- the 27739-token request.
func TestPrepareMessagesForAI_KeepsStagedMegacaveArtifact(t *testing.T) {
	registerActionModelForAttachmentsTest(t, "test-action-model")
	in := checkpointPairMessages(120_000)
	for i := range in {
		if in[i].Role == "tool" && in[i].Name == "eco_checkpoint" {
			in[i].Name = "megacave_D"
			in[i].ToolCallID = "megacave_D"
		}
		if in[i].Role == "assistant" && len(in[i].ToolCalls) > 0 {
			in[i].ToolCalls[0].Function.Name = "megacave_D"
			in[i].ToolCalls[0].ID = "megacave_D"
		}
	}
	out, _, _ := PrepareMessagesForAI(in, utils.Model{Name: "test-action-model"})

	var got string
	for _, m := range out {
		if m.Role == "tool" && strings.HasPrefix(m.Name, "megacave_") {
			got = m.TextContent()
		}
	}
	if len(got) < 100_000 {
		t.Errorf("staged megacave artifact stubbed to %d chars -- the resolver folded history and destroyed its own summary: %q",
			len(got), first(got, 200))
	}
}

// TestBuildAttachmentIndex_DoesNotOfferCheckpointToTheModel: the tool-side
// attachment tools (conversation_attachments / read_document) resolve through
// the same index. A checkpoint summary offered as a retrievable "attachment" is
// nonsense to the model, and indexing it is what lets the placeholder replace
// it.
func TestBuildAttachmentIndex_DoesNotOfferCheckpointToTheModel(t *testing.T) {
	in := checkpointPairMessages(235_059)
	idx := utils.BuildAttachmentIndex(in, nil)
	for _, it := range idx.Items {
		if it.MessageIndex < len(in) && in[it.MessageIndex].Name == "eco_checkpoint" {
			t.Errorf("checkpoint summary indexed as attachment %s (%d bytes) -- the attachment pipeline replaces it with a placeholder and the roll-up is lost",
				it.ID, it.Size)
		}
	}
}

func countUserAfterTest(msgs []utils.Message, idx int) int {
	n := 0
	for i := idx + 1; i < len(msgs); i++ {
		if msgs[i].Role == "user" {
			n++
		}
	}
	return n
}
