package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// eco_summarize.go — the LLM summarisation call, reintroduced as the LAST
// THING that touches the context the durable fold is about to evict.
//
// Shape of the design (and why):
//
//   - The summary request is NOT hand-rolled. It is the chat request, rebuilt
//     through the production builder (buildChatRequestParts) for the SAME
//     conversation and the SAME chat model, so the serialized prefix — system
//     prompt, tool schemas, the eco-filtered history — is byte-for-byte the
//     prefix the backend already has cached. The backend (llama.cpp/avalon/
//     strata, and the cloud prefix caches) reuses the longest common prefix, so
//     the summary call costs the eviction it is summarising and nothing more.
//     Hand-rolling a payload here would re-prefill the entire conversation.
//
//   - The CLOCK is stripped from the replay (stripCurrentTimeTail). It is the
//     only per-turn mutation of a chat request, so it is the one thing that
//     would make the replay non-identical, and a model writing a durable
//     checkpoint of a history that ends on a timestamp narrates that timestamp
//     into the roll-up that is stored forever.
//
//   - The summarisation instruction is appended as a real trailing USER turn,
//     so the assistant can answer it. It rides at the very END, past the
//     cached prefix: the prefix stays intact.
//
//   - The model is the conversation's CHAT model (SelectModel on the
//     conversation's own selection) — never the "fast" shortcut. The fast
//     model's KV cache is not warm for this conversation, so routing the
//     summary to it would pay a full prefill AND produce a summary from a model
//     that never saw the conversation.
//
//   - The LLM is the FIRST attempt, and its failure can never lose a fold. The
//     machine-only megacave render is the fallback and the fold proceeds
//     untouched by the LLM's absence (see durableFold's branch, and the AST
//     guard in ultracave_llm_guard_test.go).
//
//   - The stored roll-up is a CHAIN of megacaved summaries: the LLM text is
//     itself megacaved before it is stored, the prior roll-up is carried
//     forward, and when the chain outgrows the bound the PRIOR is re-megacaved
//     (MegacaveSummaryChain, registered as db's escape valve) — deterministic,
//     lossy-compress, never a silent drop.

// maxSummaryOutputTokens is the sane ceiling on the summary's output budget:
// min(window - prefix, this). It replaces the old hardcoded 8192, which cannot
// hold the durable checkpoint of a whole evicted history (a 100k-char roll-up
// needs far more than 8192 tokens to be written verbatim-dense).
const maxSummaryOutputTokens = 32_768

// minSummaryOutputTokens is the floor: a summarisation call that gets less than
// this is not worth making (the fold falls back to the machine render, which
// costs no prefill at all).
const minSummaryOutputTokens = 1_024

// summaryCharsPerToken is the chars->tokens estimate used for the summary
// budget. It is the SAME 3.0 the preemptive context guard in index.go uses, so
// the fold and the guard agree about the size of a request.
const summaryCharsPerToken = 3

// summaryHTTPTimeout bounds the summarisation round-trip. A self-hosted
// summariser on a big prompt is slow (observed: ~3.5 min per summary on
// swift-1.5-iq3_s), so this is generous, but it is a HARD bound: the eco
// single-flight slot must not be held forever by a wedged proxy.
const summaryHTTPTimeout = 10 * time.Minute

// summaryHTTPClient is the summariser's client.
//
// It MUST NOT share utils.HTTPClient.Transport, and the deployed build proves
// why. That transport sets ResponseHeaderTimeout: 90s, which bounds the time to
// the FIRST RESPONSE HEADER. For a STREAMING chat request that is the time to
// the first SSE event, so 90s is generous and a long stream is unaffected. The
// summary request is Stream: false — a one-shot completion — so its headers
// arrive only when the WHOLE summary has been generated, so the 90s caps the
// ANSWER at 90 seconds of generation. Measured decode rate on the deployed
// backend (tok_out / (total_ms - ttfb_ms), decode-dominated samples only,
// n=2139): median 36.6 tok/s, p10 24.5, min 3.6. The summary asks for up to
// maxSummaryOutputTokens (32768) tokens, which needs ~896s at the median rate —
// far past 90s. The inherited bound therefore shot every summary, at exactly
// 90s. (Do NOT derive this rate from tok_out / ttfb_ms: chat requests are
// Stream:true, so ttfb_ms is PREFILL, and that ratio measures nothing.)
//
// The error string pins the owner. Go emits "net/http: timeout awaiting
// response headers" ONLY from Transport.ResponseHeaderTimeout
// (net/http/transport.go, errTimeout); a Client.Timeout produces
// "context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)". The deployed log has the former, five folds running, and
// "LLM fold summary" appears ZERO times against 30 machine-only fallbacks:
//
//	[Eco] machine-only FALLBACK fold (LLM summariser unavailable: summary
//	request failed: Post "http://127.0.0.1:8787/v1/chat/completions":
//	net/http: timeout awaiting response headers)
//
// The summariser's own ceiling (summaryHTTPTimeout, 10min) was never the
// binding constraint; it is the bound this transport clone restores. Raising
// the header budget to it adds NO new exposure: the round-trip was already
// capped at 10min by Client.Timeout, and the summariser calls
// summaryHTTPClient.Do directly (no DoLLMHTTPWithRetry, so no retry storm).
var summaryHTTPClient = &http.Client{
	Timeout:   summaryHTTPTimeout,
	Transport: summaryTransport(),
}

