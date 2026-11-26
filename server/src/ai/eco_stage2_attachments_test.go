package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// The durable fold's roll-up carries ONE block listing the attachment ids of
// the rows it folded, so the model can re-read them
// (conversation_attachments mode=get). #218 put that block in the STAGE-1
// render (MachineExcerptMegacaveCapped appends it) and in the stage-2 INPUT
// (buildSummaryRequestFromMegacave costs it as incompressible), but NOT in the
// stage-2 SUCCESS output: llmFoldSummaryRenderer rendered only the model's
// text, so on the path where the model writes the checkpoint, the ids survived
// only if the model happened to copy them.
//
// The ids are row-derived (att_<db row id>_<part>) and folded rows are flagged,
// never deleted, so the block written at fold time keeps resolving for the life
// of the conversation. These tests pin that the block is appended by the code,
// from the fold set, on BOTH renderers.

// TestLLMStage2RendererAppendsAttachmentBlock: the block comes from the fold
// set the storage layer hands in, not from the model's prose.
func TestLLMStage2RendererAppendsAttachmentBlock(t *testing.T) {
	folded := []utils.Message{
		{ID: "101", Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "look at this", Filename: "config.yaml"},
		})},
		{Role: "tool", Name: "filesystem_server__fs_read", Content: utils.NewTextContent("file body")},
	}
	block := megacaveAttachmentBlock(folded)
	if block == "" {
		t.Skip("fixture produces no attachment block (no stable ids): renderer has nothing to append")
	}

	render := llmFoldSummaryRenderer("MODEL CHECKPOINT TEXT THAT SAYS NOTHING ABOUT ATTACHMENTS", 100_000)
	out := render(folded)
	if !strings.Contains(out, "MODEL CHECKPOINT TEXT") {
		t.Fatal("the renderer dropped the model's checkpoint")
	}
	if !strings.Contains(out, "Folded attachments:") {
		t.Fatal("the stage-2 checkpoint has no attachment retrieval block. The model was asked " +
			"for a checkpoint of text, so copying ids was up to it; a checkpoint that omits them " +
			"breaks the promise the block makes.\n--- render ---\n" + out)
	}
	// The ids must be the FOLD SET's row ids. Positional att_N shifted under
	// compaction (measured: att_0 was S0000S before a fold and S0003S after),
	// so a fold-written pointer named the wrong content; row-derived ids cannot.
	for _, id := range attachmentIDsInBlock(block) {
		if !strings.HasPrefix(id, "att_101_") {
			t.Fatalf("block id %q is not derived from the folded row 101: %s", id, block)
		}
	}
}

// TestStage1RendererAppendsAttachmentBlock: same guarantee for the
// deterministic path, pinned so a refactor of the machine renderer cannot lose
// it silently.
func TestStage1RendererAppendsAttachmentBlock(t *testing.T) {
	folded := []utils.Message{
		{ID: "202", Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "look at this", Filename: "notes.md"},
		})},
	}
	block := megacaveAttachmentBlock(folded)
	if block == "" {
		t.Skip("fixture produces no attachment block")
	}
	out := MachineExcerptMegacaveCapped(folded, 2_000, 100_000)
	if !strings.Contains(out, "Folded attachments:") {
		t.Fatal("the stage-1 render lost its attachment retrieval block")
	}
	// Same step, same scoped set: the block is produced from the fold set only.
	if !strings.Contains(out, block) {
		t.Fatal("stage-1 block differs from megacaveAttachmentBlock(foldable)")
	}
}

// TestStage2RefineUsesTheSameBlockForTheChain: the roll-up refine shortens the
// archive, and the archive's attachment ids must survive that rewrite — a
// refine that drops them strands every attachment the conversation folded.
func TestRefineKeepsAttachmentIDsInsideTheCeiling(t *testing.T) {
	folded := []utils.Message{
		{ID: "303", Role: "user", Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "one", Filename: "a.txt"},
		})},
	}
	block := megacaveAttachmentBlock(folded)
	if block == "" {
		t.Skip("fixture produces no attachment block")
	}
	ids := attachmentIDsInBlock(block)
	if len(ids) == 0 {
		t.Skip("no ids in the fixture block")
	}
	archive := strings.Repeat("archive body line\n", 400) + "\n" + block
	chain := MegacaveSummaryChain(archive, 2_000)
	if chain == "" {
		t.Fatal("chain render produced nothing")
	}
	if len(chain) > 2_000 {
		t.Fatalf("chain render exceeded its ceiling: %d > 2000", len(chain))
	}
	// The bounded chain render keeps the newest content (tail-keep), which is
	// where the block sits: a block cut from the archive is attachments the
	// model can no longer reach.
	for _, id := range ids {
		if !strings.Contains(chain, id) {
			t.Fatalf("the bounded archive render dropped attachment id %s — attachments become unreachable", id)
		}
	}
}

func attachmentIDsInBlock(block string) []string {
	var out []string
	for _, tok := range strings.Fields(block) {
		tok = strings.Trim(tok, ".,[]")
		if strings.HasPrefix(tok, "att_") {
			out = append(out, tok)
		}
	}
	return out
}

// TestRefineRollupRefusesUndeclaredWindow: the refine is an LLM round-trip, so
// its max_tokens must be sized from a DECLARED window. Guessing one is the
// assumption this subsystem now refuses.
func TestRefineRollupRefusesUndeclaredWindow(t *testing.T) {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("ask")},
		checkpointMsg(),
		toolMsgWithSummary("the archive"),
	}
	if PriorSummaryFromView(msgs) == "" {
		t.Fatal("fixture has no prior summary: the refine needs the archive to shorten")
	}
	// An unregistered model declares nothing: window 0, refuse, no request.
	Models.mu.Lock()
	delete(Models.byName, "undeclared-refine-model")
	Models.mu.Unlock()
	ok, err := RefineRollupSummary(nil, "conv-refine", msgs,
		utils.Model{Name: "undeclared-refine-model"}, ChatPayload{ConversationID: "conv-refine"})
	if ok {
		t.Fatal("refine ran against an undeclared window")
	}
	if err != nil {
		t.Fatalf("refine must REFUSE (false, nil), not error: %v", err)
	}
}

func toolMsgWithSummary(summary string) utils.Message {
	return utils.Message{
		Role:       "tool",
		Name:       db.EcoCheckpointToolName,
		ToolCallID: "eco_regression",
		Content:    utils.NewTextContent(summary),
	}
}
