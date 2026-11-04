package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/utils"
)

// summaryInFlight is the global single-flight gate for eco-mode summary
// jobs. Only one summary runs across the whole server at any time;
// conversations that miss their window pick up on the next user turn.
var summaryInFlight atomic.Bool

// defaultEcoTriggerTokens is the default absolute eco-trigger threshold (tokens)
// when config.eco.trigger_tokens is unset. The auth package keeps its own
// private copy; we mirror it here because it is unexported there.
const defaultEcoTriggerTokens = 100_000

// effectiveEcoTrigger computes the compaction trigger for a conversation,
// honouring the model's declared context window when available.
//
//   - If the model declares a context window (MaxContextTokensFor > 0) and
//     Eco.ContextFraction > 0, the candidate trigger is
//     round(ContextFraction * window) tokens. The effective trigger is then
//     min(config trigger, candidate): the static trigger stays an absolute
//     ceiling, but a small-window model now compacts much earlier instead of
//     waiting forever for a 100k floor it can never reach.
//   - If the window is unknown (0) or the fraction is disabled (<= 0), the
//     configured static trigger is used unchanged.
//
// Returns the static trigger when nothing dynamic applies, so callers keep
// their existing "cfg.TriggerTokens <= 0" gate working.
func effectiveEcoTrigger(modelName string) int {
	cfg := auth.GetConfig().Eco
	// Eco triggers when the conversation content exceeds the configured
	// absolute threshold (default 100K, configurable via config.eco.trigger_tokens)
	// OR exceeds the model's context window — whichever is SMALLER, so a
	// small-window model compacts at its window and a large-window model at
	// the configured threshold. (The old 0.8*context_fraction factor is gone:
	// it caused premature compaction and, combined with per-turn re-trimming,
	// broke prompt caching.)
	static := cfg.TriggerTokens
	if static <= 0 {
		static = defaultEcoTriggerTokens
	}
	window := Models.MaxContextTokensFor(modelName)
	if window <= 0 {
		return static
	}
	if window < static {
		return window
	}
	return static
}

// userContextFor builds a background context carrying just the userID
// claim that the db package looks for. Used by the eco summary goroutine
// so a client disconnect or LLM-loop end doesn't cancel it mid-summary.
func userContextFor(userID string) context.Context {
	return context.WithValue(context.Background(), "userID", userID)
}

// filterCheckpointsForRequest applies the eco-mode visibility rules to the
// message slice that's about to be sent to the LLM.
//
//   - ecoOn=false: drop every checkpoint pair entirely; the LLM sees the
//     full pre-summary history. Use when the user has explicitly disabled
//     eco for this conversation.
//   - ecoOn=true: keep the checkpoint pair and drop every message before
//     it. The LLM sees a system prompt, the checkpoint summary, and the
//     live tail. There is at most one checkpoint per conversation.
//
// The system prompt is prepended elsewhere (in SendChatCompletion) and is
// not part of `messages` here.
func filterCheckpointsForRequest(messages []utils.Message, ecoOn bool, contextWindow int) []utils.Message {
	return filterCheckpointsForRequestBudget(messages, ecoOn, contextWindow, -1)
}

// filterCheckpointsForRequestBudget is filterCheckpointsForRequest with an
// explicit per-request tail budget override. When `tailBudgetChars` is >= 0
// it is used INSTEAD of the derived maxTailChars(contextWindow) — the caller
// (SendChatCompletion) computes it from the ACTUAL tool schema + system
// prompt overhead, so a small-window model (65k) with a huge toolset
// (github-api + playwright + wordpress + searxng + bundled ≈ 90 schemas)
// never ships a tail that crowds the tools out of the window (observed:
// prompt 65933 tokens vs 65536 → litellm 500 → dead conversation).
func filterCheckpointsForRequestBudget(messages []utils.Message, ecoOn bool, contextWindow, tailBudgetChars int) []utils.Message {
	if tailBudgetChars < 0 {
		tailBudgetChars = maxTailChars(contextWindow)
	}
	if !ecoOn {
		// Even with eco's "live-summarise" preference OFF, we must still send a
		// BOUNDED context: the compacted older history should be represented by
		// its megacave machine excerpt + a recent tail — NOT the full raw
		// history. Sending everything (previous behaviour: FilterOutCheckpoints
		// stripped the checkpoint AND returned the whole history, and the old
		// `if toolsTaxBytes > 0` gate then re-shipped it uncapped) is exactly
		// what filled the window and produced the "context window is full"
		// error with eco disabled.
		//
		// If a megacave checkpoint exists, collapse to (checkpoint summary +
		// bounded live tail) so the request stays small. If none exists, fall
		// through to the normal budget-capped tail (never ship a huge raw
		// history).
		lastCPIdx := -1
		for i, m := range messages {
			if m.Role == "assistant" && db.IsCheckpointMessage(m) {
				lastCPIdx = i
			}
		}
		if lastCPIdx >= 0 {
			// Keep the checkpoint pair + a bounded tail after it. The
			// checkpoint summary stands in for the folded older history; the
			// budget caps the live tail.
			return capTailWithBudget(messages[lastCPIdx:], contextWindow, tailBudgetChars)
		}
		// No checkpoint: bound the whole raw history.
		return capTailWithBudget(messages, contextWindow, tailBudgetChars)
	}

	// Find the LAST checkpoint pair (assistant half) — the rolling design
	// keeps only one, but defensive against any stale extras.
	lastCheckpointIdx := -1
	for i, m := range messages {
		if m.Role == "assistant" && db.IsCheckpointMessage(m) {
			lastCheckpointIdx = i
		}
	}
	// No checkpoint yet (eco never successfully compacted this conversation):
	// a giant raw history must NOT be shipped whole — that is exactly how
	// "WordPress Security Review" (1866 msgs, ~770k tokens) kept dying with
	// context_length_exceeded. Fall through to the same budget-capped tail
	// so even a never-compacted conversation is bounded on every request.
	if lastCheckpointIdx < 0 {
		return capTailWithBudget(messages, contextWindow, tailBudgetChars)
	}
	tail := messages[lastCheckpointIdx:]

	// The checkpoint summary must ALWAYS fit inside the model's context
	// window, no matter how huge the compacted history was. Every tool
	// result is shipped to the LLM inside the summary's tool-role message,
	// and an oversized summary is the "eco compaction makes conversation
	// lose ALL context" bug: the request dies with context_length_exceeded,
	// the LLM loop then compact-durably folds the newest turns into the
	// summary, and a restarted session reloads only the checkpoint — the
	// full pre-summary history is on the other side of the boundary and
	// never re-enters the context. Cap the summary to a budget that always
	// leaves room for the tail, the system prompt, the tool schemas and the
	// model's reply (this pair's tool message is where an oversized summary
	// used to evict the entire live tail, leaving a context that was nothing
	// but summary bloat and a single in-flight user turn).
	tail = capCheckpointSummaryForWindow(tail, contextWindow, tailBudgetChars)

	// Safety net: even with eco on, the live tail after the last checkpoint
	// can grow unbounded. Never ship a context-busting tail to the provider.
	// Message-count caps are not enough: observed a 600-message tail that was
	// ~400k tokens because every tool result is huge (this conversation: avg
	// 2.5k chars/tool msg). Cap by BUDGET (sum of content length) instead,
	// always starting on a user message so the model still has a complete turn
	// boundary to respond to. The checkpoint summary before the cap keeps the
	// dropped middle coherent.
	return capTailWithBudget(tail, contextWindow, tailBudgetChars)
}

