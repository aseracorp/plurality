package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// mkAgenticView builds the LLM-facing view of a long AGENTIC conversation — the
// shape that produced the live 24289-token collapse.
//
//	[0] assistant(eco_checkpoint tool_call)
//	[1] tool(eco_checkpoint summary)     <- the pair, at the HEAD of the view
//	[2] user("the task")                 <- the ONLY user row in the view
//	[3...] assistant(tool_calls) / tool  <- hundreds of rows, no user rows
//
// GetConversationByIdInternal returns compacted=0 rows ordered by seq, so an
// agentic conversation is one user row followed by a wall of assistant/tool
// rows. The newest user message therefore sits at index 2 and everything AFTER
// it is the in-flight working context.
func mkAgenticView(priorChars, steps, toolChars int) []utils.Message {
	pair := checkpointArtifactMessages(nil, "eco_checkpoint",
		"START OF PRIOR ROLL-UP "+strings.Repeat("P", priorChars)+"\nEND OF PRIOR ROLL-UP")
	msgs := append([]utils.Message{}, pair...)
	msgs = append(msgs, mkMsg("user", "TASK ANCHOR: fix the eco compaction bug"))
	for i := 0; i < steps; i++ {
		msgs = append(msgs, utils.Message{
			Role: "assistant",
			ToolCalls: []utils.ToolCall{{
				ID:   "call_" + itoa(i%10),
				Type: "function",
				Function: utils.FunctionCall{
					Name:      "system_tools__shell_exec",
					Arguments: "{\"command\":\"step " + itoa(i%10) + "\"}",
				},
			}},
		})
		msgs = append(msgs, mkTool("system_tools__shell_exec", strings.Repeat("z", toolChars)))
	}
	return msgs
}

// liveTailRows counts rows from the newest user message onwards.
func liveTailRows(msgs []utils.Message) int {
	i := indexOfLastUser(msgs)
	if i < 0 {
		return 0
	}
	return len(msgs) - i
}

// stageOf reports which stage produced the view, from the artifact tool name.
func stageOf(msgs []utils.Message) string {
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Name, "megacave_") {
			return m.Name
		}
	}
	return "no-artifact"
}

// artifactSummary returns the summary text carried by the view's artifact.
func artifactSummary(msgs []utils.Message) (string, bool) {
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Name, "megacave_") {
			return m.TextContent(), true
		}
	}
	return "", false
}

// TestAgenticView_TailSurvivesWhenItFits is the regression test for the
// collapse-to-floor.
//
// The live view of an agentic conversation has ONE user row, at index 2, with
// the entire in-flight working context AFTER it. Stages B, C and D all defined
// the tail as "the newest user message" and sent only that row — which is how a
// 125807-token request became a flat 24289-token request for 97 consecutive
// turns (conversation 6751a6d5ac78c156d07fec21, 2026-11-14): system + tools +
// the stale checkpoint summary + ONE user row, zero live history. The live log
// reads "3 messages, ≈20151 tokens ... folding 2 of 166".
//
// Here the tail (~18k est tokens) fits every budget tested, so the whole tail
// MUST survive every stage. A stage that drops it is not a fallback, it is the
// context-loss bug.
func TestAgenticView_TailSurvivesWhenItFits(t *testing.T) {
	// 6 turns = 13 tail rows. The point of this test is "the tail fits", so the
	// tail MUST be smaller than the SMALLEST budget it sweeps (20000) — the old
	// fixture self-checked against the LARGEST budget (60000) and measured
	// ~60506 tokens, so every budget in the sweep forced a trim and the test
	// failed on its own fixture, not on the resolver.
	msgs := mkAgenticView(30_000, 6, compressibleToolChars)
	tailRows := liveTailRows(msgs)
	if tailRows < 12 {
		t.Fatalf("fixture is not agentic-shaped: tail rows %d", tailRows)
	}
	if got := approxTokens(msgs[2:]); got > 20_000 {
		t.Fatalf("fixture tail too big for this test: ~%d tokens (smallest budget swept is 20000)", got)
	}

	for _, budget := range []int{60_000, 40_000, 30_000, 24_000, 20_000} {
		out, foldFrom := resolveEcoContextCompaction(msgs, 262_144, budget)
		if len(out) == 0 {
			t.Fatalf("budget %d: resolver returned an empty view", budget)
		}
		if !latestUserRequestSurvives(out, "TASK ANCHOR") {
			t.Fatalf("budget %d: task anchor lost", budget)
		}
		if got := liveTailRows(out); got < tailRows {
			t.Fatalf("budget %d: live tail collapsed from %d rows to %d — in-flight context discarded (stage %s)",
				budget, tailRows, got, stageOf(out))
		}
		// foldFrom == 0 means "nothing folded" — the correct answer when the
		// whole history fits (byte-stable prefix for KV cache reuse). Only a
		// NON-zero foldFrom that reaches into the pair is the bug.
		if pairEnd := checkpointPairEnd(msgs); foldFrom > 0 && foldFrom <= pairEnd {
			t.Fatalf("budget %d: foldFrom %d folds the checkpoint pair (ends at %d)", budget, foldFrom, pairEnd)
		}
		var users int
		for _, m := range out {
			if m.Role == "user" {
				users++
			}
		}
		if users == 0 {
			t.Fatalf("budget %d: view has no user message", budget)
		}
		if out[0].Role == "tool" {
			t.Fatalf("budget %d: view opens on a tool row (orphan tool_call)", budget)
		}
	}
}

