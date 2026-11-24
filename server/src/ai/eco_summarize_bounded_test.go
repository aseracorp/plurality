package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// bdRows builds n stored rows, each tagged "row-NNN" so a test can recover the
// chronological position of any row a function returns.
func bdRows(n, pad int) []utils.Message {
	out := make([]utils.Message, 0, n)
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("row-%03d %s", i, strings.Repeat("x", pad))
		out = append(out, utils.Message{Role: "user", Content: utils.NewTextContent(body)})
	}
	return out
}

func bdIdx(m utils.Message) int {
	s := m.TextContent()
	if len(s) < 7 || !strings.HasPrefix(s, "row-") {
		return -1
	}
	n, err := strconv.Atoi(s[4:7])
	if err != nil {
		return -1
	}
	return n
}

// TestSplitEvictedBatchesIsChronological pins the two invariants the driver
// depends on: batches are returned oldest-first, and rows inside a batch keep
// their stored order — the summariser must read the eviction as it happened.
func TestSplitEvictedBatchesIsChronological(t *testing.T) {
	rows := bdRows(40, 1_000) // ~40 * ~1008 chars
	batches := splitEvictedBatches(rows, 4_000, 3)
	if len(batches) == 0 {
		t.Fatal("no batches for a 40-row eviction")
	}
	if len(batches) > 3 {
		t.Fatalf("%d batches, maxBatches is 3", len(batches))
	}

	last := -1
	for bi, b := range batches {
		for _, m := range b {
			i := bdIdx(m)
			if i <= last {
				t.Fatalf("batch %d: row %d came after row %d — not chronological", bi, i, last)
			}
			last = i
		}
	}

	// The newest row must be covered: it is the freshest work.
	final := batches[len(batches)-1]
	if bdIdx(final[len(final)-1]) != len(rows)-1 {
		t.Fatalf("the last batch ends at row %d, want the newest row %d",
			bdIdx(final[len(final)-1]), len(rows)-1)
	}
}

// TestSplitEvictedBatchesSelectsTheNewest: an eviction larger than the batch
// budget is covered from the NEWEST end, matching summaryInstruction's
// "cover the newest evicted rows first". The machine render covers the rest.
func TestSplitEvictedBatchesSelectsTheNewest(t *testing.T) {
	rows := bdRows(60, 1_000)
	batches := splitEvictedBatches(rows, 4_000, 2)
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2", len(batches))
	}
	covered := 0
	minIdx := len(rows)
	for _, b := range batches {
		for _, m := range b {
			covered++
			if i := bdIdx(m); i < minIdx {
				minIdx = i
			}
		}
	}
	if covered >= len(rows) {
		t.Fatalf("2 batches covered all %d rows — the budget bound is not applied", len(rows))
	}
	if minIdx == 0 {
		t.Fatal("the batches start at the OLDEST row; selection must start at the newest")
	}
	// Contiguous up to the newest row.
	if minIdx+covered != len(rows) {
		t.Fatalf("coverage is %d rows starting at %d, want a contiguous run ending at %d",
			covered, minIdx, len(rows)-1)
	}
}

// TestSplitEvictedBatchesFitsTheBudget: no batch may exceed the char budget,
// and the budget IS the summariser renderer's input capacity — a batch the
// renderer would truncate would be silently dropped behind its "older messages
// omitted" NOTE, so the two budgets must agree.
func TestSplitEvictedBatchesFitsTheBudget(t *testing.T) {
	rows := bdRows(80, 1_000)
	budget := summaryBatchChars()
	if budget != maxSummaryInputChars {
		t.Fatalf("batch budget is %d chars, want the renderer's own input budget %d",
			budget, maxSummaryInputChars)
	}
	for _, b := range splitEvictedBatches(rows, budget, summaryMaxBatches) {
		size := 0
		for _, m := range b {
			size += shippedRowChars(m)
		}
		if size > budget {
			t.Fatalf("batch of %d chars exceeds the budget %d", size, budget)
		}
		// The renderer must emit this batch verbatim: no NOTE.
		if strings.Contains(renderEvictedRowsForSummary(b), "older messages omitted") {
			t.Fatal("the renderer truncated a batch this path built — batch budget exceeds render capacity")
		}
	}
}

// TestBoundedSummaryOutputTokensCannotCollapse: the recovery path's budget
// stays usable for any input the batch builder can produce, and the ceiling
// still applies.
func TestBoundedSummaryOutputTokensCannotCollapse(t *testing.T) {
	const model = "bounded-summary-model"
	ucSeedActionModel(t, model, window262k, nil)

	// The largest input the batch builder emits: system rows at the production
	// size plus one full batch.
	input := promptOverheadTokens + summaryBatchTokens
	got := *boundedSummaryOutputTokens(model, input)
	if got <= minSummaryOutputTokens {
		t.Fatalf("the recovery path collapsed (max_tokens=%d) for its own largest input — the shape is not unconditional", got)
	}
	if got > maxSummaryOutputTokens {
		t.Fatalf("budget %d exceeds the ceiling %d", got, maxSummaryOutputTokens)
	}

	// A small window: the floor is honest, not a silent collapse.
	const small = "bounded-summary-small"
	ucSeedActionModel(t, small, 4_096, nil)
	if got := *boundedSummaryOutputTokens(small, 4_096); got != minSummaryOutputTokens {
		t.Fatalf("a full 4096-token window must clamp to the floor %d, got %d", minSummaryOutputTokens, got)
	}
}

