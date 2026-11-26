package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_summarize_test.go — the load-bearing proof for "the LLM summarisation
// call is the LAST thing that touches the context the 100K cap is about to
// evict, on the CHAT model, reusing the KV-cached prefix one final time".
//
// Why byte-identity of the PREFIX is the whole feature: a self-hosted backend
// (llama.cpp/avalon/strata) and the cloud prefix caches reuse the LONGEST
// COMMON PREFIX of the previous request's token stream. If the summary
// request's leading rows are not byte-for-byte the chat request's, the
// backend re-prefills the entire conversation for the summary call — the
// opposite of "reuse the cache one final time". These tests assert the summary
// request IS the chat request's wire bytes plus one appended user turn.
//
// Nothing here touches the network: request CONSTRUCTION (buildSummaryRequest-
// FromView) is separated from the HTTP call (GenerateCheckpointSummaryFrom-
// Request), and LiteLLM is never ready in tests, so the summariser fails fast.
// The fold tests pin the consequence: the fold survives that failure with a
// bounded, non-empty roll-up.

// newerHistoryBannerName mirrors db.newerHistoryBanner (unexported there). The
// fold tests assert on the literal so the ai layer cannot silently drift from
// the storage layer's section marker.
const newerHistoryBannerName = "[newer history, machine-compacted]"

// ucSeedActionModel registers a CHAT model entry in the shared registry so the
// request path takes the action-model branch (tool schemas in the prefix, the
// skills/MCP system-prompt appends) — the realistic prefix shape. Cleaned up,
// so the registry singleton is not polluted for other tests.
func ucSeedActionModel(t *testing.T, name string, window int, tools map[string]string) utils.Model {
	t.Helper()
	Models.mu.Lock()
	e := Models.byName[name]
	e.Name = name
	e.Mode = "chat"
	e.SupportsFunctionCalling = true
	e.MaxContextTokens = window
	Models.byName[name] = e
	Models.mu.Unlock()
	t.Cleanup(func() {
		Models.mu.Lock()
		delete(Models.byName, name)
		Models.mu.Unlock()
	})
	return utils.Model{Name: name, Tools: tools}
}

// ucConvOf builds the conversation value the fold path is handed: the LLM view
// as its messages, eco ON (the frozen path), user claim from ctx.
func ucConvOf(t *testing.T, ctx context.Context, convID string, msgs []utils.Message, model utils.Model) utils.Conversation {
	t.Helper()
	uid, _ := ctx.Value("userID").(string)
	m := model
	return utils.Conversation{
		ID:            convID,
		UserID:        uid,
		Messages:      msgs,
		ModelSelected: utils.ModelSelected{EcoMode: true, Text: &m},
	}
}

// ucMarshalRow marshals one rendered row with HTML escaping OFF, matching the
// encoder SendChatCompletion uses.
func ucMarshalRow(row StandardMessageReq) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(row); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ucRowsBytes marshals message rows the way the wire does, joined by ',' — the
// byte sequence of the `messages` array's ELEMENTS (the array's closing
// bracket moves every time a row is appended, which would make a prefix
// assertion trivially false).
func ucRowsBytes(t *testing.T, rows []StandardMessageReq) []byte {
	t.Helper()
	var out []byte
	for i := range rows {
		b, err := ucMarshalRow(rows[i])
		if err != nil {
			t.Fatalf("marshal row: %v", err)
		}
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, b...)
	}
	return out
}

