package ai

import (
	"strings"

	"github.com/azukaar/plurality/src/db"
)

// eco_fold_prior.go — the prior-roll-up block shared by both fold briefs.

// priorRollupTailChars bounds the prior roll-up quoted into the brief. The
// brief is the tail of an already-large prompt, so an unbounded quote of a
// 100 KB chain would eat the room the history needs.
const priorRollupTailChars = 4000

// priorRollupBlock names the stored roll-up the new checkpoint must not
// restate, and quotes a bounded tail of it.
//
// The quote is load-bearing: it is the model's ONLY view of what the chain
// already records. Without it the model restates the whole chain, the roll-up
// grows at every fold, and db.composeDurableSummary's escape valve truncates
// the PRIOR to make room — which is the "rolling summary" rewrite of the
// prompt head, and the cache_read=0 that follows it. Telling the model not to
// restate something it cannot see is not an instruction.
func priorRollupBlock(priorSummary string) string {
	s := strings.TrimSpace(priorSummary)
	if s == "" {
		return ""
	}
	if r := []rune(s); len(r) > priorRollupTailChars {
		s = "[... older roll-up elided ...]\n" + string(r[len(r)-priorRollupTailChars:])
	}
	return "\nThe existing roll-up is stored verbatim ahead of your output, in the section marked \"" +
		db.NewerHistoryBanner + "\". Do NOT restate it: your output ADDS to that chain.\n" +
		"Its tail, for context only (never copy it):\n" + s + "\n"
}