// capCheckpointSummaryForWindow bounds the checkpoint summary carried in
// the tool-role message of an eco checkpoint pair so it can never blow the
// model's context window BY ITSELF. The summary is the result of a
// summarisation call that folded a huge raw history (potentially hundreds
// of thousands of tokens) into one message; when it vastly exceeds the
// window, the very next request dies with context_length_exceeded, the
// LLM-loop durable compaction then folds the LIVE tail into a new summary,
// and a restarted session reloads only the new checkpoint — the summary
// is the ONLY surviving context. That is the "eco compaction makes
// conversation lose ALL context" bug (observed: e1c3b48c63eb077f0985b83a
// and a 100-assistant/tool-message wall after the checkpoint).
//
// The cap is deterministic and conservative: it keeps the number of SUMMARY
// CHARACTERS ≤ the same budget the live tail gets (maxTailChars(window) or
// the caller's overhead-aware tail budget), so the summary can never crowd
// the tail out of the request. Anything beyond the cap is truncated with a
// clear banner that tells the model to consult the compacted history (which
// the non-destructive compaction keeps in the DB) before relying on omitted
// details.
func capCheckpointSummaryForWindow(tail []utils.Message, contextWindow, tailBudgetChars int) []utils.Message {
	if len(tail) == 0 {
		return tail
	}
	budget := tailBudgetChars
	if budget < 0 {
		budget = maxTailChars(contextWindow)
	}
	for i := range tail {
		m := &tail[i]
		if m.Role != "tool" || m.Name != db.EcoCheckpointToolName {
			continue
		}
		total := m.Content.TotalContentChars()
		if total <= budget {
			continue
		}
		oldText := m.TextContent()
		keep := budget
		if keep > len(oldText) {
			keep = len(oldText)
		}
		utils.Log("[Eco] checkpoint summary oversized (%d chars > budget %d) — truncating to keep the tail inside the context window", total, budget)
		banner := fmt.Sprintf("\n\n[checkpoint summary truncated by Plurality: original length %d chars exceeded the per-request context budget; full history is preserved in the conversation for the client view]", total)
		m.Content = utils.NewTextContent(oldText[:keep] + banner)
	}
	return tail
}

// maxTailChars returns the character budget for the live tail sent to the
// provider for a given model context window. Budgeting ~1/4 of the window in
// characters ≈ ~1/4 of the window in tokens leaves generous headroom for the
// system prompt, the checkpoint summary, tool schemas and the model's reply.
// This is per-model: a small-window model (65k) must NOT ship the
// same ~100k-char tail that the 128k+ models tolerate — that is exactly what
// made long small-window conversations die with context_length_exceeded
// (observed: prompt 73077 tokens vs 65536 window → litellm 500 → dead
// conversation).

// ---------------------------------------------------------------------------
// Staged eco resolution (replaces the old per-turn trim/shrink machinery)
// ---------------------------------------------------------------------------
//
// When the conversation exceeds the eco trigger (content > window OR content >
// the configured 100K threshold), we resolve WHAT to send to the model through
// deterministic, cache-stable, no-LLM stages. Unlike the removed
// per-request re-trimming (which re-shaped the messages every tool-loop
// iteration and broke prompt caching), each stage is a single, stable
// historical collapse: the SAME conversation produces the SAME sent context
// on every turn until it changes, so the backend KV cache keeps hitting.
//
// The result includes megacave machine excerpts (MachineExcerptMegacave) that
// stand in for folded history plus a verbatim segment. Stages, in order of
// preference (least to most destructive):
//
//   A  latest full turn verbatim + megacave(everything older). The most
//      faithful: the model still sees its most recent complete turn, and the
//      whole earlier history is preserved as a deterministic excerpt.
//   B  if A still doesn't fit: megacave(everything older), keeping only the
//      most recent USER message verbatim. No separate tail anymore.
//   C  if B still doesn't fit: keep only USER messages + the LAST assistant
//      message that sits before any user message; megacave that reduced set.
//   D  if C still doesn't fit: drop megacaved turns one-by-one from the START
//      until the remainder fits. Final fallback.

// approxTokens estimates token count from total chars (~3 chars/token for the
// dense tool/code content that dominates these conversations).
func approxTokens(messages []utils.Message) int {
	chars := 0
	for i := range messages {
		chars += messages[i].Content.TotalContentChars()
	}
	return chars / 3
}

// megacaveArtifact returns a [assistant, tool] checkpoint pair carrying the
// megacave machine excerpt of the given messages, so the excerpt is rendered
// as a normal message the model can read.
func megacaveArtifact(folded []utils.Message, toolCallID string) []utils.Message {
	excerpt := MachineExcerptMegacave(folded)
	if excerpt == "" {
		return nil
	}
	aiMsg := utils.Message{
		Role: "assistant",
		ToolCalls: []utils.ToolCall{{
			ID:   toolCallID,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      "megacave_excerpt",
				Arguments: "{}",
			},
		}},
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(excerpt),
		ToolCallID: toolCallID,
		Name:       "megacave_excerpt",
	}
	return []utils.Message{aiMsg, toolMsg}
}

// indexOfLastUser returns the index of the last message with Role=="user",
// or -1.
func indexOfLastUser(messages []utils.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	return -1
}

// startOfLastTurn returns the index of the first message of the last complete
// user turn (the last "user" message), or -1.
func startOfLastTurn(messages []utils.Message) int {
	return indexOfLastUser(messages)
}

// endOfLastTurn returns the index AFTER the last user turn's trailing tool
// results (== len(messages) when the last turn extends to the end).
func endOfLastTurn(messages []utils.Message, start int) int {
	if start < 0 {
		return len(messages)
	}
	return len(messages)
}

// userOnlyWithLastAssistant reduces to USER messages plus the final assistant
// message that appears before any user message (stage C input).
func userOnlyWithLastAssistant(messages []utils.Message) []utils.Message {
	var out []utils.Message
	lastAsstBeforeUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			break
		}
		if messages[i].Role == "assistant" {
			lastAsstBeforeUser = i
		}
	}
	for i := range messages {
		if messages[i].Role == "user" {
			out = append(out, messages[i])
		}
	}
	if lastAsstBeforeUser >= 0 {
		// Insert the last assistant message before any user (immediately after
		// the latest user, so the model has recent model context too).
		out = append(out, messages[lastAsstBeforeUser])
	}
	return out
}

// resolveEcoContextCompaction computes the message view to SEND when the
// conversation exceeds the eco trigger, following stages A -> B -> C -> D.
// `all` is the full conversation (system already prepended is NOT included).
func resolveEcoContextCompaction(all []utils.Message, windowTokens int, budgetTokens int) []utils.Message {
	const toolOverheadTokens = 40 // small noise margin for the injected excerpt pair

	fits := func(msgs []utils.Message) bool {
		return approxTokens(msgs)+toolOverheadTokens <= budgetTokens
	}

	// We build the candidate from user+assistant/tool messages (system is
	// added by the caller after this).
	turnStart := startOfLastTurn(all)
	var candidate []utils.Message
	id := 0

	// STAGE A
	{
		var cand []utils.Message
		if turnStart > 0 {
			older := all[:turnStart]
			if art := megacaveArtifact(older, "megacave_A"); art != nil {
				cand = append(cand, art...)
			}
		}
		if turnStart >= 0 {
			cand = append(cand, all[turnStart:]...)
		}
		candidate = cand
		id = 1
	}
	if !fits(candidate) {
		// STAGE B — no tail; keep only the most recent user message.
		lastUser := indexOfLastUser(all)
		var cand []utils.Message
		if lastUser > 0 {
			older := all[:lastUser]
			if art := megacaveArtifact(older, "megacave_B"); art != nil {
				cand = append(cand, art...)
			}
		}
		if lastUser >= 0 {
			cand = append(cand, all[lastUser])
		}
		candidate = cand
		id = 2
	}
	if !fits(candidate) {
		// STAGE C — user messages + last assistant before a user, megacaved.
		reduced := userOnlyWithLastAssistant(all)
		var cand []utils.Message
		if art := megacaveArtifact(reduced, "megacave_C"); art != nil {
			cand = append(cand, art...)
		}
		candidate = cand
		id = 3
	}
	if !fits(candidate) {
		// STAGE D — drop megacaved excerpt turns one-by-one from the start
		// until it fits. Iteratively re-megacave the remaining set.
		candidate = nil
		id = 4
		// Greedy: repeatedly drop the oldest user turn (then assistant/tool)
		// and re-megacave until it fits or only a few messages remain.
		work := all
		for len(work) > 0 && !fits(megacaveOnly(work)) {
			// drop the oldest user turn
			work = dropOldestTurn(work)
		}
		if art := megacaveArtifact(work, "megacave_D"); art != nil {
			candidate = append(candidate, art...)
		}
	}

	utils.Log("[Eco] resolved staged compaction (stage %d ids, %d messages, ≈%d tokens / budget %d)",
		id, len(candidate), approxTokens(candidate), budgetTokens)
	return candidate
}

