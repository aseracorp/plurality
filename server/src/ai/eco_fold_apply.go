package ai

import (
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// eco_fold_apply.go — the stage-3 addendum protocol and its server-side applier.
//
// Stage 3 rewrites the ARCHIVE (the roll-up), and rewriting is exactly the
// operation the append-only contract forbids: an LLM replaying a 50k-token
// archive at ~40 tokens/second is a 20-minute fold, and ~95% of those bytes are
// the server copying its own database to itself.
//
// So stage 3 never rewrites. The model emits a structured ADDENDUM and the
// server applies it:
//
//	- <entry text>   remove this entry from the archive
//	+ <entry text>   add this ultracave-voice entry (a compaction of removed ones)
//
// Untouched entries are carried verbatim by the SERVER — zero output tokens,
// zero time. The answer is bounded by what actually changes, which is the point:
// stage 3's job is to DELETE completed work and COMPACT what remains, and both
// are edits, not a transcript.
//
// The applier is deterministic and refuses a runaway: an addendum that would
// delete most of the archive with little added back is rejected, because the
// archive is the ONLY record of rows that no longer exist, and a model that
// answers "drop 90%" is not evidence the conversation finished with it.

// entrySep separates archive entries in the roll-up. Entries are blank-line
// separated blocks; the applier never invents boundaries it cannot see.
const addendumRemovePrefix = "- "
const addendumAddPrefix = "+ "

// stage3AddendumInstruction is the brief for the archive-compaction pass.
// stage3AddendumInstruction is the brief of the archive-compaction pass.
//
// It does NOT quote the archive. An addendum edits the archive, so the archive
// is necessarily already in the request - as the checkpoint pair at the head of
// the replayed history, or as the block the caller prepends - and a quote is a
// second copy of the same text: prefill paid twice. The brief names where the
// archive is and forbids copying it into the answer.
func stage3AddendumInstruction(targetChars int) string {
	var b strings.Builder
	b.WriteString("SYSTEM TASK - COMPACT THE ARCHIVE, EDIT-ONLY (not a reply to the user)\n\n")
	b.WriteString("The EXISTING ROLL-UP below is this conversation's archive of turns that no longer fit in the " +
		"context window. Those turns are gone: the archive is their only record. The archive itself has outgrown " +
		"its budget (aim below), so it must shrink.\n\n")
	b.WriteString("You do NOT rewrite the archive. You emit an ADDENDUM of edits, one per line, in exactly this grammar:\n")
	b.WriteString("- <exact first line of an entry to DELETE>\n")
	b.WriteString("+ <a new entry, in ultracave voice, compacting what you deleted if any of it still matters>\n")
	b.WriteString("An entry is a blank-line separated block. Quote its FIRST LINE exactly, character for character, " +
		"so the server can find it. Emit NOTHING else: no commentary, no counts, no headings, no code fences around the list.\n\n")
	b.WriteString("WHAT TO DELETE: work the conversation has finished with and no longer needs - completed subtasks, " +
		"superseded plans, abandoned experiments, dead-end investigations, tool output whose result is already a " +
		"decision, file contents already written and never referenced again.\n")
	b.WriteString("WHAT TO KEEP BY NOT TOUCHING IT (silence = keep): open threads, the task in progress, decisions " +
		"and their reasons, constraints the user set, file paths and identifiers that exist, numbers, URLs, exact " +
		"commands and their results, error messages, the user's own words.\n")
	b.WriteString("When you delete several entries whose substance still matters, ADD one ultracave-voice entry that " +
		"carries the surviving facts. The payload floor applies to every '+' line.\n\n")
	b.WriteString(ultracaveFloor)
	b.WriteString(ultracaveVoice)
	b.WriteString(deltaRules)
	b.WriteString("Budget: the archive must come out at most " + itoaRows(targetChars) + " characters. " +
		"Delete whole entries before rewording any. Aim for at least " + itoaRows(targetChars/2) + " characters of net reduction.\n\n")
	b.WriteString("THE ARCHIVE is already in this request: the conversation's roll-up, in the checkpoint pair ")
	b.WriteString("at the head of the history (and, when this brief follows a copy of the archive, in the block ")
	b.WriteString("above it). Read it, edit it, and NEVER copy archive entries into your answer. Your answer is ")
	b.WriteString("the edit list, nothing else.\n\n")
	b.WriteString("Emit the addendum lines now. Start with the first '-' or '+' line.\n")
	return b.String()
}

// addendumResult is what the applier reports.
type addendumResult struct {
	Compacted string
	Removed   int
	Added     int
	Kept      int
}

// applyCompactionAddendum applies the '-'/'+ protocol to the archive.
//
// Matching is on the entry's FIRST LINE, normalized (trim, collapse internal
// whitespace, case-insensitive compare, and a prefix match so a model that
// quotes the first line with a trailing colon still resolves).
func applyCompactionAddendum(prior, addendum string, targetChars int) (addendumResult, bool) {
	// targetChars is the model's stated goal. It is reported with the result
	// and never used to justify a deletion: missing a budget is not a licence
	// to drop the record (see the runaway guard).
	res := addendumResult{}
	entries := splitArchiveEntries(prior)
	if len(entries) == 0 {
		return res, false
	}
	var removes []string
	var adds []string
	for _, line := range strings.Split(addendum, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, addendumRemovePrefix) {
			key := normEntry(strings.TrimPrefix(l, addendumRemovePrefix))
			if key != "" {
				removes = append(removes, key)
			}
		} else if strings.HasPrefix(l, addendumAddPrefix) {
			txt := strings.TrimSpace(strings.TrimPrefix(l, addendumAddPrefix))
			if txt != "" {
				adds = append(adds, txt)
			}
		}
		// Anything else is commentary: ignored, never treated as an instruction
		// to keep (a model that narrates must not be able to smuggle prose in).
	}
	if len(removes) == 0 && len(adds) == 0 {
		return res, false
	}

	removedSet := make([]bool, len(entries))
	for _, key := range removes {
		for i, e := range entries {
			if removedSet[i] {
				continue
			}
			if entryMatches(e, key) {
				removedSet[i] = true
				res.Removed++
				break
			}
		}
	}

	keptChars := 0
	kept := make([]string, 0, len(entries))
	for i, e := range entries {
		if removedSet[i] {
			continue
		}
		kept = append(kept, e)
		keptChars += len(e) + 2
	}
	addedChars := 0
	for _, a := range adds {
		addedChars += len(a) + 1
	}

	// RUNAWAY GUARD. An addendum that deletes most of the archive and adds
	// almost nothing back would leave an archive that no longer records the
	// rows it is the only record of - unrecoverable, invisible to the user, and
	// the model has no way to know which rows the conversation still needs.
	// The budget is a goal, never a licence to delete: staying under target is
	// achieved by compacting, not by dropping the record. Reject, and the
	// caller leaves the existing archive untouched.
	removedChars := 0
	for i, e := range entries {
		if removedSet[i] {
			removedChars += len(e) + 2
		}
	}
	if res.Removed >= 3 && res.Removed > res.Kept*2 && addedChars < removedChars/10 {
		utils.Log("[Eco] stage 3 addendum refused by the runaway guard: -%d entries (%d chars) +%d chars, target %d chars",
			res.Removed, removedChars, addedChars, targetChars)
		return res, false
	}

	var b strings.Builder
	for _, e := range kept {
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	for _, a := range adds {
		b.WriteString(a)
		b.WriteString("\n")
	}
	res.Compacted = strings.TrimSpace(b.String())
	res.Added = len(adds)
	res.Kept = len(kept)
	if res.Compacted == "" {
		return res, false
	}
	return res, true
}

// splitArchiveEntries splits the archive into blank-line separated blocks.
func splitArchiveEntries(text string) []string {
	var out []string
	for _, block := range strings.Split(text, "\n\n") {
		b := strings.TrimSpace(block)
		if b == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

func normEntry(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.ToLower(s)
}

// entryMatches reports whether `entry` is the entry the model named. The model
// quotes the first line; the comparison is normalized and a prefix match is
// accepted so a trailing colon, an emoji, or a truncated quote still resolves.
func entryMatches(entry, normKey string) bool {
	first := entry
	if i := strings.IndexByte(entry, '\n'); i >= 0 {
		first = entry[:i]
	}
	a := normEntry(first)
	if a == normKey {
		return true
	}
	if len(normKey) >= 12 && strings.HasPrefix(a, normKey) {
		return true
	}
	if len(a) >= 12 && strings.HasPrefix(normKey, a) {
		return true
	}
	return false
}
