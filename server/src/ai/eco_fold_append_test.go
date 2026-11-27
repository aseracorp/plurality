package ai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
)

// eco_fold_append_test.go — the append-only contract, proven END TO END.
//
// The owner's requirement, verbatim: "the llm-call skips previous summaries and
// only outputs appendings. We assume an output speed of 40t/s. writing 100K
// would take 40 minutes while most of it would be 1:1 replays, unacceptable."
//
// The unit tests pin the parts (the cap arithmetic, the stitcher, the addendum
// applier). This file drives the PRODUCTION fold path twice against a fake
// proxy, so the claims are measured on the bytes the fold really posts:
//
//	fold 1 (no prior) -> the first section, stored as-is.
//	fold 2 (prior = fold 1's roll-up) ->
//	  a. the posted ANSWER BUDGET is bounded to the new material, strictly below
//	     what writing (prior + new) would cost. At ~40 tokens/second the posted
//	     max_tokens IS the wall-clock, so this is the arithmetic that makes a
//	     40-minute replay unaffordable rather than merely discouraged;
//	  b. the BRIEF does not pay to re-quote the chain: the prefix replay ships
//	     the checkpoint pair as the first content of the history, so the model
//	     can already read the roll-up. The brief points at it; quoting a 100 KB
//	     chain is prefill cost spent on a rule the model can already see;
//	  c. the STORED roll-up GROWS BY APPEND — fold 1's text is a literal prefix
//	     of fold 2's, so the backend KV prefix over the pair extends instead of
//	     diverging. The pair is the first content after the system block, so a
//	     rewrite there collapses prefix reuse for the entire prompt.

func TestFoldWithPriorPaysOnlyForTheAppend(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-append-only-e2e", 131_072, nil)

	var posts []StandardChatRequest
	// The rows the fake model echoes as its answer, set before each fold. The
	// store gate requires the fold set user rows verbatim, and the megacave-body
	// shape renders them into a server-authored block, so scraping the wire
	// cannot find them - the caller knows the fold set and passes it.
	echo := &appendEchoRows{}
	srv := newAppendProbeProxy(t, &posts, "APPEND-SECTION-9c21", echo)
	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	// ---- fold 1: no prior, so the whole document is new ---------------------
	gen1 := ucChainHistory("append generation one", 90, 2_800)
	echo.rows = ucUserTexts(gen1)
	ucPush(t, ctx, convID, gen1...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	if _, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 2, ucTarget(conv, model)); err != nil {
		t.Fatalf("fold 1: %v", err)
	}
	prior := ucSummaryText(t, ctx, convID)
	if prior == "" {
		t.Fatal("fold 1 stored no roll-up: the fixture cannot prove an append")
	}
	if len(posts) == 0 {
		t.Fatal("fold 1 never posted to the model: stage 1 is not the LLM call")
	}
	if !strings.Contains(prior, "APPEND-SECTION-9c21") {
		t.Fatalf("fold 1 did not store the model's answer:\n%s", headRunes([]byte(prior), 300))
	}

	// ---- fold 2: the prior IS fold 1's roll-up ------------------------------
	gen2 := ucChainHistory("append generation two", 90, 2_800)
	ucPush(t, ctx, convID, gen2...)
	view = ucLLMView(t, ctx, convID)
	echo.rows = ucUserTexts(view)
	conv = ucConvOf(t, ctx, convID, view, model)
	priorText := PriorSummaryFromView(view)
	if priorText == "" {
		t.Fatal("fold 2 saw no prior roll-up: the chain did not carry forward")
	}
	postsBefore := len(posts)
	if _, err := CompactConversationMachineOnly(ctx, convID, view, priorText, 2, ucTarget(conv, model)); err != nil {
		t.Fatalf("fold 2: %v", err)
	}
	var second *StandardChatRequest
	for i := postsBefore; i < len(posts); i++ {
		if posts[i].MaxTokens != nil && len(posts[i].Messages) > 1 {
			second = &posts[i]
			break
		}
	}
	if second == nil {
		t.Fatal("fold 2 posted no summariser request: the model was skipped")
	}

	// (a) THE ANSWER BUDGET. The rows fold 2 evicts are the new material; the
	// prior is what the server already stores. A model asked to write the whole
	// document (prior + new) needs (prior+new)/3 tokens; the posted budget must
	// be bounded to about half the new material, so the replay cannot be paid
	// for. This is the requirement's arithmetic, asserted on wire bytes.
	newChars := 0
	for i := range gen2 {
		newChars += SentViewRowChars(gen2[i])
	}
	if newChars <= 0 {
		t.Fatal("fixture has no measurable new material")
	}
	answerChars := *second.MaxTokens * summaryCharsPerToken
	if whole := len(priorText) + newChars; answerChars >= whole {
		t.Fatalf("the answer budget (%d chars) can write the whole document (prior %d + new %d): a 1:1 replay is affordable",
			answerChars, len(priorText), newChars)
	}
	if half := newChars/2 + minSummaryOutputTokens*summaryCharsPerToken; answerChars > half {
		t.Fatalf("the answer budget (%d chars) exceeds half the new material (%d chars): the cap is not sized to the append",
			answerChars, half)
	}
	t.Logf("append-only: prior %d chars, new %d chars, answer budget %d tokens (~%.0fs worst case at 40 t/s)",
		len(priorText), newChars, *second.MaxTokens, float64(*second.MaxTokens)/40)

	// (b) THE BRIEF CARRIES THE CONTRACT, AND QUOTES THE CHAIN ONLY WHERE THE
	// MODEL CANNOT SEE IT.
	//
	// Two request shapes reach the wire and the assert must know which one it
	// captured:
	//
	//   - the PREFIX REPLAY (stage 1's primary path) ships the whole conversation,
	//     checkpoint pair FIRST, so the model can read the roll-up in the history.
	//     The brief points at it ("head of the history") and pays nothing to quote
	//     it - a quote here is prefill cost spent on a rule the model can see;
	//   - the MEGACAVE BODY (the fallback when the replay cannot fit the declared
	//     window) ships system rows plus a rendered fold set: there the chain is
	//     genuinely invisible, and "do not copy what I show you" is the only
	//     enforceable form, so the brief QUOTES it - bounded, from the tail, with
	//     the elision announced.
	brief := wireRowText(second.Messages[len(second.Messages)-1])
	if !strings.Contains(brief, "APPEND-ONLY CONTRACT") {
		t.Fatal("the brief lost the append-only contract")
	}
	// Shape by SIZE, not by content: the megacave-body request is the head
	// plus ONE rendered row; the prefix replay ships the whole conversation.
	// (Scanning the brief for "TOOL_RESULT " misreads it - the ultracave voice
	// rules in every brief quote that label as the format to emit.)
	isMegacaveBody := len(second.Messages) <= leadingSystemRows(second.Messages)+1
	// A raw slice of the prior: headRunes appends an ellipsis marker, and a
	// quote of a short prior contains the WHOLE text, so a decorated probe
	// would report "not quoted" for a brief that quotes it in full.
	probe := priorText
	if len(probe) > 160 {
		probe = probe[:160]
	}
	quoted := strings.Contains(brief, probe)
	if isMegacaveBody {
		if !quoted {
			t.Fatal("the megacave-body brief must QUOTE the prior: the model cannot see the chain in that shape")
		}
		if !strings.Contains(brief, "never copy any of it") {
			t.Fatal("the quoted prior is not named as do-not-copy")
		}
		if len(priorText) > priorRollupTailChars && !strings.Contains(brief, "older roll-up elided") {
			t.Fatal("a bounded prior quote must announce the elision")
		}
		t.Logf("shape: megacave-body fallback (prior %d chars, quoted tail bound %d)", len(priorText), priorRollupTailChars)
	} else {
		if !strings.Contains(brief, "head of the history") {
			t.Fatalf("the prefix replay must POINT at the chain it ships - the model is told not to copy something unnamed:\n%s",
				headRunes([]byte(brief), 400))
		}
		if quoted {
			t.Fatal("the prefix replay re-quotes the prior it already ships: pure prefill cost")
		}
		t.Logf("shape: prefix replay (chain visible in the history, not re-quoted)")
	}

	// (c) THE STORED ROLL-UP GREW BY APPEND.
	stored := ucSummaryText(t, ctx, convID)
	if stored == "" {
		t.Fatal("fold 2 stored no roll-up")
	}
	if !strings.HasPrefix(stored, prior) {
		t.Fatalf("the chain was REWRITTEN, not appended (prior %d chars, stored %d chars): the KV prefix over the pair diverges at the prompt head",
			len(prior), len(stored))
	}
	if !strings.Contains(stored, "APPEND-SECTION-9c21") {
		t.Fatal("fold 2's new section is missing from the stored chain")
	}
	if n := strings.Count(stored, db.NewerHistoryBanner); n > 1 {
		t.Fatalf("the chain nests: %d section banners", n)
	}
	if len(stored) > db.MaxDurableSummaryChars {
		t.Fatalf("stored roll-up %d chars, over the %d bound", len(stored), db.MaxDurableSummaryChars)
	}
	if !strings.Contains(stored, prior) {
		t.Fatal("generation one is not carried verbatim: an append that loses the prior is a rewrite")
	}
}