// megacaveOnly returns megacaveArtifact(msgs) flattened, or nil.
func megacaveOnly(msgs []utils.Message) []utils.Message {
	art := megacaveArtifact(msgs, "megacave_only")
	if art == nil {
		return nil
	}
	return art
}

// dropOldestTurn removes the oldest user turn (user message plus any
// assistant/tool messages belonging to it) and returns the remaining slice.
func dropOldestTurn(msgs []utils.Message) []utils.Message {
	firstUser := -1
	for i := range msgs {
		if msgs[i].Role == "user" {
			firstUser = i
			break
		}
	}
	if firstUser < 0 {
		return msgs[:0]
	}
	// drop from start through the assistant message that closes this turn (the
	// next user message, or end). Keep only messages after that turn + any
	// leading non-user (e.g. an initial system/assistant) is kept too.
	nextUser := len(msgs)
	for i := firstUser + 1; i < len(msgs); i++ {
		if msgs[i].Role == "user" {
			nextUser = i
			break
		}
	}
	out := make([]utils.Message, 0, len(msgs)-nextUser+firstUser)
	out = append(out, msgs[:firstUser]...)
	out = append(out, msgs[nextUser:]...)
	return out
}

func maxTailChars(contextWindow int) int {
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	// ~4 chars per token. The old half-window heuristic assumed a bare
	// conversation and ignored the tool-schema tax: a wide MCP toolset
	// (github-api + playwright + wordpress + searxng + bundled ≈ 90 schemas)
	// adds ~25-30k tokens to every request, and dense code/tool results run
	// ~2.5-3.5 chars/token (not 4). On a 65k-window local model that blew the
	// window (observed: prompt 65933 tokens vs 65536 → litellm 500 → dead
	// conversation). Small windows now get a QUARTER of the window, leaving
	// the other three quarters for tools, system prompt, summary, reply.
	if contextWindow <= 65_536 {
		budget := contextWindow / 4
		if budget < 8_192 {
			budget = 8_192
		}
		return budget
	}
	budget := contextWindow / 3
	if budget < 16_384 {
		budget = 16_384
	}
	if budget > 100_000 {
		budget = 100_000
	}
	return budget
}

// maxTailCharsForOverhead returns the live-tail character budget given the
// model window AND the measured token overhead of everything else in the
// request (tool schemas + system prompt + checkpoint summary). This is the
// caller-computed variant: SendChatCompletion marshals the actual tools
// array and system message, estimates their token cost, and reserves them,
// so the tail cannot crowd the tools out of a small window.
//
// Density: tool results and shell/file dumps are far denser than prose
// (~2.5-3.5 chars/token vs ~4). Use 3.0 chars/token for the remaining budget
// so the tail's true token cost stays inside what we reserved. A reply
// reservation (max_tokens + safety margin) is also kept free.
func maxTailCharsForOverhead(contextWindow, overheadTokens int) int {
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	if overheadTokens < 0 {
		overheadTokens = 0
	}
	// Reserve room for the model's reply: the configured max_tokens (4096)
	// plus a safety margin for reasoning/finish overhead.
	const replyReserve = 4096 + 2048
	available := contextWindow - overheadTokens - replyReserve
	if available <= 0 {
		available = 8_192 // tiny but non-zero floor so we never ship an empty tail
	}
	// 3 chars/token for dense tool/code content.
	budget := available * 3
	if budget > 100_000 {
		budget = 100_000
	}
	return budget
}

// capTail bounds a message slice to the live-tail budget so a conversation
// can never ship a context-busting history to the provider. It walks
// backwards from the end, accumulating the total content size, and keeps
// the newest messages that fit the budget, always starting on a user
// message (so the model still has a complete turn boundary to respond to
// and tool results whose parent call is dropped are never orphaned).
// Individual oversized messages are then truncated/dropped by
// truncateOversizedTail.
//
// This is applied BOTH when a checkpoint exists (live tail after it) and
// when there is NO checkpoint yet — a never-compacted conversation with a
// huge raw history must still be bounded on every request, or every turn
// dies with context_length_exceeded (observed: "WordPress Security
// Review", 1866 msgs / ~770k tokens / no checkpoint, eco on).
func capTail(tail []utils.Message, contextWindow int) []utils.Message {
	return capTailBudget(tail, maxTailChars(contextWindow), contextWindow)
}

// capTailWithBudget is capTail with an explicit caller-supplied tail budget
// (CHARACTERS) instead of the static window-derived maxTailChars. This is
// the overhead-aware path: SendChatCompletion measures the ACTUAL token cost
// of the tool schemas + system prompt + checkpoint summary and computes the
// remaining room for the tail. Routing that budget through here keeps the
// whole eco pipeline honest — before this fix the caller-computed budget
// was silently dropped and the STATIC (too generous) maxTailChars was used,
// so a small-window model with a wide MCP toolset could still ship a tail
// that overflowed the window and litellm rejected the request (observed:
// prompt 65933 tokens vs 65536 window → 500 → dead conversation).
func capTailWithBudget(tail []utils.Message, contextWindow, tailBudgetChars int) []utils.Message {
	return capTailBudget(tail, tailBudgetChars, contextWindow)
}

// capTailBudget bounds a message slice to an explicit character budget,
// walking backwards from the end. It is the workhorse behind capTail and
// the overhead-aware budget path.
func capTailBudget(tail []utils.Message, tailBudgetChars, contextWindow int) []utils.Message {
	if len(tail) == 0 {
		return tail
	}
	if tailBudgetChars <= 0 {
		tailBudgetChars = maxTailChars(contextWindow)
	}
	count := 0
	chars := 0
	var start int
	start = len(tail) - 1
	for start >= 0 {
		chars += tail[start].Content.TotalContentChars()
		count++
		if chars > tailBudgetChars {
			start++
			break
		}
		if start == 0 {
			break
		}
		start--
	}
	if start < 0 {
		start = 0
	}
	// The budget walker can break with start == len(tail) (it incremented
	// past the last in-range index to mark "cap from here"). That is a
	// VALID slice start — tail[len:] is the empty slice — but it is NOT a
	// valid index to dereference. Clamp it down to the last element first
	// so the boundary back-up loop below can never index out of range.
	// (Observed panic: "index out of range [N] with length N" on this
	// conversation's ~3500-message tail.)
	if start > len(tail)-1 {
		start = len(tail) - 1
	}
	// Always back up to a user boundary so we never start mid-turn (which
	// would orphan tool results whose parent is in the dropped section).
	for start > 0 && tail[start].Role != "user" {
		start--
	}
	// The budget must also bound INDIVIDUAL oversized messages. A single
	// huge message (a multi-MB pasted blob, a giant tool result, an inline
	// attachment dump) cannot be removed by the tail cap above — the walker
	// breaks at it and then re-includes it via the user-boundary back-up —
	// so a conversation that contains one keeps shipping it on every turn
	// and eventually dies with context_length_exceeded (observed: the
	// "WordPress Security Review" conversation, 1858 msgs, with a ~4.2MB
	// Cosmos config dump inline; every "continue" resumed into a provider
	// context error and the workflow dead-ended mid-work). Apply per-message
	// bounds to whatever tail is actually kept below.
	if start > 0 {
		utils.Log("[Eco] capping tail by budget: kept %d msgs / %d chars (start idx %d) for context safety", len(tail)-start, chars, start)
		tail = tail[start:]
	}
	// HARD budget cap. The user-boundary back-up above can overshoot the
	// caller budget by an entire turn's worth of tool results — observed on
	// a 131k-window model with a 100000-char budget: "kept 320 msgs /
	// 215888 chars" on conversation 319ce9283cac61666757b88a, so the
	// preemptive guard re-fired on every single turn. The back-up is
	// REQUIRED (never start mid-turn / orphan tool results), but after it
	// we keep dropping EARLIER turns (oldest-first, always at a user
	// boundary) until the total content fits the budget. Never truncate a
	// message body here — a single oversized turn (a giant tool result with
	// its still-present parent call) must survive whole so
	// truncateOversizedTail can apply its per-message bound; and tool-call
	// pairing must never be broken by cutting mid-turn (the OpenAI API
	// rejects dangling tool_calls/result pairs). Content-shrinking rows
	// instead of dropping whole turns was the earlier attempt: it broke
	// those contracts and is why per-message truncation exists below.
	tail = trimTailToFitBudget(tail, tailBudgetChars)
	return truncateOversizedTail(tail, contextWindow)
}

