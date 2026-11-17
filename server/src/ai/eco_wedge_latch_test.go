package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// Regression for the fold wedge and the shrink cadence (PR #183), on the live
// conversation's own shape.
//
// Live log, conversation 141c6b2d33616fed6f2dd8dc, 2026-11-17 13:26-13:31:
//
//	13:26:26 ~150801 > trigger -> stageA folded 153 rows -> ~112509 fits=true
//	13:31:46 ~150089 > trigger -> stageA folded 0 rows -> ~114089 fits=FALSE
//	                          -> stageD: summary -3946 chars -> durable fold: 0 rows
//
// The pair is the FIRST live row, so foldableRange(pairEnd, tailStart) is empty
// and tailStart is the newest user row. The durable fold resolved the boundary
// to that user row, where `WHERE seq < boundarySeq AND compacted = 0` can only
// match 0 rows — the 216 + 51 "folded 0 rows" lines, one per message, across
// the 29 of 47 conversations whose live pair heads the view.

// wlConv is the wedged live view: the pair IS the first live row, ONE user row,
// then the in-flight wall. Nothing sits between the pair and the user row, so
// the fold range is empty by construction.
func wlConv(t *testing.T, prior string, wallRows, rowChars int) []utils.Message {
	t.Helper()
	pair := checkpointArtifactMessages(nil, utils.EcoCheckpointToolName, prior)
	msgs := append([]utils.Message{}, pair...)
	msgs = append(msgs, mkMsg("user", "TASK ANCHOR: keep investigating the cache collapse"))
	for i := 0; i < wallRows; i++ {
		msgs = append(msgs, mkMsg("assistant", "working on step "+itoa(i)))
		msgs = append(msgs, mkTool("system_tools__shell_exec", strings.Repeat("w", rowChars)))
	}
	return msgs
}

// wlPrior is a roll-up megacave can compress (repeated lines collapse) with a
// sentinel at each end, so "the shrink kept the NEWEST part" is an exact string
// test rather than a length heuristic.
func wlPrior() string {
	return "START OF PRIOR ROLL-UP " + strings.Repeat("P", 120_000) + "\nEND OF PRIOR ROLL-UP"
}

const wlEndMarker = "END OF PRIOR ROLL-UP"

// TestResolver_WedgedConversationReportsNoFold: the wedge, closed. When the
// fold range is empty the resolver must report foldFrom = 0, so index.go never
// calls the durable fold at all.
func TestResolver_WedgedConversationReportsNoFold(t *testing.T) {
	prior := wlPrior()
	// Budget well under the ~130 KB roll-up + tail, so stages B/C/D all run and
	// stage D shrinks. Every one of them used to set foldFrom = tailStart.
	for _, budget := range []int{12_000, 20_000, 40_000, 60_000} {
		msgs := wlConv(t, prior, 8, 6_000)
		if len(foldableRange(msgs, checkpointPairEnd(msgs), startOfLastTurn(msgs))) != 0 {
			t.Fatalf("fixture is not wedged (budget %d): the fold range is not empty", budget)
		}
		out, foldFrom := resolveEcoContextCompaction(msgs, 262_144, budget)
		if foldFrom != 0 {
			t.Fatalf("budget %d: empty fold range reported foldFrom %d — the durable fold resolves that to the newest user row and can only mark 0 rows compacted (the wedge, one log line per message)",
				budget, foldFrom)
		}
		if len(out) == 0 || indexOfLastUser(out) < 0 {
			t.Fatalf("budget %d: resolver shipped an unusable view (%d msgs, users %d)", budget, len(out), indexOfLastUser(out))
		}
		if summaryCarrierText(out) == "" {
			t.Fatalf("budget %d: the view carries no summary at all — the roll-up vanished with rows represented by nothing", budget)
		}
	}
}

