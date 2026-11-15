package ai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// mkText builds a user/assistant message with the given text.
func mkMsg(role, text string) utils.Message {
	return utils.Message{Role: role, Content: utils.NewTextContent(text)}
}

// mkTool builds a tool message (e.g. a large result).
func mkTool(name, text string) utils.Message {
	return utils.Message{Role: "tool", Name: name, Content: utils.NewTextContent(text)}
}

// compressibleToolChars is the minimum tool-dump size the megacave transform
// shrinks. writeMegacaveText elides a dump only above maxMegacaveToolShowChars
// (6000); below that it just collapses whitespace, and the per-row headers it
// adds ("TOOL_RESULT shell:", "USER:") make the excerpt LARGER than the rows it
// came from. A fixture with 900-char dumps therefore measured stage A (~64669
// est tokens) ABOVE the full history (~62400), the resolver correctly answered
// "the whole history fits", returned it uncompressed, and the stage tests
// reported "no megacave artifact" while the resolver was right. Real tool
// results are far above the threshold (the measured live conversation
// compresses to ratio ~0.107), so fixtures must live on that side of the line.
const compressibleToolChars = 9_000

// mkConv builds a synthetic conversation: system + N user/assistant turns,
// each turn containing a big tool dump (to make it large).
func mkConv(turns, bigToolChars int) []utils.Message {
	var msgs []utils.Message
	for i := 0; i < turns; i++ {
		msgs = append(msgs, mkMsg("user", "user request "+itoa(i)))
		msgs = append(msgs, mkMsg("assistant", "assistant reply "+itoa(i)))
		msgs = append(msgs, mkTool("shell", strings.Repeat("x", bigToolChars)))
	}
	return msgs
}

func itoa(i int) string {
	return string(rune('0' + i%10))
}

// TestResolver_BelowTriggerReturnsUnchanged: when the whole history fits the
// content budget, the staged resolver MUST return it unchanged (byte-stable
// prefix => KV caching). This is the regime that was broken before the
// trigger gate (#154) — the resolver must never megacave content that fits.
func TestResolver_BelowTriggerReturnsUnchanged(t *testing.T) {
	msgs := mkConv(3, 100) // small
	window := 65536
	budget := 60000
	out, _ := resolveEcoContextCompaction(msgs, window, budget)
	if len(out) != len(msgs) {
		t.Fatalf("below-trigger: expected %d messages unchanged, got %d", len(msgs), len(out))
	}
	for i := range msgs {
		if out[i].TextContent() != msgs[i].TextContent() {
			t.Fatalf("message %d changed below trigger", i)
		}
	}
}

// findMegacaveArtifact returns the excerpt carried by the staged resolver's
// checkpoint artifact, if present.
//
// The artifact is an [assistant, tool] pair (see megacaveArtifact): the
// marker ("megacave_excerpt", id "megacave_A"/"B"/"C"/"D") lives in the
// assistant message's ToolCalls and in the tool message's Name/ToolCallID.
// The assistant half has NO text content at all. So asserting the marker via
// strings.Contains over concatenated TextContent() is structurally
// unsatisfiable — that is why the original StageA/StageB tests failed
// ("no megacave artifact in stage A output") while the resolver was in fact
// emitting the artifact correctly.
func findMegacaveArtifact(msgs []utils.Message) (toolCallID, excerpt string, ok bool) {
	for i := range msgs {
		m := msgs[i]
		if m.Role == "tool" && strings.HasPrefix(m.Name, "megacave") {
			return m.ToolCallID, m.TextContent(), true
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if !strings.HasPrefix(tc.Function.Name, "megacave") {
					continue
				}
				for j := i + 1; j <= i+2 && j < len(msgs); j++ {
					if msgs[j].Role == "tool" && msgs[j].ToolCallID == tc.ID {
						return tc.ID, msgs[j].TextContent(), true
					}
				}
			}
		}
	}
	return "", "", false
}

