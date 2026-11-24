package ai

import (
	"strings"
	"testing"
)

// megacave_fit_test.go — STAGE 1's fit test, tested directly.
//
// The end-to-end fixtures cannot discriminate the three conditions: the per-row
// budget is DERIVED from the ceiling, so on a real fold the size clause and the
// floor clause fire together (a fold with no room hits the floor AND overflows),
// and a fold with room passes both. Mutation-checked: dropping the size clause
// and dropping the floor clause each left the whole suite green. The predicate is
// therefore pinned on its own inputs, one clause at a time.

func TestMegacaveFitsCeiling_EmptyRenderNeverFits(t *testing.T) {
	// A generous ceiling: only the emptiness can refuse.
	for _, empty := range []string{"", "   ", "\n\n"} {
		if megacaveFitsCeiling(empty, 100_000, false) {
			t.Fatalf("an empty render (%q) reported a fit: the fold would store an empty section", empty)
		}
	}
}

func TestMegacaveFitsCeiling_OverCeilingEscalates(t *testing.T) {
	// THE SIZE CLAUSE. A render with real content (not at the floor) that is one
	// char over the ceiling must escalate to stage 2.
	render := strings.Repeat("content", 200) // 1400 chars, real content
	if !megacaveFitsCeiling(render, len(render), false) {
		t.Fatal("a render exactly at the ceiling reported no fit: the fold escalates for nothing")
	}
	if megacaveFitsCeiling(render, len(render)-1, false) {
		t.Fatal("a render over the ceiling reported a fit: the fold stores an excerpt the request cannot carry")
	}
}

func TestMegacaveFitsCeiling_AtRowFloorEscalates(t *testing.T) {
	// THE FLOOR CLAUSE. rows x (minMegacaveRowShowChars + label) of elision
	// markers is numerically in-window and content-free. It must escalate even
	// though the size test passes — this is the live incident's shape (1240 rows
	// folded into a checkpoint that described nothing).
	markerMush := strings.Repeat("[... elided 64 chars ...]\n", 1240)
	ceiling := len(markerMush) * 4 // generous: the size clause PASSES
	if megacaveFitsCeiling(markerMush, ceiling, true) {
		t.Fatal("a render at the per-row floor reported a fit: the fold stores marker-mush and never asks the model to write a real checkpoint")
	}
	// Same bytes, not at the floor: the floor is the only discriminator.
	if !megacaveFitsCeiling(markerMush, ceiling, false) {
		t.Fatal("the floor clause is not the discriminator: a render that is not at the floor and fits the ceiling must be stored")
	}
}

func TestMegacaveFitsCeiling_Fits(t *testing.T) {
	// The stage-1 success path: real content, within the ceiling, rows budgeted
	// above the floor.
	if !megacaveFitsCeiling("USER: fix the bug\nTOOL_RESULT read_file: module_0.go compiles", 10_000, false) {
		t.Fatal("a real render inside the ceiling reported no fit: every fold would escalate and pay for inference")
	}
}
