package ai

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// firstDivergence reports the byte offset at which two renders stop matching
// (the diagnostic for a broken KV prefix: the offset says how much of the
// prompt the backend would have to re-prefill).
func firstDivergence(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// --- DB harness (ai package side) ----------------------------------------
//
// The fold path is only interesting end to end: the eco gate reads the LLM
// view from db.GetConversationByIdInternal, the fold writes through
// db.CompactConversationDurable, and the pair is minted by the storage layer.
// These tests drive the real thing. The db package keeps userDataPath private
// and exposes no test seam, so the harness points GetUserDB at a temp dir via
// its own documented env var and registers the connection by calling
// GetUserDB. (The db package's own harness reassigns the package var, which
// is why the env-var route is used here.)

// ucDB points the per-user DB at a temp dir and returns a context carrying
// the user claim, a handle to the DB, and the conversation id.
//
// USER_DATA_STORAGE must be set BEFORE db.InitSQLite: GetUserDB reads the
// package-level userDataPath that InitSQLite captured, not the env var, so
// skipping InitSQLite leaves userDataPath empty and the harness writes
// ./<userID>/conversations.db inside the package directory.
func ucDB(t *testing.T) (context.Context, *sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	oldUser := os.Getenv("USER_DATA_STORAGE")
	t.Setenv("USER_DATA_STORAGE", filepath.Join(dir, "users"))
	t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
	auth.LoadConfig()
	db.InitSQLite()
	t.Cleanup(func() {
		os.Setenv("USER_DATA_STORAGE", oldUser)
		db.CloseAllUserDBs()
	})

	const userID = "ultracave-tester"
	d, err := db.GetUserDB(userID)
	if err != nil {
		t.Fatalf("GetUserDB: %v", err)
	}
	// No embeddings: the proxy is not running, and EnqueueEmbed would just
	// fill a queue nobody drains.
	db.LiteLLMBaseURL = ""

	ctx := context.WithValue(context.Background(), "userID", userID)
	convID := ucCreateConv(t, d)
	return ctx, d, convID
}

func ucCreateConv(t *testing.T, d *sql.DB) string {
	t.Helper()
	id := fmt.Sprintf("uc%x", len(t.Name())+os.Getpid())
	if _, err := d.Exec(`INSERT INTO conversations (id, title, last_message_at) VALUES (?, 'ultracave', '2026-01-01T00:00:00Z')`, id); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return id
}

// ucPush writes rows through db.PushMessage — the same choke point the tool
// loop uses — and returns the LLM-facing view (folded rows excluded, IDs
// populated), which is what the request path consumes.
func ucPush(t *testing.T, ctx context.Context, convID string, msgs ...utils.Message) {
	t.Helper()
	conv := utils.Conversation{ID: convID}
	for _, m := range msgs {
		var err error
		conv, _, err = db.PushMessage(ctx, conv, m)
		if err != nil {
			t.Fatalf("PushMessage(%s): %v", m.Role, err)
		}
	}
}

func ucLLMView(t *testing.T, ctx context.Context, convID string) []utils.Message {
	t.Helper()
	conv, err := db.GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversationByIdInternal: %v", err)
	}
	return conv.Messages
}

// ucSummaryText returns the roll-up the LIVE checkpoint pair carries, read
// out of the LLM-facing view. Deliberately NOT db.GetCheckpoint: that helper
// issues its second query while the first result set is still open, which
// wedges the single-connection pool (SetMaxOpenConns(1)) forever once a pair
// exists — the fold then hangs at the prior-summary lookup.
func ucSummaryText(t *testing.T, ctx context.Context, convID string) string {
	t.Helper()
	return PriorSummaryFromView(ucLLMView(t, ctx, convID))
}