// stageSizes measures the ESTIMATED token size of each stage candidate for a
// fixture. The summary is UNCAPPED, so these sizes are a property of the
// fixture and the megacave transform, not of a cap constant; deriving budgets
// from measurements keeps the tests honest when the transform changes.
//
// Mirrors resolveEcoContextCompaction stage-for-stage on a conversation with no
// checkpoint (pairEnd == -1).
// The probe tool-call ids MUST carry the "megacave_" prefix. The estimator
// (boundedWireChars) exempts the rolling-summary row from the per-message send
// cap via checkpointSummaryCarrier, which matches the tool name
// "megacave_<stage>" — and the summary is UNCAPPED by design. A probe named
// "probe_A" is NOT exempt, so approxTokens clamped it to
// ecoSentMessageCapChars and reported a stage size ~3x smaller than what the
// resolver actually measures. budgetForStage then produced a budget that the
// real stage A/B candidates blew through, and the resolver correctly escalated
// to C — the test then reported "expected stage A, got megacave_C" while the
// resolver was right and the PROBE was lying.
func stageSizes(t *testing.T, msgs []utils.Message) (a, b, c int) {
	t.Helper()
	pairEnd := checkpointPairEnd(msgs)
	turnStart := startOfLastTurn(msgs)
	lastUser := indexOfLastUser(msgs)

	aArt := megacaveArtifact(foldableRange(msgs, pairEnd, turnStart), "megacave_probeA", "")
	a = approxTokens(append(append([]utils.Message{}, aArt...), msgs[turnStart:]...))

	bArt := megacaveArtifact(foldableRange(msgs, pairEnd, lastUser), "megacave_probeB", "")
	b = approxTokens(append(append([]utils.Message{}, bArt...), msgs[lastUser]))

	cArt := megacaveArtifact(userOnlyWithLastAssistant(foldableRange(msgs, pairEnd, lastUser)), "megacave_probeC", "")
	c = approxTokens(append(append([]utils.Message{}, cArt...), msgs[lastUser]))
	return a, b, c
}

// budgetForStage picks a budget that lands the resolver on `stage`
// (1=A, 2=B, 3=C) for this fixture, i.e. big enough for that stage's own
// candidate and small enough to exclude the previous, larger one.
// Returns -1 when the fixture cannot discriminate the stages.
func budgetForStage(t *testing.T, msgs []utils.Message, stage int) int {
	t.Helper()
	a, b, c := stageSizes(t, msgs)
	switch stage {
	case 1:
		return a + 1000
	case 2:
		if b+50 >= a {
			return -1
		}
		return b + (a-b)/2
	case 3:
		if c+50 >= b {
			return -1
		}
		return c + (b-c)/2
	}
	return -1
}

// TestResolver_OverflowStageA: when content exceeds the budget, stage A keeps
// the latest full turn verbatim and megacaves everything older, so context is
// preserved via the excerpt rather than dropped.
func TestResolver_OverflowStageA(t *testing.T) {
	msgs := mkConv(200, compressibleToolChars) // 200 turns, big tool dumps -> far past any budget
	window := 65536
	budget := budgetForStage(t, msgs, 1)
	if budget < 0 {
		t.Fatal("fixture cannot reach stage A")
	}

	out, _ := resolveEcoContextCompaction(msgs, window, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty on overflow")
	}
	if approxTokens(out) > budget {
		t.Fatalf("stage A output exceeds budget: ~%d tokens / budget %d", approxTokens(out), budget)
	}

	artID, excerpt, ok := findMegacaveArtifact(out)
	if !ok {
		t.Fatal("no megacave artifact in stage A output")
	}
	if artID != "megacave_A" {
		t.Fatalf("expected stage A (artifact id megacave_A), got %q", artID)
	}
	if excerpt == "" {
		t.Fatal("megacave artifact carries an empty excerpt")
	}
	if !strings.Contains(excerpt, "USER:") {
		t.Errorf("excerpt does not look like a machine excerpt: %q", excerpt[:min(200, len(excerpt))])
	}

	// Stage A is the only stage that keeps the LATEST FULL TURN verbatim:
	// the newest user request and its assistant reply must survive.
	combined := ""
	for i := range out {
		combined += out[i].TextContent()
	}
	if !strings.Contains(combined, "user request 9") {
		t.Errorf("latest user turn lost in stage A")
	}
	if !strings.Contains(combined, "assistant reply 9") {
		t.Errorf("latest assistant reply lost in stage A")
	}
}