// trimTailToFitBudget hard-caps the total content length of a tail slice to
// tailBudgetChars by dropping the OLDEST complete turns (at user message
// boundaries) until the remainder fits. The newest user message and its
// in-flight turn are always preserved — the natural floor — so the model
// still has a complete, recent request to answer. Rows are dropped whole,
// never partially: a partial turn would orphan tool results whose parent
// assistant call was cut, which makes the OpenAI tool_calls/result pairing
// invalid. Individual oversized messages that survive (e.g. a giant tool
// result whose parent call is in the same turn, or a huge newest user
// message) are handled later by truncateOversizedTail with its per-message
// bound.
func trimTailToFitBudget(tail []utils.Message, tailBudgetChars int) []utils.Message {
	if len(tail) == 0 || tailBudgetChars <= 0 {
		return tail
	}
	// One prefix-sum pass makes every "does the kept suffix fit / how much
	// does the oldest turn cost" decision O(1) in the drop loop below.
	prefix := make([]int, len(tail)+1)
	for i := range tail {
		prefix[i+1] = prefix[i] + tail[i].Content.TotalContentChars()
	}
	if prefix[len(tail)] <= tailBudgetChars {
		return tail
	}

	// Floor: the last user message and everything after it (its in-flight
	// turn) are always kept — never start the kept history mid-turn. If the
	// newest turn alone exceeds the budget, that is the hard stop; nothing
	// more can be dropped without splitting a turn, and per-message
	// truncation below still bounds it.
	floor := len(tail) - 1
	for floor > 0 && tail[floor].Role != "user" {
		floor--
	}

	// Drop the oldest complete turns one at a time until the kept suffix
	// fits the budget or the floor is reached. This is the HARD cap the
	// coarse backward walk in capTailBudget cannot provide: its user-boundary
	// back-up re-includes an entire turn, which routinely overshoots the
	// caller budget (observed: "kept 320 msgs / 215888 chars" against a
	// 100000-char budget on conversation 319ce9283cac61666757b88a, so the
	// preemptive guard re-fired on every single turn).
	keptStart := 0
	// Continuity floor: never hand the model less than half the budget when
	// whole-turn dropping is the only lever left. Turn boundaries in an agent
	// conversation are COARSE — a single user turn spans hundreds of tool
	// calls — so the loop below can overshoot catastrophically (observed on
	// the "context lost" conversation 9aa5f25c582ecd78f7991138: "dropping 21
	// oldest msgs / 103822 chars (kept 1 msgs / 1 chars)" — a 2.7% overshoot
	// deleted 99.99% of the context and the model answered a bare "."). When
	// the next whole-turn drop would starve the window, stop dropping turns
	// and shrink the oversized ones inside the window instead (Phase B).
	floorChars := tailBudgetChars / 2
	for {
		keptChars := prefix[len(tail)] - prefix[keptStart]
		if keptChars <= tailBudgetChars {
			break
		}
		// Advance to the next user boundary — the start of the oldest kept
		// turn. Dropping up to it removes a WHOLE turn, never a partial one.
		next := keptStart + 1
		for next < len(tail) && tail[next].Role != "user" {
			next++
		}
		if next > floor {
			// Only the newest user turn remains and it alone exceeds the
			// budget: stop here (floor), keep it whole for the model to
			// answer; the per-message shrink below still bounds it.
			keptStart = floor
			break
		}
		if projected := prefix[len(tail)] - prefix[next]; projected < floorChars {
			break
		}
		keptStart = next
	}
	dropped := prefix[keptStart]
	if keptStart > 0 {
		utils.Log("[Eco] budget re-trim: dropping %d oldest msgs / %d chars (kept %d msgs / %d chars)", keptStart, dropped, len(tail)-keptStart, prefix[len(tail)]-dropped)
		tail = tail[keptStart:]
	}

	// Phase B — shrink-to-fit. The kept window is turn-aligned, but coarse
	// turns mean it can still overshoot the budget, and the ONLY remaining
	// lever is a whole-turn drop that would gut the context. Shrink the
	// largest messages (biggest first) until the window fits, so the model
	// keeps the BREADTH of the history — every surviving turn stays visible
	// with its head and tail — instead of a single recent turn.
	//
	// Messages are SHRUNK, never dropped: removing a row can orphan a tool
	// result whose parent assistant tool_call survives, which makes the
	// OpenAI tool_calls/result pairing invalid (that is why the row-drop path
	// above stays turn-aligned). Shrinking bodies preserves every pairing.
	//
	// The newest user turn is off-limits: it is the request the model must
	// answer, so shrink-to-fit stops there. If that turn alone exceeds the
	// budget we accept the overshoot and let truncateOversizedTail bound its
	// individual rows. A tail with NO user message at all (a pure
	// assistant/tool wall behind a checkpoint) protects nothing — every row
	// there is older tool output and all of it is eligible for shrinking.
	protectedFrom := len(tail)
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i].Role == "user" {
			protectedFrom = i
			break
		}
	}
	tail = shrinkTailToFitBudget(tail, tailBudgetChars, protectedFrom)
	return tail
}