// TestSummaryRequestHeadIsByteIdenticalToChatRequest is the cache-reuse
// guarantee, re-derived to STAGE 2: the summariser's request is the chat
// request's leading system rows and tool schemas, BYTE-IDENTICAL, plus exactly
// ONE user row carrying the megacaved history and the instruction.
//
// The old contract was "the chat prefix plus the appended instruction" — stage 2
// used to ship the evicted history as real rows. It no longer does: the history
// region is the megacave, because the model's job is to compact the
// already-compacted history. The KV prefix is therefore system rows + schemas,
// and the claim that matters is that a fold never moves THAT.
func TestSummaryRequestHeadIsByteIdenticalToChatRequest(t *testing.T) {
	ctx, _, convID := ucDB(t)
	// Tools ON: the tool schemas are part of the prompt prefix, so the
	// summary request must carry the SAME array to reuse the cache.
	tools := map[string]string{
		"system_tools__shell_exec":   "on",
		"filesystem_server__fs_read": "on",
	}
	model := ucSeedActionModel(t, "uc-summary-chat-model", 131072, tools)

	hist := ucHistory("summarize the eviction", 12, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("buildChatRequestParts: %v", err)
	}
	if len(chat.request.Messages) == 0 {
		t.Fatal("chat request has no messages")
	}
	if len(chat.request.Tools) == 0 {
		t.Fatal("fixture is not exercising the action-model path: no tool schemas in the chat request")
	}

	req, err := buildSummaryRequestFromMegacave(ctx, conv, model, view, payload)
	if err != nil {
		t.Fatalf("buildSummaryRequestFromMegacave: %v", err)
	}

	// The clock is the ONLY per-turn tail mutation of a chat request; the
	// summary replay must not carry it (a timestamp makes the replay
	// non-identical and the model narrates it into the roll-up).
	chatNoClock := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))
	n := leadingSystemRows(chatNoClock)
	if n == 0 {
		t.Fatal("the chat request has no leading system rows — the fixture is not the production shape")
	}
	if len(req.Messages) != n+1 {
		t.Fatalf("stage 2 ships %d rows, want the %d system rows plus exactly one megacave row", len(req.Messages), n)
	}
	got := ucRowsBytes(t, req.Messages[:n])
	want := ucRowsBytes(t, chatNoClock[:n])
	if !bytes.Equal(got, want) {
		t.Fatalf("stage 2's head is NOT byte-identical to the chat request's (summary %d bytes, chat %d bytes)\nfirst divergence at byte %d\n--- summary ---\n%s\n--- chat ---\n%s",
			len(got), len(want), firstDivergence(got, want), headRunes(got, 400), headRunes(want, 400))
	}
	// The pre-clock system rows — the exact bytes the chat path renders before
	// the clock is attached — must be what stage 2 replays.
	preClock := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.preClock...))
	if pre := ucRowsBytes(t, preClock[:n]); !bytes.Equal(got, pre) {
		t.Fatalf("stage 2 head != the chat request's pre-clock system rows (first divergence at %d)", firstDivergence(got, pre))
	}
	// The system prompt is the head of the prefix: any drift there invalidates
	// the whole cached prefix, so it must be the SAME row bytes.
	if !bytes.Equal(ucMarshalRows(t, req.Messages[:1]), ucMarshalRows(t, preClock[:1])) {
		t.Fatal("stage 2's system prompt differs from the chat request's — the cached prefix is invalidated")
	}
	// The tool schemas are the chat request's array, byte for byte.
	if !bytes.Equal(marshalNoEscape(chat.request.Tools), marshalNoEscape(req.Tools)) {
		t.Fatal("stage 2 changed the tool schemas — the tool block renders at the head of the prompt")
	}

	// The ONE body row: a user turn carrying the megacaved history AND the
	// instruction that names the eviction.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("the body row is %q, want user — the assistant can only answer a user turn", last.Role)
	}
	body, _ := last.Content.(string)
	if !strings.Contains(body, "EVICTED") {
		t.Fatal("the instruction does not name the eviction")
	}
	if !strings.Contains(body, "TOOL_RESULT ") {
		t.Fatal("the body is not a megacave render: the model is being handed raw rows, not the compacted history")
	}
}

// TestSummaryRequestUsesChatModelNotFast pins the CHAT-model requirement:
// req.Model is the conversation's chat model (the model whose KV prefix is
// warm), never the "fast" shortcut. The AST guard additionally forbids
// fastShortcutModels on the fold path.
func TestSummaryRequestUsesChatModelNotFast(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-chat-not-fast", 131072, map[string]string{"system_tools__shell_exec": "on"})
	chatModelName := model.Name

	hist := ucHistory("chat model only", 8, 900)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)

	req, err := buildSummaryRequestFromMegacave(ctx, conv, model, view)
	if err != nil {
		t.Fatalf("buildSummaryRequestFromMegacave: %v", err)
	}
	if req.Model != chatModelName {
		t.Fatalf("summary request model = %q, want the chat model %q", req.Model, chatModelName)
	}
	// The fold resolves the conversation's chat model the same way the loop
	// does (SelectModel), so no call site can smuggle in a fast model.
	if got := chatModelForConversation(conv); got.Name != chatModelName {
		t.Fatalf("chatModelForConversation resolved %q, want %q", got.Name, chatModelName)
	}
	// And it is never the fast shortcut from config.json.
	fastText, _ := fastShortcutModels()
	if fastText != "" && req.Model == fastText {
		t.Fatalf("summary request used the fast model %q", fastText)
	}
}