// TestResolver_OverflowStageB: when stage A is still too big, stage B keeps
// only the most recent USER message plus the megacave of everything older.
//
// A and B differ by the rows of the newest turn that B gives up (A keeps the
// whole turn verbatim, B keeps only its user row), so a budget between B's size
// and A's size lands deterministically on B.
func TestResolver_OverflowStageB(t *testing.T) {
	msgs := mkConv(200, compressibleToolChars)
	window := 65536
	budget := budgetForStage(t, msgs, 2)
	if budget < 0 {
		t.Fatal("fixture cannot discriminate A/B (stage A must be larger than stage B)")
	}

	out, _ := resolveEcoContextCompaction(msgs, window, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty")
	}
	if approxTokens(out) > budget {
		t.Fatalf("stage B output exceeds budget: ~%d tokens / budget %d", approxTokens(out), budget)
	}
	artID, _, ok := findMegacaveArtifact(out)
	if !ok {
		t.Fatal("no megacave artifact in stage B output")
	}
	if artID != "megacave_B" {
		t.Fatalf("expected stage B (artifact id megacave_B), got %q", artID)
	}

	// Stage B keeps the latest user message verbatim but drops the rest of
	// the latest turn (that is what distinguishes it from stage A).
	var sawLatestUser bool
	for i := range out {
		if out[i].Role == "user" && strings.Contains(out[i].TextContent(), "user request 9") {
			sawLatestUser = true
		}
	}
	if !sawLatestUser {
		t.Error("stage B must keep the most recent user message verbatim")
	}
	var sawLatestAssistant bool
	for i := range out {
		if out[i].Role == "assistant" && strings.Contains(out[i].TextContent(), "assistant reply 9") {
			sawLatestAssistant = true
		}
	}
	if sawLatestAssistant {
		t.Error("stage B should drop the tail beyond the latest user message")
	}
}

// TestResolver_OverflowStageC: with a budget too small for stages A and B, the
// resolver degrades to user messages plus the last assistant (stage C).
func TestResolver_OverflowStageC(t *testing.T) {
	msgs := mkConv(200, compressibleToolChars)
	budget := budgetForStage(t, msgs, 3)
	if budget < 0 {
		t.Fatal("fixture cannot discriminate B/C (stage B must be larger than stage C)")
	}
	out, foldFrom := resolveEcoContextCompaction(msgs, 65536, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty")
	}
	artID, excerpt, ok := findMegacaveArtifact(out)
	if !ok {
		t.Fatal("no megacave artifact in stage C output")
	}
	if artID != "megacave_C" {
		t.Fatalf("expected stage C (artifact id megacave_C), got %q", artID)
	}
	if excerpt == "" {
		t.Fatal("stage C artifact carries an empty excerpt")
	}
	// Stage C must fold strictly between the checkpoint pair and the newest
	// user turn: it never folds the live ask.
	lastUser := indexOfLastUser(msgs)
	if foldFrom > lastUser {
		t.Fatalf("foldFrom %d must never fold the newest user turn (idx %d)", foldFrom, lastUser)
	}
	if foldFrom <= 0 {
		t.Fatalf("stage C must fold at least one row, foldFrom=%d", foldFrom)
	}
}

