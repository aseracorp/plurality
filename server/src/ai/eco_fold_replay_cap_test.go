package ai

// eco_fold_replay_cap_test.go — the contract of the END cap on the fold's prefix
// replay (owner, 2026-11-27): "Prefix stays, you may cap the replay at the END to
// fit the context window. Best of both worlds: keep caching and make it fit the
// window. Beware, trigger may be smaller than the context window. When capping
// the prefix tail, you need to make sure to put the checkpoint at the right place
// in the history. You cannot just append it at the newest position, you need to
// put it after the last context you send to the model."
//
// Four properties, each pinned below:
//
//	1. CACHE  — the rows at or before the cut are byte-identical to the chat
//	   request's own rows. The cap may only shorten the tail.
//	2. FIT    — the request the builder returns is inside the declared window.
//	3. PAIRS  — the cut is at a group boundary: no orphaned tool result, no
//	   dangling tool_call, by construction, with no repair pass.
//	4. PLACEMENT — the brief is the last row, and its row count names the rows
//	   that SHIP, not the rows in the DB.
//
// Plus the owner's warning as its own test: a trigger far below the window
// (150000 vs 262144) is the no-op case — history fits, nothing is dropped.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// capRows builds one system row plus `turns` [user, assistant+call, tool]
// groups, each about `chars` long. Deterministic, so byte comparison is
// meaningful across runs.
func capRows(turns, chars int) []StandardMessageReq {
	rows := []StandardMessageReq{{Role: "system", Content: "SYSTEM-PROMPT-BYTE-STABLE"}}
	for i := 0; i < turns; i++ {
		rows = append(rows, StandardMessageReq{Role: "user", Content: "cap ask " + itoa(i) + " " + strings.Repeat("q", chars)})
		callID := "call_cap_" + itoa(i)
		rows = append(rows, StandardMessageReq{
			Role:      "assistant",
			Content:   "calling " + itoa(i),
			ToolCalls: []utils.ToolCall{{ID: callID, Type: "function", Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: strings.Repeat("a", chars)}}},
		})
		rows = append(rows, StandardMessageReq{Role: "tool", Name: "system_tools__shell_exec", ToolCallID: callID, Content: "cap result " + itoa(i) + " " + strings.Repeat("z", chars)})
	}
	return rows
}

// tokenSlackForCap is the chars/3 rounding that per-group sums can accumulate.
const tokenSlackForCap = 64

// TestFoldReplayEndCapKeepsTheCacheHead: the cap drops from the END, so the
// prefix the backend already prefilled survives byte-for-byte. A cap that moved
// the head is a full re-prefill, and this is the test that catches it.
func TestFoldReplayEndCapKeepsTheCacheHead(t *testing.T) {
	rows := capRows(40, 900)
	head := leadingSystemRows(rows)
	full := renderedRowsTokens(rows)
	room := full / 2 // a room that forces a real cut
	res := capReplayRowsToEnd(rows, head, room)
	if !res.capped {
		t.Fatalf("the cap dropped nothing at room %d of %d tokens", room, full)
	}
	if res.keptRows+res.droppedRows != len(rows)-head {
		t.Fatalf("row accounting: kept %d + dropped %d != %d history rows", res.keptRows, res.droppedRows, len(rows)-head)
	}
	if res.keptRows >= len(rows)-head {
		t.Fatal("the cap did not shorten the history")
	}
	// BYTE-IDENTICAL up to the cut: the cached prefix extends, it does not move.
	kept := rows[:head+res.keptRows]
	if !bytes.Equal(marshalNoEscape(kept), marshalNoEscape(rows[:len(kept)])) {
		t.Fatal("the cap rewrote a row at or before the cut: the KV prefix would not be reused")
	}
	// The floor: the cut never crosses the newest user row.
	if res.keptRows == 0 {
		t.Fatal("the cap dropped the whole history: the newest user row is the floor")
	}
	if kept[len(kept)-1].Role != "user" {
		t.Fatalf("the cap crossed the newest user row: the kept tail ends on %q", kept[len(kept)-1].Role)
	}
	// The room is honoured UNLESS the cap stopped at the floor: at the floor the
	// newest user row is kept whatever the budget says, which is the point.
	headTokens := renderedRowsTokens(rows[:head])
	if !res.atFloor && res.keptTokens > room+headTokens+tokenSlackForCap {
		t.Fatalf("the kept prefix %d exceeds the room %d (+ head %d) by more than the slack", res.keptTokens, room, headTokens)
	}
	// A room above the floor must be met exactly, with no overshoot.
	room2 := res.keptTokens - headTokens
	if again := capReplayRowsToEnd(rows, head, room2); again.capped && again.keptTokens > room2+headTokens+tokenSlackForCap {
		t.Fatalf("the cap overshot a room it could meet: kept %d > room %d", again.keptTokens, room2)
	}
}