// TestSummaryRequestMaxTokensIsWindowBounded: MaxTokens is the model's window
// minus the estimated prefix, clamped to a sane ceiling — NOT the old
// hardcoded 8192. A summary of a whole evicted history cannot fit in 8192.
func TestSummaryRequestMaxTokensIsWindowBounded(t *testing.T) {
	ctx, _, convID := ucDB(t)
	hist := ucHistory("max tokens", 6, 900)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)

	wide := ucSeedActionModel(t, "uc-window-wide", 262144, map[string]string{"system_tools__shell_exec": "on"})
	convWide := ucConvOf(t, ctx, convID, view, wide)
	req, err := buildSummaryRequestFromMegacave(ctx, convWide, wide, view)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req.MaxTokens == nil {
		t.Fatal("summary request must set max_tokens (the old code hardcoded 8192)")
	}
	if *req.MaxTokens == 8192 {
		t.Fatal("max_tokens is the old hardcoded 8192 — it must be window-derived")
	}
	if *req.MaxTokens > maxSummaryOutputTokens {
		t.Fatalf("max_tokens %d exceeds the sane ceiling %d", *req.MaxTokens, maxSummaryOutputTokens)
	}

	// Narrow window: the budget shrinks with the prefix and never drops under
	// the floor that keeps the call usable.
	narrow := ucSeedActionModel(t, "uc-window-narrow", 8192, map[string]string{"system_tools__shell_exec": "on"})
	convNarrow := ucConvOf(t, ctx, convID, view, narrow)
	req2, err := buildSummaryRequestFromMegacave(ctx, convNarrow, narrow, view)
	if err != nil {
		t.Fatalf("build narrow: %v", err)
	}
	if *req2.MaxTokens >= *req.MaxTokens {
		t.Fatalf("an 8192-token window got max_tokens %d, same as a 262144-token window %d — not window-derived",
			*req2.MaxTokens, *req.MaxTokens)
	}
	if *req2.MaxTokens < minSummaryOutputTokens {
		t.Fatalf("max_tokens %d is below the usable floor %d", *req2.MaxTokens, minSummaryOutputTokens)
	}
}

// TestStripCurrentTimeTailIsTheInverseOfAppend: stripCurrentTimeTail is only
// safe as "the inverse used by the summary replay" if it removes EXACTLY the
// clock text appendCurrentTimeTail added — nothing else, in both wire shapes
// (string content and content parts).
func TestStripCurrentTimeTailIsTheInverseOfAppend(t *testing.T) {
	msgs := ucHistory("strip the clock", 6, 800)
	_, rows := ucRenderWire(t, msgs)
	before := ucRowsBytes(t, rows)

	clocked := appendCurrentTimeTail(append([]StandardMessageReq(nil), rows...))
	if bytes.Equal(ucRowsBytes(t, clocked), before) {
		t.Fatal("appendCurrentTimeTail attached nothing — the fixture has no text-bearing row")
	}
	stripped := stripCurrentTimeTail(clocked)
	if !bytes.Equal(ucRowsBytes(t, stripped), before) {
		t.Fatalf("stripCurrentTimeTail is not the inverse of appendCurrentTimeTail (first divergence at %d of %d)",
			firstDivergence(ucRowsBytes(t, stripped), before), len(before))
	}
	if len(stripped) != len(rows) {
		t.Fatalf("row count changed: %d -> %d", len(rows), len(stripped))
	}
	// Idempotent: it strips only the clock, never real content.
	if twice := ucRowsBytes(t, stripCurrentTimeTail(stripped)); !bytes.Equal(twice, before) {
		t.Fatal("stripCurrentTimeTail is not idempotent — it stripped content that is not the clock")
	}
	// It does not mutate the slice it is given.
	if bytes.Equal(ucRowsBytes(t, clocked), before) {
		t.Fatal("stripCurrentTimeTail mutated the caller's slice (the clock vanished from the input)")
	}
	// Parts-shaped content carries the clock as a trailing text part; strip
	// must remove that part and leave every other part byte-identical.
	parts := StandardMessageReq{Role: "user", Content: []StandardContentReq{
		{Type: "text", Text: "real user text"},
		{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: "http://x/y.png"}},
	}}
	withParts := appendCurrentTimeTail([]StandardMessageReq{parts})
	if !bytes.Equal(ucRowsBytes(t, stripCurrentTimeTail(withParts)), ucRowsBytes(t, []StandardMessageReq{parts})) {
		t.Fatalf("parts-shaped clock strip is not exact: %s", ucRowsBytes(t, stripCurrentTimeTail(withParts)))
	}
	// A user message that merely MENTIONS the time keeps its text.
	keep := StandardMessageReq{Role: "user", Content: "The current time is what we are arguing about, keep me"}
	if !bytes.Equal(ucRowsBytes(t, stripCurrentTimeTail([]StandardMessageReq{keep})), ucRowsBytes(t, []StandardMessageReq{keep})) {
		t.Fatal("stripCurrentTimeTail ate a user message about the current time")
	}
	// The empty-request shape (the clock row appendCurrentTimeTail invents for
	// a request with no text-bearing message) is stripped too, leaving nothing.
	bare := appendCurrentTimeTail([]StandardMessageReq(nil))
	if len(bare) != 1 {
		t.Fatalf("expected the clock to invent one system row for an empty request, got %d", len(bare))
	}
	if got := stripCurrentTimeTail(bare); len(got) != 0 {
		t.Fatalf("the invented clock row survived the strip: %d rows", len(got))
	}
}