// TestResolver_OverflowStageDDropsOldestTurns: stage D runs when stages A, B
// and C are all too big. Its contract is the documented one — drop the OLDEST
// folded turn, one at a time, rebuilding the (UNCAPPED) summary until the view
// fits — NOT an excerpt cap.
//
// Capping the excerpt is what produced the checkpoint that ended mid-sentence
// ("Clean fi"), and a cap cannot make the fit test pass: the excerpt saturates
// at the cap, so `fits` never goes true below that floor. The old greedy loop
// therefore drained the slice to nil, megacaveArtifact(nil) returned nil, and
// the resolver emitted an EMPTY history, which the caller's `len(convTail) > 0`
// guard replaced with the FULL UNCOMPACTED history (defect #161).
func TestResolver_OverflowStageDDropsOldestTurns(t *testing.T) {
	msgs := mkConv(200, compressibleToolChars)
	a, b, c := stageSizes(t, msgs)
	if c >= b {
		t.Fatalf("fixture cannot discriminate C/D: C=%d, B=%d", c, b)
	}
	// Small enough to fail A, B and C, so the resolver must land on D.
	budget := c / 4
	out, foldFrom := resolveEcoContextCompaction(msgs, 65536, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty (caller would send the full history)")
	}
	artID, excerpt, ok := findMegacaveArtifact(out)
	if !ok {
		t.Fatal("stage D must keep a megacave artifact")
	}
	if !strings.HasPrefix(artID, "megacave_D") {
		t.Fatalf("expected stage D artifact, got %q", artID)
	}
	if excerpt == "" {
		t.Fatal("stage D artifact carries an empty excerpt")
	}
	// The newest user message survives verbatim in every stage.
	var sawLatestUser bool
	for i := range out {
		if out[i].Role == "user" && strings.Contains(out[i].TextContent(), "user request 9") {
			sawLatestUser = true
		}
	}
	if !sawLatestUser {
		t.Error("stage D must keep the newest user message verbatim")
	}
	// DROPPING turns must genuinely shrink the view: that monotone shrink is
	// what makes the loop terminate (an excerpt cap does not shrink below the
	// cap, which is why the old loop never terminated).
	if got := approxTokens(out); got >= c {
		t.Errorf("stage D did not shrink the view: ~%d tokens >= stage C's ~%d", got, c)
	}
	// The loss must be recorded OUT LOUD: the dropped rows are marked compacted
	// and are NOT represented in the summary, so the summary has to say so.
	if !strings.Contains(excerpt, "oldest context dropped") {
		t.Errorf("stage D dropped %d turns worth of history without marking it in the summary", (c-approxTokens(out))/(c/200+1))
	}
	if a <= 0 || b <= 0 {
		t.Errorf("stage sizes unmeasured: A=%d B=%d", a, b)
	}
	if foldFrom <= 0 {
		t.Errorf("stage D must fold at least one row, foldFrom=%d", foldFrom)
	}
}

// TestResolver_StageDMarksNothingWhenThereIsNothingToFold: a loss marker must
// only ever claim a loss that happened.
//
// The old version of this test asked for "stage D, whole folded set kept, no
// marker" via a budget just under stage C. That state is unreachable: stage D
// rebuilds the summary from the FULL folded set (stage C only reduced the SET to
// user rows), so D's first candidate is always >= C's, and D is entered only
// because C did not fit — so D always has to drop at least one turn before it
// fits. Asserting "no drop" through the ladder therefore pins a state the code
// can never reach.
//
// The reachable no-drop state is a conversation with nothing to fold: the newest
// user turn is everything after the checkpoint pair. D then runs with an empty
// folded set, so there is no excerpt AND no rows given up — neither an artifact
// nor a marker may appear, and the oversized newest message must survive
// bounded (it is the request; folding older history cannot shrink it).
func TestResolver_StageDMarksNothingWhenThereIsNothingToFold(t *testing.T) {
	pair := checkpointArtifactMessages(nil, "eco_checkpoint", "PRIOR ROLL-UP")
	huge := strings.Repeat("z", ecoSentMessageCapChars+50000)
	msgs := append(append([]utils.Message{}, pair...), mkMsg("user", huge))

	budget := 200 // the newest message alone is far over it
	out, foldFrom := resolveEcoContextCompaction(msgs, 65536, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty (caller would send the full history)")
	}
	if artID, excerpt, ok := findMegacaveArtifact(out); ok {
		if strings.Contains(excerpt, "oldest context dropped") {
			t.Errorf("stage D claims a drop it did not make: %q", excerpt[:160])
		}
		// The prior roll-up must still be carried forward verbatim — it is the
		// only representation of everything folded in earlier rounds.
		if !strings.Contains(excerpt, "PRIOR ROLL-UP") {
			t.Errorf("prior roll-up lost from the summary: %q", excerpt[:160])
		}
		_ = artID
	}
	var sawNewestUser bool
	for i := range out {
		if out[i].Role == "user" && strings.Contains(out[i].TextContent(), "zzzz") {
			sawNewestUser = true
		}
	}
	if !sawNewestUser {
		t.Error("the newest user message must survive in every stage")
	}
	if foldFrom > len(pair) {
		t.Errorf("nothing is foldable past the checkpoint pair, foldFrom=%d (pair ends at %d)", foldFrom, len(pair)-1)
	}
}

