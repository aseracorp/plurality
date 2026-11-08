package ai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// ---------------------------------------------------------------------------
// Bug 4: "Eco compaction does not kick in when it should."
//
// Reproduction of the production failure, written against the EXISTING API so
// it compiles (and fails) on the pre-fix tree.
//
// Live evidence (server.log, conversation aeb657c899e600ff1a822ffb):
//
//	[Eco] content ≈52508 tokens ≤ trigger 209715 — sending unchanged (stable prefix)
//	[Eco] compression-aware guard: est 438246 tokens / factor 1.32 = 331647 vs window 262144
//	[Eco] preemptive context guard: estimated 438246 tokens vs window 262144
//	[LLMLoop] context length exceeded — compacting history (durable) and retrying
//	[LLMLoop] compacted conversation ...: durable removed 0, in-memory 6 -> 6 messages
//	[LLMLoop] Error calling LLM : request estimated to exceed the context window
//
// One request, two estimates. The eco TRIGGER read ~52k tokens and shipped the
// history UNCHANGED; the wire GUARD on the very same request read ~438k. The
// delta is the payload `MessageContent.TotalContentChars()` does not count:
// image/document parts (re-inflated to base64 by PrepareMessagesForAI) and
// assistant tool_call ARGUMENTS. The trigger can therefore never reach the
// threshold, the resolver concludes "full history fits", durable compaction
// folds 0 messages, the retry re-ships the identical oversized request, and
// the conversation dead-ends with "request exceeds the context window".
//
// strata rejects as soon as prompt + reserved output reaches max_context
// (262144): the observed prompt sizes cluster at 79066 / 79829 / 80621 with
// max_tokens = 262144 - prompt_tokens, i.e. ~8 tokens of headroom. Compaction
// must fire on the COMPLETE request size with the output budget reserved.
// ---------------------------------------------------------------------------

// registerBug4Model installs a model in the registry singleton the way
// Refresh() does, so the window math runs against a real declared
// context_window. The registry is EMPTY in unit tests by default — the exact
// "window unknown" path that makes MaxContextTokensFor return 0.
func registerBug4Model(name string, window int) {
	Models.mu.Lock()
	defer Models.mu.Unlock()
	entry := ModelEntry{
		Name:                    name,
		Mode:                    "chat",
		SupportsFunctionCalling: true,
		MaxContextTokens:        window,
	}
	Models.byName[name] = entry
	for i := range Models.list {
		if Models.list[i].Name == name {
			Models.list[i] = entry
			return
		}
	}
	Models.list = append(Models.list, entry)
}

func itoaB4(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// buildBug4Conv reproduces the failing conversation shape: a text history that
// stays comfortably BELOW the eco trigger, an attachment turn whose payload
// PrepareMessagesForAI re-inflates to base64, and a live tool chain whose
// assistant tool_call arguments ride on the wire.
//
//	textTokens: estimated tokens of plain prose (the ONLY thing the pre-fix
//	            trigger counted)
//	attachTokens: estimated tokens of base64 image payload (NOT counted pre-fix)
//	toolArgTokens: estimated tokens of assistant tool_call arguments (NOT counted)
func buildBug4Conv(textTokens, attachTokens, toolArgTokens, toolResultChars int) []utils.Message {
	var msgs []utils.Message
	// 40 user + 40 assistant prose messages.
	per := textTokens * 3 / 80
	if per < 1 {
		per = 1
	}
	for i := 0; i < 40; i++ {
		msgs = append(msgs, utils.Message{Role: "user", Content: utils.NewTextContent(strings.Repeat("u", per))})
		msgs = append(msgs, utils.Message{Role: "assistant", Content: utils.NewTextContent(strings.Repeat("a", per))})
	}
	// Attachment turn: image parts are invisible to TotalContentChars, and
	// PrepareMessagesForAI keeps the newest user image intact on every request.
	dataURI := "data:image/png;base64," + strings.Repeat("A", attachTokens*3)
	msgs = append(msgs, utils.Message{
		Role: "user",
		Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: "here are the screenshots"},
			{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: dataURI}},
		}),
	})
	// Live tool chain: tool_call arguments are marshalled into the request.
	id := "call_live"
	args := `{"command":"` + strings.Repeat("x", toolArgTokens*3) + `"}`
	msgs = append(msgs, utils.Message{
		Role:    "assistant",
		Content: utils.NewTextContent(""),
		ToolCalls: []utils.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: args},
		}},
	})
	msgs = append(msgs, utils.Message{
		Role:       "tool",
		Name:       "system_tools__shell_exec",
		ToolCallID: id,
		Content:    utils.NewTextContent(strings.Repeat("t", toolResultChars)),
	})
	return msgs
}