// summaryTransport clones the shared transport and raises ONLY the header
// budget. Connection pooling, dial and TLS bounds are inherited unchanged, so
// the summariser keeps reusing the proxy's keep-alive connections.
//
// The type assertion is guarded, not assumed: this runs in a package-level
// initializer, and a panic here would take the whole server down. If the shared
// transport is ever not an *http.Transport, fall back to a fresh one with the
// same bounds rather than inheriting a 90s header budget we cannot control.
func summaryTransport() http.RoundTripper {
	return summaryTransportWithHeaderTimeout(summaryHTTPTimeout)
}

// summaryTransportWithHeaderTimeout is summaryTransport with the header budget
// as a parameter, so the end-to-end test can exercise the SAME wiring (clone +
// raise) against a slow server on a second scale. Production passes
// summaryHTTPTimeout.
func summaryTransportWithHeaderTimeout(headerBudget time.Duration) *http.Transport {
	if shared, ok := utils.HTTPClient.Transport.(*http.Transport); ok {
		t := shared.Clone()
		// For a one-shot (Stream:false) call, "time to headers" IS "time to the
		// complete answer", so the header budget must equal the round-trip
		// budget. Inheriting the shared 90s shot every summary.
		t.ResponseHeaderTimeout = headerBudget
		return t
	}
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerBudget,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
	}
}

func init() {
	// The storage layer's escape valve must be the SAME deterministic
	// lossy-compress operation the rest of the roll-up uses. db cannot import
	// ai (ai imports db), so the compressor is injected here.
	db.SetSummaryChainCompressor(MegacaveSummaryChain)
}

// MegacaveSummaryChain compresses a roll-up text through the megacave transform
// into at most `maxChars` characters. Deterministic (no clock, no random, no
// LLM), so the escape valve in db.composeDurableSummary is byte-stable.
//
// It is the ONLY lossy-compress operation allowed on the chain: re-summarising
// the prior roll-up with an LLM would destroy the one representation every row
// an earlier fold compacted still has.
func MegacaveSummaryChain(text string, maxChars int) string {
	if strings.TrimSpace(text) == "" || maxChars <= 0 {
		return ""
	}
	var b strings.Builder
	writeLine := func(s string) {
		if s == "" {
			return
		}
		b.WriteString(s)
		b.WriteString("\n")
	}
	show := maxChars - megacaveLabelOverheadChars
	if show < minMegacaveRowShowChars {
		show = minMegacaveRowShowChars
	}
	writeMegacaveText(&b, text, writeLine, show)
	return capMegacaveExcerpt(strings.TrimSpace(b.String()), maxChars)
}

// chatModelForConversation resolves the CHAT model of a conversation exactly
// the way the LLM loop does (SelectModel, including the vision swap when the
// history carries images and the text model cannot see them). The summary is
// sent to THAT model: it is the model whose KV prefix is warm and that owns the
// conversation.
func chatModelForConversation(conv utils.Conversation) utils.Model {
	return SelectModel(conv.ModelSelected, conv)
}

// chatPayloadForConversation rebuilds the request-shaping part of the chat
// payload from PERSISTED conversation state, so a summary request built after
// the loop ends assembles the same system prompt and the same tool schemas the
// loop's last request carried.
//
// WHAT IT CANNOT REPRODUCE, stated plainly: two payload fields are per-request
// and never persisted — the client-side tool definitions a Flutter client ships
// inline (ClientSideTools) and the per-request AvailableSkills list. When the
// last real chat request carried client-side tools, the replayed prefix differs
// in the tool-schema section, and the summary call pays a prefill for that
// section instead of reusing it. Nothing is lost — the summary is complete — it
// is just not free for that shape of conversation. The fold tests build the
// chat request from the SAME payload, which is the contract the byte-identity
// assertion pins.
func chatPayloadForConversation(conv utils.Conversation) ChatPayload {
	p := ChatPayload{
		ConversationID: conv.ID,
		ModelSelected:  conv.ModelSelected,
	}
	if conv.MiniApp != nil {
		p.MiniApp = *conv.MiniApp
	}
	return p
}