// TestResolver_WedgeShrinksTheRollUp: foldFrom = 0 must not neuter the fit.
// Stage D still shrinks the roll-up so the request fits (as far as the tail
// physically allows) and the anchor survives.
func TestResolver_WedgeShrinksTheRollUp(t *testing.T) {
	prior := wlPrior()
	const budget = 12_000
	msgs := wlConv(t, prior, 4, 3_000)
	out, foldFrom := resolveEcoContextCompaction(msgs, 262_144, budget)
	if foldFrom != 0 {
		t.Fatalf("expected foldFrom 0, got %d", foldFrom)
	}
	// The shrink must do real work, not nothing. NOTE the expectation: #182
	// bounds the roll-up to 10 % deletion per pass over 8 passes (~57 % total),
	// deliberately — an unbounded pass once deleted 96 % of the roll-up and the
	// request missed the budget anyway. So a ~58k-token roll-up beside a
	// 4k-token tail cannot reach a 12k budget in ONE resolution; the loss
	// spreads across rounds. Asserting a hard fit here would assert that #182's
	// bound does not exist. What must be true: the roll-up shrank a lot, the
	// newest part survives, and the shipped view is much smaller than the
	// history it replaces.
	if got := approxTokens(out); got >= approxTokens(msgs) {
		t.Fatalf("shrink did nothing: ~%d tokens in, ~%d out", approxTokens(msgs), got)
	}
	excerpt, ok := artifactSummary(out)
	if !ok {
		t.Fatal("no artifact in the view")
	}
	if len(excerpt) >= len(prior) {
		t.Fatalf("roll-up not shrunk: shipped %d chars from a stored %d", len(excerpt), len(prior))
	}
	if !strings.Contains(excerpt, wlEndMarker) {
		t.Fatalf("shrink dropped the NEWEST part of the roll-up (direction is a tested product rule — TestAgenticView_SummaryShrinkDropsOldest): %q", first60(excerpt))
	}
}

// --- end-to-end: the durable loop, on the real app DB path -----------------

const (
	wlUser     = "sam"
	wlConvID   = "c_wedge_latch"
	wlWindow   = 262_144
	wlBudget   = 20_000
	wlRowChars = 1_000
)

func wlCtx() context.Context {
	return context.WithValue(context.Background(), "userID", wlUser)
}

// wlSetup creates the LIVE, ALREADY-WEDGED state production has: a block of
// compacted=1 history, the checkpoint pair as the next rows, then a live tail.
// That is 29 of 47 conversations — the live pair heads the LLM view, so the
// fold range is empty and the durable fold can only match 0 rows.
func wlSetup(t *testing.T, prior string) *sql.DB {
	t.Helper()
	t.Setenv("USER_DATA_STORAGE", t.TempDir())
	db.InitSQLite()
	handle, err := db.GetUserDB(wlUser)
	if err != nil {
		if strings.Contains(err.Error(), "no such module") {
			t.Skipf("sqlite lacks FTS5/vec0 modules: %v", err)
		}
		t.Fatalf("GetUserDB: %v", err)
	}
	t.Cleanup(func() { db.CloseAllUserDBs() })
	if _, err := handle.Exec(
		`INSERT INTO conversations (id, title, last_message_at) VALUES (?, 'wedge', '2026-01-01T00:00:00Z')`,
		wlConvID); err != nil {
		t.Fatalf("conversation row: %v", err)
	}
	// Folded history: present in the DB, invisible to the LLM view, carried
	// ONLY by the pair summary.
	for seq := 1; seq <= 6; seq++ {
		wlRaw(t, handle, seq, "user", "folded old message "+itoa(seq), 1)
	}
	// The live pair at seq 7/8.
	calls, _ := json.Marshal([]map[string]any{{"id": "eco_seed", "type": "function",
		"function": map[string]string{"name": utils.EcoCheckpointToolName, "arguments": "{}"}}})
	wlRaw(t, handle, 7, "assistant", "", 0, string(calls))
	wlRaw(t, handle, 8, "tool", prior, 0, "", utils.EcoCheckpointToolName, "eco_seed")
	// The live tail: one user row, then the in-flight wall.
	wlRaw(t, handle, 9, "user", "TASK ANCHOR: keep investigating the cache collapse", 0)
	return handle
}

func wlRaw(t *testing.T, h *sql.DB, seq int, role, text string, compacted int, rest ...string) {
	t.Helper()
	content, err := json.Marshal(utils.NewTextContent(text))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	name, toolCallID, toolCalls := "", "", ""
	if len(rest) > 0 {
		toolCalls = rest[0]
	}
	if len(rest) > 1 {
		name = rest[1]
	}
	if len(rest) > 2 {
		toolCallID = rest[2]
	}
	if _, err := h.Exec(
		`INSERT INTO messages (conversation_id, seq, role, content, name, tool_call_id, tool_calls, compacted)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		wlConvID, seq, role, string(content), name, toolCallID, toolCalls, compacted); err != nil {
		t.Fatalf("insert seq %d: %v", seq, err)
	}
}

// wlGrow appends the in-flight working context one turn grows: two tool calls
// and their results. The tail is append-only, exactly like a live agent loop.
func wlGrow(t *testing.T, h *sql.DB, turn int) {
	t.Helper()
	var seq int
	if err := h.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id = ?`, wlConvID).Scan(&seq); err != nil {
		t.Fatalf("next seq: %v", err)
	}
	for j := 0; j < 2; j++ {
		id := "call_" + itoa(turn) + "_" + itoa(j)
		args, _ := json.Marshal(map[string]string{"command": "step " + itoa(turn)})
		calls, _ := json.Marshal([]map[string]any{{"id": id, "type": "function",
			"function": map[string]string{"name": "system_tools__shell_exec", "arguments": string(args)}}})
		wlRaw(t, h, seq, "assistant", strings.Repeat("t", wlRowChars), 0, string(calls))
		seq++
		wlRaw(t, h, seq, "tool", strings.Repeat("z", wlRowChars*3), 0, "", "system_tools__shell_exec", id)
		seq++
	}
}