// shrinkTailToFitBudget trims a turn-aligned tail to tailBudgetChars by
// cutting the LARGEST messages first (keeping each message's head and tail
// around an elision banner) until the window fits. Rows are never removed,
// so assistant tool_calls ↔ tool result pairing survives. Messages at or
// after protectedFrom (the newest user turn — the live request the model
// must answer) are left untouched.
//
// Greedy largest-first is deliberate: it spreads the elision across the
// fewest messages, so the maximum number of turns stay visible, and it is
// deterministic (no randomness, no ordering dependence on map iteration).
func shrinkTailToFitBudget(tail []utils.Message, tailBudgetChars, protectedFrom int) []utils.Message {
	if len(tail) == 0 || tailBudgetChars <= 0 {
		return tail
	}
	sizes := make([]int, len(tail))
	total := 0
	for i := range tail {
		sizes[i] = tail[i].Content.TotalContentChars()
		total += sizes[i]
	}
	if total <= tailBudgetChars {
		return tail
	}
	// Work on a COPY: `tail` reaches us as a sub-slice (tail[start:]) of the
	// caller's message slice, so assigning tail[idx] in place would rewrite
	// the elements of the caller's backing array — permanently shrinking the
	// in-memory conversation after a single request. Every message in the
	// package's compaction path is treated as immutable-by-contract.
	out := make([]utils.Message, len(tail))
	copy(out, tail)
	tail = out

	// Order indices by size, largest first (stable: ties keep chronological
	// order so the OLDEST of equally-sized messages shrinks first, protecting
	// the most recent turns).
	order := make([]int, len(tail))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a]] > sizes[order[b]] })

	shrunk := 0
	shrunkMsgs := 0
	for _, idx := range order {
		excess := total - tailBudgetChars
		if excess <= 0 {
			break
		}
		if idx >= protectedFrom {
			continue // the live user turn is never shrunk
		}
		if sizes[idx] <= minShrinkableMsgChars {
			continue // already at the floor; cutting more destroys meaning
		}
		// Never shrink a message below the floor: many small messages carry
		// more context than one giant one.
		cut := excess
		if max := sizes[idx] - minShrinkableMsgChars; cut > max {
			cut = max
		}
		if cut <= 0 {
			continue
		}
		newMsg, removed := shrinkMessageContent(tail[idx], sizes[idx]-cut)
		if removed <= 0 {
			continue
		}
		tail[idx] = newMsg
		sizes[idx] -= removed
		total -= removed
		shrunk += removed
		shrunkMsgs++
	}
	if shrunkMsgs > 0 {
		utils.Log("[Eco] shrink-to-fit: elided %d chars across %d oversized msgs (kept %d msgs / %d chars, budget %d)",
			shrunk, shrunkMsgs, len(tail), total, tailBudgetChars)
	}
	return tail
}

// minShrinkableMsgChars is the per-message floor for shrink-to-fit: a message
// is never squeezed below this many characters of text, so shrinking spreads
// across the big dumps instead of annihilating small turns.
const minShrinkableMsgChars = 2_000

// shrinkMessageContent rebuilds a message so its total content fits
// keepChars, preserving every non-text part (images, documents, snippets)
// and keeping the HEAD and TAIL of the text around an elision banner — for
// tool output, the head (command) and tail (result/errors) are the parts
// that carry meaning. Returns the new message and how many characters were
// elided. Text parts are merged into one (non-text parts keep their order),
// which is safe for the provider request path.
func shrinkMessageContent(m utils.Message, keepChars int) (utils.Message, int) {
	parts := m.Content.ContentParts()
	var text strings.Builder
	textOnly := make([]utils.ContentPart, 0, len(parts))
	nonText := make([]utils.ContentPart, 0, 4)
	for _, p := range parts {
		if (p.Type == "text" || p.Type == "snippet") && p.Filename == "" {
			text.WriteString(p.Text)
			continue
		}
		if p.Type == "text" || p.Type == "snippet" {
			// Attachment-labelled text part: keep as-is, it is short.
			textOnly = append(textOnly, p)
			continue
		}
		nonText = append(nonText, p)
	}
	full := text.String()
	if len(full) <= keepChars {
		return m, 0
	}
	banner := fmt.Sprintf("\n\n[...middle elided by Plurality context-safety: original length %d chars exceeded the per-request context budget; the head and tail are kept, full content remains in the conversation history]\n\n", len(full))
	budget := keepChars - len(banner)
	if budget < 0 {
		budget = 0
	}
	head := budget * 7 / 10
	tailChars := budget - head
	out := full
	if tailChars > 0 && len(full) > head {
		out = full[:head] + banner + full[len(full)-tailChars:]
	} else {
		out = full[:budget]
	}
	newParts := make([]utils.ContentPart, 0, len(nonText)+len(textOnly)+1)
	newParts = append(newParts, nonText...)
	newParts = append(newParts, textOnly...)
	newParts = append(newParts, utils.ContentPart{Type: "text", Text: out})
	// Copy the message BY VALUE and swap only Content: every metadata field
	// (ID, ToolCalls, ToolCallID, Name, tokens, model) is preserved exactly,
	// and a future field added to utils.Message cannot be silently dropped by
	// a hand-written struct literal.
	shrunkMsg := m
	shrunkMsg.Content = utils.NewPartsContent(newParts)
	return shrunkMsg, len(full) - len(out)
}

// truncateOversizedTail applies per-message bounds to the tail slice before
// it is shipped to the provider:
//
//   - a "tool" message whose body is oversized is DROPPED entirely (its
//     parent assistant call was already dropped or is itself being
//     truncated, so a dangling huge result is pure context waste);
//   - any other oversized message (user/assistant) is truncated to
//     maxKeepTailChars with a clear banner, so the model still sees the
//     head of the message and the message role (the turn boundary) is
//     preserved — a 4MB pasted blob can never exceed the context window by
//     itself.
//
// Returning a NEW slice; the input is not mutated.
// maxKeepTailChars returns the per-message character bound for the model's
// context window. A single oversized message (multi-MB paste, giant tool
// result) can never blow the window on its own; the bound is sized to the
// model so small-window models (65k) don't get a whole 100k-char
// message (~25k tokens) that leaves no room for the rest.
func maxKeepTailChars(contextWindow int) int {
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	// A single oversized message gets the same half-window budget as the
	// overall tail (a message bigger than the whole tail budget would
	// bypass the walker), floored at 16k chars so normal reads of big
	// pastes aren't chopped unnecessarily.
	budget := contextWindow / 2
	if budget < 16_384 {
		budget = 16_384
	}
	if budget > 100_000 {
		budget = 100_000
	}
	return budget
}

func truncateOversizedTail(tail []utils.Message, contextWindow int) []utils.Message {
	if len(tail) == 0 {
		return tail
	}
	// Which assistant tool_calls survive in the trimmed tail? Only drop an
	// oversized tool result when NO surviving assistant call references it
	// (i.e. the call was dropped too). If the call is still present, we must
	// keep the result (truncated) or OpenAPI rejects the dangling
	// tool_calls/result pairing.
	keptCalls := make(map[string]bool)
	for _, m := range tail {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				keptCalls[tc.ID] = true
			}
		}
	}
	out := make([]utils.Message, 0, len(tail))
	for i := range tail {
		m := tail[i]
		total := m.Content.TotalContentChars()
		if total <= maxKeepTailChars(contextWindow) {
			out = append(out, m)
			continue
		}
		if m.Role == "tool" && !keptCalls[m.ToolCallID] {
			utils.Log("[Eco] dropping oversized tool result (%d chars) from tail", total)
			continue
		}
		// Multi-part message whose TOTAL size blows the bound. Keep the first
		// text part (up to the bound, with a banner) and drop the rest so the
		// hidden multi-MB later parts can never reach the provider.
		txt := ""
		for _, p := range m.Content.ContentParts() {
			if p.Type == "text" {
				txt += p.Text
			}
		}
		truncLen := len(txt)
		if truncLen > maxKeepTailChars(contextWindow) {
			truncLen = maxKeepTailChars(contextWindow)
		}
		trunc := txt[:truncLen]
		m.Content = utils.NewTextContent(trunc +
			"\n\n[message truncated by Plurality: original length " +
			fmt.Sprintf("%d", total) +
			" chars across all content parts exceeded the per-message context budget]")
		out = append(out, m)
	}
	return out
}

// lastAssistantPromptTokens scans the conversation backwards and returns
// the prompt_tokens value of the most recent assistant message that
// reported usage. Returns 0 if no such message exists yet (brand-new
// conversation on its first user turn).
func lastAssistantPromptTokens(messages []utils.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
			return messages[i].PromptTokens
		}
	}
	return 0
}