// summaryInstruction is the trailing USER turn of the summary request. It
// states the eviction, the verbatim-preservation contract, and points at the
// existing roll-up section so the model does not spend its budget restating
// what is already covered there.
func summaryInstruction(priorSummary string, evictedRows int) string {
	var b strings.Builder
	b.WriteString("SYSTEM TASK — DURABLE CONTEXT CHECKPOINT (not a reply to the user)\n\n")
	b.WriteString("The conversation crossed the durable-compaction cap. The OLDEST " + itoaRows(evictedRows) +
		" of the messages above this instruction are about to be EVICTED from your live context: they are being folded " +
		"out of every future request. After this fold you will never see them again.\n\n")
	b.WriteString("Write the durable checkpoint for the EVICTED (oldest) messages. The newest messages stay in your " +
		"context, so they matter less; the evicted range is what must survive in your text.\n\n")

	b.WriteString("1. Preserve VERBATIM: code, file paths, identifiers, IDs, error messages, exact commands and their output, " +
		"URLs, numeric values, and the user's requests in their own words.\n")
	b.WriteString("2. Preserve every tool-call outcome: which tool, the key arguments, and the substantive result " +
		"(files written, values returned, errors raised). Drop only ceremonial framing.\n")
	b.WriteString("3. Be DENSE: bullets/sections, no preamble, no closing remarks, no meta-commentary about the task. " +
		"Length should match the information density of the evicted rows, not a fixed target.\n")
	b.WriteString("4. Do NOT summarise what the existing roll-up already covers — the section marked \"" +
		db.NewerHistoryBanner + "\" is already stored verbatim ahead of your output. Your checkpoint ADDS to that chain.\n")
	b.WriteString("5. Cover the newest evicted rows first (what you are about to lose is the freshest work), then the older ones.\n")
	if s := strings.TrimSpace(priorSummary); s != "" {
		b.WriteString("\nThe tail of the existing roll-up, for context only (do not restate it):\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

// buildSummaryRequestFromMegacave is STAGE 2 of the durable fold: the model
// summarises the MEGACAVED history, never the raw rows.
//
// THE ORDER (the owner's design, #197 onwards): a fold first MEGACAVES the
// history — the deterministic machine render, lossless for user rows. As long as
// that render fits the fold's ceiling it IS the checkpoint and the model is
// never called. Only when the megacaved history is still over budget does stage
// 2 run, and what stage 2 sends is the MEGACAVED text: the model's job is to
// compact the already-compacted history further, not to re-read 2.8 MB of raw
// tool output.
//
// WHY THE LOSSY CAP IS GONE. This function used to be handed the RAW evicted
// range, which is unbounded (a deployed fold carried 1240 rows / 2824666 chars),
// so it needed a window cap that DROPPED ROWS from the summariser's view — the
// summariser then wrote a checkpoint about a fraction of the history it was
// supposed to summarise. Here the input is the megacave, and the megacave is
// budget-driven: the per-row show and the total ceiling are DERIVED from the
// room this request has left (window - head - schemas - instruction - reply
// reserve). The transform shrinks to fit; nothing is dropped silently.
//
// THE HEAD IS STILL NEVER MOVED. The request is the chat request's leading
// system rows and tool schemas, byte-for-byte, taken from a replay over the
// FULL slice (PrepareMessagesForAI numbers attachments by scan order over the
// slice it is given, so only the full slice renders the head identically),
// followed by ONE user row. Every history row is stripped — the tail-strip rule
// taken to its end. The KV prefix covers system + schemas; the history region is
// where the fold legitimately diverges, because the fold is the user-visible
// event that changes the context.
//
// ctx carries the per-user claim the builder needs for the important_memory
// block and the MCP/skill appends — the system prompt must be the same bytes.
func buildSummaryRequestMegacaveStage(ctx context.Context, conv utils.Conversation, model utils.Model, foldable []utils.Message, stage foldStage, payload ...ChatPayload) (StandardChatRequest, error) {
	if model.Name == "" {
		model = chatModelForConversation(conv)
	}
	if model.Name == "" {
		return StandardChatRequest{}, fmt.Errorf("no chat model resolved for conversation %s — the fold must fall back to the machine-only render", conv.ID)
	}
	if len(foldable) == 0 {
		return StandardChatRequest{}, fmt.Errorf("stage 2 has no history to summarise for conversation %s", conv.ID)
	}
	p := chatPayloadForConversation(conv)
	if len(payload) > 0 {
		p = payload[0]
	}
	if p.ConversationID == "" {
		p.ConversationID = conv.ID
	}

	// The head, from the full replay, byte-for-byte.
	parts, err := buildChatRequestParts(ctx, model, conv, p)
	if err != nil {
		return StandardChatRequest{}, fmt.Errorf("cannot replay the chat request for the fold summary: %w", err)
	}
	// Drop the clock: it is the only per-turn mutation of the prefix, and the
	// roll-up must not narrate a timestamp.
	full := stripCurrentTimeTail(append([]StandardMessageReq(nil), parts.request.Messages...))
	head := append([]StandardMessageReq(nil), full[:leadingSystemRows(full)]...)

	instruction := foldInstruction(stage, PriorSummaryFromView(conv.Messages), len(foldable), false)
	window := Models.DeclaredContextWindow(model.Name)
	schemaTokens := len(marshalNoEscape(parts.request.Tools)) / summaryCharsPerToken
	headTokens := renderedRowsTokens(head)
	instrTokens := len(instruction) / summaryCharsPerToken

	// THE SUMMARISER'S HISTORY ROOM. The summariser's own reply room is its
	// output floor plus the safety half of the guard reservation.
	// guardReplyReserve ITSELF is not subtracted — it is the CHAT request's
	// max_tokens reservation, and the summariser's max_tokens is computed FROM
	// this arithmetic, so subtracting it reserves the reply twice (measured: an
	// 8192-token window reported an impossible head and refused a call that
	// fits).
	roomTokens := window - headTokens - schemaTokens - instrTokens - guardSafetyReserve - minSummaryOutputTokens
	if window > 0 && roomTokens < minSummaryOutputTokens {
		// THE HEAD ITSELF DOES NOT LEAVE ROOM. No history can be sent in this
		// shape, so stage 2 cannot run: refuse and let the fold keep the stage-1
		// machine render. That is a decision, not a provider 500 and not a cap.
		return StandardChatRequest{}, fmt.Errorf("stage 2 has no room in the window: head %d + schemas %d + instruction %d tokens + safety %d + output floor %d vs window %d — the fold keeps the stage-1 machine render",
			headTokens, schemaTokens, instrTokens, guardSafetyReserve, minSummaryOutputTokens, window)
	}

	// THE MEGACAVE, SIZED TO THIS ROOM. User rows are verbatim and cannot be
	// shrunk, so they are paid for FIRST and the per-row budget is split over
	// only the rows megacave can compress (assistant + tool). The total ceiling
	// is the room itself, so the body this request ships is bounded by
	// construction.
	roomChars := roomTokens * summaryCharsPerToken
	incompressible := MegacaveIncompressibleChars(foldable) + MegacaveAttachmentPointerChars(foldable)
	show := MegacaveRowBudget(roomChars-incompressible, MegacaveCompressibleRows(foldable))
	megacaved := MachineExcerptMegacaveCapped(foldable, show, roomChars)

	// One user row: the megacaved history, then the instruction that names it.
	// Two rows would put a user turn between the history and the task, and the
	// history is not a turn the model should answer.
	body := megacaved + "\n\n" + instruction
	bodyTokens := len(body) / summaryCharsPerToken
	if window > 0 && headTokens+schemaTokens+bodyTokens+guardSafetyReserve+minSummaryOutputTokens > window {
		// The budget-driven render is bounded, so reaching here means the head
		// grew past its arithmetic (a fat system prompt, a huge schema set). Say
		// so precisely; the fold keeps the stage-1 render.
		return StandardChatRequest{}, fmt.Errorf("stage 2 body does not fit the window: head %d + schemas %d + megacaved body %d tokens + safety %d + output floor %d vs window %d — the fold keeps the stage-1 machine render",
			headTokens, schemaTokens, bodyTokens, guardSafetyReserve, minSummaryOutputTokens, window)
	}

	req := StandardChatRequest{
		Model:       model.Name, // the CHAT model, never the fast shortcut
		Messages:    append(head, StandardMessageReq{Role: "user", Content: body}),
		Stream:      false, // one-shot: the summary is a text answer, not a stream
		Tools:       parts.request.Tools,
		CachePrompt: parts.request.CachePrompt,
		NKeep:       parts.request.NKeep,
	}
	maxTokens := summaryOutputTokens(model.Name, headTokens+schemaTokens+bodyTokens, nil)
	// APPEND-ONLY, ENFORCED BY THE CAP. A fold payload carries a bound on the
	// ANSWER (deltaOutputTokens): the append-only contract says the model writes
	// only what is new, and at ~40 tokens/second a window-sized budget is an open
	// invitation to replay the roll-up the server already stores - 100k tokens of
	// replay is 40 minutes, ~95% of it the server copying its own database to
	// itself. 0 means no fold cap (a normal chat-shaped call).
	if len(payload) > 0 && payload[0].FoldAnswerCap > 0 && payload[0].FoldAnswerCap < maxTokens {
		maxTokens = payload[0].FoldAnswerCap
	}
	req.MaxTokens = &maxTokens
	utils.Log("[EcoSummary] stage 2: %d rows megacaved to %d chars (~%d tokens, per-row show %d, verbatim user rows %d chars) for a %d-token room, head %d + schemas %d, output budget %d, window %d",
		len(foldable), len(megacaved), bodyTokens, show, incompressible, roomTokens, headTokens, schemaTokens, maxTokens, window)
	return req, nil
}

// attemptFoldSummaryFromMegacave is stage 2's entry point: ask the chat model to
// compact the megacaved history further. Any error — no proxy, non-200, empty
// answer, deadline, no room — is returned to the caller, which MUST keep the
// stage-1 machine render. The fold is never lost because stage 2 failed.
func attemptFoldSummaryMegacaveStage(ctx context.Context, conv utils.Conversation, model utils.Model, foldable []utils.Message, stage foldStage, payload ...ChatPayload) (string, error) {
	req, err := buildSummaryRequestMegacaveStage(ctx, conv, model, foldable, stage, payload...)
	if err != nil {
		return "", err
	}
	if req.MaxTokens != nil && *req.MaxTokens <= minSummaryOutputTokens {
		return "", fmt.Errorf("summary budget collapsed (max_tokens=%d for a %d-token window): the prefix already fills it",
			*req.MaxTokens, Models.DeclaredContextWindow(req.Model))
	}
	return GenerateCheckpointSummaryFromRequest(ctx, req)
}

// buildSummaryRequestFromMegacave is the STAGE-2 megacave request (summarise +
// drop completed context), kept under its name so the AST guard's assertions
// and the existing budget/strip tests keep their meaning. Stage 1's fallback
// uses buildSummaryRequestMegacaveStage with foldStageCompress.
func buildSummaryRequestFromMegacave(ctx context.Context, conv utils.Conversation, model utils.Model, foldable []utils.Message, payload ...ChatPayload) (StandardChatRequest, error) {
	return buildSummaryRequestMegacaveStage(ctx, conv, model, foldable, foldStageSummarize, payload...)
}

// attemptFoldSummaryFromMegacave is stage 2's entry point, named for the guard.
func attemptFoldSummaryFromMegacave(ctx context.Context, conv utils.Conversation, model utils.Model, foldable []utils.Message, payload ...ChatPayload) (string, error) {
	return attemptFoldSummaryMegacaveStage(ctx, conv, model, foldable, foldStageSummarize, payload...)
}

// renderedRowsTokens counts the rendered rows of a summary request in the
// chars/3 measure the eco estimator uses.
func renderedRowsTokens(rows []StandardMessageReq) int {
	chars := 0
	for _, r := range rows {
		if s, ok := r.Content.(string); ok {
			chars += len(s)
			continue
		}
		// Multimodal / parts-shaped content: bound it by its marshalled size,
		// the same measure the schemas get.
		chars += len(marshalNoEscape(r.Content))
	}
	for _, r := range rows {
		for _, tc := range r.ToolCalls {
			chars += len(tc.Function.Arguments) + len(tc.Function.Name) + len(tc.ID)
		}
	}
	return chars / summaryCharsPerToken
}

// leadingSystemRows is the count of system rows at the head of a rendered
// request. They are the prompt head: identity, style, the roll-up banner
// vocabulary, the important_memory block. The summariser ships them from the
// FULL replay, byte-for-byte, because only the full slice renders them
// identically (the attachment index is numbered by scan order over the whole
// slice).
func leadingSystemRows(rows []StandardMessageReq) int {
	n := 0
	for n < len(rows) && rows[n].Role == "system" {
		n++
	}
	return n
}

// commonRenderedPrefixRows returns how many leading rows of `a` and `b` are
// byte-identical on the wire. It is the strip point: everything at or before it
// is provably the same bytes the chat request sent, so the KV prefix extends.
func commonRenderedPrefixRows(a, b []StandardMessageReq) int {
	n := 0
	for n < len(a) && n < len(b) {
		if !bytes.Equal(marshalNoEscape(a[n]), marshalNoEscape(b[n])) {
			break
		}
		n++
	}
	return n
}

// summaryOutputTokens sizes the summary's output budget: the model's window
// minus the estimated prefix, clamped to [minSummaryOutputTokens,
// maxSummaryOutputTokens].
//
// The history part of the prefix is the SENT VIEW (viewTokens), NOT the
// marshalled bytes. The summary request replays the conversation's cached
// prefix, so the history it ships is exactly the rows the view counts.
// Marshalled bytes / 3 over-count grossly (re-inflated base64 attachments
// dominate and are not ~3 chars/token at the provider): measured on prod
// conversation c363b2b3 the whole-body measure said 859818 tokens for a
// 262144-token window while the sent view was 41135, which collapsed EVERY
// fold's budget to minSummaryOutputTokens — so every fold went machine-only
// ("summary budget collapsed (max_tokens=1024 for a 262144-token window)") and
// the conversation never got an LLM checkpoint of the rows it was losing.
//
// The tool schemas DO cost window — the chat template renders them into the
// prefix — so they keep their own allowance (schemas only, no attachments).
// The budget-collapsed case stays reachable and is now genuine: it fires only
// when the sent view + schemas really fill the window, in which case the
// summary cannot be written in this shape and the fold falls back to the
// machine-only render, which costs no prefill at all.
func summaryOutputTokens(modelName string, viewTokens int, tools []utils.ToolsRequest) int {
	window := Models.DeclaredContextWindow(modelName)
	schemaTokens := len(marshalNoEscape(tools)) / summaryCharsPerToken
	prefixTokens := viewTokens + schemaTokens
	budget := window - prefixTokens
	if budget > maxSummaryOutputTokens {
		budget = maxSummaryOutputTokens
	}
	if budget < minSummaryOutputTokens {
		// The prefix fills the window: the summary cannot be written in this
		// shape, so the call is not worth making (the caller falls back).
		budget = minSummaryOutputTokens
	}
	return budget
}

// marshalNoEscape marshals with HTML escaping OFF, the encoder SendChatCompletion
// uses: escaped bytes inflate the wire size 3-6x on code-heavy content and would
// mis-size the budget.
func marshalNoEscape(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// RefineRollupSummary is the last durable move against a context overflow, for
// the case where a fold can no longer shrink the view: every foldable row is
// already flagged compacted, so the only text left that can be made smaller is
// the roll-up standing in for them.
//
// It is an LLM pass over the roll-up (see refineRollupInstruction), sent on the
// conversation's own chat model through the same route as its real requests,
// and the answer REPLACES the roll-up IN PLACE (db.RewriteCheckpointSummary):
// same pair, same row ids, same seq layout, only the summary text changes. The
// rows behind it are already marked compacted, so no live turn is touched, and
// nothing is dropped from the request without being recorded.
//
// Returns (refined, err). false with a nil error means "cannot help here":
// no pair, no declared window to size the request against, no room in that
// window for a summariser round-trip, or the model answered empty. The caller
// must then fail the turn honestly — the previous behaviour, an in-memory trim,
// silently shortened the history and is deleted.
func RefineRollupSummary(ctx context.Context, conversationID string, messages []utils.Message, model utils.Model, payload ChatPayload) (bool, error) {
	prior := strings.TrimSpace(PriorSummaryFromView(messages))
	if prior == "" {
		return false, nil
	}
	if model.Name == "" {
		return false, nil
	}
	window := Models.DeclaredContextWindow(model.Name)
	if window <= 0 {
		// No declared window: the summariser's max_tokens cannot be sized, and
		// guessing one is the assumption this path refuses.
		utils.Log("[Eco] roll-up refine refused: model %s declares no context window", model.Name)
		return false, nil
	}

	// Replay the conversation's own head (system prompt + tool schemas), so the
	// prefill the backend already has is reused, then strip the clock: it is the
	// only per-turn mutation of the prefix and the roll-up must not narrate a
	// timestamp.
	// ModelSelected comes from the payload the loop's requests carried, exactly
	// as durableFold builds its summariser conversation: buildChatRequestParts
	// resolves the model through it (including the vision swap), so the replayed
	// head matches the head of the conversation's real requests and the backend
	// prefix hits.
	p := payload
	p.ConversationID = conversationID
	// ModelSelected comes from the payload the loop's requests carried, exactly
	// as durableFold builds its summariser conversation: buildChatRequestParts
	// resolves the model through it (including the vision swap), so the replayed
	// head matches the head of the conversation's real requests and the backend
	// prefix hits.
	conv := utils.Conversation{ID: conversationID, Messages: messages, ModelSelected: p.ModelSelected}
	if uid, ok := ctx.Value("userID").(string); ok {
		conv.UserID = uid
	}
	parts, err := buildChatRequestParts(ctx, model, conv, p)
	if err != nil {
		return false, fmt.Errorf("cannot replay the chat request for the roll-up refine: %w", err)
	}
	head := append([]StandardMessageReq(nil), parts.request.Messages[:leadingSystemRows(parts.request.Messages)]...)
	head = stripCurrentTimeTail(head)
	schemaTokens := len(marshalNoEscape(parts.request.Tools)) / summaryCharsPerToken
	headTokens := renderedRowsTokens(head)

	// The roll-up gets the room the head does not take, and the refine output
	// must fit below it, so the request is bounded by construction.
	// The brief cost is sized with a nominal target: the room does not exist
	// until the brief cost is known, and the brief is a fixed-shape block, so its
	// size barely moves with the number in it.
	instrTokens := len(stage3AddendumInstruction(0)) / summaryCharsPerToken
	room := window - headTokens - schemaTokens - instrTokens - guardSafetyReserve - minSummaryOutputTokens
	if room < minSummaryOutputTokens {
		utils.Log("[Eco] roll-up refine refused: no room in the window (head %d + schemas %d + instruction %d + safety %d + output floor %d vs %d)",
			headTokens, schemaTokens, instrTokens, guardSafetyReserve, minSummaryOutputTokens, window)
		return false, nil
	}
	// Cap the roll-up going in, so a roll-up already over the window cannot make
	// this request itself the one the provider rejects. The megacave chain
	// compressor is the same bounded, idempotent render the fold uses.
	roomChars := room * summaryCharsPerToken
	chain := MegacaveSummaryChain(prior, roomChars)
	if strings.TrimSpace(chain) == "" {
		return false, nil
	}
	// STAGE 3 IS AN EDIT, NOT A TRANSCRIPT. The brief asks for a '-'+'+ addendum
	// (stage3AddendumInstruction) and the server applies it (applyCompactionAddendum),
	// so untouched archive entries cost ZERO output tokens. The brief this replaces
	// demanded "the new archive only": at ~40 tokens/second a 50k-token archive is a
	// 20-minute refine, and ~95% of those bytes are a replay of what the server has.
	body := chain + "\n\n" + stage3AddendumInstruction(room*summaryCharsPerToken/2)
	bodyTokens := len(body) / summaryCharsPerToken
	if headTokens+schemaTokens+bodyTokens+guardSafetyReserve+minSummaryOutputTokens > window {
		return false, nil
	}

	req := StandardChatRequest{
		Model:       model.Name,
		Messages:    append(head, StandardMessageReq{Role: "user", Content: body}),
		Stream:      false,
		Tools:       parts.request.Tools,
		CachePrompt: parts.request.CachePrompt,
		NKeep:       parts.request.NKeep,
	}
	maxTokens := summaryOutputTokens(model.Name, headTokens+schemaTokens+bodyTokens, nil)
	// The answer is an edit list, not an archive: cap it to the archive size, so a
	// replay of the archive cannot be paid for in output tokens at ~40 tokens/second.
	if editCap := len(chain) / summaryCharsPerToken; editCap > 0 && editCap < maxTokens {
		maxTokens = editCap
	}
	req.MaxTokens = &maxTokens

	summariserCtx, cancel := summariserCallContext(ctx)
	defer cancel()
	text, err := GenerateCheckpointSummaryFromRequest(summariserCtx, req)
	if err != nil {
		return false, err
	}
	res, ok := applyCompactionAddendum(chain, text, room*summaryCharsPerToken)
	if !ok {
		// No edits, nothing matched, or the runaway guard refused it. The archive is
		// the ONLY record of rows that no longer exist, so a refusal leaves it
		// untouched - never a partial rewrite, never a silent shrink.
		utils.Log("[Eco] roll-up refine refused the addendum (no usable edits, or the runaway guard): archive left unchanged")
		return false, nil
	}
	refined := strings.TrimSpace(res.Compacted)
	if refined == "" {
		return false, nil
	}
	utils.Log("[Eco] roll-up addendum applied: -%d entries, +%d entries, %d entries kept (%d -> %d chars)",
		res.Removed, res.Added, res.Kept, len(prior), len(refined)) // The refined text is stored, so it must obey the same ceiling the fold's
	// own renderers do: an unbounded roll-up re-inflates the next reload above
	// the eco trigger and rewrites the prompt head every turn.
	bound := durableRollupBound(overflowFoldBudgetTokens(window), SentViewTokens(messages))
	if bound > 0 {
		refined = MegacaveSummaryChain(refined, bound)
	}
	if strings.TrimSpace(refined) == "" || len(refined) >= len(prior) {
		// No shrink: writing it would pay a prompt-head content change for
		// nothing. Leave the archive alone and let the caller fail the turn.
		utils.Log("[Eco] roll-up refine made no progress (%d chars -> %d chars): archive left unchanged", len(prior), len(refined))
		return false, nil
	}
	dbWriteCtx, cancelWrite := durableWriteContext(ctx)
	defer cancelWrite()
	if err := db.RewriteCheckpointSummary(dbWriteCtx, conversationID, refined); err != nil {
		return false, err
	}
	utils.Log("[Eco] roll-up refined durably: %d chars -> %d chars (window %d, output budget %d)", len(prior), len(refined), window, maxTokens)
	return true, nil
}

// llmFoldSummaryRenderer is the SUCCESS path of the fold: the LLM's checkpoint
// text is itself megacaved, so the stored roll-up stays a chain of megacaved
// summaries and stays inside the same ceiling the machine render respects.
//
// The attachment retrieval block is appended HERE, from the fold set the
// storage layer hands in, and NOT taken from the model's prose. The machine
// renderer gets its block from MachineExcerptMegacaveCapped; on this path the
// summariser is asked for a checkpoint of the text, so copying the ids would
// be up to the model, and a checkpoint that drops them silently breaks the
// promise the block makes — "call conversation_attachments with one of these
// ids" is only a promise while the ids are in the text. The ids are row-derived
// and stable (att_<db row id>_<part>), the folded rows are flagged not deleted,
// so the block written at fold time keeps resolving for the life of the
// conversation.
func llmFoldSummaryRenderer(llmText string, ceiling int) func([]utils.Message) string {
	return func(folded []utils.Message) string {
		text := MegacaveSummaryChain(llmText, ceiling)
		if text == "" {
			// Defensive: an empty render must not store an empty section.
			return ""
		}
		block := megacaveAttachmentBlock(folded)
		if block == "" {
			return text
		}
		// Stay inside the ceiling the caller was asked to respect. The block is
		// bounded (<=40 ids) and costed as incompressible by the fold's own
		// budget arithmetic, so this is the same ceiling, not a second one.
		return capMegacaveExcerpt(text+"\n"+block, ceiling)
	}
}

// machineOnlyFoldRenderer is the FALLBACK path of the fold: the deterministic
// megacave render, byte-identical to what this path stored before the LLM was
// reintroduced. Named so the AST guard can pin "the summariser call site's
// failure path calls the machine-only renderer".
func machineOnlyFoldRenderer(show, ceiling int) func([]utils.Message) string {
	return func(m []utils.Message) string {
		return MachineExcerptMegacaveCapped(m, show, ceiling)
	}
}

// GenerateCheckpointSummaryFromRequest posts an already-assembled chat request
// to the model's own chat endpoint and returns choices[0].message.content.
//
// The URL is ChatCompletionURLForName(req.Model) — NOT LiteLLMBaseURL — so the
// summary rides the SAME route the conversation's real requests ride
// (headroom -> caveman-proxy -> LiteLLM). That is what makes cache reuse
// possible: a summary posted to a different endpoint is a different cache.
//
// Fail fast, never hang: LiteLLMReady() gates the call (a proxy that was never
// started returns immediately), and summaryHTTPClient bounds the round-trip for
// a proxy that accepts and stalls. Non-200 is logged and returned as an error —
// the caller falls back to the machine-only fold, so no fold is ever lost to a
// model failure.
//
// It takes the caller's ctx (the shape in the design sketch omits it): the fold
// runs on a user-scoped context that a client disconnect does NOT cancel, while
// a turn the user cancelled must not leave a multi-minute summary running. The
// ctx is also how a caller's deadline shows up as the design's "ctx deadline"
// failure mode, which is one of the reasons the fallback exists.
func GenerateCheckpointSummaryFromRequest(ctx context.Context, req StandardChatRequest) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(req.Model) == "" {
		return "", fmt.Errorf("summary request has no model")
	}
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}
	body := marshalNoEscape(req)
	if len(body) == 0 {
		return "", fmt.Errorf("cannot marshal the summary request")
	}
	url := ChatCompletionURLForName(req.Model) + "/v1/chat/completions"
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	ApplyChatOptimizationHeaders(req.Model, httpReq)

	// Honour the caller's context (a cancelled turn or a shutting-down server
	// must not keep a summary running), and bound the round-trip by the
	// summariser's own ceiling, whichever comes first.
	callCtx, cancelCall := context.WithTimeout(ctx, summaryHTTPTimeout)
	defer cancelCall()
	resp, err := summaryHTTPClient.Do(httpReq.WithContext(callCtx))
	if err != nil {
		utils.Error("[EcoSummary] summary request failed", err)
		return "", fmt.Errorf("summary request failed: %w", err)
	}
	defer resp.Body.Close()
	out, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("summary response unreadable: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		utils.Error("[EcoSummary] summary request rejected", nil, strconv.Itoa(resp.StatusCode), headOf(string(out), 400))
		return "", fmt.Errorf("summary request failed with status %d", resp.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("failed to parse summary response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in summary response")
	}
	text := strings.TrimSpace(result.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("empty summary content")
	}
	return text, nil
}

// GenerateCheckpointSummary keeps its historical signature for any legacy
// caller: a two-message summarisation prompt on `model`, max_tokens 8192,
// LiteLLM directly. The fold path does NOT use it — the fold builds a replayed
// request (buildSummaryRequestFromView) and posts it to the chat model's own
// route via GenerateCheckpointSummaryFromRequest, which is what reuses the
// KV-cached prefix.
func GenerateCheckpointSummary(text, model string) (string, error) {
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}
	maxTokens := 8192
	return GenerateCheckpointSummaryFromRequest(context.Background(), StandardChatRequest{
		Model: model,
		Messages: []StandardMessageReq{
			{
				Role: "system",
				Content: "You are summarising older conversation history that the live LLM will no longer see directly. " +
					"Your summary REPLACES those messages in the live context, so any detail you drop is lost to the model. " +
					"Rules:\n" +
					"1. Preserve verbatim: code blocks, file paths, identifiers, IDs, error messages, command outputs, URLs, numeric values, exact quoted user requests.\n" +
					"2. Preserve the FULL text content of any inline pasted snippet or text attachment shown to you under '[attachment: ...]' headers — copy them into a clearly-labeled section, do not paraphrase code or text the user pasted.\n" +
					"3. Preserve tool_call -> TOOL_RESULT outcomes: which tool was called, the key arguments, and the substantive result (paths created, errors returned, data retrieved). Drop only obviously verbose framing.\n" +
					"4. For image / document / binary attachments noted as '[image attachment omitted]' or '[document attachment: ...]', just record that the attachment existed (filename and any context the user gave).\n" +
					"5. Preserve user goals, decisions, plans, and rejected options chronologically so the live LLM understands where the conversation stands.\n" +
					"6. Be dense and structured (sections / bullets), but do NOT be brief at the cost of dropping concrete content. Length should match the input's information density.\n" +
					"Respond with the summary text only — no preamble, no closing remarks.",
			},
			{Role: "user", Content: text},
		},
		MaxTokens: &maxTokens,
		Stream:    false,
	})
}

// headOf truncates for a log line.
// itoaRows keeps the instruction free of strconv noise.
func itoaRows(n int) string { return strconv.Itoa(n) }

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
