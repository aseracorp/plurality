package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The checkpoint pair sits at the HEAD of the view: it is the first content
// after the system block, and the durable fold stores exactly this text
// verbatim (db.CompactConversationDurableWithSummary). The backend conversation
// cache can only extend past it while its leading bytes stay put.
//
// Two facts measured on the running system make the placement of the loss
// markers a cache question and not a style question:
//
//   - the live checkpoint row is 77823 bytes and starts with
//     "[oldest context dropped:129 messages of this conversation..."
//     — a per-fold count occupying offset 0 of the prompt's first content;
//   - the engine reports "prompt ... = 24576 reused + ... read", and 24576 is
//     exactly the system-prompt-only boundary, i.e. NOTHING after the system
//     block is being reused.
//
// These tests pin the property that keeps the prefix cacheable: the head of the
// summary must not depend on a number that changes every fold, and the summary
// must grow by APPEND so the bytes already sent stay a prefix of the next
// request. mkMsg / mkTool / findMegacaveArtifact come from eco_resolver_test.go
// (same package).

func checkpointSummary(folded []utils.Message, prior string, droppedRows int) (string, bool) {
	art := megacaveArtifactWithDropMarker(folded, "megacave_D", prior, droppedRows)
	if art == nil {
		return "", false
	}
	for _, m := range art {
		if m.Role == "tool" {
			return m.TextContent(), true
		}
	}
	return "", false
}

// TestDropMarker_IsNotAtHeadOfCheckpointContent: the marker must never occupy
// offset 0 of the summary. The loss still has be named out loud — asserting it
// is present is the point of the companion assertion — it just must not be the
// first bytes of the prompt's first content block.
func TestDropMarker_IsNotAtHeadOfCheckpointContent(t *testing.T) {
	folded := []utils.Message{mkMsg("user", "ask one"), mkMsg("assistant", "reply one")}
	got, ok := checkpointSummary(folded, "", 7)
	if !ok {
		t.Fatal("stage D shipped no artifact — a fold with rows given up must keep a summary row")
	}
	if strings.HasPrefix(got, "[oldest context dropped") {
		t.Errorf("per-fold drop count sits at offset 0 of the summary, so the prompt head moves every fold: %q", first(got, 90))
	}
	if !strings.Contains(got, "messages of this conversation were folded away") {
		t.Errorf("drop must be named in the summary: %q", first(got, 120))
	}
}

// TestDropMarker_HeadIsInvariantAcrossRounds: the strongest form of the cache
// property. Fold N and fold N+k report different row counts; the leading line of
// the summary must be byte-identical, because the cache matches on prefix.
// If the head is a per-round count, every fold invalidates everything after the
// system block.
func TestDropMarker_HeadIsInvariantAcrossRounds(t *testing.T) {
	folded := []utils.Message{
		mkMsg("user", "ask one"),
		mkTool("shell", strings.Repeat("x", compressibleToolChars)),
		mkMsg("assistant", "reply one"),
	}
	var head string
	for _, dropped := range []int{1, 7, 129, 500} {
		got, ok := checkpointSummary(folded, "", dropped)
		if !ok {
			t.Fatalf("droppedRows=%d: no artifact", dropped)
		}
		line := strings.SplitN(strings.TrimSpace(got), "\n", 2)[0]
		if head == "" {
			head = line
			continue
		}
		if line != head {
			t.Errorf("summary head moved with the drop count (droppedRows=%d): %q != %q — a moving head forces the backend to re-prefill everything after the system block",
				dropped, line, head)
		}
	}
	if head == "" {
		t.Fatal("no summary produced")
	}
}

// TestDropMarker_SummaryGrowsByAppend: the carried roll-up is a PREFIX of the
// next round's summary, so bytes the backend already prefilled stay valid. The
// new excerpt and its marker may only be APPENDED.
func TestDropMarker_SummaryGrowsByAppend(t *testing.T) {
	round1 := []utils.Message{mkMsg("user", "ask one"), mkMsg("assistant", "reply one")}
	carried1, ok := checkpointSummary(round1, "", 0)
	if !ok {
		t.Fatal("round 1 produced no summary")
	}
	round2 := []utils.Message{mkMsg("user", "ask two"), mkMsg("assistant", "reply two")}
	carried2, ok := checkpointSummary(round2, carried1, 3)
	if !ok {
		t.Fatal("round 2 produced no summary")
	}
	if !strings.HasPrefix(carried2, strings.TrimSpace(carried1)) {
		t.Errorf("round 2 rewrote the head of the roll-up instead of appending to it:\n round1=%q\n round2=%q",
			first(carried1, 120), first(carried2, 120))
	}
}

// TestDropMarker_SummarySurvivesAnEmptyExcerpt: a fold that gives up rows must
// never return nil, because the durable fold marks those rows compacted=1 and a
// nil artifact means nothing represents them any more. With the marker moved
// out of the front, the emptiness test runs on the CARRIED text and the marker
// is what keeps the pair alive when the excerpt renders empty.
func TestDropMarker_SummarySurvivesAnEmptyExcerpt(t *testing.T) {
	folded := []utils.Message{mkMsg("user", "   ")}
	got, ok := checkpointSummary(folded, "", 1)
	if !ok {
		t.Fatal("fold gave up a row and shipped no summary — the durable fold would delete that history with no trace")
	}
	if !strings.Contains(got, "messages of this conversation were folded away") {
		t.Errorf("the summary must state the loss when it has nothing else to say: %q", first(got, 120))
	}
}