// findCutoffIndex returns the index in `messages` that marks the start of
// the live tail to keep — everything strictly before this index goes into
// the new checkpoint summary. messages[cutoff..] is preserved; the
// returned index always sits at a clean turn boundary (no message
// splitting).
//
// Algorithm (walking forward from oldest):
//   - targetDrop = lastPromptTokens − targetTokens. This is how many
//     cumulative tokens we want to shave off the head.
//   - PromptTokens on an assistant message is already cumulative — it's
//     the size of the prompt at the moment that assistant spoke, i.e.
//     system + all preceding messages.
//   - The first assistant whose PromptTokens ≥ targetDrop is the one
//     whose turn pushed the running total past the threshold. Compact
//     through the end of that assistant's turn; cutoff is the next
//     message (typically the next user turn, after any trailing tool
//     results).
//   - If even the last non-final assistant doesn't cross targetDrop, fall
//     back to ending the compaction right before the final assistant's
//     turn so we still make progress.
//   - The cutoff must move past any existing checkpoint pair so we don't
//     loop on the same range.
func findCutoffIndex(messages []utils.Message, lastPromptTokens, targetTokens int, existingCheckpointEndIdx int) int {
	if lastPromptTokens <= targetTokens {
		// The recorded lastPT is often LOWER than the target even when the
		// true persisted history is gigantic: the LLM only ever saw a
		// budget-capped tail, so its usage reflects the small tail, not the
		// full DB history (observed: conversation 319ce9283cac61666757b88a —
		// lastPT ~30k vs trigger 104857 but ~700k tokens persisted, so every
		// eco tick logged "no satisfying cutoff ... skip" and NO checkpoint
		// was ever moved forward). Fall back to a CHAR-BASED cutoff: keep the
		// newest messages that fit within the target token budget (~4
		// chars/token, same density assumption the request guard uses) and
		// fold everything older into the rolling summary. This guarantees
		// eco compaction always advances on oversized never-compacted
		// conversations instead of skipping forever.
		return findCharCutoffIndex(messages, targetTokens, existingCheckpointEndIdx)
	}
	targetDrop := lastPromptTokens - targetTokens

	lastAsstIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			lastAsstIdx = i
			break
		}
	}
	if lastAsstIdx < 0 {
		return -1
	}

	minCutoff := existingCheckpointEndIdx + 1
	if minCutoff < 1 {
		minCutoff = 1
	}

	// Walk from oldest forward. PromptTokens is cumulative, so the first
	// assistant with PT ≥ targetDrop is where we've crossed. But we must
	// pick the LAST such assistant (closest to the end): picking the first
	// one makes cutoff land right after the old checkpoint, so on the next
	// eco tick cutoff < minCutoff and compaction bails forever — leaving
	// the live tail to grow unbounded (observed: one checkpoint at seq 139,
	// then 3,740 raw messages with no further compaction).
	//
	// Walking farthest-forward maximises how much history each compact
	// folds into the summary, so cutoff strictly advances every tick.
	crossingAsst := -1
	lastCrossing := -1
	for i, m := range messages {
		if i >= lastAsstIdx {
			break
		}
		if m.Role != "assistant" || m.PromptTokens <= 0 {
			continue
		}
		if m.PromptTokens >= targetDrop {
			if crossingAsst < 0 {
				crossingAsst = i
			}
			lastCrossing = i
		}
	}
	// Prefer the farthest-forward crossing (strictly past the old
	// checkpoint) so cutoff advances; if none exists beyond the
	// checkpoint, fall back to the plain first crossing.
	if lastCrossing >= 0 {
		crossingAsst = lastCrossing
	}

	// Fallback: nobody crossed. Use the last non-final assistant so we at
	// least compact everything up to the most recent turn we can safely
	// drop. If there is no such assistant (only the final asst exists),
	// no compaction is possible.
	if crossingAsst < 0 {
		for i := lastAsstIdx - 1; i >= 0; i-- {
			if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
				crossingAsst = i
				break
			}
		}
	}
	if crossingAsst < 0 {
		return -1
	}

	// Cutoff = first message after this assistant's turn. If the chosen
	// assistant had tool_calls, the matching role:"tool" results sit
	// right after it — step past every one so the assistant's tool_calls
	// and their results stay together in the dropped section. We never
	// want a kept tail that starts with an orphan tool result whose
	// parent tool_call lives in the (now-summarised) checkpoint.
	cutoff := crossingAsst + 1
	for cutoff < len(messages) && messages[cutoff].Role == "tool" {
		cutoff++
	}

	if cutoff < minCutoff {
		return -1
	}
	if cutoff >= len(messages) {
		// Would leave nothing live — refuse.
		return -1
	}
	return cutoff
}

// findCharCutoffIndex is the char-budget fallback used when the recorded
// lastPromptTokens is at/below the target (the LLM only ever saw a
// budget-capped tail, so its usage understates the real DB history — observed
// conversation 319ce9283cac61666757b88a: lastPT ~30k but >700k tokens
// persisted, so every eco tick logged "no satisfying cutoff ... skip" and no
// checkpoint ever moved). It drops the OLDEST whole turns (user-anchored)
// until the kept suffix fits within ~4 chars/token of the target budget, and
// always preserves the newest user turn — the same floor rule as the live
// tail. Returns the index of the first kept message, or -1 when there is
// nothing to fold.
func findCharCutoffIndex(messages []utils.Message, targetTokens, existingCheckpointEndIdx int) int {
	if len(messages) == 0 || targetTokens <= 0 {
		return -1
	}
	targetChars := targetTokens * 4 // ~4 chars/token, same density as the request guard
	prefix := make([]int, len(messages)+1)
	for i := range messages {
		prefix[i+1] = prefix[i] + messages[i].Content.TotalContentChars()
	}
	if prefix[len(messages)] <= targetChars {
		// The whole history already fits within the target budget — nothing
		// to fold without starving the live turn.
		return -1
	}
	// Floor: the newest user message and its in-flight turn are ALWAYS kept.
	floor := len(messages) - 1
	for floor > 0 && messages[floor].Role != "user" {
		floor--
	}
	keptStart := 0
	for {
		if prefix[len(messages)]-prefix[keptStart] <= targetChars {
			break
		}
		// Advance to the next user boundary — the start of the oldest kept
		// turn. Dropping up to it removes a WHOLE turn, never a partial one.
		next := keptStart + 1
		for next < len(messages) && messages[next].Role != "user" {
			next++
		}
		if next > floor {
			// Only the newest user turn remains and it alone exceeds the
			// budget: keep it whole (the per-message tail cap still bounds
			// it on the request path).
			keptStart = floor
			break
		}
		keptStart = next
	}
	if keptStart <= existingCheckpointEndIdx || keptStart >= len(messages) {
		return -1
	}
	return keptStart
}

// maxSummaryInputChars bounds the plain-text input fed to the
// summarisation model. The fast model (used for checkpoints) has a
// 131k-token window; feeding it a huge raw history (observed: ~770k tokens
// on "WordPress Security Review") made every eco compaction fail with
// context_length_exceeded and NO checkpoint was ever written. 80k chars ≈
// 20k tokens leaves ample room for system prompt + output.
const maxSummaryInputChars = 80_000