// TestAgenticView_PriorSummarySurvives: on the agentic shape the fold range
// between the pair and the newest user turn is EMPTY, so the carried-forward
// roll-up is the only representation of everything folded in earlier rounds
// (the durable fold marks those rows compacted=1 — nothing else in the
// conversation carries them). Contract, split by what is physically possible:
//
//   - tail beside the smallest artifact FITS the budget: the roll-up's NEWEST
//     part must survive (the shrink cuts its head, never its tail), and any
//     shrink must be announced.
//   - the tail ALONE exceeds the budget: nothing can keep both, the roll-up may
//     be given up — but then the view must SAY so. A checkpoint that silently
//     represents nothing is the silent-amnesia shape.
func TestAgenticView_PriorSummarySurvives(t *testing.T) {
	const priorChars = 30_000
	const endMarker = "END OF PRIOR ROLL-UP"
	const startMarker = "START OF PRIOR ROLL-UP"
	const fullDropBanner = "dropped in full"
	for _, budget := range []int{60_000, 20_000, 4_000, 1_000, 200} {
		msgs := mkAgenticView(priorChars, 20, compressibleToolChars)
		tail := TruncateOversizedMessages(msgs[startOfLastTurn(msgs):])
		minArt := mustCheckpointArtifact(nil, "megacave_D", "", 0, 0, 0)
		tailAlone := approxTokens(append(append([]utils.Message{}, minArt...), tail...))
		canKeepRollUp := tailAlone+40 < budget

		out, foldFrom := resolveEcoContextCompaction(msgs, 262_144, budget)
		excerpt, ok := artifactSummary(out)
		if !ok {
			// No artifact at all means the prior roll-up is gone AND the view
			// declares foldFrom > 0: the durable fold marks the folded rows
			// compacted=1 and nothing represents them. Silent amnesia.
			t.Fatalf("budget %d: prior checkpoint summary dropped entirely (no artifact in the view)", budget)
		}
		// A NON-zero foldFrom must start strictly after the pair. foldFrom == 0
		// means "nothing foldable" (the pair heads the live view, so the fold
		// range is empty) and cannot reach into anything — asserting on 0 was
		// the shape the sibling test TestAgenticView_TailSurvivesWhenItFits
		// already guards with `foldFrom > 0 &&`.
		if pe := checkpointPairEnd(msgs); foldFrom > 0 && foldFrom <= pe {
			t.Fatalf("budget %d: foldFrom %d reaches into the checkpoint pair", budget, foldFrom)
		}

		kept := strings.Contains(excerpt, endMarker)
		bannered := strings.Contains(excerpt, fullDropBanner) || strings.Contains(excerpt, "rolled into this checkpoint")
		if canKeepRollUp {
			if !kept {
				t.Fatalf("budget %d: newest part of the prior roll-up lost (tail alone fits: ~%d tokens / %d): %q",
					budget, tailAlone, budget, first(excerpt, 160))
			}
			// Shrunk => head (oldest) went, announced, and what remains is a
			// SUFFIX of the stored roll-up, not an arbitrary middle slice.
			if !strings.Contains(excerpt, startMarker) {
				if !strings.Contains(excerpt, "oldest context dropped") {
					t.Errorf("budget %d: roll-up shrank (head marker gone) with no drop marker", budget)
				}
				if i := strings.Index(excerpt, endMarker); i > 4_000 &&
					!strings.Contains(excerpt[i-4_000:i], strings.Repeat("P", 200)) {
					t.Errorf("budget %d: summary is not a suffix of the stored roll-up", budget)
				}
			}
			continue
		}
		if !kept && !bannered {
			t.Fatalf("budget %d: roll-up given up silently — no marker says the checkpoint represents nothing: %q",
				budget, first(excerpt, 200))
		}
	}
}

