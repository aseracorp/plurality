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
// stageSizes measures the ESTIMATED token size of each stage candidate for a
// fixture. The summary is UNCAPPED, so these sizes are a property of the
// fixture and the megacave transform, not of a cap constant; deriving budgets
// from measurements keeps the tests honest when the transform changes.
//
// Mirrors resolveEcoContextCompaction stage-for-stage on a conversation with no
// checkpoint (pairEnd == -1). Since the fold-loop fix every stage keeps the
// SAME live tail (all rows from the newest user turn onwards) and differs only
// in how much of the FOLDED set the summary represents, so the probe measures
// exactly that: A = whole folded set, B = the B floor (oldest turns given up,
// at least one turn kept), C = the B floor reduced to user/assistant rows.
// The probe tool-call ids MUST carry the "megacave_" prefix: the estimator
// (boundedWireChars) exempts the rolling-summary row from the per-message send
// cap via checkpointSummaryCarrier, matching the tool name "megacave_<stage>".
// A probe named "probe_A" is NOT exempt, so approxTokens clamps it and reports
// a stage size far below what the resolver measures — budgets derived from it
// then land on the wrong stage.
func stageSizes(t *testing.T, msgs []utils.Message) (a, b, c int) {
	t.Helper()
	pairEnd := checkpointPairEnd(msgs)
	tailStart := startOfLastTurn(msgs)
	if tailStart >= 0 && tailStart <= pairEnd {
		tailStart = pairEnd + 1
	}
	if tailStart < 0 {
		tailStart = 0
	}
	tail := TruncateOversizedMessages(msgs[tailStart:])
	folded := foldableRange(msgs, pairEnd, tailStart)

	aArt := megacaveArtifact(folded, "megacave_probeA", "")
	a = approxTokens(append(append([]utils.Message{}, aArt...), tail...))

	// B floor: give up the OLDEST folded turns, keeping at least one turn
	// represented. dropOldestTurn drains to EMPTY (len 0 < len(prev) keeps the
	// loop going), while searchVariants(allowEmpty:false) stops one variant
	// short of the empty one — so the probe must stop at the last NON-EMPTY
	// variant. Probing the empty variant measures C's floor and the fixture
	// then reports "cannot discriminate B/C".
	floor := folded
	for {
		next := dropOldestTurn(floor)
		if len(next) >= len(floor) || len(next) == 0 {
			break
		}
		floor = next
	}
	bArt := megacaveArtifactWithDropMarker(floor, "megacave_probeB", "", len(folded)-len(floor))
	b = approxTokens(append(append([]utils.Message{}, bArt...), tail...))

	cFloor := userOnlyWithLastAssistant(floor)
	cArt := megacaveArtifactWithDropMarker(cFloor, "megacave_probeC", "", len(folded)-len(cFloor))
	c = approxTokens(append(append([]utils.Message{}, cArt...), tail...))
	return a, b, c
}

// budgetForStage picks a budget that lands the resolver on `stage`
// (1=A, 2=B, 3=C) for this fixture, i.e. big enough for that stage's own
// candidate and small enough to exclude the previous, larger one.
// Returns -1 when the fixture cannot discriminate the stages.
//
// BUDGET-AWARE (step 4 of the plan): the resolver renders the machine excerpt
// with a per-row budget DERIVED from the budget it is given
// (MegacaveRowBudget), so a stage's size is a function of the budget, not a
// constant of the fixture. Probing uncapped sizes and then picking a budget
// from them lands the resolver one stage early — the exact failure the probe
// comments above describe, one layer up. The budgets are therefore found by
// bisecting on "does this stage fit at this budget", using the same rendering
// the resolver uses.
func budgetForStage(t *testing.T, msgs []utils.Message, stage int) int {
	t.Helper()
	a, _, c := stageSizes(t, msgs)
	if stage == 1 {
		return a + 1000
	}
	lo, hi := c/4, a+1000 // the answer for stages 2/3 lies in this range
	switch stage {
	case 2:
		// The budget that lands the resolver on B: the last budget at which
		// stage A's own candidate does NOT fit (the resolver escalates), and at
		// which B's best candidate DOES fit.
		esc := escalationPoint(t, msgs, 1, lo, hi)
		if esc < 0 {
			return -1 // A always fits: B is unreachable on this fixture
		}
		budget := esc - 1
		if budget <= c+50 {
			return -1 // below C's floor: cannot discriminate B/C
		}
		if probeStageSize(msgs, budget, 2) > budget {
			return -1 // B does not fit there either: the resolver would land on C
		}
		return budget
	case 3:
		esc := escalationPoint(t, msgs, 2, lo, hi)
		if esc < 0 {
			return -1 // B always fits: C is unreachable on this fixture
		}
		budget := esc - 1
		if probeStageSize(msgs, budget, 3) > budget {
			return -1 // C does not fit there: the resolver would land on D
		}
		return budget
	}
	return -1
}