// renderMessagesForSummary produces a plain-text dump of the given message
// slice suitable for feeding to the summarisation model, bounded to
// maxSummaryInputChars characters. Walks every content part of every
// message and includes inline text — pasted snippets, attached file
// contents that travelled inline, and tool results. Skips image_url parts
// and parts whose Text is a storage path rather than real content
// (post-ExtractBlobsFromMessage, document parts like pdf/docx/xlsx/pptx
// have part.Text rewritten to a URL path, so we surface only the filename
// in that case).
//
// The budget preserves BOTH ends of the compacted range: the OLDEST
// messages (where the original task, goals and early decisions live) are
// always included first, so a huge compaction can never silently erase the
// very context the summary is meant to carry forward; the REMAINING budget
// is then spent on the NEWEST messages (the recent state the live model
// just lost). This is the fix for the "eco compaction wipes context"
// symptom: the previous newest-only walk meant a large compacted range fed
// the summariser ONLY its tail — the original task and early decisions were
// dropped before the summary model ever saw them, so the resulting
// checkpoint discarded the conversation's original goal. A banner notes
// when the middle was omitted. Never returns a context-busting blob to the
// summary model.
func renderMessagesForSummary(messages []utils.Message) string {
	var b strings.Builder
	// 1) Always keep the OLDEST messages first (the head of the compacted
	//    range: the original task, goals, decisions). The earliest context is
	//    the part the live tail will never re-show, so it is the most
	//    important thing a checkpoint must preserve.
	headBudget := maxSummaryInputChars
	var head []utils.Message
	for _, m := range messages {
		if db.IsCheckpointMessage(m) {
			continue
		}
		if int64(headBudget) <= 0 {
			break
		}
		mBytes := m.Content.TotalContentChars()
		if mBytes > perMsgBudget {
			mBytes = perMsgBudget
		}
		if mBytes > headBudget {
			continue
		}
		headBudget -= mBytes
		head = append(head, m)
	}

	// 2) Spend whatever budget remains on the NEWEST messages (the recent
	//    state the live model just lost), skipping anything already taken by
	//    the head so no message is counted twice.
	tailBudget := headBudget
	var tail []utils.Message
	for i := len(messages) - 1; i >= 0 && int64(tailBudget) > 0; i-- {
		m := messages[i]
		if db.IsCheckpointMessage(m) {
			continue
		}
		if messageInSlice(m, head) {
			continue
		}
		mBytes := m.Content.TotalContentChars()
		if mBytes > perMsgBudget {
			mBytes = perMsgBudget
		}
		if mBytes > tailBudget {
			continue
		}
		tailBudget -= mBytes
		tail = append(tail, m)
	}

	// Combined output, chronological (head then reversed tail).
	keptCount := len(head) + len(tail)
	if keptCount < len(messages) {
		b.WriteString("[NOTE: the middle of the compacted range was omitted to fit the summary input budget — the original task/goals (oldest) and the recent state (newest) are preserved.]\n\n")
	}
	for _, m := range head {
		writeSummaryMessage(&b, m)
	}
	for i := len(tail) - 1; i >= 0; i-- {
		writeSummaryMessage(&b, tail[i])
	}
	return b.String()
}

// messageInSlice reports whether m is already among the head messages kept
// from the oldest end (identity match on role/tool_call_id/Content), so the
// newest-end pass never double-counts it.
func messageInSlice(m utils.Message, slice []utils.Message) bool {
	// Messages are identified by (role, name, tool_call_id, serialized content
	// text). We cannot compare utils.MessageContent by ==: it contains an
	// unexported []ContentPart (a slice), which is not comparable in Go, so the
	// previous `slice[i].Content == m.Content` failed to compile
	// (invalid operation: slice-bearing struct cannot be compared). Identity is
	// unique enough for the summary head/tail de-duplication this serves.
	mt := m.TextContent()
	for i := range slice {
		if slice[i].TextContent() == mt && slice[i].Role == m.Role &&
			slice[i].Name == m.Name && slice[i].ToolCallID == m.ToolCallID {
			return true
		}
	}
	return false
}