// TestAgenticView_OversizedTailDegradesGradually covers the physically
// unavoidable case: the tail alone is bigger than the budget, so NOTHING can
// make it fit. The resolver must degrade the tail GRADUALLY — keep the newest
// user row plus as many recent rows as fit — and never collapse to "one user
// row and nothing else", and it must SAY so in the summary.
func TestAgenticView_OversizedTailDegradesGradually(t *testing.T) {
	// ~363k est tokens of tail: exceeds any window the deployment declares.
	msgs := mkAgenticView(4_000, 120, compressibleToolChars)
	tailRows := liveTailRows(msgs)

	out, foldFrom := resolveEcoContextCompaction(msgs, 262_144, 60_000)
	if len(out) == 0 {
		t.Fatal("resolver returned an empty view")
	}
	got := liveTailRows(out)
	if got < 2 {
		t.Fatalf("tail collapsed to %d rows: the collapse-to-floor shape (old bug: 1 row, 97 turns)", got)
	}
	if got >= tailRows {
		t.Fatalf("fixture is not oversized: tail %d rows survived a 60000 budget", got)
	}
	// The trim must be gradual, not a cliff: a 60k budget against ~363k of tail
	// must keep materially more than the bare anchor.
	if got < 8 {
		t.Fatalf("tail trimmed to %d rows of %d — that is a cliff, not a graded degrade", got, tailRows)
	}
	// The loss must be declared, and must be labelled as EPHEMERAL (the rows
	// are not durable-folded, they come back next turn).
	excerpt, ok := artifactSummary(out)
	if !ok {
		t.Fatal("no artifact in the view")
	}
	if !strings.Contains(excerpt, "live context trimmed") {
		t.Errorf("tail trimmed by %d rows with no marker in the summary", tailRows-got)
	}
	if !strings.Contains(excerpt, "still in the conversation") {
		t.Errorf("marker does not state that the trim is ephemeral")
	}
	// The task anchor is kept.
	if !latestUserRequestSurvives(out, "TASK ANCHOR") {
		t.Error("task anchor lost")
	}
	// The fold must not reach into the tail.
	if foldFrom > indexOfLastUser(msgs) {
		t.Errorf("foldFrom %d folds past the newest user message", foldFrom)
	}
}