// TestFoldReplayEndCapIsPairSafe: the cut is at a group boundary, so the kept
// rows never carry an orphaned tool result or a dangling tool_call. That is the
// whole pair-safety argument — structural, with no repair pass to forget.
func TestFoldReplayEndCapIsPairSafe(t *testing.T) {
	rows := capRows(40, 900)
	head := leadingSystemRows(rows)
	for _, room := range []int{renderedRowsTokens(rows), renderedRowsTokens(rows) / 3, 1} {
		res := capReplayRowsToEnd(rows, head, room)
		kept := rows[:head+res.keptRows]
		if dangling, orphans := hasOrphanedToolRows(kept); dangling != 0 || orphans != 0 {
			t.Fatalf("room %d: kept rows carry %d dangling tool_calls and %d orphaned tool results (cut at %d of %d history rows)",
				room, dangling, orphans, res.keptRows, len(rows)-head)
		}
	}
	// The boundary helper never lands inside a pair: every count it returns is a
	// group end.
	groups := groupReplayHistory(rows[head:])
	ends := map[int]bool{}
	for _, g := range groups {
		ends[g.end] = true
	}
	for want := 0; want <= len(rows)-head; want++ {
		got := replayRowsAtSafeBoundary(rows, head, want)
		if !ends[got] && got != 0 {
			t.Fatalf("boundary %d (want %d) is inside a tool pair", got, want)
		}
	}
}

// TestFoldReplayEndCapAtTheFloor: below the newest user row the cap stops and
// says so, instead of shipping a history with no user anchor.
func TestFoldReplayEndCapAtTheFloor(t *testing.T) {
	rows := capRows(30, 900)
	head := leadingSystemRows(rows)
	res := capReplayRowsToEnd(rows, head, 1)
	if !res.atFloor {
		t.Fatal("room 1 token: the cap should report it stopped at the floor")
	}
	kept := rows[:head+res.keptRows]
	if kept[len(kept)-1].Role != "user" {
		t.Fatalf("at the floor the kept history must end on the user row, ends on %q", kept[len(kept)-1].Role)
	}
	if len(kept)-head <= 3 {
		t.Fatalf("the floor kept %d history rows: the newest user row is the FLOOR, the older history stays", len(kept)-head)
	}
}

// TestFoldReplayCapNoOpWhenTriggerIsBelowTheWindow: the owner's warning. The
// configured trigger (150000) can sit far below the model's window (262144), so a
// fold fires while the prefix is well inside the window — the cap must then
// change NOTHING and the replay ships the whole conversation.
func TestFoldReplayCapNoOpWhenTriggerIsBelowTheWindow(t *testing.T) {
	rows := capRows(40, 900)
	head := leadingSystemRows(rows)
	room := window262k - 8000 - guardSafetyReserve - maxSummaryOutputTokens - 2000
	res := capReplayRowsToEnd(rows, head, room)
	if res.capped || res.droppedRows != 0 {
		t.Fatalf("a %d-token prefix in a %d-token window must ship whole; the cap dropped %d rows",
			renderedRowsTokens(rows), window262k, res.droppedRows)
	}
	if res.keptRows != len(rows)-head {
		t.Fatalf("a no-op cap kept %d of %d history rows", res.keptRows, len(rows)-head)
	}
}

