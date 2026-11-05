package ai

import (
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
	out := resolveEcoContextCompaction(msgs, window, budget)
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

// budgetForStage derives budgets that deterministically land the resolver on a
// given stage, relative to the megacave excerpt cap (the stages are defined by
// what the 60k-char excerpt cap allows through, so deriving budgets from the
// constant keeps the tests honest if the cap changes).
func budgetForStage(capTokens, perTurnTokens, lastTurnTokens, lastUserTokens int, stage int) int {
	switch stage {
	case 1: // excerpt + last full turn fits
		return capTokens + lastTurnTokens + 1000
	case 2: // excerpt + last full turn does NOT fit; excerpt + last user fits
		// The excerpt saturates at the cap, so stage A ~ cap+oneTurn and
		// stage B ~ cap+oneUser. Land between them.
		_ = lastUserTokens
		return capTokens + 200
	case 3: // neither fits; user-only + last assistant fits
		return lastUserTokens*10 + 500
	default: // stage D: everything above fails, drop-oldest re-megacaves
		return 200
	}
}

// TestResolver_OverflowStageA: when content exceeds the budget, stage A keeps
// the latest full turn verbatim and megacaves everything older, so context is
// preserved via the excerpt rather than dropped.
func TestResolver_OverflowStageA(t *testing.T) {
	const capTokens = maxMegacaveExcerptChars / 3
	msgs := mkConv(200, 900) // 200 turns, big tool dumps -> exceeds the excerpt cap
	window := 65536
	budget := budgetForStage(capTokens, 931/3, 931/3, 5, 1)

	out := resolveEcoContextCompaction(msgs, window, budget)
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
func TestResolver_OverflowStageB(t *testing.T) {
	const capTokens = maxMegacaveExcerptChars / 3
	msgs := mkConv(200, 900)
	window := 65536
	budget := budgetForStage(capTokens, 931/3, 931/3, 5, 2)

	out := resolveEcoContextCompaction(msgs, window, budget)
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

// TestResolver_OverflowStageC: with a budget too small for stages A/B, the
// resolver degrades to user-only plus the last assistant (stage C).
func TestResolver_OverflowStageC(t *testing.T) {
	out := resolveEcoContextCompaction(mkConv(200, 900), 65536, 2500)
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
}

// TestResolver_OverflowStageDShrinksExcerpt: stage D runs when the budget is
// below what a megacave excerpt can naturally be (the excerpt saturates at
// maxMegacaveExcerptChars). Stage D must then cap the EXCERPT ITSELF to the
// remaining budget and keep the newest user message verbatim, so the output
// both fits and stays well-formed.
func TestResolver_OverflowStageDShrinksExcerpt(t *testing.T) {
	msgs := mkConv(200, 900)
	// Stage C (user-only megacave of 200 turns) is ~1409 tokens, so a budget
	// below ~1449 reaches stage D. The minimum stage D candidate is the
	// artifact pair plus the newest turn (user + assistant + 900-char tool
	// result, ~330 tokens), so ~1000 leaves stage D a fit it can only reach by
	// SHRINKING the excerpt — which is what this asserts.
	budget := 1000
	out := resolveEcoContextCompaction(msgs, 65536, budget)
	if len(out) == 0 {
		t.Fatal("resolver returned empty")
	}
	artID, excerpt, ok := findMegacaveArtifact(out)
	if !ok {
		t.Fatal("stage D must keep a megacave artifact when the excerpt can be capped")
	}
	if !strings.HasPrefix(artID, "megacave_D") {
		t.Fatalf("stage D artifact id should be megacave_D, got %q", artID)
	}
	if excerpt == "" {
		t.Fatal("stage D artifact carries an empty excerpt")
	}
	if len(excerpt) >= maxMegacaveExcerptChars {
		t.Errorf("stage D excerpt was not capped to the budget: %d chars", len(excerpt))
	}
	var sawLatestUser bool
	for i := range out {
		if out[i].Role == "user" && strings.Contains(out[i].TextContent(), "user request 9") {
			sawLatestUser = true
		}
	}
	if !sawLatestUser {
		t.Error("stage D must keep the newest user message verbatim")
	}
	if approxTokens(out) > budget {
		t.Errorf("stage D output exceeds budget: ~%d tokens / budget %d", approxTokens(out), budget)
	}
}

// TestResolver_NeverReturnsEmptyOnTinyBudget: regression test. The megacave
// excerpt is capped at maxMegacaveExcerptChars (~20k est tokens) while
// contentBudget floors at 8192, so for any budget below the cap stage D's
// shrink loop can never fit. The old stage D returned NIL in that case, and
// the caller's `if len(convTail) > 0` guard then sent the FULL uncompressed
// history — an oversized request on the exact path meant to shrink it. The
// resolver must therefore NEVER return an empty slice, and the newest user
// request must survive compaction — verbatim when a stage keeps the newest
// turn, or inside the megacave excerpt at stage C, which by design represents
// user messages only through the excerpt.
func TestResolver_NeverReturnsEmptyOnTinyBudget(t *testing.T) {
	for _, budget := range []int{200, 1000, 8192} {
		out := resolveEcoContextCompaction(mkConv(500, 900), 65536, budget)
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
	out := resolveEcoContextCompaction(mkConv(200, 900), 65536, 4000)
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
	first := resolveEcoContextCompaction(msgs, window, budget)
	second := resolveEcoContextCompaction(first, window, budget)
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