// TestResolver_StageDDropMarkerCountIsAccurate: the marker names a row count, and
// the durable fold marks exactly those rows compacted, so the number has to be
// the number of folded rows NOT represented in the summary. A wrong count is a
// false audit trail of context loss.
func TestResolver_StageDDropMarkerCountIsAccurate(t *testing.T) {
	msgs := mkConv(200, compressibleToolChars)
	_, _, c := stageSizes(t, msgs)
	out, _ := resolveEcoContextCompaction(msgs, 65536, c/4)
	_, excerpt, ok := findMegacaveArtifact(out)
	if !ok {
		t.Fatal("stage D must keep a megacave artifact")
	}
	var claimed int
	if _, err := fmt.Sscanf(excerpt, "[oldest context dropped: %d ", &claimed); err != nil {
		t.Fatalf("marker not parseable: %q", excerpt[:120])
	}
	foldedTotal := len(foldableRange(msgs, checkpointPairEnd(msgs), indexOfLastUser(msgs)))
	if claimed <= 0 || claimed > foldedTotal {
		t.Fatalf("marker claims %d dropped rows, folded set is %d rows", claimed, foldedTotal)
	}
}

// TestResolver_NeverReturnsEmptyOnTinyBudget: regression test for defect #161.
// The old stage D hard-capped the megacave excerpt, so for any budget below the
// cap the shrink loop could never fit, returned NIL, and the caller's
// `if len(convTail) > 0` guard then sent the FULL uncompressed history — an
// oversized request on the exact path meant to shrink it. The resolver must
// therefore NEVER return an empty slice, and the newest user request must
// survive compaction — verbatim when a stage keeps the newest turn, or inside
// the megacave excerpt at stage C, which by design represents user messages
// only through the excerpt.
func TestResolver_NeverReturnsEmptyOnTinyBudget(t *testing.T) {
	for _, budget := range []int{200, 1000, 8192} {
		out, _ := resolveEcoContextCompaction(mkConv(500, compressibleToolChars), 65536, budget)
		if len(out) == 0 {
			t.Fatalf("budget %d: resolver returned empty (caller would send full history)", budget)
		}
		if !latestUserRequestSurvives(out, "user request 9") {
			t.Errorf("budget %d: newest user request not recoverable from compacted output", budget)
		}
	}
}

// latestUserRequestSurvives reports whether needle is present in the resolver
// output either as a verbatim user message or inside the megacave excerpt.
func latestUserRequestSurvives(msgs []utils.Message, needle string) bool {
	for i := range msgs {
		if msgs[i].Role == "user" && strings.Contains(msgs[i].TextContent(), needle) {
			return true
		}
	}
	if _, excerpt, ok := findMegacaveArtifact(msgs); ok && strings.Contains(excerpt, needle) {
		return true
	}
	return false
}