// TestAgenticView_SummaryShrinkDropsOldest encodes the agreed last-resort rule
// (notes/2026-11-14-eco-fold-loop-design.md): when the summary has to shrink,
// it keeps the NEWEST content and drops the OLDEST.
func TestAgenticView_SummaryShrinkDropsOldest(t *testing.T) {
	// Build a roll-up whose oldest part is identifiable.
	prior := "OLDEST SECTION " + strings.Repeat("A", 40_000) + "\n\nNEWEST SECTION " + strings.Repeat("B", 8_000)
	msgs := checkpointArtifactMessages(nil, "eco_checkpoint", prior)
	msgs = append(msgs, mkMsg("user", "TASK ANCHOR: small live ask"))

	out, _ := resolveEcoContextCompaction(msgs, 262_144, 8_000)
	excerpt, ok := artifactSummary(out)
	if !ok {
		t.Fatal("no artifact in the view")
	}
	if !strings.Contains(excerpt, "NEWEST SECTION") {
		t.Error("summary shrink dropped the NEWEST content — it must keep it and drop the oldest")
	}
	if strings.Contains(excerpt, "OLDEST SECTION") {
		t.Error("summary shrink kept the oldest content while dropping newer content")
	}
	if !strings.Contains(excerpt, "oldest context dropped") {
		t.Error("summary shrink happened with no drop marker")
	}
}

// TestAgenticView_TailFitsIsNeverTrimmed: the tail trim must fire ONLY when the
// live tail alone physically cannot fit.
//
// The step-2 shrink used to size the roll-up by arithmetic —
// (budget - tailTokens) * 3 — while fits() measures the whole candidate through
// boundedWireChars, where every row carries wire overhead. The summary was
// therefore sized too large, the step-2 fit test failed, and step 3 fired on a
// false premise: it logged "live tail exceeds budget" for a tail of ~18161
// tokens against a budget of 24000 (5839 tokens of headroom) and dropped 2 live
// in-flight rows to buy room it did not need. That is the context-loss bug in
// its mildest form — the rows it discards are the agent's current working set,
// and the request was already going to fit.
//
// Contract: if the tail beside the SMALLEST artifact the resolver can emit fits
// the budget, the resolver must reach that budget by shrinking the roll-up
// alone and keep every tail row.
func TestAgenticView_TailFitsIsNeverTrimmed(t *testing.T) {
	const steps = 6
	const tailRowsWant = 2*steps + 1
	for _, budget := range []int{28_000, 26_000, 24_000, 22_000, 20_500} {
		msgs := mkAgenticView(30_000, steps, compressibleToolChars)
		tail := TruncateOversizedMessages(msgs[startOfLastTurn(msgs):])
		tailRows := len(tail)
		if tailRows != tailRowsWant {
			t.Fatalf("fixture shape drifted: tail rows %d, want %d", tailRows, tailRowsWant)
		}
		// Physical feasibility: the tail beside the smallest possible artifact.
		minArt := mustCheckpointArtifact(nil, "megacave_D", "", 0, 0, 0)
		tailAlone := approxTokens(append(append([]utils.Message{}, minArt...), tail...))
		if tailAlone+40 > budget {
			t.Fatalf("fixture is not in the fitted regime: tail alone ~%d tokens / budget %d", tailAlone, budget)
		}

		out, _ := resolveEcoContextCompaction(msgs, 262_144, budget)
		if len(out) == 0 {
			t.Fatalf("budget %d: resolver returned an empty view", budget)
		}
		if got := liveTailRows(out); got < tailRows {
			t.Fatalf("budget %d: tail trimmed to %d of %d rows although the tail alone fits (~%d tokens / %d) — context given up for nothing (stage %s)",
				budget, got, tailRows, tailAlone, budget, stageOf(out))
		}
		if !latestUserRequestSurvives(out, "TASK ANCHOR") {
			t.Fatalf("budget %d: task anchor lost", budget)
		}
		// The roll-up may shrink to reach the budget, and must say so.
		excerpt, ok := artifactSummary(out)
		if !ok {
			t.Fatalf("budget %d: no artifact — folded rows unrepresented", budget)
		}
		if !strings.Contains(excerpt, "END OF PRIOR ROLL-UP") && !strings.Contains(excerpt, "rolled into this checkpoint") {
			t.Fatalf("budget %d: newest roll-up content lost while trimming the tail (summary %q)", budget, first(excerpt, 120))
		}
	}
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