// measureBug4WireTokens builds the request the way SendChatCompletion does
// (PrepareMessagesForAI -> convertMessagesToOpenAI -> JSON) and returns the
// estTokens the preemptive guard computes from the marshalled wire bytes.
// This is the ground truth for "what the provider receives".
func measureBug4WireTokens(t *testing.T, conv []utils.Message, model utils.Model, tools []utils.ToolsRequest) int {
	t.Helper()
	prepared, _, _ := PrepareMessagesForAI(conv, model)
	wire, _ := convertMessagesToOpenAI(prepared, model)
	req := StandardChatRequest{
		Model:         model.Name,
		Messages:      wire,
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
		Tools:         tools,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return buf.Len() / 3
}

// TestSentViewTokensCountsAttachmentsAndToolArgs pins the trigger contract:
// the eco trigger must estimate EVERYTHING that is sent — system prompt,
// tool schemas, memory snippets, the reserved output budget (the callers pass
// those as `overhead`), the conversation, AND the attachment / tool-argument
// payload that rides in the message list.
//
// Pre-fix, SentViewTokens counts only Type=="text" parts: a 15.7 MB attachment
// conversation estimates at ~52k tokens while the wire carries ~438k, so the
// trigger can never fire and compaction never kicks in.
func TestSentViewTokensCountsAttachmentsAndToolArgs(t *testing.T) {
	const attachTokens = 20_000
	const toolArgTokens = 3_000
	msgs := buildBug4Conv(45_000, attachTokens, toolArgTokens, 1_200)

	got := SentViewTokens(msgs)

	// What the pre-fix estimator returned: prose only.
	textOnly := 0
	for i := range msgs {
		textOnly += msgs[i].Content.TotalContentChars()
	}
	textOnlyTokens := textOnly / 3

	if got <= textOnlyTokens {
		t.Fatalf("SentViewTokens = %d, prose-only count = %d: the base64 attachment payload and "+
			"the assistant tool_call arguments are sent on the wire but are NOT counted, so the "+
			"eco trigger under-reads the request (~%d est tokens missing) and compaction never fires",
			got, textOnlyTokens, attachTokens+toolArgTokens)
	}
	want := textOnlyTokens + attachTokens + toolArgTokens
	if got < want-64 || got > want+64 {
		t.Fatalf("SentViewTokens = %d, want ~%d (prose %d + attachments %d + tool args %d)",
			got, want, textOnlyTokens, attachTokens, toolArgTokens)
	}
	t.Logf("sent view = %d est tokens vs prose-only %d (attachments %d + tool args %d now counted)",
		got, textOnlyTokens, attachTokens, toolArgTokens)
}

// TestSentViewTokensSkipsCompactedRowsAndIsBounded guards the fix's other
// half: rows durable compaction already folded must stay OUT of the estimate
// (that is the #160 cache-bust fix), so counting the wire payload must not
// re-inflate the trigger for an already-compacted conversation.
func TestSentViewTokensSkipsCompactedRowsAndIsBounded(t *testing.T) {
	msgs := buildBug4Conv(45_000, 20_000, 3_000, 1_200)
	live := SentViewTokens(msgs)

	// Mark everything except the newest user turn + checkpoint pair folded.
	for i := range msgs {
		if i < len(msgs)-3 {
			msgs[i].Compacted = true
		}
	}
	compactedView := SentViewTokens(msgs)
	if compactedView >= live {
		t.Fatalf("compacted rows must stay out of the trigger estimate: full view %d, folded view %d", live, compactedView)
	}
	t.Logf("full sent view %d est tokens -> folded view %d", live, compactedView)
}

// TestBug4_TriggerMustFireBeforeTheWindowIsExceeded is THE reproduction.
//
// Model window 81920 (the observed 79066/79829/80621 cluster sits at exactly
// such an edge), eco trigger_tokens above the point where the request stops
// fitting, prose volume below the trigger, attachments + tool args pushing the
// real request over the window.
//
// Pre-fix: est (prose only) <= trigger -> "[Eco] ... sending unchanged" ->
// the request that goes out is BIGGER than the window -> strata rejects with
// "request exceeds the context window" and the conversation dies.
// Post-fix: the estimate matches the wire, the trigger fires, and the request
// that goes out fits the window with room left for the model's reply.
func TestBug4_TriggerMustFireBeforeTheWindowIsExceeded(t *testing.T) {

	const window = 81_920
	const modelName = "bug4-model"
	registerBug4Model(modelName, window)
	model := utils.Model{Name: modelName}

	const systemChars = 4_000
	tools := bug4ToolSchemas(1_000)
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	// The same overhead terms SendChatCompletion feeds the trigger.
	overhead := len(toolsJSON)/3 + systemChars/4 + 512

	// Payload tuned to the observed production shape: prose that on its own reads
	// BELOW the eco trigger, plus attachment + tool-argument volume the pre-fix
	// estimator was blind to, pushing the real request past the window.
	conv := buildBug4Conv(45_000, 40_000, 4_000, 1_200)

	// Ground truth: what the provider receives, measured from the wire bytes.
	wireTokens := measureBug4WireTokens(t, conv, model, tools)
	if wireTokens <= window {
		t.Fatalf("fixture is not production-shaped: the request (%d est tokens) fits the %d window, so it cannot reproduce the failure",
			wireTokens, window)
	}

	// The trigger's own estimate, exactly as the request path computes it.
	triggerEst := SentViewTokens(conv) + overhead
	trigger := effectiveEcoTrigger(modelName)

	// (1) The trigger estimate must reflect the request that is SENT. A 8x
	// blind spot (observed live: 52k est vs 438k wire) is the root cause.
	if float64(wireTokens-triggerEst) > 0.15*float64(wireTokens) {
		t.Fatalf("eco trigger under-reads the request: est=%d tokens vs %d tokens on the wire "+
			"(window %d). %d est tokens of attachment / tool-argument payload are not counted, so "+
			"the trigger reports content below the threshold and ships an OVERSIZED request unchanged.",
			triggerEst, wireTokens, window, wireTokens-triggerEst)
	}

	// (2) Compaction must therefore be scheduled BEFORE the window is crossed.
	if triggerEst <= trigger {
		t.Fatalf("eco compaction did not trigger: est=%d tokens <= trigger=%d while the request "+
			"is %d tokens vs a %d window. The threshold ignores the window and the reserved output "+
			"budget, so a request past the context window is shipped UNCHANGED.",
			triggerEst, trigger, wireTokens, window)
	}

	// (3) When it fires it must be deterministic, must fit, must fold >0, and
	// must never erase the conversation.
	contentBudget := window - overhead - 6144 // 6144 = the reply reservation
	if contentBudget <= 0 {
		t.Fatalf("content budget collapsed to %d — the overhead tax exceeds the window", contentBudget)
	}
	view, foldFrom := resolveEcoContextCompaction(conv, window, contentBudget)
	if len(view) == 0 {
		t.Fatalf("resolver returned an empty view: the caller's `len(convTail) > 0` guard ships the FULL UNCOMPACTED history")
	}
	if foldFrom <= 0 {
		t.Fatalf("resolver folded 0 messages: durable compaction persists nothing, so every later request re-inflates and compaction looks like it never kicks in")
	}
	if approxTokens(view) > contentBudget {
		t.Fatalf("compacted view is %d est tokens > content budget %d — the request would still exceed the window",
			approxTokens(view), contentBudget)
	}
	// Live user turn survives verbatim.
	newestUser := indexOfLastUser(conv)
	if newestUser < 0 {
		t.Fatalf("fixture has no user message")
	}
	wantText := conv[newestUser].TextContent()
	var survived bool
	for i := range view {
		if view[i].TextContent() == wantText {
			survived = true
			break
		}
	}
	if !survived {
		t.Fatalf("compaction dropped the newest user message — the model lost the live request")
	}
	t.Logf("OK: wire %d > window %d -> compaction fires; %d msgs -> %d msgs, %d -> %d est tokens (budget %d), foldFrom=%d",
		wireTokens, window, len(conv), len(view), approxTokens(conv), approxTokens(view), contentBudget, foldFrom)
}

// TestBug4_FittingConversationIsSentUnchanged: the fix must NOT compact what
// fits. A byte-stable view below the trigger is what keeps the
// strata/llama.cpp KV prompt cache hot, so it must survive the fix.
func TestBug4_FittingConversationIsSentUnchanged(t *testing.T) {
	const window = 262_144
	const modelName = "bug4-bigmodel"
	registerBug4Model(modelName, window)
	model := utils.Model{Name: modelName}

	var conv []utils.Message
	for i := 0; i < 6; i++ {
		conv = append(conv, utils.Message{Role: "user", Content: utils.NewTextContent("short ask " + itoaB4(i))})
		conv = append(conv, utils.Message{Role: "assistant", Content: utils.NewTextContent("short answer " + itoaB4(i))})
	}
	tools := bug4ToolSchemas(300)
	toolsJSON, _ := json.Marshal(tools)
	overhead := len(toolsJSON)/3 + 8_000/4 + 512

	wireTokens := measureBug4WireTokens(t, conv, model, tools)
	triggerEst := SentViewTokens(conv) + overhead
	trigger := effectiveEcoTrigger(modelName)
	if wireTokens > window {
		t.Fatalf("fixture must fit the window: wire %d vs %d", wireTokens, window)
	}
	if triggerEst > trigger {
		t.Fatalf("a trivially fitting conversation must not be compacted: est=%d > trigger=%d (wire %d, window %d)",
			triggerEst, trigger, wireTokens, window)
	}
	view, foldFrom := resolveEcoContextCompaction(conv, window, window-overhead-6144)
	if foldFrom != 0 || len(view) != len(conv) {
		t.Fatalf("fitting history must be returned unchanged (byte-stable prefix for KV caching): foldFrom=%d, %d -> %d msgs",
			foldFrom, len(conv), len(view))
	}
}