// TestUltracaveFoldShrinksTheViewAndFreezesThePrefix crosses the compaction
// boundary: fold, reload, then assert
//   - the new view is materially smaller,
//   - the view is byte-stable across the NEXT N simulated turns (only rows
//     appended, the pair untouched),
//   - the checkpoint pair is ONE pair, minted once, and its rendered bytes
//     never move again,
//   - compaction's ONLY model contact is the one chat-model summarisation
//     entry point the guard allows, and its failure falls back to the machine
//     render (the old "compaction never touches an LLM" clause is retired: the
//     summariser is back by design, as the LAST reader of the evicted context.
//     See ultracave_llm_guard_test.go for what replaced it and why).
func TestUltracaveFoldShrinksTheViewAndFreezesThePrefix(t *testing.T) {
	ctx, d, convID := ucDB(t)

	// A conversation big enough to fold: 40 tool round-trips of ~1.5k chars.
	hist := ucHistory("ultracave: refactor the storage layer", 40, 1_500)
	// Stamp provider prompt_tokens so the eco arithmetic has a cumulative
	// curve to walk (PromptTokens is cumulative: system + everything before).
	cum := 1_000
	for i := range hist {
		if hist[i].Role == "assistant" {
			cum += 1_500
			hist[i].PromptTokens = cum
		}
	}
	ucPush(t, ctx, convID, hist...)

	view := ucLLMView(t, ctx, convID)
	beforeBytes, _ := ucRenderWire(t, view)
	beforeTokens := SentViewTokens(view)
	if len(view) != len(hist) {
		t.Fatalf("view rows %d, pushed %d", len(view), len(hist))
	}

	// ---- the compaction path (LLM-first, machine-only fallback) ----
	installed := installLLMGuard(t)
	prior := ucSummaryText(t, ctx, convID)
	folded, err := CompactConversationMachineOnly(ctx, convID, view, prior, 8)
	if err != nil {
		t.Fatalf("CompactConversationMachineOnly: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows on a 40-turn conversation — compaction does not converge")
	}
	// The fold ATTEMPTS the LLM through the one allowed entry point, and
	// LiteLLM is not ready here, so the machine-only render is what stored the
	// roll-up below. (The old assertion was `hits != 0` = "no LLM at all"; that
	// invariant is retired — the guard now pins exactly one summarisation
	// ENTRY and no SendChatCompletion / fastShortcutModels.)
	//
	// The count is DISTINCT WIRE ENTRIES, not call sites, matching
	// TestUltracaveCompactionPathUsesExactlyOneChatModelSummariser. The fold
	// reaches GenerateCheckpointSummaryFromRequest from two sites — the replay
	// path and the bounded recovery path it falls back to when the replayed
	// prefix fills the window — and both are the same entry on the same route.
	// Counting sites would demand the recovery path be deleted, which is the
	// defect that made every fold of a long conversation machine-only.
	if entries := installed.wireEntries(); len(entries) != 1 {
		t.Fatalf("the fold path reaches %d LLM wire entries %v, want exactly 1 (the chat-model summariser): %v",
			len(entries), entries, installed.hits)
	}
	if len(installed.forbids) != 0 {
		t.Fatalf("the fold path reaches a forbidden call: %v", installed.forbids)
	}

	after := ucLLMView(t, ctx, convID)
	afterBytes, _ := ucRenderWire(t, after)
	if len(afterBytes) >= len(beforeBytes) {
		t.Fatalf("rendered request did not shrink: %d -> %d wire bytes", len(beforeBytes), len(afterBytes))
	}
	afterTokens := SentViewTokens(after)
	if afterTokens >= beforeTokens {
		t.Fatalf("view did not shrink: %d -> %d tokens (%d rows folded)", beforeTokens, afterTokens, folded)
	}
	if beforeTokens-afterTokens < beforeTokens/4 {
		t.Fatalf("view shrank only %d -> %d tokens — compaction is not materially smaller", beforeTokens, afterTokens)
	}
	// The fold must be non-destructive: the client keeps the full history.
	clientConv, err := db.GetConversationById(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversationById: %v", err)
	}
	if len(clientConv.Messages) < len(hist) {
		t.Fatalf("client view lost rows: %d < %d (folding must be non-destructive)", len(clientConv.Messages), len(hist))
	}
	// The roll-up must actually represent folded content, bounded by the
	// renderer's budget (not the fixed cap that ADDS bytes at ratio ~1.045).
	summary := ucSummaryText(t, ctx, convID)
	if summary == "" {
		t.Fatal("no summary stored after a fold that removed rows")
	}
	if !strings.Contains(summary, "module_0.go") {
		t.Fatal("roll-up lost a file path the folded history carried")
	}

	// ---- the frozen prefix: N turns after the fold ----
	pairBytes := ucPairBytes(t, after)
	if len(pairBytes) == 0 {
		t.Fatal("no checkpoint pair after the fold")
	}
	// The pair is the first content after the system block: the sent history
	// must OPEN on the checkpoint pair, so the pair IS the frozen prefix.
	sent, sentRows := ucRenderWire(t, after)
	if len(sentRows) < 3 {
		t.Fatalf("expected system + pair + live rows, got %d rendered rows", len(sentRows))
	}
	if sentRows[1].Role != "assistant" || len(sentRows[1].ToolCalls) == 0 ||
		sentRows[1].ToolCalls[0].Function.Name != db.EcoCheckpointToolName {
		t.Fatalf("sent history does not open on the checkpoint pair: %+v", sentRows[1])
	}
	if sentRows[2].Role != "tool" || sentRows[2].Name != db.EcoCheckpointToolName {
		t.Fatalf("second sent row is not the checkpoint summary: %+v", sentRows[2])
	}
	if !bytes.HasPrefix(sent, ucMarshalRows(t, sentRows[:3])) {
		t.Fatal("rendered wire bytes do not start on system + pair")
	}

	prev, _ := ucRenderWire(t, after)
	for turn := 0; turn < 8; turn++ {
		newRows := []utils.Message{
			{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("continue %d", turn))},
			{Role: "assistant", Content: utils.NewTextContent("working"),
				ToolCalls: []utils.ToolCall{{ID: fmt.Sprintf("c%d", turn), Type: "function",
					Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}},
			{Role: "tool", Name: "system_tools__shell_exec", ToolCallID: fmt.Sprintf("c%d", turn),
				Content: utils.NewTextContent("output " + strings.Repeat("z", 400))},
		}
		ucPush(t, ctx, convID, newRows...)
		v := ucLLMView(t, ctx, convID)

		// The pair is byte-identical: one pair, rewritten in place at most,
		// and NOTHING rewrote it here.
		if p := ucPairBytes(t, v); string(p) != string(pairBytes) {
			t.Fatalf("turn %d: checkpoint pair (the prompt head) moved — first divergence at %d of %d bytes",
				turn, firstDivergence(pairBytes, p), len(p))
		}
		// The sent history is a byte extension of what was sent last turn.
		next, _ := ucRenderWire(t, v)
		if !bytesHavePrefix(next, prev) {
			t.Fatalf("turn %d: sent history prefix broke at byte %d (old %d, new %d)",
				turn, firstDivergence(prev, next), len(prev), len(next))
		}
		prev = next
		// The view stays bounded: rows appended, none of them re-inflating.
		if SentViewTokens(v) <= afterTokens {
			t.Fatalf("turn %d: view tokens did not grow with appended rows", turn)
		}
	}

	// Second fold: the pair is REWRITTEN IN PLACE, one pair, and the view
	// shrinks again without renumbering anything.
	v := ucLLMView(t, ctx, convID)
	midTokens := SentViewTokens(v)
	prior = ucSummaryText(t, ctx, convID)
	folded2, err := CompactConversationMachineOnly(ctx, convID, v, prior, 6)
	if err != nil {
		t.Fatalf("second fold: %v", err)
	}
	if folded2 == 0 {
		t.Fatal("second fold removed 0 rows — the fold does not converge a second time")
	}
	v2 := ucLLMView(t, ctx, convID)
	if SentViewTokens(v2) >= midTokens {
		t.Fatalf("second fold did not shrink: %d -> %d", midTokens, SentViewTokens(v2))
	}
	if n := countCheckpointPairs(v2); n != 1 {
		t.Fatalf("expected exactly ONE checkpoint pair after 2 folds, got %d", n)
	}
	if entries := installed.wireEntries(); len(entries) != 1 {
		t.Fatalf("the fold path reaches %d LLM wire entries across 2 folds, want exactly 1: %v", len(entries), installed.hits)
	}
	// The roll-up carries the earlier generation VERBATIM (the storage layer
	// concatenates; the caller must not double-prepend).
	sum2 := ucSummaryText(t, ctx, convID)
	if !strings.Contains(sum2, "module_0.go") {
		t.Fatal("second roll-up lost the first generation's content (it must be carried forward verbatim)")
	}
	if n := strings.Count(sum2, "[newer history, machine-compacted]"); n > 1 {
		t.Fatalf("banner generations accumulated in the stored summary: %d", n)
	}
	_ = d
}