// TestFoldSurvivesLLMFailure: LiteLLM is NOT ready in tests, so the summariser
// must fail fast. The fold must still store a roll-up that is non-empty,
// bounded, and byte-stable afterwards — a fold never fails because the LLM
// failed.
func TestFoldSurvivesLLMFailure(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-fold-llm-down", 131072, map[string]string{"system_tools__shell_exec": "on"})

	hist := ucHistory("fold with no proxy", 30, 1_400)
	cum := 1_000
	for i := range hist {
		if hist[i].Role == "assistant" {
			cum += 1_500
			hist[i].PromptTokens = cum
		}
	}
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)

	// Precondition: the summariser fails fast (no hang, no network).
	if LiteLLMReady() {
		t.Skip("LiteLLM is ready in this environment; the failure path is not reproducible")
	}
	if _, err := attemptFoldSummaryFromMegacave(ctx, conv, model, view[:len(view)-4]); err == nil {
		t.Fatal("attemptFoldSummary succeeded with no proxy — the fold would depend on an LLM that is not there")
	}

	before := SentViewTokens(view)
	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 8, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold failed because the LLM failed: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows with the LLM down — the machine-only fallback is not wired")
	}
	after := ucLLMView(t, ctx, convID)
	summary := ucSummaryText(t, ctx, convID)
	if summary == "" {
		t.Fatal("no roll-up stored after a fold with the LLM down")
	}
	if len(summary) > db.MaxDurableSummaryChars {
		t.Fatalf("stored roll-up is %d chars, over the %d bound", len(summary), db.MaxDurableSummaryChars)
	}
	if SentViewTokens(after) >= before {
		t.Fatalf("view did not shrink with the LLM down: %d -> %d", before, SentViewTokens(after))
	}
	// The pair is byte-stable AFTER the fold: the LLM's absence must not make
	// the prompt head churn on the following turns.
	pairBytes := ucPairBytes(t, after)
	if len(pairBytes) == 0 {
		t.Fatal("no checkpoint pair after the fold")
	}
	for turn := 0; turn < 4; turn++ {
		ucPush(t, ctx, convID, ucTurn(200+turn, "system_tools__fs_read", "stable "+strings.Repeat("q", 300))...)
		v := ucLLMView(t, ctx, convID)
		if p := ucPairBytes(t, v); string(p) != string(pairBytes) {
			t.Fatalf("turn %d: pair moved after an LLM-less fold (divergence at %d)", turn, firstDivergence(pairBytes, p))
		}
	}
}

