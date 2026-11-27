package ai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stageSpy answers the fold ladder's three briefs and records which briefs it
// saw, in order. The brief text is the ONLY reliable way to name a stage from
// the request: stage 1's megacave fallback logs the word "stage 2", and the
// same builder serves all three stages.
type stageSpy struct {
	mu       sync.Mutex
	calls    []string // "1" / "2" / "3" per call, in order
	bodies   map[string][]string
	userRows []string
	t1, t2   func() string
	t3       func(body string) string
}

func (s *stageSpy) bodiesFor(mark string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies[mark]...)
}

func (s *stageSpy) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := string(body)
	mark := ""
	switch {
	case strings.Contains(req, "COMPACT THE ARCHIVE, EDIT-ONLY"):
		mark = "3"
	case strings.Contains(req, "SUMMARISE the range"):
		mark = "2"
	case strings.Contains(req, "COMPRESS that range"):
		mark = "1"
	}
	var answer string
	if mark == "1" {
		answer = s.t1()
	} else if mark == "2" {
		answer = s.t2()
	} else if mark == "3" {
		answer = s.t3(req)
	}
	s.mu.Lock()
	s.calls = append(s.calls, mark)
	if s.bodies == nil {
		s.bodies = map[string][]string{}
	}
	s.bodies[mark] = append(s.bodies[mark], req)
	s.mu.Unlock()
	resp := map[string]interface{}{
		"choices": []map[string]interface{}{
			{"message": map[string]interface{}{"role": "assistant", "content": answer}},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *stageSpy) seen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.calls, ",")
}

// padUser echoes the fold set's user rows verbatim (the fold rejects a
// checkpoint that paraphrases them) after a marker line and FILLER, so the
// answer is a known size and the projection into the next stage is arithmetic
// the test controls.
// padToCeiling pads to just under the fold's ANSWER ceiling (so the validator's
// size rule passes) and above the fold target (so the projection into the next
// stage fires). Both numbers are computed at runtime from the model's window,
// which is why the sizes are passed in.
func (s *stageSpy) padTo(marker, endMarker string, ceilChars, overChars int) string {
	var b strings.Builder
	b.WriteString(marker + "\n")
	filler := "filler line that keeps the answer long so the projection crosses the fold target\n"
	for b.Len() < ceilChars*9/10 {
		b.WriteString(filler)
	}
	// Quote ONE short user row (what a real model does), never the whole set:
	// reproducing two thirds of the fold set's user rows is a replay, and the
	// server carries them in the stored roll-up itself.
	for _, u := range s.userRows {
		if len(u) > 0 && len(u) < 120 {
			b.WriteString("\nuser: " + u)
			break
		}
	}
	// A marker at the END of the section, because the stage briefs quote the
	// prior's TAIL (priorRollupTailChars) — a head marker is elided from the
	// quote, and an assertion on it would test the elision, not the plumbing.
	b.WriteString("\n" + endMarker)
	return b.String()
}

// TestFoldLadderReachesStage2AndStage3 drives the WHOLE ladder in one fold:
// stage 1 compresses (LLM success), the projected view is still over target so
// stage 2 summarises (LLM success), the archive is still over target so stage 3
// compacts it by edit (LLM success), and the compacted archive is what stores.
//
// WHY THIS TEST EXISTS: across the whole log history the ladder produced
// 2 stage-1 successes, 0 stage-2 successes and 0 stage-3 successes. The two
// success paths that have never run in production had no test either, so
// "stages 2 and 3 exist" was unverified in code and in the field at once.
//
// The fixture is sized so each stage's arithmetic fires: a small window (small
// target), no tool schemas (stage 2's body must fit the window — at the live
// 28395 tokens of schemas it cannot, and that is a separate, real defect), and
// answers padded past the projection bar.
func TestFoldLadderReachesStage2AndStage3(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-ladder-all3", 8_192, nil)

	hist := ucHistory("ladder", 20, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)

	spy := &stageSpy{userRows: ucUserTexts(view)}
	// ceiling ~= foldedChars/2/3, target ~= (window-4096-10240-4096)*2/3+4096.
	// The fixture's folded range is the bulk of the history; pad to 90 % of the
	// ceiling and the answer is over the target for an 8192-token window.
	spy.t1 = func() string { return spy.padTo("STAGE1-COMPRESSED: 4c88", "STAGE1-SECTION-END-4c88", 3300, 1) }
	spy.t2 = func() string { return spy.padTo("STAGE2-SUMMARY: 9f01", "STAGE2-SECTION-END-9f01", 3300, 1) }
	spy.t3 = func(body string) string {
		// Answer the stage-3 grammar: delete an entry by its FIRST LINE, add one
		// ultracave entry that carries the surviving facts.
		return "- STAGE1-COMPRESSED: 4c88\n+ [done] stage 1's range was compressed, then stage 2 summarised it; see module_0.go"
	}
	srv := httptest.NewServer(http.HandlerFunc(spy.serve))
	t.Cleanup(srv.Close)
	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 3, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows")
	}
	seen := spy.seen()
	t.Logf("stage calls in order: [%s] rows=%d", seen, folded)
	if !strings.Contains(seen, "1") {
		t.Fatal("stage 1 never ran")
	}
	if !strings.Contains(seen, "2") {
		t.Fatalf("stage 2 never ran (calls=[%s]): the projection after stage 1 did not cross the target", seen)
	}
	if !strings.Contains(seen, "3") {
		t.Fatalf("stage 3 never ran (calls=[%s]): the archive after stage 2 did not cross the target", seen)
	}
	// NO-ERASURE ACROSS STAGES. Stage 2 ends in RewriteCheckpointSummary, a
	// FULL-STATE replacement of the archive text. Pre-fix it was handed the
	// PRE-FOLD prior as its prior, so its rewrite deleted the section stage 1 had
	// just written - and after the fold that section is the ONLY record of the
	// rows that left the context. The fix passes the chain stage 1 wrote, so
	// stage 2's request must show stage 1's section.
	var sawStage1InStage2 bool
	for _, b := range spy.bodiesFor("2") {
		if strings.Contains(b, "STAGE1-SECTION-END-4c88") {
			sawStage1InStage2 = true
		}
	}
	if !sawStage1InStage2 {
		t.Fatal("stage 2's request did not carry stage 1's section: its rewrite of the archive " +
			"erases the only record of the rows the fold just evicted")
	}
	// Stage 3's edits must apply to the archive the model was SHOWN: the bounded
	// chain, which for this fixture is the model's own stage-2 output.
	var sawStage2InStage3 bool
	for _, b := range spy.bodiesFor("3") {
		if strings.Contains(b, "STAGE2-SECTION-END-9f01") {
			sawStage2InStage3 = true
		}
	}
	if !sawStage2InStage3 {
		t.Fatal("stage 3's request did not carry the archive stage 2 wrote")
	}

	stored := ucSummaryText(t, ctx, convID)
	if !strings.Contains(stored, "[done] stage 1's range was compressed") {
		t.Fatalf("stage 3's compaction entry is not what stores:\n%s", headRunes([]byte(stored), 400))
	}
	if strings.Contains(stored, "STAGE1-COMPRESSED: 4c88") {
		t.Fatal("stage 3's delete did not apply: the deleted entry is still in the archive")
	}
}