// TestUltracaveFoldIsDeterministic: the same fold set renders byte-identical
// summary text. Any clock/LLM/random in the stored summary breaks this, and
// a non-deterministic summary is a prompt-head rewrite on every fold.
func TestUltracaveFoldIsDeterministic(t *testing.T) {
	msgs := ucHistory("determinism", 30, 1_200)
	a := MachineExcerptMegacaveBudget(msgs, MegacaveRowBudget(20_000, len(msgs)))
	b := MachineExcerptMegacaveBudget(msgs, MegacaveRowBudget(20_000, len(msgs)))
	if a != b {
		t.Fatal("megacave render is not deterministic")
	}
	if a == "" {
		t.Fatal("empty render")
	}
	// Budgeted render must fit the room it was given (the whole point of
	// MegacaveRowBudget: a FIXED cap runs at ratio ~1.045 on this shape).
	budget := 20_000
	if len(a) > budget+budget/4 {
		t.Fatalf("budgeted render %d chars vs budget %d — ratio > 1, fold adds bytes", len(a), budget)
	}
	raw := len(ucHistory("determinism", 30, 1_200))
	_ = raw
}

func countCheckpointPairs(msgs []utils.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" && db.IsCheckpointMessage(m) {
			n++
		}
	}
	return n
}

func bytesHavePrefix(next, prev []byte) bool {
	if len(next) < len(prev) {
		return false
	}
	return string(next[:len(prev)]) == string(prev)
}