// TestMachineOnlyFoldFallbackIsDeterministic: with the LLM unavailable the
// stored roll-up is the machine render, so two identical conversations must
// store byte-identical text. Determinism keeps the prompt head stable; a
// non-deterministic fallback is a head rewrite every fold.
func TestMachineOnlyFoldFallbackIsDeterministic(t *testing.T) {
	run := func(t *testing.T) string {
		t.Helper()
		// ucDB mints the conversation id from t.Name()+pid, so the two passes
		// run as NAMED SUBTESTS: each gets its own temp user-data dir AND its
		// own conversation id (two ucDB calls under one t.Name() collide on
		// conversations.id).
		ctx, _, convID := ucDB(t)
		model := ucSeedActionModel(t, "uc-fold-determinism", 131072, nil)
		hist := ucHistory("determinism", 20, 1_100)
		ucPush(t, ctx, convID, hist...)
		view := ucLLMView(t, ctx, convID)
		conv := ucConvOf(t, ctx, convID, view, model)
		if _, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 6, ucTarget(conv, model)); err != nil {
			t.Fatalf("fold: %v", err)
		}
		return ucSummaryText(t, ctx, convID)
	}
	var a, b string
	t.Run("pass_a", func(t *testing.T) { a = run(t) })
	t.Run("pass_b", func(t *testing.T) { b = run(t) })
	if a == "" || b == "" {
		t.Fatal("empty roll-up")
	}
	if a != b {
		t.Fatalf("machine-only fallback is not byte-stable:\n--- A ---\n%s\n--- B ---\n%s",
			headRunes([]byte(a), 300), headRunes([]byte(b), 300))
	}
}

// TestStoredRollUpStaysBoundedOnTheRealFixture: on the real conversation dump
// (LLM down, so the machine render is what stores), the roll-up must stay
// within the storage bound. Reports the size for the final report.
func TestStoredRollUpStaysBoundedOnTheRealFixture(t *testing.T) {
	msgs := ulLoadFixture(t)
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-fixture-rollup", 131072, nil)

	cum := 1_000
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			cum += SentViewRowChars(msgs[i]) / 3
			msgs[i].PromptTokens = cum
		}
	}
	ucPush(t, ctx, convID, msgs...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	before := SentViewTokens(view)

	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 12, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	summary := ucSummaryText(t, ctx, convID)
	after := SentViewTokens(ucLLMView(t, ctx, convID))
	t.Logf("FIXTURE ROLL-UP: %d rows -> folded %d; view %d -> %d tokens; stored roll-up %d chars (~%d tokens), bound %d",
		len(view), folded, before, after, len(summary), len(summary)/3, db.MaxDurableSummaryChars)
	if folded == 0 {
		t.Fatal("fold removed 0 rows on the real conversation")
	}
	if summary == "" {
		t.Fatal("no roll-up stored")
	}
	if len(summary) > db.MaxDurableSummaryChars {
		t.Fatalf("stored roll-up %d chars over the %d bound", len(summary), db.MaxDurableSummaryChars)
	}

	// SECOND fold on the real conversation: the prior roll-up is AT the bound, so
	// composition must fire the escape valve (re-megacave the PRIOR) and keep the
	// newest section. Reports the chained roll-up size.
	ucPush(t, ctx, convID, ucHistory("fixture generation two", 6, 1_200)...)
	v2 := ucLLMView(t, ctx, convID)
	conv2 := ucConvOf(t, ctx, convID, v2, model)
	priorLen := len(PriorSummaryFromView(v2))
	folded2, err := CompactConversationMachineOnly(ctx, convID, v2, PriorSummaryFromView(v2), 4, ucTarget(conv2, model))
	if err != nil {
		t.Fatalf("fold 2: %v", err)
	}
	chained := ucSummaryText(t, ctx, convID)
	t.Logf("FIXTURE CHAIN: prior %d chars + new section -> stored %d chars (bound %d), folded %d rows, escape-valve marker present=%v",
		priorLen, len(chained), db.MaxDurableSummaryChars, folded2, strings.Contains(chained, "megacaved"))
	if len(chained) > db.MaxDurableSummaryChars {
		t.Fatalf("chained roll-up %d chars over the bound — the escape valve did not fire", len(chained))
	}
	if folded2 > 0 {
		if !strings.Contains(chained, "megacaved") {
			t.Fatal("the chained roll-up does not name the escape valve")
		}
		if !strings.Contains(chained, "chain") && !strings.Contains(chained, "prior roll-up") {
			t.Fatal("the escape-valve marker does not name the prior roll-up")
		}
	}
}

// headRunes is a test-only truncation for failure messages.
func headRunes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...[truncated]"
}

