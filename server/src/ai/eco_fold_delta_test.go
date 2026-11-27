package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// eco_fold_delta_test.go — the append-only contract, tested.
//
// The requirement: the fold's LLM call must not re-emit the existing roll-up.
// At ~40 tokens/second, writing 100k tokens is 40 minutes, and a fold that
// replays the chain spends ~95% of that time copying the server's own database
// back to the server. So three separate mechanisms enforce it, and each is
// tested: the brief (names the prior, forbids copying), the output cap (a
// replay becomes physically impossible), and the stitcher (a replay that
// happens anyway is stripped, not stored twice).

func TestDeltaOutputTokensBoundedToNewMaterial(t *testing.T) {
	// A small fold must NOT get a window-sized budget: that is the invitation
	// to replay. 3000 chars folded, ceiling 60000.
	got := deltaOutputTokens(3000, 60000)
	want := 3000/2/summaryCharsPerToken + minSummaryOutputTokens
	if got != want {
		t.Fatalf("delta budget = %d, want %d (folded/2/3 + floor)", got, want)
	}
	if got >= 60000/summaryCharsPerToken {
		t.Fatalf("delta budget %d is not bounded to the new material", got)
	}
	// The cap must be well below the size of a chain-replay: a 100k-token prior
	// must not fit in the answer budget.
	if priorTokens := 100000; got >= priorTokens {
		t.Fatalf("budget %d allows replaying a %d-token roll-up", got, priorTokens)
	}
}

func TestDeltaOutputTokensFloorsAndCaps(t *testing.T) {
	if got := deltaOutputTokens(0, 0); got != minSummaryOutputTokens {
		t.Fatalf("empty fold: got %d, want the floor %d", got, minSummaryOutputTokens)
	}
	if got := deltaOutputTokens(10_000_000, 10_000_000); got != maxSummaryOutputTokens {
		t.Fatalf("huge fold: got %d, want the hard cap %d", got, maxSummaryOutputTokens)
	}
	// Monotone in the folded size: more new material must allow a longer answer.
	small := deltaOutputTokens(6000, 200000)
	big := deltaOutputTokens(600000, 200000)
	if big <= small {
		t.Fatalf("not monotone: %d folded -> %d, %d folded -> %d", 6000, small, 600000, big)
	}
}

func TestStitchDeltaAppendsAndStripsReplay(t *testing.T) {
	prior := strings.Repeat("A", 3000) + "\n\nsection two\n"
	// Honest delta: a new section, appended after the prior, prior untouched.
	stitched, added, replayed := stitchDelta(prior, "NEW SECTION\n\n- did the thing")
	if replayed != 0 {
		t.Fatalf("honest delta reported %d replayed chars", replayed)
	}
	if added != len("NEW SECTION\n\n- did the thing") {
		t.Fatalf("added = %d, want the new section's length", added)
	}
	if !strings.HasPrefix(stitched, prior) {
		t.Fatal("the prior was modified: the KV prefix over it would diverge")
	}
	if !strings.HasSuffix(stitched, "did the thing") {
		t.Fatal("the new section was not appended at the tail")
	}
	// The prior's bytes must survive VERBATIM: this is what keeps the stored
	// pair a byte-for-byte prefix of the next turn's prompt.
	if strings.Count(stitched, "AAAA") < strings.Count(prior, "AAAA") {
		t.Fatal("the prior was not carried verbatim")
	}

	// Disobedient delta: the model replays the chain, then adds. The overlap is
	// stripped; only the new bytes are appended.
	replay := prior[len(prior)-1500:] + "\n\nNEW SECTION"
	s2, added2, rep2 := stitchDelta(prior, replay)
	if rep2 < 1400 {
		t.Fatalf("a 1500-char replay was not detected: stripped %d chars", rep2)
	}
	if added2 <= 0 || !strings.HasSuffix(s2, "NEW SECTION") {
		t.Fatalf("after stripping the replay the new section must remain: added=%d text=%q", added2, s2)
	}
	if strings.Count(s2, "AAAA") != strings.Count(prior, "AAAA") {
		t.Fatal("the chain was duplicated by the replay")
	}

	// Full replay: nothing new. Appends nothing, reports 0 added.
	full, added3, rep3 := stitchDelta(prior, prior)
	if want := len(strings.TrimSpace(prior)); added3 != 0 || rep3 != want {
		t.Fatalf("full replay: added=%d replayed=%d, want 0/%d", added3, rep3, want)
	}
	if full != strings.TrimSpace(prior) {
		t.Fatal("a full replay must leave the archive byte-identical (modulo the trim stitchDelta applies)")
	}
}

