package ai

import (
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// eco_fold_delta.go — the append-only contract for every fold stage.
//
// WHY THIS FILE EXISTS. The fold's roll-up is a CHAIN: the prior text is the
// record of every row an earlier fold compacted, and db.composeDurableSummary
// carries it forward VERBATIM and appends the new section at the tail. A model
// asked to "write the checkpoint for this range" does not know that, and will
// obediently re-emit the whole chain — a 1:1 replay of text the server already
// has. Measured against a self-hosted model at ~40 tokens/second, a 100k-token
// replay is a 40-minute fold, and ~95% of those bytes are the server copying
// its own database to itself.
//
// So the instruction is explicit and the code enforces it three ways:
//
//	1. the brief names the prior text and forbids copying it, quoting it,
//	   reflowing it, re-emitting its banners, or writing a preamble;
//	2. max_tokens is sized to the NEW section, not the window: an LLM cannot
//	   replay the chain if the answer is capped below the chain's size;
//	3. the stitcher detects an emitted replay (the answer begins with text the
//	   prior already contains) and strips the overlap before appending, so a
//	   disobedient answer degrades to an append instead of duplicating history.
//
// The bytes on the WIRE stay byte-identical: stage 2 and stage 3 send the SAME
// prefix replay stage 1 sent (buildSummaryRequestFromView over the same
// conversation), so the KV cache that stage 1 warmed is reused, and the brief —
// the only new tokens — is the appended user row.

// deltaRules is shared by all three briefs: the append-only contract.
const deltaRules = "APPEND-ONLY CONTRACT: your answer is APPENDED after the existing roll-up, so it " +
	"contains ONLY the new section for the rows named above. Never copy, quote, reflow or re-emit any part " +
	"of the existing roll-up, never restate a fact it already records, never write a preamble, a repeated " +
	"heading, a closing remark or a note about what you did. Your answer is a fragment, not a document: it " +
	"begins mid-document, exactly where the existing roll-up ends.\n\n"

// existingRollupForDelta quotes the prior chain the model must not reproduce.
// Bounded: the prior is carried by the server, the quote exists so the model can
// tell what is already recorded.
func existingRollupForDelta(priorSummary string, visible bool) string {
	s := strings.TrimSpace(priorSummary)
	if s == "" {
		return "EXISTING ROLL-UP: none. This is the first section, so the whole document is new.\n\n"
	}
	if visible {
		return "EXISTING ROLL-UP: the checkpoint pair at the head of the history above. It is already stored, " +
			"and your answer is APPENDED after it - so never copy, quote, reflow or re-emitting any of it.\n\n"
	}
	if r := []rune(s); len(r) > priorRollupTailChars {
		s = "[... older roll-up elided ...]\n" + string(r[len(r)-priorRollupTailChars:])
	}
	return "EXISTING ROLL-UP (already stored; never copy any of it):\n" + s + "\n\n"
}

// deltaOutputTokens bounds the ANSWER to the size of the new material.
//
// The cap is the point: at ~40 t/s a fold's wall-clock is max_tokens / 40, and
// the only reason a fold ever takes tens of minutes is that the model is
// allowed to write out text the server already holds. Sizing the budget to the
// folded range (not to the window) makes a full-chain replay physically
// impossible, which is a stronger guarantee than any instruction.
func deltaOutputTokens(foldedChars int, ceiling int) int {
	// The new section may cost at most the fold's ceiling, and half the wire
	// size of the rows it replaces (the same "a fold must provably shrink" rule
	// the machine render obeys), with slack so terse output is not clipped.
	budget := ceiling / summaryCharsPerToken
	if half := foldedChars / 2 / summaryCharsPerToken; half < budget {
		budget = half
	}
	budget += minSummaryOutputTokens
	if budget > maxSummaryOutputTokens {
		budget = maxSummaryOutputTokens
	}
	if budget < minSummaryOutputTokens {
		budget = minSummaryOutputTokens
	}
	return budget
}

// stitchDelta appends the model's new section to the prior chain.
//
// Anti-replay: an LLM that ignores the append-only rule opens its answer with
// the chain it was shown. The overlap is detected on a tail/head comparison and
// removed, so the stored text is prior + genuinely new bytes. Detection is
// anchored on the prior's TAIL (where the append point is), not on any interior
// substring, so a legitimate new section that shares a heading with the prior
// is not mangled.
//
// Returns (stitchedText, newSectionChars, replayedChars).
func stitchDelta(priorSummary, delta string) (string, int, int) {
	prior := strings.TrimSpace(priorSummary)
	section := strings.TrimSpace(delta)
	if section == "" {
		return prior, 0, 0
	}
	if prior == "" {
		return section, len(section), 0
	}
	stripped := stripReplayOverlap(prior, section)
	replayed := len(section) - len(stripped)
	if stripped == "" {
		// The whole answer was a replay of the prior. Appending nothing is the
		// honest outcome; the caller treats 0 new chars as "no progress" and
		// escalates.
		utils.Log("stitch: the answer was a 1:1 replay of the prior roll-up (%d chars) - nothing appended", replayed)
		return prior, 0, replayed
	}
	return prior + "\n\n" + stripped, len(stripped), replayed
}

// replayOverlapMinChars is the shortest overlap treated as a replay. Below it
// the shared text is a heading, a path, a repeated phrase — normal prose, not
// the model copying the chain.
const replayOverlapMinChars = 400

// stripReplayOverlap removes the leading run of `section` that already exists
// in the prior, at any offset.
//
// The two shapes a replay takes: the model copies the chain from its start (the
// answer is a prefix of the stored roll-up, then the new material), or it copies
// a suffix from an arbitrary offset. Anchoring on the prior's TAIL only catches
// the second shape when the offset is exactly the probe length, so a chain
// copied from the top slips through and gets appended - duplicating the archive,
// which is the failure this function exists to prevent.
//
// So: the longest prefix of `section` that occurs anywhere in `prior` is the
// replay. It is found by binary search on prefix length (the property is
// monotone: if a prefix of length k occurs, every shorter prefix occurs), which
// costs O(log n) substring searches instead of scanning the texts.
func stripReplayOverlap(prior, section string) string {
	k := longestPrefixInPrior(prior, section)
	if k < replayOverlapMinChars {
		return section
	}
	return strings.TrimSpace(section[k:])
}

// longestPrefixInPrior is the length of the longest prefix of `section` that
// occurs in `prior` as a substring.
func longestPrefixInPrior(prior, section string) int {
	hi := len(section)
	if hi > len(prior) {
		hi = len(prior)
	}
	lo := 0
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if strings.Contains(prior, section[:mid]) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// answerReplaysUserRows reports whether the model's answer reproduces the fold
// set's user rows — the replay append-only forbids.
//
// THE OWNER'S VERBATIM RULE STILL HOLDS; IT MOVED. User rows used to be demanded
// of the MODEL (this function's predecessor returned the inverse: a checkpoint
// was rejected unless it carried every user row byte-for-byte). That gate ordered
// the model to copy the largest part of the fold set — live, conv 9f0ab2352bf2bdc40e77384b
// at 21:16: 11529 chars of user text = ~3843 tokens — while deltaOutputTokens
// caps the answer to a fraction of it. The task was unsatisfiable: a compliant
// model truncated mid-copy and was rejected ("the checkpoint does not carry the
// user rows verbatim", 22:37 and 23:19 in the live log), and a lazy one paraphrased
// and was rejected too. Both paths discarded the LLM's answer and stored the
// machine render, so stage 1 could not succeed on a conversation with real user
// text. The rule now lives in llmFoldSummaryRenderer, which appends the user rows
// server-side: verbatim carriage at ZERO output tokens, guaranteed, not requested.
//
// So the check inverts. A row is "replayed" when the answer contains most of it
// (short rows must be near-exact; long ones at 60 %), counted against a
// threshold, so a genuine summary that quotes ONE decisive user sentence — the
// correct thing to do — is not a replay.
func answerReplaysUserRows(section string, rows []utils.Message) bool {
	hits := 0
	total := 0
	for _, m := range rows {
		if m.Role != "user" || m.Compacted {
			continue
		}
		txt := strings.TrimSpace(m.TextContent())
		if len(txt) < 24 {
			continue // too short to be a replay; quoting it is normal
		}
		total++
		if answerCarriesText(section, txt) {
			hits++
		}
	}
	if total == 0 {
		return false
	}
	// Two thirds of the fold set's user rows reproduced is not summarising, it is
	// transcription. One third, or a single row, is quoting.
	return hits*3 >= total*2 && hits >= 2
}

// answerCarriesText is the containment test answerReplaysUserRows needs on text
// the model may have trimmed: exact containment, else a prefix of the row long
// enough that a paraphrase cannot reach it by accident.
func answerCarriesText(section, txt string) bool {
	if strings.Contains(section, txt) {
		return true
	}
	if len(txt) < 120 {
		return false
	}
	return strings.Contains(section, txt[:len(txt)/2])
}