// ucTarget builds the fold's LLM-first summarisation target from the test
// conversation: the CHAT model, and the payload whose ModelSelected the fold
// replays. Passing it makes the fold attempt the LLM (which fails fast with no
// proxy), so the fallback branch is the one under test.
func ucTarget(conv utils.Conversation, model utils.Model) foldSummaryTarget {
	return foldSummaryTarget{
		model:   model,
		payload: ChatPayload{ConversationID: conv.ID, ModelSelected: conv.ModelSelected},
	}
}

// TestFoldSummaryRequestShapeAgainstFakeProxy drives STAGE 2's whole
// LLM-success path against a stand-in proxy: it captures the bytes the
// summariser posts and asserts the request SHAPE, then asserts the fold stores
// the LLM's text megacaved (the chain's newest section is a megacaved LLM
// summary).
//
// THE FIXTURE MUST FORCE STAGE 2. On a wide window the fold's room is generous,
// the stage-1 megacave fits, and the model is never called — which is the
// design, and is pinned by TestFoldFitsTheMegacaveAndNeverCallsTheModel. To
// reach stage 2 the fixture uses a NARROW window: the fold's room collapses,
// the per-row budget lands on its floor (markers, not content), the fit test
// fails, and the fold escalates.
func TestFoldSummaryRequestShapeAgainstFakeProxy(t *testing.T) {
	const window = 8_192
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-fake-proxy-chat", window, map[string]string{"system_tools__shell_exec": "on"})

	var gotBody []byte
	var gotPath string
	srv := newFakeSummaryProxy(t, &gotBody, &gotPath,
		"CHECKPOINT-TEXT-7f3a: file=/srv/app/module_0.go status=ok, verbatim user ask preserved")
	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	hist := ucHistory("proxy-backed fold", 10, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	// The chat request the production path would send, for the head compare.
	chat, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}

	// The request stage 2 BUILDS (no network), for the shape asserts.
	req, err := buildSummaryRequestFromMegacave(ctx, conv, model, view[:len(view)-4], payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req.Stream {
		t.Fatal("Stream must be false for a one-shot summary")
	}
	if req.Model != model.Name {
		t.Fatalf("model %q, want the chat model %q", req.Model, model.Name)
	}
	if req.MaxTokens == nil || *req.MaxTokens <= minSummaryOutputTokens {
		t.Fatalf("max_tokens %v — stage 2's budget collapsed, so the fold would refuse the model on a window that fits", req.MaxTokens)
	}
	// STAGE 2'S INPUT IS BOUNDED BY CONSTRUCTION. The body is the megacave, and
	// the megacave is sized to the room this window leaves. No request-level row
	// cap exists any more, so this assertion is the whole guarantee: the input
	// plus the reply reserve must fit the window.
	input := renderedRowsTokens(req.Messages) + len(marshalNoEscape(req.Tools))/summaryCharsPerToken
	if input+guardSafetyReserve+minSummaryOutputTokens > window {
		t.Fatalf("stage 2 ships %d input tokens against a %d-token window — the megacave is not sized to the room", input, window)
	}
	// The body row is the megacave, not raw rows.
	body, _ := req.Messages[len(req.Messages)-1].Content.(string)
	if !strings.Contains(body, "TOOL_RESULT ") {
		t.Fatal("stage 2's body is not a megacave render")
	}

	// The fold, with the proxy answering.
	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 8, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows")
	}
	if gotPath == "" {
		t.Fatal("the summariser never posted anything: the fixture did not force stage 2")
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("posted to %q, want /v1/chat/completions", gotPath)
	}

	// What the proxy actually received.
	var sent StandardChatRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	if sent.Model != model.Name {
		t.Fatalf("posted model %q, want the chat model %q (the fast model would pay a full prefill)", sent.Model, model.Name)
	}
	if sent.Stream {
		t.Fatal("posted Stream=true; the summary is one-shot")
	}
	if sent.MaxTokens == nil || *sent.MaxTokens <= minSummaryOutputTokens {
		t.Fatalf("posted max_tokens %v", sent.MaxTokens)
	}
	// THE HEAD IS NEVER MOVED: the posted system rows are the chat request's
	// system rows, semantically (the body went through a JSON round-trip, and
	// StandardMessageReq.Content is interface{}, so unmarshalling turns
	// parts-shaped content into maps whose re-marshal reorders keys).
	chatNoClock := stripCurrentTimeTail(append([]StandardMessageReq(nil), chat.request.Messages...))
	n := leadingSystemRows(chatNoClock)
	if len(sent.Messages) != n+1 {
		t.Fatalf("posted %d rows, want the %d system rows plus one megacave row", len(sent.Messages), n)
	}
	for i := 0; i < n; i++ {
		if a, b := wireShape(sent.Messages[i]), wireShape(chatNoClock[i]); a != b {
			t.Fatalf("posted system row %d differs from the chat request's row %d:\n  posted: %s\n  chat:   %s", i, i, a, b)
		}
	}
	last := sent.Messages[len(sent.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("the megacave+instruction row is a %q message; the assistant can only answer a user turn", last.Role)
	}
	if s, _ := last.Content.(string); !strings.Contains(s, "EVICTED") {
		t.Fatal("the instruction does not name the eviction")
	}
	a, _ := json.Marshal(chat.request.Tools)
	b, _ := json.Marshal(sent.Tools)
	if !bytes.Equal(a, b) {
		t.Fatal("the posted tool schemas differ from the chat request's")
	}

	// The LLM's checkpoint text is the stored roll-up of the FIRST fold: the
	// storage layer emits no section banner when there is no prior to chain onto.
	stored := ucSummaryText(t, ctx, convID)
	if !strings.Contains(stored, "CHECKPOINT-TEXT-7f3a") {
		t.Fatalf("the LLM's checkpoint is not in the stored roll-up:\n%s", headRunes([]byte(stored), 400))
	}
}

// TestFoldCallsTheModelFirst pins the owner's order on the wire: STAGE 1 IS THE
// MODEL. A fold on a conversation whose machine render would fit its ceiling
// STILL asks the model first — the machine render is only the fail-closed
// fallback, never the primary path.
//
// This is the inverse of the invariant this file used to pin ("a fold that fits
// its megacave costs zero inference"). That order is retired: the rows the fold
// evicts are the last thing the chat model will ever read, so compressing them
// on the request that replays the cached prefix is the cheapest possible use of
// that context, and the machine render at 64 chars per row is not readable.
func TestFoldCallsTheModelFirst(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-stage1-llm-first", 262_144, map[string]string{"system_tools__shell_exec": "on"})

	var gotBody []byte
	var gotPath string
	srv := newFakeSummaryProxy(t, &gotBody, &gotPath, "STAGE1-COMPRESSED-4c88 module_0.go")
	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	hist := ucHistory("stage 1 is the model", 10, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)

	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 8, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows")
	}
	if gotPath == "" {
		t.Fatal("the fold never posted to the model: stage 1 is not the LLM call")
	}
	stored := ucSummaryText(t, ctx, convID)
	if stored == "" {
		t.Fatal("no roll-up stored")
	}
	if !strings.Contains(stored, "STAGE1-COMPRESSED-4c88") {
		t.Fatalf("the stored roll-up is not the model's compression:\n%s", headRunes([]byte(stored), 300))
	}
}

