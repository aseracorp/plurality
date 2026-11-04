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

	// If the WHOLE history already fits within the content budget, return it
	// UNCHANGED. This is what keeps the below-trigger path byte-stable across
	// turns (checked every request), so the backend KV prompt cache keeps
	// hitting. Compaction only starts when content does not fit.
	if fits(all) {
		utils.Log("[Eco] staged resolver: full history fits (≈%d / %d tokens) — unchanged, stable prefix", approxTokens(all), budgetTokens)
		return all
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