func wlPairState(t *testing.T, h *sql.DB) (id, seq int64) {
	t.Helper()
	if err := h.QueryRow(`SELECT id, seq FROM messages
		WHERE conversation_id = ? AND role = 'tool' AND name = ? AND compacted = 0`,
		wlConvID, utils.EcoCheckpointToolName).Scan(&id, &seq); err != nil {
		t.Fatalf("live pair: %v", err)
	}
	return
}

func wlStoredSummary(t *testing.T, h *sql.DB) string {
	t.Helper()
	var content *string
	if err := h.QueryRow(`SELECT content FROM messages
		WHERE conversation_id = ? AND role = 'tool' AND name = ? AND compacted = 0
		ORDER BY seq DESC LIMIT 1`, wlConvID, utils.EcoCheckpointToolName).Scan(&content); err != nil {
		t.Fatalf("stored summary: %v", err)
	}
	if content == nil {
		return ""
	}
	var mc utils.MessageContent
	mc.UnmarshalJSON([]byte(*content))
	return mc.TextContent()
}

func wlCompactedCount(t *testing.T, h *sql.DB) int {
	t.Helper()
	var n int
	if err := h.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND compacted = 1`, wlConvID).Scan(&n); err != nil {
		t.Fatalf("compacted count: %v", err)
	}
	return n
}

// wlRound drives one request exactly as index.go orders it: load -> gate ->
// resolve -> durable fold (only when the resolver asks) -> shrink latch.
// latch=false reproduces the pre-#183 request path (the control).
func wlRound(t *testing.T, h *sql.DB, latch bool) (view []utils.Message, foldFrom, folded int, shipped string) {
	t.Helper()
	conv, err := db.GetConversationByIdInternal(wlCtx(), wlConvID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	view, foldFrom = resolveEcoContextCompaction(conv.Messages, wlWindow, wlBudget)
	if foldFrom > 0 {
		folded, _ = db.CompactConversationDurableWithSummary(wlCtx(), wlConvID, conv.Messages, foldFrom,
			func(msgs []utils.Message) string { return MachineExcerptMegacave(msgs) }, shippedSummary(view))
	}
	shipped = summaryInView(t, view)
	if latch {
		persistRolledUpShrinkIfNoProgress(wlCtx(), wlConvID, view, foldFrom, folded)
	}
	return
}

// summaryInView returns the summary text the view ships: the megacave artifact
// when a stage built one, otherwise the carried pair row (eco_checkpoint). A
// turn whose history FITS ships the pair row verbatim — that is the byte-stable
// cache-hot case, so the fixture must read it, not demand an artifact.
func summaryInView(t *testing.T, view []utils.Message) string {
	t.Helper()
	if s, ok := artifactSummary(view); ok {
		return s
	}
	for i := range view {
		m := view[i]
		if m.Role == "tool" && m.Name == utils.EcoCheckpointToolName {
			return m.TextContent()
		}
	}
	t.Fatal("view carries no summary at all")
	return ""
}

const wlTurns = 10

// TestWedge_RollupShrinkIsLatchedInPlace runs the wedged conversation for
// wlTurns turns of tail growth and asserts the #183 contract.
func TestWedge_RollupShrinkIsLatchedInPlace(t *testing.T) {
	h := wlSetup(t, wlPrior())
	pairID0, pairSeq0 := wlPairState(t, h)
	compacted0 := wlCompactedCount(t, h)
	stored0 := len(wlStoredSummary(t, h))

	var shippedLens []int
	var storedLens []int
	var bodyEqualsStored []bool
	for turn := 1; turn <= wlTurns; turn++ {
		wlGrow(t, h, turn)
		view, foldFrom, folded, shipped := wlRound(t, h, true)
		shippedLens = append(shippedLens, len(shipped))
		st := wlStoredSummary(t, h)
		storedLens = append(storedLens, len(st))
		// The ROLL-UP BODY the view ships vs what is stored: equal means the
		// head is read from durable state, not recomputed for this request.
		bodyEqualsStored = append(bodyEqualsStored, rollupBodyFromShippedView(view) == st)
		if len(view) == 0 || indexOfLastUser(view) < 0 {
			t.Fatalf("turn %d: unusable view", turn)
		}
		if foldFrom != 0 || folded != 0 {
			t.Fatalf("turn %d: wedged conversation asked for a fold (foldFrom %d, folded %d)", turn, foldFrom, folded)
		}
		t.Logf("turn %d: shipped summary %d chars | stored %d chars (start %d) | compacted %d",
			turn, len(shipped), storedLens[turn-1], stored0, wlCompactedCount(t, h))
	}

	// (1) No wedge: the pair never moved and nothing new was compacted.
	pairID1, pairSeq1 := wlPairState(t, h)
	if pairID1 != pairID0 || pairSeq1 != pairSeq0 {
		t.Fatalf("the pair moved: id %d->%d seq %d->%d", pairID0, pairID1, pairSeq0, pairSeq1)
	}
	if c := wlCompactedCount(t, h); c != compacted0 {
		t.Fatalf("compacted rows grew %d -> %d on a conversation with nothing to fold", compacted0, c)
	}

	// (2) The shrink is durable and monotone.
	if storedLens[wlTurns-1] >= stored0 {
		t.Fatalf("the shrink was never persisted: stored %d -> %d chars", stored0, storedLens[wlTurns-1])
	}
	for i := 1; i < len(storedLens); i++ {
		if storedLens[i] > storedLens[i-1] {
			t.Fatalf("stored summary grew on turn %d: %d -> %d (the #177 re-inflation shape)", i+1, storedLens[i-1], storedLens[i])
		}
	}

	// (3) THE DISCRIMINATOR: the roll-up body the view ships IS the stored
	// summary. Pre-#183 this is impossible — the stored roll-up never shrinks,
	// so the shipped roll-up is always a per-turn recomputation of "budget
	// minus tail" (measured: shipped 37018 / 24722 / … against a stored 174000
	// that never moved). With the latch, stage D cuts the stored text once, the
	// cut lands durably, and later turns read it back: the stored roll-up is a
	// monotone step function (measured 174000 -> 36829 -> 24533 -> … -> 12237,
	// pinned from turn 7 on), so the bytes at offset 0 of the prompt stop
	// moving and prefix reuse extends THROUGH the summary (~55k tokens live)
	// instead of stalling at the 24576 checkpoint floor.
	//
	// NOTE on scope: the shipped text can still carry the ephemeral
	// tail-trim notice appended AFTER the roll-up (turns 8-10 ship 12460 chars
	// = 12237 stored + the notice). That marker sits behind the whole roll-up
	// and describes live-tail rows, which re-prefill by definition because they
	// grow every turn — it cannot invalidate the roll-up's prefix, and #183
	// deliberately does NOT persist per-request markers (they would make an
	// ephemeral loss permanent). The head is the roll-up; the head is stable.
	stableFrom := -1
	for i := range bodyEqualsStored {
		if bodyEqualsStored[i] {
			if stableFrom < 0 {
				stableFrom = i
			}
		} else if stableFrom >= 0 {
			t.Fatalf("turn %d: the shipped roll-up stopped matching the stored summary after latching (bodies %v) — the durable shrink is being recomputed",
				i+1, bodyEqualsStored)
		}
	}
	if stableFrom < 0 {
		t.Fatalf("the shipped roll-up never equalled the stored summary (shipped lens %v, stored lens %v) — the shrink is not durable",
			shippedLens, storedLens)
	}
	if stableFrom > 3 {
		t.Fatalf("the head only stabilised at turn %d (shipped lens %v) — the shrink did not converge quickly enough to matter",
			stableFrom+1, shippedLens)
	}
	t.Logf("shipped lens %v | stored lens %v | body==stored from turn %d", shippedLens, storedLens, stableFrom+1)
}

// TestWedge_WithoutLatchTheHeadKeepsMoving is the control: the same fixture and
// the same growth, with persistRolledUpShrinkIfNoProgress switched OFF. It
// reproduces production (2026-11-17 13:26 -> 13:31): the fold removes 0 rows,
// the shrink is thrown away, and the shipped summary — the head of the prompt —
// is recomputed every turn. If this test ever passes, the latch is NOT what
// stabilises the head and #183 is not the fix.
func TestWedge_WithoutLatchTheHeadKeepsMoving(t *testing.T) {
	h := wlSetup(t, wlPrior())
	stored0 := len(wlStoredSummary(t, h))
	var shippedTexts []string
	for turn := 1; turn <= 6; turn++ {
		wlGrow(t, h, turn)
		_, _, _, shipped := wlRound(t, h, false)
		shippedTexts = append(shippedTexts, shipped)
	}
	storedAtEnd := wlStoredSummary(t, h)
	if len(storedAtEnd) != stored0 {
		t.Fatalf("control wrote to the DB (stored %d -> %d) — the control must be the pre-#183 path", stored0, len(storedAtEnd))
	}
	distinct := map[string]bool{}
	for _, s := range shippedTexts {
		distinct[s] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("control shipped an identical summary on all %d turns — the fixture stopped reproducing the cadence; re-derive it before trusting the latched test", len(shippedTexts))
	}
	// The sharp half of the control: pre-#183 the shipped roll-up can NEVER
	// equal the stored summary, because the stored roll-up never shrinks — the
	// shipped text is a per-turn recomputation of "budget minus tail". #182's
	// quantum makes the shipped text coarsely stable on its own, so shipped
	// byte-identity alone is NOT the discriminator; shipped-roll-up == stored
	// is. If this loop ever trips, the control has stopped being a control and
	// the latched test proves nothing.
	for i, sh := range shippedTexts {
		if sh == storedAtEnd {
			t.Fatalf("control turn %d shipped text == stored summary — the pre-#183 path cannot produce that; the control has stopped being a control", i+1)
		}
	}
	t.Logf("control: %d distinct shipped summaries over %d turns, none equal to the stored summary (pre-#183 cadence reproduced)", len(distinct), len(shippedTexts))
}

// TestRollupBodyFromShippedView_StripsPerRequestMarkers: the latch must persist
// the roll-up body, NEVER the per-request loss markers. tailTrimmedMarker counts
// live-tail rows left out of THIS request (they return next turn); storing it in
// the pair row makes an ephemeral loss permanent and duplicates a banner that
// grows every turn, inside the only row that represents every folded message.
func TestRollupBodyFromShippedView_StripsPerRequestMarkers(t *testing.T) {
	prior := wlPrior()
	kept := prior[len(prior)/2:] // what stage D would ship: the NEWEST half
	art := mustCheckpointArtifact(nil, "megacave_D", kept, 0, 5_000, 7)
	view := append([]utils.Message{}, art...)
	view = append(view, mkMsg("user", "TASK ANCHOR"))

	body := rollupBodyFromShippedView(view)
	if body == "" {
		t.Fatal("no body extracted")
	}
	// Derived from the production marker heads, not hand-typed prose, so the
	// check cannot silently stop matching after a reword.
	for _, head := range dropMarkerPrefixes {
		if strings.Contains(body, head) {
			t.Fatalf("latch body still carries a per-request marker (%q) — it would become permanent and grow every turn", head)
		}
	}
	// The caller shipped a SHRUNK roll-up beside the markers; the body must be
	// exactly that text — markers stripped, nothing else touched.
	if len(body) >= len(prior) {
		t.Fatalf("body not shrunk: %d vs %d", len(body), len(prior))
	}
	// The body must be EXACTLY the shipped roll-up: markers gone, bytes
	// untouched. A SUFFIX of the stored prior, so no mid-text surgery moves
	// bytes in the middle of the prompt head.
	if body != kept {
		t.Fatalf("body is not the shipped roll-up verbatim (body %d chars, kept %d): %q", len(body), len(kept), first60(body))
	}
	if !strings.HasSuffix(prior, body) {
		t.Fatalf("body is not a suffix of the stored roll-up: %q", first60(body))
	}
	if !strings.Contains(body, wlEndMarker) {
		t.Fatal("body lost the newest part of the roll-up")
	}

	// The empty-roll-up banners are meta-text, never roll-up content: latching
	// them would replace the summary of 857 folded rows with a sentence.
	for _, kept := range []string{"", "x"} {
		v := mustCheckpointArtifact(nil, "megacave_D", kept, 3, 0, 0)
		if got := rollupBodyFromShippedView(v); got != "" && !strings.Contains(kept, got) {
			t.Fatalf("banner text leaked into the latch body: %q", first60(got))
		}
	}
}

func first60(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "..."
	}
	return s
}
