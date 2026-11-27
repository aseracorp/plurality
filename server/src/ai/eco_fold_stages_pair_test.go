package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
)

// The fold ladder's stages address the checkpoint pair by NAME, and stage 1's
// brief makes a claim about it in prose. This file pins that the pair they all
// address is the FOLD's pair, on the live two-pair shape (see pairFixture).
//
// STAGE 1 (foldStageCompress) compresses the evicting range and drops nothing.
// Its preferred request shape is the prefix replay, and with priorVisible=true
// the brief tells the model the archive is "the checkpoint pair at the head of
// the history above". That sentence is only true if the shipped view STARTS at
// the fold's pair. Pre-fix it started at the model-issued pair, so the brief
// pointed the model at a 319-char "tool not advertised" error and called it the
// archive.
//
// STAGE 2 (foldStageSummarize) summarises and drops completed context. Its
// megacave shape runs with priorVisible=false, so the brief QUOTES the prior,
// tail-elided to priorRollupTailChars. Pre-fix the quote was the 319-char
// error, whole — the model was told that string is the archive, and its
// summary became the new archive.
//
// STAGE 3 (foldStageCompact) edits the ARCHIVE by list, server-side, and writes
// it with RewriteCheckpointSummary → findLiveCheckpoint. That lookup takes the
// FIRST pair by seq ASC; the ai layer's prior read (PriorSummaryFromView) took
// the LAST pair in the slice. Two pairs means those two reads name different
// rows: the fold writes the roll-up into one pair while the request ships the
// other. The fix keys both on the fold's call-id prefix.
func TestFoldStagesAllAddressTheFoldsPair(t *testing.T) {
	msgs := pairFixture(t)
	rollup := msgs[1].Content.TextContent()
	modelErr := msgs[3].Content.TextContent()

	// STAGE 1, prefix replay: the brief's "pair at the head" claim.
	view := filterCheckpointsForRequest(msgs, true, 262_144)
	if !db.IsFoldCheckpointMessage(view[0]) {
		t.Fatalf("stage 1's brief says the archive is the pair at the head of the history, "+
			"but the shipped view starts at role=%q foldPair=%v", view[0].Role, db.IsFoldCheckpointMessage(view[0]))
	}
	if view[1].Content.TextContent() != rollup {
		t.Fatalf("the row after the head pair must be the roll-up (the archive the brief names)")
	}
	if view[1].Content.TextContent() == modelErr {
		t.Fatalf("stage 1's archive is the model-issued error string")
	}

	// STAGE 1's prior, as the fold reads it, is the roll-up — stitchDelta
	// appends the model's new section AFTER it, so a wrong prior becomes the
	// head of the new archive.
	prior := PriorSummaryFromView(msgs)
	if prior != rollup {
		t.Fatalf("PriorSummaryFromView = %d chars, want the roll-up's %d", len(prior), len(rollup))
	}
	chain, added, replayed := stitchDelta(prior, "NEW: fixed the pair selection")
	if added == 0 {
		t.Fatalf("stage 1 stitch added nothing (replay %d chars)", replayed)
	}
	if !strings.HasPrefix(chain, rollup) {
		t.Fatalf("the stitched chain lost the roll-up as its head: %q", chain[:60])
	}
	if strings.Contains(chain, modelErr) {
		t.Fatalf("the chain carries the model-issued error as archive text")
	}

	// STAGE 2, megacave shape (priorVisible=false): the brief quotes the prior,
	// tail-elided. Post-fix the quote is the roll-up's tail; pre-fix the 319-char
	// error was quoted whole.
	brief2 := foldInstruction(foldStageSummarize, prior, len(msgs), false)
	if !strings.Contains(brief2, "older roll-up elided") {
		t.Fatalf("stage 2's brief must elide the prior to %d chars; the roll-up is far bigger and "+
			"the brief rides every fold call", priorRollupTailChars)
	}
	if strings.Contains(brief2, modelErr) {
		t.Fatalf("stage 2's brief quoted the model-issued error as the EXISTING ROLL-UP")
	}
	briefOld := foldInstruction(foldStageSummarize, modelErr, len(msgs), false)
	if strings.Contains(briefOld, "older roll-up elided") {
		t.Fatalf("the pre-fix prior (short error) was quoted whole — the model was told that string is the archive")
	}

	// STAGE 3's edit-list applies to the chain the ROLL-UP is, not the error.
	brief3 := foldInstruction(foldStageCompact, prior, 100_000, false)
	if strings.Contains(brief3, modelErr) {
		t.Fatalf("stage 3's brief must not present the model-issued error as the archive")
	}
	// The edit protocol's grammar is pinned in eco_fold_delta_test.go; here only
	// that a real edit list on a roll-up-shaped chain applies.
	entries := "[2026-11-25] assistant: archived the old investigation\n\n" +
		"[2026-11-26] user: the negation that must survive: do NOT deploy"
	res, ok := applyCompactionAddendum(entries, "- [2026-11-25] assistant: archived the old investigation", 100_000)
	if !ok {
		t.Fatalf("stage 3 refused a valid edit list on the chain: %d->%d chars", len(entries), len(res.Compacted))
	}
	if strings.Contains(res.Compacted, "old investigation") || !strings.Contains(res.Compacted, "do NOT deploy") {
		t.Fatalf("stage 3 applied the delete wrong: %q", res.Compacted)
	}
}
