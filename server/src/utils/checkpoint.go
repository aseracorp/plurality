package utils

import "strings"

// EcoCheckpointToolName is the function name carried by the assistant tool call
// of an eco rolling-checkpoint pair; the tool-role half carries the same name in
// msg.Name and the summary text in its Content.
//
// The canonical definition lives HERE, not in db, because every consumer of the
// rule needs it and db imports utils: the persistence layer (db.IsCheckpointMessage
// / GetCheckpoint), the sender (ai.TruncateOversizedMessages), the estimator
// (ai.boundedWireChars) and the attachment pipeline (BuildAttachmentIndex).
// Duplicating the string per package is what let the attachment indexer stay
// ignorant of it.
const EcoCheckpointToolName = "eco_checkpoint"

// MegacaveArtifactNamePrefix marks the same roll-up inside the in-memory staged
// eco view, where the resolver names the artifact "megacave_<stage>" (see
// ai.megacaveArtifact). It carries identical content under a per-stage name.
const MegacaveArtifactNamePrefix = "megacave_"

// IsCheckpointSummaryCarrier reports whether m is the TOOL half of a checkpoint
// pair, i.e. the single message whose text content is the roll-up of every row
// durable compaction folded away (those rows are marked compacted = 1 and are no
// longer loaded into the request).
//
// The carrier is the conversation's ONLY representation of that history, so it
// must reach the provider verbatim. It is deliberately exempt from the
// per-message size cap (ai.TruncateOversizedMessages) and from the estimator's
// clamp (ai.boundedWireChars): its size is decided by the eco fold loop's fit
// test against the context window, not by a constant.
//
// It must equally be exempt from the attachment pipeline. The carrier is a
// tool-role message holding one large text part, which is exactly the shape
// BuildAttachmentIndex indexes as a "stale attachment" (non-image text over
// 3 KB, threshold 1 user message once it passes 50 KB) — and PrepareMessagesForAI
// then swaps the entire roll-up for a one-line placeholder.
//
// Measured live (conversation 141c6b2d33616fed6f2dd8dc, 2026-11-16 15:35:15):
//
//	[Attachments] Indexed and removed 1 attachment(s) (230 KB saved)
//	[Eco] content ~114829 tokens <= trigger 150000 -- sending unchanged
//	[Eco] compression-aware guard: est 36459 tokens ...
//
// The "1 attachment / 230 KB" is the carrier (content 235059 chars). The gate
// and the guard then disagree by 114829-36459 = 78370 tokens, and 235059/3 =
// 78353 -- the whole roll-up, deleted between measuring and sending. The
// provider answered with prompt_tokens 27739: 102 folded messages, and every
// earlier fold, reached the model as nothing.
func IsCheckpointSummaryCarrier(m Message) bool {
	if m.Role != "tool" {
		return false
	}
	if m.Name == EcoCheckpointToolName {
		return true
	}
	return strings.HasPrefix(m.Name, MegacaveArtifactNamePrefix)
}
