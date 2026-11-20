package db

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// checkpoint_bootstrap_test.go — the BOOTSTRAP fold: a conversation that has
// NEVER been compacted (no checkpoint pair) and whose history exceeds the
// per-request tail budget. Production evidence for why this must fold
// (deployment 2026-11-20, conversation c363b2b3, /app/data/server.log):
//
//	12:04:05 capping tail by budget: kept 6 msgs / 359315 chars (start idx 1607)
//	12:32:57 capping tail by budget: kept 13 msgs / 1189684 chars (start idx 1628)
//	   ...    [Eco] conv c363b2b3 lastPT=85121 below trigger=150000 — skip
//
// The sliding tail cap (ai.capTail, the bootstrap path) evicts rows on every
// turn, and its eviction caps the provider's prompt at ~33k tokens, so
// lastPT can NEVER reach trigger=150000: the fold that mints the pair never
// runs, the pair never exists, the sliding window runs forever. The gate fix
// (ai.bootstrapFoldNeeded, pinned by ai/eco_bootstrap_cap_test.go) hands that
// eviction pressure to the durable fold. This file pins what the fold then
// does at the storage layer: mints ONE pair, flags rows (never deletes), and
// leaves a bounded frozen LLM view.

// bsGiantTurn: user -> assistant(tool_call) -> tool(result over the
// per-message bound), the 890KB docker-list shape that dragged the window.
func bsGiantTurn(n int, chars int) []utils.Message {
	callID := "bs_giant_" + itoaTest(n)
	user := utils.Message{Role: "user", Content: utils.NewTextContent("bootstrap dump " + itoaTest(n))}
	asst := utils.Message{Role: "assistant", Content: utils.NewTextContent("run dump " + itoaTest(n))}
	asst.ToolCalls = []utils.ToolCall{{ID: callID, Type: "function",
		Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{}"}}}
	tool := utils.Message{Role: "tool", Content: utils.NewTextContent(strings.Repeat("D", chars))}
	tool.Name = "system_tools__shell_exec"
	tool.ToolCallID = callID
	return []utils.Message{user, asst, tool}
}

// TestBootstrapFoldMintsFrozenPair: the fold a bootstrap conversation needs.
// Red for the GATE (the gate lives in ai and is pinned there); this test pins
// the storage half of the fix: a fold of a never-compacted conversation
// mints exactly one pair, flags rows non-destructively, keeps the client
// history whole, and leaves an LLM view that opens on the pair.
func TestBootstrapFoldMintsFrozenPair(t *testing.T) {
	userID := "boot-fold-user"
	db := openFrozenDB(t, userID)
	convID := "boot-conv"
	seedConv(t, db, convID)

	var all []utils.Message
	for i := 0; i < 25; i++ {
		all = append(all, bsGiantTurn(i, 2_000)...)
	}
	all = append(all, bsGiantTurn(50, 300_000)...) // the 890KB-shaped dump
	for i := 51; i < 70; i++ {
		all = append(all, bsGiantTurn(i, 2_000)...)
	}
	rows := appendRows(t, db, convID, all...)

	// No pair yet: the LLM view is the whole raw history (bootstrap).
	before, err := GetConversationByIdInternal(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}
	for _, m := range before.Messages {
		if IsCheckpointMessage(m) {
			t.Fatal("fixture must start with no checkpoint pair")
		}
	}

	// Fold boundary: keep the newest user-anchored turn (a legal live-window
	// start; the storage layer clamps to a user row itself).
	foldFrom := len(rows) - 3
	folded, err := CompactConversationDurable(frozenCtx(userID), convID, rows, foldFrom, nil)
	if err != nil {
		t.Fatalf("bootstrap fold: %v", err)
	}
	if folded == 0 {
		t.Fatal("bootstrap fold removed 0 rows — pair never minted")
	}

	after, err := GetConversationByIdInternal(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}

	// ONE pair, minted, and it heads the LLM view (frozen path).
	pairs := 0
	firstPairIdx := -1
	for i, m := range after.Messages {
		if IsCheckpointMessage(m) {
			pairs++
			if firstPairIdx < 0 {
				firstPairIdx = i
			}
		}
	}
	if pairs != 2 {
		t.Fatalf("checkpoint rows after fold: %d, want 2 (one pair)", pairs)
	}
	if firstPairIdx != 0 {
		t.Fatalf("pair starts at %d, want 0 (first content of the prompt)", firstPairIdx)
	}
	// The tool half carries a non-empty summary standing in for folded rows.
	summary := ""
	for _, m := range after.Messages {
		if m.Role == "tool" && IsCheckpointMessage(m) {
			summary = m.TextContent()
		}
	}
	if strings.TrimSpace(summary) == "" {
		t.Fatal("checkpoint summary is empty — folded rows represented by nothing")
	}

	// NON-DESTRUCTIVE: every row is flagged, none deleted; the client view
	// keeps the full history.
	var compactedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND compacted = 1`, convID).
		Scan(&compactedCount); err != nil {
		t.Fatalf("count compacted: %v", err)
	}
	if compactedCount != folded {
		t.Fatalf("compacted rows %d, fold reported %d", compactedCount, folded)
	}
	client, err := GetConversationById(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("client load: %v", err)
	}
	if len(client.Messages) < len(rows) {
		t.Fatalf("client view shrank: %d -> %d rows (fold must be non-destructive)",
			len(rows), len(client.Messages))
	}

	// The LLM view is now SMALLER than the raw history it replaces — the
	// bootstrap window's job is done and the pair bounds the head.
	if len(after.Messages) >= len(before.Messages) {
		t.Fatalf("LLM view did not shrink: %d -> %d rows", len(before.Messages), len(after.Messages))
	}
	t.Logf("bootstrap fold: %d rows folded, pair at 0, LLM view %d -> %d rows, summary %d chars",
		folded, len(before.Messages), len(after.Messages), len(summary))
}

// TestBootstrapFoldIsMachineOnlyAtStorageLayer: with machineSummary nil the
// storage layer stores a bounded mechanical excerpt, so a bootstrap fold
// NEVER depends on an LLM being reachable. The fold that mints the pair must
// work on a box whose model is down.
func TestBootstrapFoldIsMachineOnlyAtStorageLayer(t *testing.T) {
	userID := "boot-offline-user"
	db := openFrozenDB(t, userID)
	convID := "boot-offline"
	seedConv(t, db, convID)

	var all []utils.Message
	for i := 0; i < 40; i++ {
		all = append(all, bsGiantTurn(i, 1_500)...)
	}
	rows := appendRows(t, db, convID, all...)

	folded, err := CompactConversationDurable(frozenCtx(userID), convID, rows, len(rows)-3, nil)
	if err != nil {
		t.Fatalf("fold with nil summariser: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows")
	}
	after, err := GetConversationByIdInternal(frozenCtx(userID), convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var summary string
	for _, m := range after.Messages {
		if m.Role == "tool" && IsCheckpointMessage(m) {
			summary = m.TextContent()
		}
	}
	if strings.TrimSpace(summary) == "" {
		t.Fatal("nil summariser stored an empty summary")
	}
	if len(summary) > MaxDurableSummaryChars {
		t.Fatalf("stored summary %d chars, over the bound %d", len(summary), MaxDurableSummaryChars)
	}
	t.Logf("machine-only fold: %d rows folded, summary %d chars (bound %d)",
		folded, len(summary), MaxDurableSummaryChars)
}