func TestStripReplayOverlapIgnoresShortOverlap(t *testing.T) {
	prior := strings.Repeat("x", 2000) + "\n# Decisions"
	// A new section that starts with a shared heading is normal prose, not a
	// replay: below the detection floor nothing is stripped.
	section := "# Decisions\n- chose the append-only protocol"
	if got := stripReplayOverlap(prior, section); got != section {
		t.Fatalf("short shared prefix was stripped: %q", got)
	}
}

// The verbatim rule moved from the model's ANSWER to the stored roll-up (the
// server appends the fold set's user rows itself), so the gate inverts: it now
// rejects TRANSCRIPTION, and a compression that quotes one decisive user sentence
// must pass, because that is the correct model behaviour.
func TestAnswerReplaysUserRowsRejectsTranscription(t *testing.T) {
	long := "please implement the megacave plan exactly as written, and do NOT restart the service " +
		"while the migration is running, because it drops in-flight requests"
	rows := []utils.Message{
		{Role: "user", Content: utils.NewTextContent(long)},
		{Role: "user", Content: utils.NewTextContent("and keep the plan text as the spec of record")},
		{Role: "assistant", Content: utils.NewTextContent("done")},
	}
	// A compression that quotes ONE user sentence: not a replay.
	compression := "TASK OF RECORD: user asked for the megacave plan; constraint: do NOT restart the service"
	if answerReplaysUserRows(compression, rows) {
		t.Fatal("a compression that quotes one decisive user row was called a replay")
	}
	// Transcribing the set: a replay.
	var transcription strings.Builder
	for _, m := range rows {
		if m.Role == "user" {
			transcription.WriteString("USER:\n" + m.Content.TextContent() + "\n\n")
		}
	}
	if !answerReplaysUserRows(transcription.String(), rows) {
		t.Fatal("transcribing every user row was not called a replay")
	}
	// Nothing to replay (no user text) is not a replay.
	if answerReplaysUserRows("anything", []utils.Message{{Role: "user", Content: utils.NewTextContent("")}}) {
		t.Fatal("an empty user row must not fail the check")
	}
}

// The server, not the model, carries the user's words into the roll-up.
func TestLLMRendererCarriesUserRowsVerbatim(t *testing.T) {
	rows := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("no, do NOT restart the service")},
		{Role: "assistant", Content: utils.NewTextContent("I stopped the restart.")},
	}
	rendered := llmFoldSummaryRenderer("did the deploy plan", 10_000)(rows)
	if !strings.Contains(rendered, "no, do NOT restart the service") {
		t.Fatalf("the stored roll-up lost the user's own words (negation destroyed):\n%s", rendered)
	}
	if !strings.Contains(rendered, "did the deploy plan") {
		t.Fatal("the model's compression is not in the stored roll-up")
	}
}

// The fixture prior sits UNDER priorRollupTailChars, so the brief quotes it
// in full; a prior past the bound is quoted by its tail, asserted below.
func TestDeltaBriefNamesThePriorAndForbidsCopying(t *testing.T) {
	prior := strings.Repeat("P", 2000)
	b1 := stage1CompressInstruction(prior, 12, false)
	b2 := stage2SummarizeInstruction(prior, 12, false)
	for _, b := range []string{b1, b2} {
		if !strings.Contains(b, "APPEND-ONLY CONTRACT") {
			t.Fatal("the brief is missing the append-only contract")
		}
		if !strings.Contains(b, "never copy any of it") {
			t.Fatal("the brief does not forbid copying the existing roll-up")
		}
		if !strings.Contains(b, "Your answer is a fragment") {
			t.Fatal("the brief does not name the answer as a fragment")
		}
		if !strings.Contains(b, prior) {
			t.Fatal("the brief does not show the prior text the model must not reproduce")
		}
	}
	// A long prior is quoted by its TAIL (the append point), never in full:
	// quoting the whole archive in the brief is itself a replay cost.
	long := strings.Repeat("Q", 400000)
	b := existingRollupForDelta(long, false)
	if len(b) > priorRollupTailChars+400 {
		t.Fatalf("the prior was quoted in full: %d chars", len(b))
	}
	if !strings.Contains(b, "older roll-up elided") {
		t.Fatal("an elided prior must say so")
	}
	// No prior: the brief must say the whole document is new.
	if !strings.Contains(existingRollupForDelta("", false), "first section") {
		t.Fatal("first fold must be told there is no existing roll-up")
	}
}