// escalationPoint returns the SMALLEST budget at which stage `stage`'s candidate
// FITS, i.e. the boundary at which the resolver stops escalating past it. The
// caller uses budget = return-1 to land the resolver ON the next stage.
//
// `size > budget` is true at small budgets (the render floors at
// minMegacaveRowShowChars per row) and false at large ones (the render plateaus
// at the uncapped size while the budget keeps growing), so the predicate is
// monotone and the boundary bisects. Returns -1 when the stage fails across the
// whole range and `lo` when it already fits at `lo`.
func escalationPoint(t *testing.T, msgs []utils.Message, stage, lo, hi int) int {
	t.Helper()
	fails := func(budget int) bool { return probeStageSize(msgs, budget, stage) > budget }
	if !fails(lo) {
		return lo
	}
	if fails(hi) {
		return -1
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if fails(mid) {
			lo = mid + 1 // still escalating at mid: the boundary is above it
		} else {
			hi = mid // fits at mid: the boundary is at or below it
		}
	}
	return lo
}

// probeStageSize mirrors the resolver's rendering of stage `stage` (1=A, 2=B,
// 3=C) at `budget`, including the per-row megacave budget the resolver derives
// from it.
//
// For B and C it measures the FLOOR variant (the smallest allowed one), because
// that is the variant deciding escalation: the resolver escalates past a stage
// exactly when even its floor misses the budget. Measuring every variant makes
// the probe O(N^2) in characters — the same cost the production binary search
// exists to avoid (a 500-turn fixture spent 147 s in stage D).
func probeStageSize(msgs []utils.Message, budget, stage int) int {
	pairEnd := checkpointPairEnd(msgs)
	tailStart := startOfLastTurn(msgs)
	if tailStart >= 0 && tailStart <= pairEnd {
		tailStart = pairEnd + 1
	}
	if tailStart < 0 {
		tailStart = 0
	}
	tail := TruncateOversizedMessages(msgs[tailStart:])
	folded := foldableRange(msgs, pairEnd, tailStart)
	room := (budget - approxTokens(tail) - megacaveArtifactOverheadTokens) * 3
	size := func(set []utils.Message, drop int) int {
		art := megacaveArtifactWithDropMarker(set, "megacave_probe", "", drop,
			MegacaveRowBudget(room, len(set)))
		return approxTokens(append(append([]utils.Message{}, art...), tail...))
	}
	if stage == 1 {
		return size(folded, 0)
	}
	floor := folded
	for {
		next := dropOldestTurn(floor)
		if len(next) >= len(floor) {
			break
		}
		if stage == 2 && len(next) == 0 {
			// allowEmpty: false — the resolver's stage B stops one variant short
			// of the empty one. Walking to the empty variant measures C's floor and
			// makes B look like it fits at every budget, so the bisection returns
			// the boundary of a stage that cannot exist and the fixture reports
			// "cannot discriminate B/C".
			break
		}
		if stage == 3 {
			next = userOnlyWithLastAssistant(next)
			if len(next) >= len(floor) {
				break
			}
		}
		floor = next
	}
	if stage == 2 && len(floor) == len(folded) {
		// allowEmpty: false with a single-turn set: the floor equals A.
		return size(folded, 0)
	}
	return size(floor, len(folded)-len(floor))
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
	// Stage B must NOT give up the live tail. The old contract ("B keeps only
	// the newest user row") is the collapse-to-floor bug: on the agentic shape
	// the newest user row is index 2 and everything after it is the in-flight
	// working context, so shipping that one row discarded the agent's whole
	// context for 97 consecutive turns (conversation 6751a6d5ac78c156d07fec21).
	// B differs from A only in how much FOLDED history the summary keeps.
	if !sawLatestAssistant {
		t.Error("stage B must keep the live tail (rows after the newest user message) — dropping it is the collapse-to-floor bug")
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
	// CONTRACT AMENDED (owner-authorized, brief step "amend the destructive
	// contracts"): the old assert was `foldFrom > 0` — stage C must durably
	// fold. Stage C reduces the summary to user/assistant rows, so its summary
	// represents a strict SUBSET of the fold set (measured: 13 of 597 rows).
	// The authorization gate refuses to persist a fold whose rows the shipped
	// summary does not represent, because the stored summary is the only
	// representation of a compacted row: folding 597 rows from a 13-row summary
	// is the annihilation (live: conv 9658ae45a32388ed1dafaac1, 232 rows marked
	// from a ~1384-token summary).
	//
	// So the durable half of this contract is gone by design, and what replaces
	// it is the part that protects the user: the view that ships FITS the
	// budget, and the durable state is untouched (foldFrom == 0 = ephemeral,
	// every row returns next request). Durability lives in stage A, which
	// represents the whole fold set losslessly (see
	// TestResolver_LosslessStageAFoldsDurably and the closed-loop test).
	if foldFrom != 0 {
		t.Fatalf("a lossy stage must NOT persist a fold: gate should force foldFrom = 0, got %d", foldFrom)
	}
	if got := approxTokens(out); got > budget {
		t.Fatalf("stage C must still ship within budget: ~%d tokens / budget %d", got, budget)
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
	// CONTRACT AMENDED (owner-authorized, brief step "amend the destructive
	// contracts"): the old assert was `foldFrom > 0`. Stage D fits by shipping
	// the SMALLEST artifact — measured "represents 0 of 597 fold-set rows" — so
	// the authorization gate refuses to persist it: a fold from a summary that
	// represents nothing is deletion, not compaction. Stage D ships EPHEMERAL
	// (the request gets its budget; every row comes back next turn), and
	// durability is stage A's job. The shrink/loss-marker asserts above are the
	// parts of this contract that survive.
	if foldFrom != 0 {
		t.Errorf("a lossy stage must NOT persist a fold: gate should force foldFrom = 0, got %d", foldFrom)
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
		// The two loss markers SHARE the "oldest context dropped:" prefix and
		// count DIFFERENT things (folded rows vs characters of the roll-up), so
		// a coarse Contains on the prefix audits the wrong loss. The rows marker
		// is the one that would be a lie here: the folded set is EMPTY, so no
		// row was given up and the durable fold marks nothing.
		if strings.Contains(excerpt, "messages of this conversation were folded away") {
			t.Errorf("stage D claims it folded rows away, folded set is empty: %q", first(excerpt, 200))
		}
		if strings.Contains(excerpt, "live context trimmed") {
			t.Errorf("stage D claims a tail trim with nothing trimmed: %q", first(excerpt, 200))
		}
		// The prior roll-up is the only representation of everything folded in
		// earlier rounds, so it is dropped ONLY when the request physically has
		// no room for it: here the newest message alone (bounded to
		// ecoSentMessageCapChars) dwarfs the 200-token budget, so the roll-up
		// cannot be kept and the honest empty-roll-up banner is correct. If the
		// budget leaves room, the roll-up must be carried forward verbatim.
		anchorTokens := approxTokens(TruncateOversizedMessages(msgs[2:]))
		if room := 200 - anchorTokens; room > 60 && !strings.Contains(excerpt, "PRIOR ROLL-UP") {
			t.Errorf("prior roll-up dropped although ~%d tokens of room remain: %q", room, first(excerpt, 200))
		}
		// An empty roll-up must always carry a banner saying so — the fold marks
		// the represented rows compacted=1, so a silent checkpoint is permanent
		// amnesia. Two banners exist: the plain "nothing to summarise" one, and
		// the "dropped in full" one stage D emits when it had a roll-up and the
		// request cannot fit any of it.
		bannered := strings.Contains(excerpt, "rolled into this checkpoint") ||
			strings.Contains(excerpt, "dropped in full")
		if !strings.Contains(excerpt, "PRIOR ROLL-UP") && !bannered {
			t.Errorf("empty roll-up carried with no honest banner: %q", first(excerpt, 200))
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
	foldedTotal := len(foldableRange(msgs, checkpointPairEnd(msgs), startOfLastTurn(msgs)))
	tailTotal := len(msgs) - startOfLastTurn(msgs)

	// Two distinct losses, two distinct markers, two denominators. The durable
	// one counts FOLDED rows (compacted=1, gone); the tail one counts rows
	// trimmed from the LIVE tail for this request only (compacted=0, they come
	// back). Asserting a single literal silently audits the wrong loss.
	var rowsClaimed, tailClaimed, charsClaimed int
	var sawRows, sawTail, sawChars bool
	if i := strings.Index(excerpt, "[oldest context dropped: "); i >= 0 {
		if _, err := fmt.Sscanf(excerpt[i:], "[oldest context dropped: %d ", &rowsClaimed); err == nil {
			sawRows = true
		}
	}
	if i := strings.Index(excerpt, "[oldest context dropped: "); i >= 0 {
		var n int
		if _, err := fmt.Sscanf(excerpt[i:], "[oldest context dropped: %d characters", &n); err == nil {
			charsClaimed, sawChars = n, true
		}
	}
	if i := strings.Index(excerpt, "[live context trimmed for this request: the "); i >= 0 {
		if _, err := fmt.Sscanf(excerpt[i:], "[live context trimmed for this request: the %d most recent", &tailClaimed); err == nil {
			sawTail = true
		}
	}
	if !sawRows && !sawTail && !sawChars {
		t.Fatalf("no loss marker in the stage D summary: %q", first(excerpt, 200))
	}
	if sawRows && (rowsClaimed <= 0 || rowsClaimed > foldedTotal) {
		t.Fatalf("marker claims %d folded rows dropped, folded set is %d rows", rowsClaimed, foldedTotal)
	}
	if sawChars && charsClaimed <= 0 {
		t.Fatalf("chars marker claims %d characters", charsClaimed)
	}
	if sawTail {
		// The tail trim is bounded by the tail, and must be counted as
		// "rows given up beyond the task anchor", never as the anchor itself.
		if tailClaimed <= 0 || tailClaimed > tailTotal-1 {
			t.Fatalf("tail marker claims %d trimmed rows, tail is %d rows (anchor always kept)", tailClaimed, tailTotal)
		}
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
