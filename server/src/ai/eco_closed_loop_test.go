package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// Closed-loop test: the whole compaction cycle, on the REAL app DB path.
//
// The bug this reproduces: "a conversation grew to the model's context size and
// then FAILED TO CONTINUE, instead of compacting earlier and continuing
// cleanly." Every other eco test drives the resolver on an in-memory slice, and
// that is why the bug survived: the resolver can return a view that fits while
// the DURABLE fold marks rows compacted that the sent view does not represent,
// so the NEXT request reloads a conversation no smaller than the one just sent.
// The fold reports progress, the request keeps inflating, and the loop wedges.
// Live signatures: "durable compaction folded 0 messages at boundary 2" on
// every message, and the 16 accumulated eco_checkpoint pairs measured in
// conversation 6751a6d5ac78c156d07fec21.
//
// So this test loops the production sequence end to end:
//
//	load (GetConversationByIdInternal)
//	  -> eco gate (index.go)
//	  -> resolveEcoContextCompaction  (the view that gets SENT)
//	  -> db.CompactConversationDurable (the fold that must make it DURABLE)
//	  -> reload, append a new turn, repeat
//
// asserting every round: the sent view fits the budget, the fold made real
// progress, exactly ONE live checkpoint pair exists, and the ORIGINAL ask is
// still represented. Convergence, not just a one-shot fit.
//
// The per-user SQLite DB is resolved from USER_DATA_STORAGE by db.InitSQLite
// (the same path the server uses), so t.Setenv keeps this off real data. The
// test skips if this sqlite build lacks FTS5/vec0, mirroring the db tests.

const (
	clUser     = "sam"
	clConvID   = "c_closed_loop"
	clBudget   = 40_000 // content budget (ecoContentBudget analogue)
	clWindow   = 262_144
	clToolRun  = 9_000 // chars of compressible tool output per tool row
	clRounds   = 8     // rounds of "agent works, then the user speaks again"
	clAskMark  = "ORIGINAL ASK"
	clTurnMark = "TASK STEP"
)

// clCtx is the request context the app path expects: CompactConversationDurable
// and GetConversationByIdInternal both read ctx.Value("userID").
func clCtx() context.Context {
	return context.WithValue(context.Background(), "userID", clUser)
}

// clSetup points USER_DATA_STORAGE at a temp dir, runs InitSQLite (the real app
// path — InitSQLite reads the env var, GetUserDB then opens
// users-data/<user>/conversations.db and applies the schema), creates the
// conversation row and returns the handle.
func clSetup(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("USER_DATA_STORAGE", t.TempDir())
	db.InitSQLite()
	handle, err := db.GetUserDB(clUser)
	if err != nil {
		if strings.Contains(err.Error(), "no such module") {
			t.Skipf("sqlite lacks FTS5/vec0 modules: %v", err)
		}
		t.Fatalf("GetUserDB: %v", err)
	}
	// GetUserDB caches the handle in a process-wide sync.Map. CloseAllUserDBs
	// closes and evicts every cached handle — this package has no other DB, and
	// the next test re-opens one.
	t.Cleanup(func() { db.CloseAllUserDBs() })
	if _, err := handle.Exec(
		`INSERT INTO conversations (id, title, last_message_at) VALUES (?, 'closed loop', '2026-01-01T00:00:00Z')`,
		clConvID); err != nil {
		t.Fatalf("conversation row: %v", err)
	}
	return handle
}

// clInsert writes one message at the next seq, using the same content encoding
// the app writes (utils.NewTextContent -> MarshalJSON), so the loader's
// unmarshalMessageContent path is exercised.
func clInsert(t *testing.T, handle *sql.DB, role, text string, name, toolCallID string, toolCalls string) {
	t.Helper()
	var seq int
	if err := handle.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id = ?`, clConvID).Scan(&seq); err != nil {
		t.Fatalf("next seq: %v", err)
	}
	content, err := json.Marshal(utils.NewTextContent(text))
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if _, err := handle.Exec(
		`INSERT INTO messages (conversation_id, seq, role, content, name, tool_call_id, tool_calls)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		clConvID, seq, role, string(content), name, toolCallID, toolCalls); err != nil {
		t.Fatalf("insert %s seq %d: %v", role, seq, err)
	}
}

// clAddRound appends one user turn plus the agent work that follows it: the
// agentic shape the LLM-facing view actually has (one user row, then a wall of
// assistant/tool rows). The first round carries the ORIGINAL ASK marker.
func clAddRound(t *testing.T, handle *sql.DB, round int) {
	t.Helper()
	ask := fmt.Sprintf("%s: fix the eco compaction bug in server/src/ai/eco.go", clAskMark)
	if round > 1 {
		ask = fmt.Sprintf("%s: continue step %d of the eco compaction fix", clTurnMark, round)
	}
	clInsert(t, handle, "user", ask+fmt.Sprintf("\nround %d detail line\nend of round %d", round, round), "", "", "")
	clInsert(t, handle, "assistant", fmt.Sprintf("Working on round %d.", round), "", "", "")
	for j := 0; j < 3; j++ {
		id := fmt.Sprintf("call_%d_%d", round, j)
		args, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("step %d tool %d", round, j)})
		calls, _ := json.Marshal([]map[string]interface{}{{
			"id": id, "type": "function",
			"function": map[string]string{"name": "system_tools__shell_exec", "arguments": string(args)},
		}})
		clInsert(t, handle, "assistant", "", "", "", string(calls))
		clInsert(t, handle, "tool", strings.Repeat("x", clToolRun), "system_tools__shell_exec", id, "")
	}
	clInsert(t, handle, "assistant", fmt.Sprintf("Round %d complete.", round), "", "", "")
}