// TestBoundedSummaryRequestShape: the recovery request ships NO tool schemas
// (they measured 28349 tokens on prod and are what collapsed the replay path),
// keeps the conversation's system rows, ends with the checkpoint instruction,
// and carries a budget above the collapse floor.
func TestBoundedSummaryRequestShape(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "bounded-shape-model", window262k, map[string]string{"system_tools__shell_exec": "on"})

	hist := ucHistory("bounded shape", 10, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	batch := splitEvictedBatches(view, summaryBatchChars(), summaryMaxBatches)
	if len(batch) == 0 {
		t.Fatal("no batch built from the conversation view")
	}
	req, err := buildBoundedSummaryRequest(ctx, conv, model, batch[0], "PRIOR ROLL-UP TEXT", payload)
	if err != nil {
		t.Fatalf("bounded request: %v", err)
	}
	if req.Tools != nil {
		t.Fatalf("the recovery request ships %d tool schemas — the block that collapsed the replay path", len(req.Tools))
	}
	if req.MaxTokens == nil || *req.MaxTokens <= minSummaryOutputTokens {
		t.Fatalf("budget %v, want above the collapse floor %d", req.MaxTokens, minSummaryOutputTokens)
	}
	if req.Stream {
		t.Fatal("the summary must be one-shot")
	}

	// System rows first, instruction last, and the prior roll-up is carried.
	if req.Messages[0].Role != "system" {
		t.Fatalf("first message is %q, want system", req.Messages[0].Role)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("last message is %q, want the instruction as a user row", last.Role)
	}
	instr, _ := last.Content.(string)
	if !strings.Contains(instr, "DURABLE CONTEXT CHECKPOINT") {
		t.Fatal("the last message is not the checkpoint instruction")
	}
	if !strings.Contains(instr, "PRIOR ROLL-UP TEXT") {
		t.Fatal("the prior roll-up is not passed to the instruction — the chain restarts from nothing")
	}
	// The batch's own text is in the request verbatim.
	var body string
	for _, m := range req.Messages {
		if s, ok := m.Content.(string); ok {
			body += s
		}
	}
	if !strings.Contains(body, "module_0.go") {
		t.Fatal("the evicted batch's content is missing from the request")
	}
}

// TestBudgetCollapseRoutesToTheRecoveryPath is the regression.
//
// On the deployed box EVERY fold of a long conversation reported
//
//	summary budget collapsed (max_tokens=1024 for a 262144-token window)
//
// and folded megabytes into the machine-only roll-up WITHOUT EVER CALLING THE
// MODEL. /app/data/server.log on the #208 build: 0 "LLM fold summary" lines, 32
// "machine-only FALLBACK fold" lines, 21 of them budget collapses, the newest
// at 09:59:50 — "1240 rows, room=162452 chars, folded=2824666 chars". The shape
// is the FROZEN view: the checkpoint pair at the head plus an uncapped live
// tail, so the replayed prefix is at the window and the replay's output budget
// floors. The collapse must route to the bounded recovery path, which reaches
// the proxy.
func TestBudgetCollapseRoutesToTheRecoveryPath(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "bounded-collapse-model", window262k,
		map[string]string{"system_tools__shell_exec": "on"})

	// The deployed shape: a frozen view whose live tail fills the window. The
	// rows sit at the per-message cap (the fat tool results of a real agent
	// conversation), so a handful of rows carry the ~900k chars ≈ 300k tokens
	// of live tail that fills a 262144-token window. Few rows keeps the test at
	// second scale; the arithmetic is the same either way.
	view := []utils.Message{checkpointMsg(), toolMsg()}
	for i := 0; i < 9; i++ {
		view = append(view, utils.Message{Role: "user",
			Content: utils.NewTextContent("ask " + itoa(i) + " " + strings.Repeat("y", maxCompactMessageChars))})
	}
	ucPush(t, ctx, convID, view...)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	// Precondition: the replay path collapses on this conversation.
	replay, err := buildSummaryRequestFromView(ctx, conv, model, view, payload)
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}
	if replay.MaxTokens == nil || *replay.MaxTokens > minSummaryOutputTokens {
		t.Fatalf("the replay budget did NOT collapse (max_tokens=%v) — the test no longer reproduces the deployed shape",
			*replay.MaxTokens)
	}

	// The proxy records every call and answers with a checkpoint.
	var calls int
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"CHECKPOINT-%d"}}]}`, calls)
	}))
	t.Cleanup(srv.Close)
	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() { LiteLLMBaseURL = oldBase; litellmReady.Store(prevReady) })

	text, err := attemptFoldSummary(ctx, conv, model, view, payload)
	if err != nil {
		t.Fatalf("the fold lost the LLM summary on a budget collapse: %v", err)
	}
	if calls == 0 {
		t.Fatal("the recovery path never reached the model — the deployed defect survives")
	}
	if calls > summaryMaxBatches {
		t.Fatalf("%d model calls, the batch bound is %d", calls, summaryMaxBatches)
	}
	if !strings.Contains(text, "CHECKPOINT-") {
		t.Fatalf("the fold's checkpoint is not the model's text:\n%s", headOf(text, 200))
	}

	// Every recovery call ships no tools: that is the block it drops, and the
	// block that collapsed the replay path (measured 28349 tokens on prod).
	for i, b := range bodies {
		var body map[string]interface{}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatalf("call %d body: %v", i+1, err)
		}
		if _, ok := body["tools"]; ok {
			t.Fatalf("recovery call %d ships tool schemas", i+1)
		}
		if body["stream"] != false {
			t.Fatalf("recovery call %d is not one-shot", i+1)
		}
	}
}
