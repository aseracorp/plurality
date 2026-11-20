package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_followup_fold_test.go — the overflow fold is TARGET-DRIVEN and has
// HYSTERESIS.
//
// Measured live, 2026-11-20, conversation c363b2b3 (/app/data/server.log): the
// context-length recovery fold ran 20 times in one day and the live row count
// went 1828 -> 1855, UP, because every fold removed ~2 rows and the loop
// counted that as progress. Each fold rewrites the checkpoint pair — the HEAD
// of the prompt — so the churn costs KV prefix reuse and buys nothing: the loop
// cannot converge.

// foldRows builds a conversation of `pairs` tool round-trips, each row roughly
// `chars` wide, with a user row every 5 rows so char-boundary snapping has
// legal landings. No PromptTokens: this is the shape the recovery fold sees
// right after a provider 500, where the token arithmetic has nothing to work
// with and the CHAR boundary is the only measure left.
func foldRows(pairs, chars int) []utils.Message {
	msgs := []utils.Message{}
	body := strings.Repeat("x", chars)
	for i := 0; i < pairs; i++ {
		if i%5 == 0 {
			msgs = append(msgs, utils.Message{Role: "user",
				Content: utils.NewTextContent("ask " + itoa(i))})
		}
		msgs = append(msgs,
			utils.Message{Role: "assistant", Content: utils.NewTextContent("call " + itoa(i))},
			utils.Message{Role: "tool", Name: "filesystem_server__fs_read",
				Content: utils.NewTextContent(body)},
		)
	}
	return msgs
}

// TestOverflowFoldCharBoundaryFoldsARealFraction is the 1855-row production
// shape. Before this fix the boundary fell back to "keep the newest 24 rows",
// which after the newest-user-row floor and the tool-row snap is a fold of ~2
// rows. The char boundary (the measure the frozen path ships) must fold the
// oldest rows until the REST fits the target, so ONE fold removes a meaningful
// fraction of the conversation.
func TestOverflowFoldCharBoundaryFoldsARealFraction(t *testing.T) {
	target := ecoTargetTokens()
	msgs := foldRows(1200, 200) // ~300k wire chars, far over any target
	view := SentViewTokens(msgs)
	if view <= target {
		t.Fatalf("fixture is not over target: view %d tokens, target %d", view, target)
	}

	cand := UltracaveOverflowFoldFrom(msgs, 24)
	if cand <= 0 {
		t.Fatalf("no fold boundary (got %d) on a %d-row conversation %d tokens over target %d",
			cand, len(msgs), view, target)
	}
	// One fold must move the conversation materially: the boundary is expected
	// at the point where the REST fits HALF the target, so it sits far back.
	if got, want := cand, ultracaveFloorBoundary(msgs); got < want {
		t.Fatalf("boundary %d folds less than the expected region (want >= %d): view %d tokens, target %d",
			got, want, view, target)
	}
	if cand >= len(msgs)/2 {
		t.Logf("fold removes %d of %d live rows (view %d tokens, target %d)", cand, len(msgs), view, target)
	} else {
		t.Fatalf("fold removes only %d of %d rows — the 2-row churn shape", cand, len(msgs))
	}
	if bought := ultracaveFoldProgressTokens(msgs, cand); bought < ultracaveMinFoldProgressTokens() {
		t.Fatalf("accepted fold buys %d tokens, under the hysteresis floor %d", bought, ultracaveMinFoldProgressTokens())
	}
}

// ultracaveFloorBoundary is the minimum boundary the fix must reach: the newest
// user row is the FLOOR, so the boundary cannot pass it, and the char boundary
// must be a substantial distance back from it.
func ultracaveFloorBoundary(msgs []utils.Message) int {
	lu := -1
	for i, m := range msgs {
		if m.Role == "user" {
			lu = i
		}
	}
	return lu / 2
}

// TestOverflowFoldHysteresisRefusesATinyFold pins the new contract: in the
// churn regime (view over target), a fold that buys less than
// ultracaveMinFoldProgressTokens is REFUSED with -1. No boundary means no
// write, which leaves the checkpoint pair BYTE-IDENTICAL — the existing 0-row
// contract — and the caller falls through to the ephemeral in-memory trim.
func TestOverflowFoldHysteresisRefusesATinyFold(t *testing.T) {
	target := ecoTargetTokens()
	msgs := []utils.Message{}
	// A small, cheap prefix: 100 rows of ~50 chars, so folding it all buys
	// well under the floor.
	for i := 0; i < 100; i++ {
		msgs = append(msgs, utils.Message{Role: "assistant",
			Content: utils.NewTextContent(strings.Repeat("a", 50))})
	}
	// The newest user turn, then a huge tail: the tail alone puts the SENT
	// VIEW over target, which is the churn regime, while the rows the boundary
	// could fold are almost free.
	big := strings.Repeat("b", maxCompactMessageChars)
	msgs = append(msgs,
		utils.Message{Role: "user", Content: utils.NewTextContent("the live ask")},
		// lastPT just over target, so findCutoffIndex DOES run and picks a
		// boundary near the end — the exact 2-row fold the loop used to accept.
		utils.Message{Role: "assistant", Content: utils.NewTextContent(big), PromptTokens: target + 10_000},
		utils.Message{Role: "tool", Name: "filesystem_server__fs_read", Content: utils.NewTextContent(big)},
	)

	view := SentViewTokens(msgs)
	if view <= target {
		t.Fatalf("fixture is not over target: view %d tokens, target %d", view, target)
	}
	cand := UltracaveOverflowFoldFrom(msgs, 24)
	if cand != -1 {
		t.Fatalf("tiny fold accepted at boundary %d; hysteresis must refuse it (floor %d tokens, view %d, target %d)",
			cand, ultracaveMinFoldProgressTokens(), view, target)
	}
}

// TestOverflowFoldHonoursNewestUserFloorAndToolSnap pins the invariants the
// char-boundary fallback must not break: the newest user row is never folded
// away, and the live window never opens on an orphan tool result.
func TestOverflowFoldHonoursNewestUserFloorAndToolSnap(t *testing.T) {
	msgs := foldRows(1200, 200)
	// End on a tool row whose user row is far back: the floor must clamp.
	msgs = append(msgs, utils.Message{Role: "tool", Name: "filesystem_server__fs_read",
		Content: utils.NewTextContent("trailing tool result")})

	cand := UltracaveOverflowFoldFrom(msgs, 24)
	if cand <= 0 {
		t.Fatalf("no boundary (got %d)", cand)
	}
	lu := -1
	for i, m := range msgs {
		if m.Role == "user" {
			lu = i
		}
	}
	if cand > lu {
		t.Fatalf("boundary %d folds past the newest user row %d", cand, lu)
	}
	if msgs[cand].Role == "tool" {
		t.Fatalf("boundary %d opens the live window on an orphan tool row", cand)
	}
	if db.IsCheckpointMessage(msgs[cand]) {
		t.Fatalf("boundary %d sits on the checkpoint pair", cand)
	}
}