// clLivePairs counts the live (compacted=0) checkpoint pairs. The live
// conversation 6751a6d5ac78c156d07fec21 had accumulated SIXTEEN of them: every
// fold added a pair instead of superseding the old one, so the LLM view carried
// 16 transcripts of 61-68 KB each and the history stopped shrinking.
func clLivePairs(t *testing.T, handle *sql.DB) int {
	t.Helper()
	var n int
	if err := handle.QueryRow(
		`SELECT COUNT(*) FROM messages
		 WHERE conversation_id = ? AND role = 'tool' AND name = 'eco_checkpoint' AND compacted = 0`,
		clConvID).Scan(&n); err != nil {
		t.Fatalf("count pairs: %v", err)
	}
	return n
}

// clLiveSummary returns the summary text of the live checkpoint tool row — the
// string that sits at the head of every prompt after the system block.
func clLiveSummary(t *testing.T, handle *sql.DB) string {
	t.Helper()
	var content *string
	err := handle.QueryRow(
		`SELECT content FROM messages
		 WHERE conversation_id = ? AND role = 'tool' AND name = 'eco_checkpoint' AND compacted = 0
		 ORDER BY seq DESC LIMIT 1`, clConvID).Scan(&content)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("live summary: %v", err)
	}
	if content == nil {
		return ""
	}
	var mc utils.MessageContent
	mc.UnmarshalJSON([]byte(*content))
	return mc.TextContent()
}

// clAskRepresented reports whether the original ask is still in the request:
// verbatim as a live user row, or inside the checkpoint summary.
func clAskRepresented(view []utils.Message, summary string) bool {
	for _, m := range view {
		if m.Role == "user" && strings.Contains(m.TextContent(), clAskMark) {
			return true
		}
	}
	return strings.Contains(summary, clAskMark)
}

// clRunRound drives the production sequence for one request: gate -> resolver
// -> durable fold, exactly as index.go orders them. Returns the view that would
// be sent, the fold boundary, and how many rows the fold marked compacted.
func clRunRound(t *testing.T, handle *sql.DB) (view []utils.Message, foldFrom, folded int) {
	t.Helper()
	conv, err := db.GetConversationByIdInternal(clCtx(), clConvID)
	if err != nil {
		t.Fatalf("GetConversationByIdInternal: %v", err)
	}
	est := SentViewTokens(conv.Messages)
	if est <= clBudget {
		// Below the trigger: index.go sends the conversation unchanged.
		return conv.Messages, 0, 0
	}
	view, foldFrom = resolveEcoContextCompaction(conv.Messages, clWindow, clBudget)
	if foldFrom > 0 {
		// index.go's sequence: store the summary the resolver shipped, so the
		// durable state is the view that was budgeted to fit — not a bigger
		// one the fold composes itself.
		folded, err = db.CompactConversationDurableWithSummary(
			clCtx(), clConvID, conv.Messages, foldFrom,
			func(msgs []utils.Message) string { return MachineExcerptMegacave(msgs) },
			shippedSummary(view),
		)
		if err != nil {
			t.Fatalf("CompactConversationDurableWithSummary: %v", err)
		}
	}
	return view, foldFrom, folded
}