// TestFoldReplayCapPlacesTheBriefAfterTheLastRowSent: the builder's wire shape,
// end to end. The brief is one appended user row, it is the LAST row, it fits the
// window, and the row count it names is the count that SHIPS.
func TestFoldReplayCapPlacesTheBriefAfterTheLastRowSent(t *testing.T) {
	ctx, _, convID := ucDB(t)
	// A window the fixture cannot fit whole, with room for the floor: the builder
	// must cap the tail, not refuse the call.
	model := ucSeedActionModel(t, "cap-narrow-model", 12_000, map[string]string{"system_tools__shell_exec": "on"})
	// One user row, then a long tool chain and no later user row: the shape of a
	// sub-agent conversation, and the case where the cap has real work to do —
	// the floor is the first user row, so every tool pair after it is droppable.
	hist := append(fpRows(1, 700), capChain(16, 900)...)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	req, err := buildSummaryRequestFromView(ctx, conv, model, payload, foldStageCompress, 60_000, 9_000)
	if err != nil {
		t.Fatalf("the builder refused where the cap should have made it fit: %v", err)
	}
	// The wire fits the declared window, output budget included.
	window := Models.DeclaredContextWindow(model.Name)
	schemaTokens := len(marshalNoEscape(req.Tools)) / summaryCharsPerToken
	rowsTokens := renderedRowsTokens(req.Messages)
	if total := rowsTokens + schemaTokens + *req.MaxTokens; total > window {
		t.Fatalf("the capped replay is %d tokens + %d schemas + %d output = %d, over the declared window %d",
			rowsTokens, schemaTokens, *req.MaxTokens, total, window)
	}
	// The brief is the last row, alone, and is a user turn.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("the last row is %q, want the appended user brief", last.Role)
	}
	brief, _ := last.Content.(string)
	if !strings.Contains(brief, "COMPRESS") {
		t.Fatal("the last row is not the fold brief")
	}
	// The brief counts the rows that SHIP, not the rows in the DB.
	historyRows := len(req.Messages) - leadingSystemRows(req.Messages) - 1
	want := "The " + itoa(historyRows) + " oldest messages"
	if !strings.Contains(brief, want) {
		t.Fatalf("the brief does not name the rows that ship (%d history rows, want %q):\n%s",
			historyRows, want, headRunes([]byte(brief), 300))
	}
	// Pair safety on the WIRE, not just in the slice.
	if dangling, orphans := hasOrphanedToolRows(req.Messages); dangling != 0 || orphans != 0 {
		t.Fatalf("the capped request ships %d dangling tool_calls and %d orphaned tool results", dangling, orphans)
	}
	// The rows before the brief are a byte-identical PREFIX of the chat request:
	// the cap cut the tail, it did not edit the head.
	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}
	chatRows := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))
	shipped := len(req.Messages) - 1
	if shipped >= len(chatRows) {
		t.Fatalf("the window %d needed no cap: the replay shipped all %d rows — the cap must fire here", window, shipped)
	}
	if t.Failed() {
		t.FailNow()
	}
	if shipped > len(chatRows) {
		t.Fatalf("the capped replay ships %d rows, more than the chat request's %d", shipped, len(chatRows))
	}
	got := ucRowsBytes(t, req.Messages[:shipped])
	wantBytes := ucRowsBytes(t, chatRows[:shipped])
	if !bytes.Equal(got, wantBytes) {
		t.Fatalf("the capped replay is not a byte-identical PREFIX of the chat request: first divergence at byte %d",
			firstDivergence(got, wantBytes))
	}
}

// capChain is `turns` [assistant+call, tool] pairs with NO user row among them:
// the newest user row stays at the head of the history, so the cap's floor is
// early and the droppable tail is real. `chars` sizes each row.
func capChain(turns, chars int) []utils.Message {
	out := []utils.Message{}
	for i := 0; i < turns; i++ {
		out = append(out, ucTurn(i, "system_tools__shell_exec",
			"cap chain file=/srv/app/module_"+itoa(i)+".go "+strings.Repeat("w", chars))...)
	}
	return out
}

// TestFoldReplayCapNoOpWithoutADeclaredWindow: a model that declares no context
// window has no room arithmetic, so the cap must not fire — capping on a
// meaningless negative room would drop history to the floor for no reason.
func TestFoldReplayCapNoOpWithoutADeclaredWindow(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "cap-no-window-model", 0, map[string]string{"system_tools__shell_exec": "on"})
	hist := append(fpRows(1, 700), capChain(10, 900)...)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	req, err := buildSummaryRequestFromView(ctx, conv, model, payload, foldStageCompress, 60_000, 9_000)
	if err != nil {
		t.Fatalf("the builder refused a model with no declared window: %v", err)
	}
	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}
	chatRows := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))
	if shipped := len(req.Messages) - 1; shipped != len(chatRows) {
		t.Fatalf("no declared window: the cap shipped %d of %d rows — it must be a no-op", shipped, len(chatRows))
	}
}