func TestStage3AddendumIsAnEditList(t *testing.T) {
	b := stage3AddendumInstruction(4000)
	if !strings.Contains(b, "- <exact first line of an entry to DELETE>") {
		t.Fatal("the addendum grammar is missing the delete form")
	}
	if !strings.Contains(b, "+ <a new entry") {
		t.Fatal("the addendum grammar is missing the add form")
	}
	// The old brief demanded the whole rewritten archive; that is the replay.
	if strings.Contains(b, "Output the new archive only") {
		t.Fatal("the brief asks the model to re-emit the archive")
	}
}

func TestApplyCompactionAddendumEditsDeterministically(t *testing.T) {
	chain := strings.Join([]string{
		"[2026-11-25] user: start the deploy plan",
		"[2026-11-25] assistant: drafted the plan in /app/plan.md",
		"[2026-11-25] user: no, do NOT restart the service",
		"[2026-11-26] assistant: investigation of the cache miss reached a conclusion",
		"[2026-11-26] user: keep going, ship it",
	}, "\n\n")

	addendum := strings.Join([]string{
		"- [2026-11-26] assistant: investigation of the cache miss reached a conclusion",
		"- [2026-11-25] assistant: drafted the plan in /app/plan.md",
		"+ [done] the deploy plan was drafted, then the cache-miss investigation closed; see /app/plan.md",
	}, "\n")

	res, ok := applyCompactionAddendum(chain, addendum, 4000)
	if !ok {
		t.Fatal("a well-formed addendum was refused")
	}
	if res.Removed != 2 || res.Added != 1 {
		t.Fatalf("removed=%d added=%d, want 2/1", res.Removed, res.Added)
	}
	if strings.Contains(res.Compacted, "investigation of the cache miss") {
		t.Fatal("a deleted entry survived")
	}
	// The user's negation is untouched: silence means keep, and the server
	// carries it. Zero output tokens were spent on it.
	if !strings.Contains(res.Compacted, "no, do NOT restart the service") {
		t.Fatal("an untouched entry was dropped")
	}
	if !strings.Contains(res.Compacted, "keep going, ship it") {
		t.Fatal("an untouched entry was dropped")
	}
	if !strings.Contains(res.Compacted, "/app/plan.md") {
		t.Fatal("the compaction entry was not added")
	}
	if len(res.Compacted) >= len(chain) {
		t.Fatalf("the archive did not shrink: %d -> %d", len(chain), len(res.Compacted))
	}
}

func TestApplyCompactionAddendumRefusesRunaway(t *testing.T) {
	var entries []string
	for i := 0; i < 40; i++ {
		entries = append(entries, "[2026-11-26] entry "+strings.Repeat("e", 60)+itoaRows(i))
	}
	chain := strings.Join(entries, "\n\n")
	var lines []string
	for i := 0; i < 34; i++ {
		lines = append(lines, addendumRemovePrefix+entries[i])
	}
	res, ok := applyCompactionAddendum(chain, strings.Join(lines, "\n"), 4000)
	if ok {
		t.Fatalf("a runaway addendum was applied: -%d entries +%d chars", res.Removed, res.Added)
	}
}

func TestApplyCompactionAddendumRefusesGarbage(t *testing.T) {
	chain := "[2026-11-26] first\n\n[2026-11-26] second"
	for _, junk := range []string{"", "I reviewed the archive and it looks fine.", "+", "-"} {
		if _, ok := applyCompactionAddendum(chain, junk, 100); ok {
			t.Fatalf("garbage addendum accepted: %q", junk)
		}
	}
}

func TestAddendumMatchesTruncatedQuotes(t *testing.T) {
	chain := "[2026-11-26] assistant: wrote the fold protocol into eco_fold_delta.go and ran the race suite\n\n[2026-11-26] user: ship it"
	// The model quotes a prefix of the first line - the normal shape.
	res, ok := applyCompactionAddendum(chain, "- [2026-11-26] assistant: wrote the fold protocol", 4000)
	if !ok || res.Removed != 1 {
		t.Fatalf("a truncated quote did not match: ok=%v removed=%d", ok, res.Removed)
	}
	if !strings.Contains(res.Compacted, "ship it") {
		t.Fatal("the untouched entry was lost")
	}
}