// TestEcoClosedLoop_FoldConvergesAndPreservesContext is the end-to-end
// regression for "the conversation grew to the context size and then failed to
// continue, instead of compacting earlier and continuing cleanly".
//
// It runs the production sequence for clRounds user turns and asserts, every
// round:
//
//  1. FIT      — the view that gets sent fits the content budget.
//  2. PROGRESS — when the resolver asks for a fold, the durable fold marks rows
//     (no "folded 0 rows at boundary 2" churn).
//  3. CONVERGE — after the fold, RELOADING the conversation yields a view that
//     fits the budget. This is the property the in-memory tests
//     cannot see: if the fold marks rows compacted that the sent
//     view does not represent, the next request inflates again and
//     the loop never converges.
//  4. ONE PAIR — exactly one live eco_checkpoint pair exists after every round
//     (the live conversation had 16).
//  5. NO LOSS  — every user row this conversation folded away (compacted=1) is
//     represented in the live checkpoint summary. A fold that marks
//     rows compacted without saying what they contained is silent
//     amnesia, which is the reported "lost context".
//  6. ANCHOR   — the original ask is still in the request (verbatim live row or
//     inside the summary), so the model never loses the task.
func TestEcoClosedLoop_FoldConvergesAndPreservesContext(t *testing.T) {
	handle := clSetup(t)

	compactionRounds := 0
	var loadedEachRound []int

	for round := 1; round <= clRounds; round++ {
		clAddRound(t, handle, round)

		before, err := db.GetConversationByIdInternal(clCtx(), clConvID)
		if err != nil {
			t.Fatalf("round %d preload: %v", round, err)
		}
		beforeTokens := SentViewTokens(before.Messages)

		view, foldFrom, folded := clRunRound(t, handle)
		after, err := db.GetConversationByIdInternal(clCtx(), clConvID)
		if err != nil {
			t.Fatalf("round %d reload: %v", round, err)
		}
		afterTokens := SentViewTokens(after.Messages)
		loadedEachRound = append(loadedEachRound, afterTokens)
		summary := clLiveSummary(t, handle)

		t.Logf("round %d: live rows %d/%d ~%d -> sent %d rows ~%d | foldFrom %d folded %d | reload ~%d tokens | pairs %d | summary %d chars",
			round, len(before.Messages), len(after.Messages), beforeTokens,
			len(view), approxTokens(view), foldFrom, folded, afterTokens,
			clLivePairs(t, handle), len(summary))

		// 1. FIT
		if got := approxTokens(view); got > clBudget+40 {
			t.Fatalf("round %d: sent view is ~%d tokens, over the %d budget — the request exceeds the window",
				round, got, clBudget)
		}

		// 2. PROGRESS
		if foldFrom > 0 && folded == 0 {
			t.Fatalf("round %d: resolver asked for a fold at %d and durable compaction folded 0 rows — history NOT shrinking (the live wedge)",
				round, foldFrom)
		}
		if beforeTokens > clBudget {
			compactionRounds++
			if foldFrom <= 0 {
				t.Fatalf("round %d: %d tokens over budget %d and the resolver folded nothing",
					round, beforeTokens, clBudget)
			}
		}

		// 3. CONVERGE — a compaction round must leave the reloaded conversation
		// small enough that the NEXT request does not need to compact again on
		// the same bytes.
		if foldFrom > 0 && afterTokens > clBudget {
			t.Fatalf("round %d: fold marked %d rows compacted at %d, yet the reloaded view is ~%d tokens (budget %d) — every later request re-inflates, so compaction never kicks in durably",
				round, folded, foldFrom, afterTokens, clBudget)
		}

		// 4. ONE PAIR
		if pairs := clLivePairs(t, handle); pairs > 1 {
			t.Fatalf("round %d: %d live eco_checkpoint pairs (expected at most 1 — 16 pairs is the live accumulation)",
				round, pairs)
		}

		// 5. NO LOSS — every folded user row must be represented in the summary.
		rows, err := handle.Query(
			`SELECT content FROM messages
			 WHERE conversation_id = ? AND role = 'user' AND compacted = 1`, clConvID)
		if err != nil {
			t.Fatalf("round %d: folded rows: %v", round, err)
		}
		var foldedAsks []string
		for rows.Next() {
			var content *string
			if err := rows.Scan(&content); err != nil {
				rows.Close()
				t.Fatalf("round %d: scan folded row: %v", round, err)
			}
			if content == nil {
				continue
			}
			var mc utils.MessageContent
			mc.UnmarshalJSON([]byte(*content))
			for _, line := range strings.Split(mc.TextContent(), "\n") {
				if strings.HasPrefix(line, "round ") && strings.HasSuffix(line, " detail line") {
					foldedAsks = append(foldedAsks, line)
				}
			}
		}
		rows.Close()
		for _, ask := range foldedAsks {
			if !strings.Contains(summary, ask) {
				t.Fatalf("round %d: %q is compacted=1 (gone from the LLM view) and is NOT in the checkpoint summary — silent context loss\nsummary head: %q",
					round, ask, first(summary, 300))
			}
		}

		// 6. ANCHOR
		if !clAskRepresented(view, summary) {
			t.Fatalf("round %d: the original ask is neither a live user row nor in the summary — the model has no task anchor", round)
		}
	}

	// The loop must actually have exercised compaction, otherwise it proves
	// nothing.
	if compactionRounds == 0 {
		t.Fatalf("fixture never crossed the budget (%d tokens) — clRounds/clToolRun too small", clBudget)
	}
	// Convergence over the whole run: the last round's reloaded view must not
	// be larger than the smallest one seen.
	min := loadedEachRound[0]
	for _, v := range loadedEachRound {
		if v < min {
			min = v
		}
	}
	if last := loadedEachRound[len(loadedEachRound)-1]; last > clBudget {
		t.Fatalf("after %d rounds the conversation sits at ~%d tokens, over budget %d — the request keeps inflating (last %v)",
			clRounds, last, clBudget, loadedEachRound)
	}
	t.Logf("compaction fired on %d/%d rounds; reloaded view sizes: %v (min %d, budget %d)",
		compactionRounds, clRounds, loadedEachRound, min, clBudget)
}