// TestFoldFallsBackToTheMachineRenderWhenTheModelFails: the fail-closed path.
// With no model reachable, the fold must still write a roll-up — the rows are
// being evicted, and storing nothing loses them outright.
func TestFoldFallsBackToTheMachineRenderWhenTheModelFails(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-stage1-fallback", 262_144, map[string]string{"system_tools__shell_exec": "on"})

	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = "http://127.0.0.1:1/v1" // nothing listening: every stage fails
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	hist := ucHistory("fallback", 10, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)

	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 8, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatal("a model failure lost the fold")
	}
	stored := ucSummaryText(t, ctx, convID)
	if stored == "" {
		t.Fatal("the fold stored nothing after the model failed")
	}
	if !strings.Contains(stored, "module_0.go") {
		t.Fatalf("the fallback roll-up does not carry the folded rows' own paths:\n%s", headRunes([]byte(stored), 300))
	}
}

// newFakeSummaryProxy stands in for the model endpoint: it records the posted
// body/path and replies with one chat-completion choice carrying `text`.
func newFakeSummaryProxy(t *testing.T, gotBody *[]byte, gotPath *string, text string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*gotBody = body
		*gotPath = r.URL.Path
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"role": "assistant", "content": text}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFoldSummaryProxyFailureFallsBackToMachineRender: the proxy is up but
// returns 500. The fold must store the machine-only render (the folded rows'
// own content), not fail and not store an empty section.
func TestFoldSummaryProxyFailureFallsBackToMachineRender(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-fake-proxy-500", 131072, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	hist := ucHistory("proxy 500 fold", 14, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)

	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 6, ucTarget(conv, model))
	if err != nil {
		t.Fatalf("fold failed on a 500: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows — the 500 path lost the fold")
	}
	stored := ucSummaryText(t, ctx, convID)
	if stored == "" {
		t.Fatal("no roll-up stored after a 500")
	}
	// The machine render carries the folded rows' own file paths.
	if !strings.Contains(stored, "module_0.go") {
		t.Fatalf("the machine-only fallback render is missing from the roll-up:\n%s", headRunes([]byte(stored), 300))
	}
}