// writeSummaryMessage renders a single message into the summary builder in
// the canonical eco-summary shape (USER/ASSISTANT/TOOL_RESULT blocks).
func writeSummaryMessage(b *strings.Builder, m utils.Message) {
	switch m.Role {
	case "user":
		b.WriteString("USER:\n")
		writeContentPartsBudget(b, m.ContentParts())
		b.WriteString("\n")
	case "assistant":
		b.WriteString("ASSISTANT:\n")
		writeContentPartsBudget(b, m.ContentParts())
		for _, tc := range m.ToolCalls {
			b.WriteString("[tool_call ")
			b.WriteString(tc.Function.Name)
			b.WriteString("] args=")
			b.WriteString(tc.Function.Arguments)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	case "tool":
		b.WriteString("TOOL_RESULT ")
		b.WriteString(m.Name)
		b.WriteString(":\n")
		writeContentPartsBudget(b, m.ContentParts())
		b.WriteString("\n")
	}
}

// writeContentPartsBudget is writeContentParts with a per-message cap: any
// single part is truncated to perMsgBudget chars so one giant tool dump
// (observed: 58KB shell outputs) can never blow the summary input budget.
// The budget is applied conservatively relative to maxSummaryInputChars.
const perMsgBudget = 20_000

func writeContentPartsBudget(b *strings.Builder, parts []utils.ContentPart) {
	for _, p := range parts {
		switch {
		case p.Type == "text" || p.Type == "snippet":
			if p.Text == "" {
				continue
			}
			if p.Filename != "" {
				b.WriteString("[attachment: ")
				b.WriteString(p.Filename)
				b.WriteString("]\n")
			}
			if len(p.Text) > perMsgBudget {
				b.WriteString(p.Text[:perMsgBudget])
				b.WriteString("\n[...truncated by Plurality context-safety: original length ")
				b.WriteString(fmt.Sprintf("%d", len(p.Text)))
				b.WriteString(" chars]\n")
			} else {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		case p.Type == "image_url":
			b.WriteString("[image attachment omitted]\n")
		case docsupport.IsDocumentType(p.Type):
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[document attachment: ")
			b.WriteString(label)
			b.WriteString("] (binary, content not inlined)\n")
		default:
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[binary attachment: ")
			b.WriteString(label)
			b.WriteString("]\n")
		}
	}
}

// writeContentParts serialises a content-part slice for the summary
// input. We whitelist the types we know carry real inline text — anything
// else surfaces as an attachment marker (filename only) so arbitrary
// binaries (.iso, .zip, raw uploads) can never leak data URIs or
// storage paths into the summariser as if they were the user's prose.
//
// Whitelisted as inline text:
//   - "text"     — plain text content part
//   - "snippet"  — user-pasted text/code snippet
//
// Marker only (no inline body):
//   - "image_url"                  — image, omitted
//   - docsupport.IsDocumentType(t) — pdf/docx/xlsx/pptx etc.; stored text
//     is a storage path, not real content
//   - everything else              — generic "file" or unknown type; we
//     can't tell if Text is inline content
//     or a binary reference, so we only
//     surface the filename
func writeContentParts(b *strings.Builder, parts []utils.ContentPart) {
	for _, p := range parts {
		switch {
		case p.Type == "text" || p.Type == "snippet":
			if p.Text == "" {
				continue
			}
			if p.Filename != "" {
				b.WriteString("[attachment: ")
				b.WriteString(p.Filename)
				b.WriteString("]\n")
			}
			b.WriteString(p.Text)
			b.WriteString("\n")
		case p.Type == "image_url":
			b.WriteString("[image attachment omitted]\n")
		case docsupport.IsDocumentType(p.Type):
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[document attachment: ")
			b.WriteString(label)
			b.WriteString("] (binary, content not inlined)\n")
		default:
			// Generic file or unknown type — never inline Text since it
			// could be a data: URI or storage path. Just surface the
			// filename (or type as a last-resort label).
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[binary attachment: ")
			b.WriteString(label)
			b.WriteString("]\n")
		}
	}
}

// runEcoSummary is the entry point spawned as a goroutine on every user
// message. It returns quickly without doing work in any of these cases:
//   - global single-flight slot is busy
//   - conversation has eco mode disabled
//   - last prompt_tokens is below the configured trigger
//   - no satisfying cutoff (conversation isn't long enough yet)
//
// When all gates pass, it summarises the oldest turns into a new checkpoint
// pair and atomically swaps out any prior pair via db.ReplaceCheckpoint.
// Errors are logged and swallowed — the next user turn will retry.
func runEcoSummary(ctx context.Context, conversationID string) {
	utils.Log("[Eco] tick for conv %s", conversationID)
	if !summaryInFlight.CompareAndSwap(false, true) {
		utils.Log("[Eco] summary already in flight — skipping conv %s", conversationID)
		return
	}
	defer summaryInFlight.Store(false)

	// Bound every DB access in this goroutine. The single SQLite connection
	// (SetMaxOpenConns(1)) is shared with live reads/writes (message pushes,
	// the embed worker, conversation loads); if the pool is wedged we must
	// fail fast and release the global single-flight slot so normal traffic
	// and future compactions are not starved. The summary LLM call
	// (GenerateCheckpointSummary) runs on its own HTTP client and is not
	// affected by this deadline.
	dbCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	conv, err := db.GetConversationByIdInternal(dbCtx, conversationID)
	if err != nil {
		utils.Error("[Eco] could not load conversation", err)
		return
	}
	if !conv.ModelSelected.EcoMode {
		utils.Log("[Eco] conv %s has eco mode off — skip", conversationID)
		return
	}

	cfg := auth.GetConfig().Eco
	if cfg.TriggerTokens <= 0 || cfg.TargetTokens <= 0 {
		utils.Log("[Eco] eco config disabled (trigger=%d target=%d) — skip", cfg.TriggerTokens, cfg.TargetTokens)
		return
	}

	// Resolve the conversation's chat model so the trigger can be scaled to
	// its context window (dynamic compaction).
	modelName := ""
	if conv.ModelSelected.Text != nil {
		modelName = conv.ModelSelected.Text.Name
	} else if conv.ModelSelected.Vision != nil {
		modelName = conv.ModelSelected.Vision.Name
	}
	trigger := effectiveEcoTrigger(modelName)
	if trigger <= 0 {
		trigger = cfg.TriggerTokens
	}

	lastPT := lastAssistantPromptTokens(conv.Messages)
	if lastPT == 0 {
		utils.Log("[Eco] conv %s has no assistant prompt_tokens yet — skip", conversationID)
		return
	}
	if lastPT <= trigger {
		// The recorded lastPT is often LOWER than the trigger even when the
		// true persisted history is gigantic: the LLM only ever saw a
		// budget-capped tail, so its usage reflects the small tail, not the
		// full DB history. On conversation 319ce9283cac61666757b88a this
		// left one checkpoint stuck at seq 154 while 839 messages (≈700k
		// tokens) piled up behind it — every turn then dead-ended in the
		// preemptive context guard. Fall back to a raw-history-size gate: if
		// the persisted message content (minus checkpoints) exceeds the
		// trigger's worth of characters, durable-compact now instead of
		// waiting forever for lastPT to catch up.
		rawChars := 0
		for _, m := range conv.Messages {
			if db.IsCheckpointMessage(m) {
				continue
			}
			rawChars += m.Content.TotalContentChars()
		}
		// ~4 chars/token — same density assumption the request guard uses.
		if rawChars/4 <= trigger {
			utils.Log("[Eco] conv %s lastPT=%d below trigger=%d (model context-aware) — skip", conversationID, lastPT, trigger)
			return
		}
		utils.Log("[Eco] conv %s lastPT=%d below trigger=%d but raw history ≈%d chars → durable-compacting", conversationID, lastPT, trigger, rawChars)
	} else {
		utils.Log("[Eco] conv %s lastPT=%d > trigger=%d (model context-aware) — compacting toward target=%d", conversationID, lastPT, trigger, cfg.TargetTokens)
	}

	// Locate the existing checkpoint (if any) and compute where the new
	// cutoff must land.
	existingCheckpointEndIdx := -1
	var oldPairIDs []int64
	var priorSummary string
	if pair, err := db.GetCheckpoint(dbCtx, conversationID); err == nil && pair != nil {
		priorSummary = pair.Summary
		oldPairIDs = []int64{pair.AssistantID}
		if pair.ToolID != 0 {
			oldPairIDs = append(oldPairIDs, pair.ToolID)
		}
		// existingCheckpointEndIdx is the index in conv.Messages of the
		// tool half of the pair. We find the assistant half by tool-call
		// name match, then add 1 for the tool half.
		for i, m := range conv.Messages {
			if m.Role == "assistant" && db.IsCheckpointMessage(m) {
				existingCheckpointEndIdx = i + 1 // assume tool half follows immediately
				break
			}
		}
	}

	cutoff := findCutoffIndex(conv.Messages, lastPT, cfg.TargetTokens, existingCheckpointEndIdx)
	if cutoff < 0 {
		utils.Log("[Eco] no satisfying cutoff for conv %s (lastPT=%d target=%d, %d messages) — skip", conversationID, lastPT, cfg.TargetTokens, len(conv.Messages))
		return
	}
	utils.Log("[Eco] conv %s cutoff=%d (of %d messages), prior checkpoint end=%d", conversationID, cutoff, len(conv.Messages), existingCheckpointEndIdx)

	// Slice of messages to fold into the new summary: everything strictly
	// between the existing checkpoint and the cutoff.
	startIdx := 0
	if existingCheckpointEndIdx >= 0 {
		startIdx = existingCheckpointEndIdx + 1
	}
	if startIdx >= cutoff {
		return
	}

	// Compaction is MACHINE-ONLY — it never calls an LLM. The folded history
	// (everything between the previous checkpoint and the cutoff) is replaced
	// by the deterministic megacave machine excerpt (Caveman
	// /caveman-compress-style; no model call): code/paths/IDs verbatim,
	// JSON compressed, prose trimmed. This is a pure history transform for the
	// future conversation workflow — no request is sent to a chat model.
	// (The previous two-stage LLM "megacave"/"dumb eco" summary calls both ran
	// model inference, which blocked the retry for minutes on the slow
	// self-hosted model.)
	excerptMessages := conv.Messages[startIdx:cutoff]
	summary := MachineExcerptMegacave(excerptMessages)
	if priorSummary != "" {
		// Keep the rolling carry-forward: the machine excerpt stands in for
		// NEW messages, the prior checkpoint for everything older.
		summary = priorSummary + "\n\n[newer history, machine-compacted]\n" + summary
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = "[compacted by Plurality — machine excerpt unavailable; older messages were rolled into this checkpoint]"
	}

	// Build the assistant + tool pair. Look up the actual DB seq for the
	// message at the cutoff slice position — gaps can exist after prior
	// checkpoint replacements, so slice index != seq in general.
	insertSeq, err := db.MessageSeqAt(dbCtx, conversationID, cutoff)
	if err != nil {
		utils.Error("[Eco] could not resolve insert seq", err)
		return
	}
	toolCallID := "eco_" + randomHex(8)
	now := time.Now().Format(time.RFC3339)

	assistantMsg := utils.Message{
		Role:      "assistant",
		Content:   utils.MessageContent{},
		Timestamp: now,
		ToolCalls: []utils.ToolCall{{
			ID:   toolCallID,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      db.EcoCheckpointToolName,
				Arguments: "{}",
			},
		}},
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(summary),
		ToolCallID: toolCallID,
		Name:       db.EcoCheckpointToolName,
		Timestamp:  now,
	}

	if err := db.ReplaceCheckpoint(dbCtx, conversationID, oldPairIDs, assistantMsg, toolMsg, insertSeq); err != nil {
		utils.Error("[Eco] persisting checkpoint failed", err)
		return
	}
	utils.Log("[Eco] checkpoint written for conv %s (cutoff=%d, lastPT=%d → kept tail ≈%d tokens)", conversationID, cutoff, lastPT, lastPT-tokensBeforeCutoff(conv.Messages, cutoff))
}

// tokensBeforeCutoff is a logging helper — it returns the prompt_tokens
// value of the first assistant message at or after the cutoff, which
// approximates "tokens of system + everything before cutoff". Returns 0 if
// no assistant follows.
func tokensBeforeCutoff(messages []utils.Message, cutoff int) int {
	for i := cutoff; i < len(messages); i++ {
		if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
			return messages[i].PromptTokens
		}
	}
	return 0
}

// randomHex mirrors db.GenerateID but with a caller-chosen byte width. Used
// for synthesising tool_call_id values that won't collide with real LLM
// output (those are model-generated strings).
func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