// wireRowText reads a posted row's text. Content is interface{} on the wire: a
// bare string, or a parts array.
func wireRowText(m StandardMessageReq) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []StandardContentReq:
		var b strings.Builder
		for _, p := range c {
			b.WriteString(p.Text)
		}
		return b.String()
	default:
		return string(marshalNoEscape(c))
	}
}

// newAppendProbeProxy records every posted request and answers with a COMPLIANT
// checkpoint: the marker plus the user rows it was shown, copied verbatim, and
// nothing from the prior roll-up. durableFold rejects a checkpoint that
// paraphrases a user row (the fold evicts the source row, so a paraphrase is
// all that survives), so a fixture answering with canned prose is a
// non-compliant model and the fold correctly falls back to the machine render.
// appendEchoRows carries the user rows the fake model answers with. A pointer
// so the handler sees the value set at the time of each fold.
type appendEchoRows struct{ rows []string }

func newAppendProbeProxy(t *testing.T, posts *[]StandardChatRequest, marker string, echo *appendEchoRows) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req StandardChatRequest
		if err := json.Unmarshal(body, &req); err == nil {
			*posts = append(*posts, req)
		}
		// A COMPLIANT MODEL under the append-only contract: it answers with ITS
		// COMPRESSION and quotes at most one short user sentence. It does not
		// transcribe the fold set's user rows — llmFoldSummaryRenderer appends them
		// to the stored roll-up server-side, and reproducing two thirds of them is
		// what answerReplaysUserRows rejects (that rejection is the live defect the
		// fold used to die on: 22:37 and 23:19, "does not carry the user rows
		// verbatim").
		answer := marker
		for _, u := range echo.rows {
			if len(u) > 0 && len(u) < 120 {
				answer += "\nuser: " + u
				break
			}
		}
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"role": "assistant", "content": answer}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}