// TestResolver_ArtifactIsWellFormed: the artifact must be a valid assistant
// tool_call + matching tool result pair. An orphan tool message (no preceding
// tool_calls) is a hard API error for strict providers, and the artifact is
// injected at the FRONT of the history, so a malformed pair would break every
// compacted request.
func TestResolver_ArtifactIsWellFormed(t *testing.T) {
	out, _ := resolveEcoContextCompaction(mkConv(200, compressibleToolChars), 65536, 4000)
	if len(out) < 2 {
		t.Fatalf("expected at least the artifact pair, got %d messages", len(out))
	}
	if out[0].Role != "assistant" {
		t.Fatalf("artifact must open with the assistant tool_call half, got role %q", out[0].Role)
	}
	if len(out[0].ToolCalls) != 1 {
		t.Fatalf("assistant half must carry exactly one tool call, got %d", len(out[0].ToolCalls))
	}
	callID := out[0].ToolCalls[0].ID
	if callID == "" {
		t.Fatal("assistant half tool call has no id")
	}
	if out[0].ToolCalls[0].Function.Name == "" {
		t.Fatal("assistant half tool call has no function name")
	}
	if out[1].Role != "tool" {
		t.Fatalf("artifact second half must be role=tool, got %q", out[1].Role)
	}
	if out[1].ToolCallID != callID {
		t.Fatalf("tool result ToolCallID %q does not match the tool call id %q", out[1].ToolCallID, callID)
	}
	if out[1].Name == "" {
		t.Fatal("tool result must carry a Name (megacave_excerpt) so it renders correctly")
	}
	if out[1].TextContent() == "" {
		t.Fatal("tool result must carry the excerpt")
	}
}

// TestResolver_IdempotentOnAlreadySmall: resolving a small conversation twice
// must return the same unchanged result (stability across turns).
func TestResolver_IdempotentOnAlreadySmall(t *testing.T) {
	msgs := mkConv(2, 50)
	window := 65536
	budget := 60000
	first, _ := resolveEcoContextCompaction(msgs, window, budget)
	second, _ := resolveEcoContextCompaction(first, window, budget)
	if len(first) != len(second) {
		t.Fatalf("not idempotent: %d vs %d messages", len(first), len(second))
	}
	for i := range first {
		if first[i].TextContent() != second[i].TextContent() {
			t.Fatalf("message %d changed between calls", i)
		}
	}
}

// TestMegacavePreservesCodeBlock: MachineExcerptMegacave must keep code blocks
// verbatim (the /caveman-compress contract the user asked for).
func TestMegacavePreservesCodeBlock(t *testing.T) {
	code := "```go\nfunc main() { println(\"hello\") }\n```"
	msgs := []utils.Message{mkMsg("user", "here is my code:\n"+code)}
	out := MachineExcerptMegacave(msgs)
	if !strings.Contains(out, "func main() { println(\"hello\") }") {
		t.Errorf("megacave did not preserve code block verbatim:\n%s", out)
	}
}

// TestResolver_EveryStageIsSendable asserts the invariant the per-stage tests
// were missing: a resolved view must be an ACCEPTABLE chat request. Megacaving
// every user row into a tool message leaves no role:"user" row at all, and the
// chat template rejects that outright (measured on strata: stage C returned
// 400 "No user query found in messages" while fits/A/B/D cached at 98-99.9%).
// A compaction stage that cannot be sent is not a fallback, it is an outage.
func TestResolver_EveryStageIsSendable(t *testing.T) {
	msgs := mkConv(200, compressibleToolChars)
	a, b, c := stageSizes(t, msgs)
	budgets := map[string]int{
		"fits":  60000,
		"A":     a + 1000,
		"B":     b + (a-b)/2,
		"C":     c + (b-c)/2,
		"D":     200,
		"D-sat": c / 4,
	}
	for name, budget := range budgets {
		out, _ := resolveEcoContextCompaction(msgs, 65536, budget)
		if len(out) == 0 {
			t.Fatalf("%s: empty view", name)
		}
		var users, tools, opens int
		for _, m := range out {
			switch m.Role {
			case "user":
				users++
			case "tool":
				tools++
			}
		}
		if out[0].Role == "tool" {
			opens = 1
		}
		if users == 0 {
			t.Fatalf("%s: view has NO user message (roles: user=%d tool=%d) — every provider rejects this request", name, users, tools)
		}
		if opens == 1 {
			t.Fatalf("%s: view starts on a tool row — orphan tool_call", name)
		}
		// The newest user turn must survive every stage: it is the live ask.
		var newest string
		for _, m := range msgs {
			if m.Role == "user" {
				newest = m.TextContent()
			}
		}
		var found bool
		for _, m := range out {
			if m.Role == "user" && m.TextContent() == newest {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: newest user turn %q was folded away", name, newest)
		}
	}
}
