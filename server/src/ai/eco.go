package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
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

// effectiveEcoTrigger computes the compaction trigger for a conversation:
// the configured absolute threshold, lowered to the model's declared context
// window when that window is smaller. So compaction fires on EITHER condition
// — crossing TriggerTokens, or no longer fitting the context — and a
// small-window model compacts at its window instead of waiting for a 100k
// floor it can never reach. There is no window multiplicator: the window
// itself is the ceiling, and the staged resolver fits the history to it.
//
// Returns the static trigger when the window is unknown (0).
func effectiveEcoTrigger(modelName string) int {
	cfg := auth.GetConfig().Eco
	static := cfg.TriggerTokens
	if static <= 0 {
		static = defaultEcoTriggerTokens
	}
	window := Models.MaxContextTokensFor(modelName)
	if window > 0 && window < static {
		return window
	}
	return static
}
func userContextFor(userID string) context.Context {
	return context.WithValue(context.Background(), "userID", userID)
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
// SentViewTokens estimates the token size of the conversation view that is
// ACTUALLY sent to the model.
//
// The request path loads conversations with includeCompacted=true (the client
// needs the full history), so the raw slice carries rows that durable
// compaction has already folded into a checkpoint and that are NOT part of the
// request. Counting them made the eco trigger read ~1.25M tokens while the sent
// view was ~23k (observed live: a 54x disagreement on the same request). The
// trigger then fired durable compaction on EVERY turn, and each firing inserts
// a checkpoint pair at the kept-window boundary — rewriting the HEAD of the
// history, the exact pattern that collapses KV prompt-cache reuse to the stable
// system block (reproduced on strata: reuse 32049 -> 17116).
func SentViewTokens(messages []utils.Message) int {
	chars := 0
	for i := range messages {
		if messages[i].Compacted {
			continue // folded into a checkpoint; not part of the request
		}
		chars += messageWireChars(messages[i])
	}
	return chars / 3
}

func approxTokens(messages []utils.Message) int {
	chars := 0
	for i := range messages {
		chars += messageWireChars(messages[i])
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
//
// It returns the view plus `foldFrom`: the index in `all` from which the LIVE
// tail begins, i.e. how much of the history the chosen stage folded into the
// excerpt. The durable path needs that number — it is the whole point of the
// staged design: the context window is the target, so durable compaction must
// fold EXACTLY what the resolver dropped, no more (context loss) and no less
// (the "folded 0 every step" shape that leaves the next request oversized).
// Stages A-D always keep the newest user turn and everything after it, so
// foldFrom never splits the live turn. foldFrom is 0 when the history already
// fits (view returned unchanged).
func resolveEcoContextCompaction(all []utils.Message, windowTokens int, budgetTokens int) (msgs []utils.Message, foldFrom int) {
	const toolOverheadTokens = 40 // small noise margin for the injected excerpt pair

	fits := func(msgs []utils.Message) bool {
		return approxTokens(msgs)+toolOverheadTokens <= budgetTokens
	}

	// If the WHOLE history already fits within the content budget, return it
	// UNCHANGED. This is what keeps the below-trigger path byte-stable across
	// turns (checked every request), so the backend KV prompt cache keeps
	// hitting. Compaction only starts when content does not fit.
	if fits(all) {
		utils.Log("[Eco] staged resolver: full history fits (≈%d / %d tokens) — unchanged, stable prefix", approxTokens(all), budgetTokens)
		return all, 0
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
		foldFrom = turnStart
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
		foldFrom = lastUser
	}
	if !fits(candidate) {
		// STAGE C — user messages + last assistant before a user, megacaved.
		//
		// The newest user message is kept VERBATIM, exactly as stage D does.
		// Megacaving the WHOLE reduced set (which is every user row) leaves the
		// view with ZERO role:"user" messages, and every chat template rejects
		// that: strata answers 400 "No user query found in messages." Measured
		// end-to-end against the live engine — stages fits/A/B/D all cache at
		// 98-99.9% reuse, stage C alone was an outright request failure. The
		// stage-C unit tests only assert the artifact exists, so a view that
		// cannot be sent passed them; see TestResolver_EveryStageIsSendable.
		lastUser := indexOfLastUser(all)
		var reduced []utils.Message
		if lastUser > 0 {
			reduced = userOnlyWithLastAssistant(all[:lastUser])
		}
		var cand []utils.Message
		if art := megacaveArtifact(reduced, "megacave_C"); art != nil {
			cand = append(cand, art...)
		}
		if lastUser >= 0 {
			cand = append(cand, all[lastUser])
		}
		candidate = cand
		id = 3
		foldFrom = lastUserIdxSafe(all)
	}
	if !fits(candidate) {
		// STAGE D — the budget is SMALLER THAN A MEGACAVE EXCERPT CAN EVER BE.
		// The excerpt saturates at maxMegacaveExcerptChars (60_000 chars
		// = ~20_000 est tokens), so shrinking the folded set turn-by-turn
		// cannot help: `fits` can never become true below that floor. The old
		// greedy loop drained the slice to nil, megacaveArtifact(nil) returned
		// nil, and the resolver emitted an EMPTY history — the caller's
		// `if len(convTail) > 0` guard then silently shipped the FULL
		// UNCOMPACTED history, an oversized request exactly when compaction was
		// needed. contentBudget floors at 8192, so this was reachable for every
		// model with a window under ~29k tokens.
		//
		// Correct move: shrink the EXCERPT ITSELF to the budget, and always keep
		// the newest user message verbatim so the model sees the live request.
		candidate = nil
		id = 4
		foldFrom = lastUserIdxSafe(all)
		lastUser := indexOfLastUser(all)
		var tail []utils.Message
		older := all
		if lastUser >= 0 {
			tail = all[lastUser:]
			older = all[:lastUser]
		}
		// Room left for the excerpt once the live tail + the artifact pair's
		// structural overhead are accounted for.
		room := budgetTokens - approxTokens(tail) - toolOverheadTokens - 20
		if art := megacaveArtifactBudgeted(older, "megacave_D", room); art != nil {
			candidate = append(candidate, art...)
		}
		candidate = append(candidate, tail...)
		if approxTokens(candidate) > budgetTokens {
			utils.Log("[Eco] stage D: budget %d tokens below the live tail (~%d) — sending tail + maximal excerpt",
				budgetTokens, approxTokens(tail))
		}
	}

	// Never fold the whole history: an empty live tail is how the caller ends
	// up shipping the FULL uncompressed history (its `len(convTail) > 0`
	// guard), the oversized-request defect #161 removed.
	if foldFrom >= len(all) {
		foldFrom = lastUserIdxSafe(all)
	}
	if foldFrom < 0 {
		foldFrom = 0
	}
	utils.Log("[Eco] resolved staged compaction (stage %d ids, %d messages, ≈%d tokens / budget %d, folding %d of %d)",
		id, len(candidate), approxTokens(candidate), budgetTokens, foldFrom, len(all))
	return candidate, foldFrom
}

// lastUserIdxSafe returns the index of the newest user message, or 0 when the
// conversation has none. Used as the durable fold floor: the newest user turn
// is never folded away.
func lastUserIdxSafe(all []utils.Message) int {
	if i := indexOfLastUser(all); i > 0 {
		return i
	}
	return 0
}

// megacaveArtifactBudgeted is megacaveArtifact with the excerpt hard-capped at
// budgetTokens (chars = tokens*3, the same conversion approxTokens uses). Stage
// D needs this: there the budget is BELOW the excerpt's natural saturation size,
// so the excerpt — not the conversation — is the thing that must shrink. A
// non-positive budget yields a small placeholder rather than nil, so the caller
// always receives a well-formed assistant/tool pair.
func megacaveArtifactBudgeted(folded []utils.Message, toolCallID string, budgetTokens int) []utils.Message {
	excerpt := MachineExcerptMegacave(folded)
	if excerpt == "" {
		return nil
	}
	if maxChars := budgetTokens * 3; maxChars > 0 && len(excerpt) > maxChars {
		excerpt = truncateExcerpt(excerpt, maxChars)
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

// truncateExcerpt keeps the head and the tail (60/40) of an excerpt around an
// elision banner, so the oldest context and the most recent context — the two
// ends a model actually needs — survive a hard budget cut.
func truncateExcerpt(s string, maxChars int) string {
	if maxChars <= 0 {
		return "[megacave excerpt elided: budget too small]"
	}
	if len(s) <= maxChars {
		return s
	}
	banner := "\n...[megacave excerpt truncated: elided for budget]...\n"
	if maxChars <= len(banner) {
		return s[:maxChars]
	}
	room := maxChars - len(banner)
	head := room * 60 / 100
	tail := room - head
	return s[:head] + banner + s[len(s)-tail:]
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

// ecoContentBudget is the content budget the staged resolver must fit within:
// the model's context window minus room for the system prompt, tool schemas
// and the model's reply. Callers that do not have the marshalled request in
// hand (the tool-loop overflow recovery, the eco tick) use this so the view
// they SEND and the fold they PERSIST come from the same number. The request
// path computes a tighter budget from the measured tool-schema + system byte
// counts; this is the standing approximation.
func ecoContentBudget(modelName string) int {
	window := Models.MaxContextTokensForPrefixed(modelName)
	reserve := ecoReplyReserveForName(modelName) + 512
	// Same OR gate as the request path (see index.go): compaction fires when
	// content crosses the eco trigger OR no longer fits the window, so the
	// resolver has to aim at the TIGHTER of the two ceilings. A window-derived
	// budget alone lets a large-window model answer "fits" long after the
	// trigger, which folds 0 and compacts nothing. `reserve` is subtracted from
	// both sides so the comparison stays on the conversation-content basis the
	// resolver's fits() uses.
	budget := window - reserve
	if trigger := effectiveEcoTrigger(modelName); trigger > 0 {
		if tb := trigger - reserve; tb < budget {
			budget = tb
		}
	}
	if budget > 0 {
		return budget
	}
	return 8192
}

// ecoCutoffIndex returns the index in `messages` where the LIVE tail begins:
// everything before it is folded into the rolling checkpoint.
//
// The staged resolver decides this, which is the whole point of the design:
// there is no "target" token count. The context window is the target, so the
// same stages A -> D that build the outgoing request view also say how much
// history has to go, and the durable fold matches it exactly. Folding less
// leaves the next request oversized (the "folded 0 every step" shape); folding
// more loses context the model still needs.
//
// Returns -1 when nothing can be folded (the history already fits, or the
// conversation is too short to give up any turn).
func ecoCutoffIndex(messages []utils.Message, modelName string) int {
	if len(messages) == 0 {
		return -1
	}
	window := Models.MaxContextTokensForPrefixed(modelName)
	_, foldFrom := resolveEcoContextCompaction(messages, window, ecoContentBudget(modelName))
	if foldFrom <= 0 {
		return -1
	}
	// Never fold the newest user turn, and never fold everything.
	if floor := startOfLastTurn(messages); foldFrom > floor {
		foldFrom = floor
	}
	if foldFrom <= 0 || foldFrom >= len(messages) {
		return -1
	}
	return foldFrom
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
	if cfg.TriggerTokens <= 0 {
		utils.Log("[Eco] eco config disabled (trigger=%d) — skip", cfg.TriggerTokens)
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
			rawChars += messageWireChars(m)
		}
		// ~4 chars/token — same density assumption the request guard uses.
		if rawChars/4 <= trigger {
			utils.Log("[Eco] conv %s lastPT=%d below trigger=%d (model context-aware) — skip", conversationID, lastPT, trigger)
			return
		}
		utils.Log("[Eco] conv %s lastPT=%d below trigger=%d but raw history ≈%d chars → durable-compacting", conversationID, lastPT, trigger, rawChars)
	} else {
		utils.Log("[Eco] conv %s lastPT=%d > trigger=%d (model context-aware) — compacting to fit the context window", conversationID, lastPT, trigger)
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

	cutoff := ecoCutoffIndex(conv.Messages, modelName)
	if cutoff < 0 {
		utils.Log("[Eco] no satisfying cutoff for conv %s (lastPT=%d, %d messages) — skip", conversationID, lastPT, len(conv.Messages))
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