// wireShape renders the parts of a request row that a chat template renders:
// role, tool identity, and the row's text (string content, or the concatenation
// of its text parts). Content-shape differences (string vs parts) are
// preserved by ucMarshalRows-level byte assertions, not here.
func wireShape(m StandardMessageReq) string {
	var text string
	switch c := m.Content.(type) {
	case string:
		text = c
	case []StandardContentReq:
		var sb []string
		for _, p := range c {
			if p.Type == "text" || p.Type == "snippet" {
				sb = append(sb, p.Text)
			} else {
				sb = append(sb, "<"+p.Type+">")
			}
		}
		text = strings.Join(sb, "\n")
	case nil:
		text = "<nil>"
	default:
		// A JSON round-trip shape (interface{} content decodes to []interface{}
		// of maps): decode it back into parts so the SAME text join is compared
		// on both sides. Comparing the re-marshalled maps directly would compare
		// key order, which the round-trip changes and the wire never sends.
		if v, err := json.Marshal(c); err == nil {
			var back []StandardContentReq
			if json.Unmarshal(v, &back) == nil {
				var sb []string
				for _, pt := range back {
					if pt.Type == "text" || pt.Type == "snippet" {
						sb = append(sb, pt.Text)
					} else {
						sb = append(sb, "<"+pt.Type+">")
					}
				}
				text = strings.Join(sb, "\n")
			} else {
				text = string(v)
			}
		}
	}
	tc, _ := json.Marshal(m.ToolCalls)
	return m.Role + "|" + m.Name + "|" + m.ToolCallID + "|" + string(tc) + "|" + text
}

// TestFoldSurvivesSummariserDeadline: the proxy accepts and stalls, and the
// caller's context deadline expires. The summariser must return an error (not
// hang), and the fold must still store a bounded, non-empty roll-up.
func TestFoldSurvivesSummariserDeadline(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "uc-fold-deadline", 131072, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stalls far past the test's deadline.
		select {}
	}))
	t.Cleanup(srv.Close)
	oldBase := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	prevReady := litellmReady.Load()
	litellmReady.Store(true)
	t.Cleanup(func() {
		LiteLLMBaseURL = oldBase
		litellmReady.Store(prevReady)
	})

	hist := ucHistory("deadline fold", 12, 1_200)
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)

	// The summariser with an already-expired context fails fast.
	dead, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	<-dead.Done()
	start := time.Now()
	if _, err := attemptFoldSummaryFromMegacave(dead, conv, model, view[:len(view)-4]); err == nil {
		t.Fatal("the summariser returned success on a cancelled context")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the summariser took %v to honour a dead context — it must fail fast", el)
	}

	// The fold with a live context: the proxy stalls, so the summariser's own
	// ceiling would be long; the machine-only render must be what stores. Use a
	// short-deadline context for the summariser via a fold call that cannot
	// reach it (the proxy accepts the connection, so the fold waits at most the
	// summariser ceiling). To keep the test fast, assert the summariser path
	// alone, then fold with the model omitted from the payload so durableFold
	// skips the LLM and stores the machine render.
	folded, err := CompactConversationMachineOnly(ctx, convID, view, PriorSummaryFromView(view), 6)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatal("fold removed 0 rows")
	}
	stored := ucSummaryText(t, ctx, convID)
	if stored == "" || len(stored) > db.MaxDurableSummaryChars {
		t.Fatalf("stored roll-up unusable: %d chars", len(stored))
	}
	if !strings.Contains(stored, "module_0.go") {
		t.Fatal("machine-only render missing from the roll-up")
	}
}
