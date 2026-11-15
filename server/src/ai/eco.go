package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

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
		chars += boundedWireChars(messages[i])
	}
	return chars / 3
}

// ecoSentMessageCapChars is the per-message volume ceiling of the view that is
// SENT to the provider. TruncateOversizedMessages (context_compaction.go)
// bounds every outgoing message to maxCompactMessageChars, so anything above
// that in the stored history is NOT sent. The estimator must measure the same
// bound, or the eco trigger charges the request for bytes the provider never
// sees.
//
// Live proof (conversation 08611c509032e6831ba6031f, "System Status Check"):
// one user message held a pasted logfile of 991440 chars. Raw counting made
// that single row ~330k est tokens, so `content ≈388917 tokens > trigger
// 150000` on EVERY turn — the gate demanded compaction that can never succeed,
// because no fold of the OLDER history shrinks one oversized NEWEST message.
const ecoSentMessageCapChars = maxCompactMessageChars

// boundedWireChars is messageWireChars clamped to what is actually sendable.
// The clamp applies to the NON-IMAGE payload only, mirroring the sender
// (TruncateOversizedMessages bounds the non-image bytes of every outgoing
// message and never touches image parts): image bytes are real provider context
// cost — one screenshot is 10-50k tokens — and clamping them too would recreate
// the blind spot this estimator exists to close (conversation
// aeb657c899e600ff1a822ffb: trigger ≈52508 tokens vs 438246 on the wire, request
// shipped unchanged, conversation dead on "request exceeds the context window").
func boundedWireChars(m utils.Message) int {
	if checkpointSummaryCarrier(m) {
		// Exempt from the per-message cap, exactly like the sender
		// (TruncateOversizedMessages). Counting it clamped would let the trigger
		// read the summary as smaller than it is and ship a request the
		// preemptive context guard then rejects.
		return messageWireChars(m)
	}
	img := imageWireChars(m.Content)
	nonImage := messageWireChars(m) - img
	if nonImage > ecoSentMessageCapChars {
		nonImage = ecoSentMessageCapChars
	}
	return nonImage + img
}

func approxTokens(messages []utils.Message) int {
	chars := 0
	for i := range messages {
		// SAME basis as SentViewTokens / the sender: rows durable compaction has
		// already folded (compacted = 1) are NOT part of the request, so they
		// must not be charged to it. The request path loads conversations with
		// includeCompacted = true, so the slice handed to the resolver carries
		// them; counting them made the resolver measure a request that never
		// leaves and demand folds that shrink nothing.
		if messages[i].Compacted {
			continue
		}
		chars += boundedWireChars(messages[i])
	}
	return chars / 3
}

// megacaveArtifact returns a [assistant, tool] checkpoint pair carrying the
// megacave machine excerpt of the given messages, so the excerpt is rendered
// as a normal message the model can read.
func megacaveArtifact(folded []utils.Message, toolCallID string, prior string) []utils.Message {
	excerpt := carryForward(prior, MachineExcerptMegacave(folded))
	if excerpt == "" {
		return nil
	}
	return checkpointArtifactMessages(folded, toolCallID, excerpt)
}

// checkpointPairEnd returns the index of the LAST message of the eco
// checkpoint pair that sits at the head of the LLM view, or -1 when the
// conversation has no checkpoint. The pair is [assistant(tool_call
// eco_checkpoint), tool(summary)]; the view loads it as the first rows
// (GetConversationByIdInternal returns compacted=0 rows ordered by seq, the
// pair rows are the only non-compacted rows below the fold boundary).
//
// Stages must fold strictly AFTER this index: folding the pair would destroy
// the summary of everything it stands for and rewrite the head of the prompt
// (prefix-cache collapse).
func checkpointPairEnd(all []utils.Message) int {
	last := -1
	for i := range all {
		if db.IsCheckpointMessage(all[i]) {
			last = i
		}
	}
	return last
}

// foldableRange is the slice of history a stage may fold into the summary:
// the rows strictly after the checkpoint pair and strictly before `end`.
// Returns nil when the range is empty or `end` is negative, so
// megacaveArtifact yields no artifact (nothing to summarise) and the stage
// degenerates to sending the tail verbatim.
func foldableRange(all []utils.Message, pairEnd, end int) []utils.Message {
	start := pairEnd + 1
	if end < 0 || start >= end {
		return nil
	}
	// The slice handed to the resolver can contain rows durable compaction has
	// ALREADY folded (the request path loads includeCompacted = true). Those are
	// represented by the prior checkpoint summary — re-summarising them into the
	// new one duplicates the same history in every prompt. Checkpoint rows
	// inside the range (a second pair, legacy data) are skipped for the same
	// reason.
	out := make([]utils.Message, 0, end-start)
	for i := start; i < end; i++ {
		if all[i].Compacted || db.IsCheckpointMessage(all[i]) {
			continue
		}
		out = append(out, all[i])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// priorSummaryFromView returns the summary text carried by the checkpoint pair
// at the head of the view, or "". It is the roll-up of everything the
// conversation folded away in earlier rounds.
//
// It must be carried forward VERBATIM into the next summary (carryForward),
// never re-megacaved: the megacave transform compresses prose hard, and running
// it over the prior summary degrades it a little more every round — the
// "summary of a summary of a summary" erosion. It must also never simply be
// dropped: the durable fold replaces the pair, so a summary that does not carry
// the prior one silently deletes all older history (the fold's own
// PRIOR CHECKPOINT: plumbing built the text into a string the machine path
// then threw away, so every fold lost the previous summary).
func priorSummaryFromView(all []utils.Message, pairEnd int) string {
	if pairEnd < 0 || pairEnd >= len(all) {
		return ""
	}
	m := all[pairEnd]
	if m.Role != "tool" || !db.IsCheckpointMessage(m) {
		return ""
	}
	return strings.TrimSpace(m.Content.TextContent())
}

// carryForward renders the new summary as prior + newly folded history, so one
// checkpoint pair always represents the WHOLE prefix it stands for.
func carryForward(prior, excerpt string) string {
	prior = strings.TrimSpace(prior)
	excerpt = strings.TrimSpace(excerpt)
	if prior == "" {
		return excerpt
	}
	if excerpt == "" {
		return prior
	}
	return prior + "\n\n[newer history, machine-compacted]\n" + excerpt
}

// oldestDroppedMarker names, in the summary text itself, how much folded
// history stage D had to give up. The dropped rows are marked compacted by the
// durable fold, so without this marker the loss is invisible: the model reads a
// summary that silently omits the oldest turns. Naming the size keeps the loss
// honest and inspectable (and inspectable from the stored checkpoint alone).
func oldestDroppedMarker(droppedRows int) string {
	return fmt.Sprintf("[oldest context dropped: %d messages of this conversation were folded away and are NOT represented in this summary \u2014 the request cannot fit them.]\n\n", droppedRows)
}

// megacaveArtifactWithDropMarker is megacaveArtifact plus an explicit notice of
// how much folded history stage D had to drop from the summary.
//
// The composition (carry forward FIRST, then test for emptiness) is the whole
// point and mirrors megacaveArtifact: stage D sets foldFrom to pairEnd+1, i.e.
// the fold supersedes the checkpoint PAIR itself. Checking the new excerpt alone
// and returning nil when it is empty (nothing left to summarise, or nothing
// foldable at all) drops the prior roll-up from the view AND lets the durable
// fold mark the pair compacted with no replacement summary — permanently
// deleting every row earlier folds represented. That is the "context lost on
// restart" bug reached from the other side, so the emptiness test runs on the
// CARRIED text.
//
// The marker goes AFTER the carried roll-up, never in front of it: the pair is
// the first content of the prompt after the system block, and a drop count that
// changes every round at the HEAD moves the prompt prefix and invalidates the
// backend KV cache exactly like the nested "PRIOR CHECKPOINT:" banners did.
func megacaveArtifactWithDropMarker(folded []utils.Message, toolCallID string, prior string, droppedRows int) []utils.Message {
	newExcerpt := MachineExcerptMegacave(folded)
	if droppedRows > 0 {
		newExcerpt = oldestDroppedMarker(droppedRows) + newExcerpt
	}
	carried := strings.TrimSpace(carryForward(prior, newExcerpt))
	if carried == "" {
		return nil
	}
	return checkpointArtifactMessages(folded, toolCallID, carried)
}

// checkpointArtifactMessages wraps a rendered summary in the
// [assistant tool_call, tool result] pair the model reads.
func checkpointArtifactMessages(folded []utils.Message, toolCallID, excerpt string) []utils.Message {
	_ = folded
	aiMsg := utils.Message{
		Role: "assistant",
		ToolCalls: []utils.ToolCall{{
			ID:   toolCallID,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      toolCallID,
				Arguments: "{}",
			},
		}},
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(excerpt),
		ToolCallID: toolCallID,
		Name:       toolCallID,
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
		// "Fits" is measured on the BOUNDED view (boundedWireChars), so a single
		// oversized row (the 991440-char pasted logfile) does NOT demand
		// compaction — no fold of older history can shrink it, and demanding one
		// is the wedge that ran the full compaction path on every turn. Return
		// the view bounded the same way the estimator measures it, so the caller
		// sends exactly what was budgeted. Truncation is deterministic, so the
		// prefix stays byte-stable across turns (KV cache).
		return TruncateOversizedMessages(all), 0
	}

	// The checkpoint pair is the head of the LLM view and the carrier of the
	// rolling summary. It is NEVER a fold target: folding it (a) destroys the
	// summary of everything it represents and (b) rewrites the first ~65 KB of
	// every prompt, which collapses the backend KV prefix cache to the system
	// block. Stages below fold strictly the rows AFTER the pair.
	pairEnd := checkpointPairEnd(all)
	prior := priorSummaryFromView(all, pairEnd)

	// We build the candidate from user+assistant/tool messages (system is
	// added by the caller after this).
	turnStart := startOfLastTurn(all)
	var candidate []utils.Message
	id := 0
	foldFrom = 0

	// STAGE A — latest full turn verbatim + megacave of everything between the
	// checkpoint pair and that turn. The summary is UNCAPPED: it is the roll-up
	// of the history being given up, and the fit test below is what decides how
	// much history that is.
	{
		var cand []utils.Message
		folded := foldableRange(all, pairEnd, turnStart)
		if art := megacaveArtifact(folded, "megacave_A", prior); art != nil {
			cand = append(cand, art...)
		}
		if turnStart >= 0 {
			cand = append(cand, all[turnStart:]...)
		}
		candidate = cand
		id = 1
		foldFrom = turnStart
		utils.Log("[Eco/stageA] folded %d rows -> view %d msgs ~%d tokens (budget %d, fits=%v)",
			len(folded), len(candidate), approxTokens(candidate), budgetTokens, fits(candidate))
	}
	if !fits(candidate) {
		// STAGE B — no tail: keep only the most recent USER message verbatim,
		// megacave everything between the pair and it. Differs from A when the
		// last turn carries assistant/tool rows after the user row.
		lastUser := indexOfLastUser(all)
		var cand []utils.Message
		folded := foldableRange(all, pairEnd, lastUser)
		if art := megacaveArtifact(folded, "megacave_B", prior); art != nil {
			cand = append(cand, art...)
		}
		if lastUser >= 0 {
			cand = append(cand, all[lastUser])
		}
		candidate = cand
		id = 2
		foldFrom = lastUser
		utils.Log("[Eco/stageB] folded %d rows -> view %d msgs ~%d tokens (budget %d, fits=%v)",
			len(folded), len(candidate), approxTokens(candidate), budgetTokens, fits(candidate))
	}
	if !fits(candidate) {
		// STAGE C — user messages + last assistant before a user, megacaved.
		//
		// The newest user message is kept VERBATIM, exactly as stage D does.
		// Megacaving the WHOLE reduced set (which is every user row) leaves the
		// view with ZERO role:"user" messages, and every chat template rejects
		// that: strata answers 400 "No user query found in messages." Measured
		// end-to-end against the live engine — stages A/B/D all cache at
		// 98-99.9% reuse, stage C alone was an outright request failure. The
		// stage-C unit tests only asserted the artifact exists, so a view that
		// cannot be sent passed them; see TestResolver_EveryStageIsSendable.
		lastUser := indexOfLastUser(all)
		var reduced []utils.Message
		if lastUser > 0 {
			reduced = userOnlyWithLastAssistant(foldableRange(all, pairEnd, lastUser))
		}
		var cand []utils.Message
		if art := megacaveArtifact(reduced, "megacave_C", prior); art != nil {
			cand = append(cand, art...)
		}
		if lastUser >= 0 {
			cand = append(cand, all[lastUser])
		}
		candidate = cand
		id = 3
		foldFrom = lastUserIdxSafe(all)
		utils.Log("[Eco/stageC] folded %d rows -> view %d msgs ~%d tokens (budget %d, fits=%v)",
			len(reduced), len(candidate), approxTokens(candidate), budgetTokens, fits(candidate))
	}
	if !fits(candidate) {
		// STAGE D — give up the OLDEST folded turns until the view fits, keeping
		// the summary of everything that remains (still UNCAPPED). The live tail
		// stays the newest user message verbatim, as stages B and C send it.
		//
		// This is the documented stage-D contract ("drop megacaved turns
		// one-by-one from the START until the remainder fits"), NOT an excerpt
		// cap. Capping the excerpt is what produced the truncated checkpoint
		// ending mid-sentence ("Clean fi"), and a cap cannot help: the excerpt
		// saturates at the cap, so `fits` can never go true below that floor —
		// the old greedy loop drained the slice to nil, megacaveArtifact(nil)
		// returned nil, and the resolver emitted an EMPTY history, which the
		// caller's `len(convTail) > 0` guard then replaced with the FULL
		// UNCOMPACTED history (defect #161).
		//
		// Dropping a turn genuinely shrinks the summary (measured: the megacave
		// transform runs at ratio ~0.107, so total = W - 0.893*folded is
		// monotone in folded), which is what makes the search terminate.
		//
		// The dropped rows are marked compacted by the durable fold and are NOT
		// in the summary, so the loss is recorded OUT LOUD in the summary text
		// (dropMarker) instead of silently eating the oldest context.
		lastUser := indexOfLastUser(all)
		// The verbatim tail of stage D is the newest USER message only — exactly
		// the shape stage C settled on. Taking the WHOLE newest turn here
		// (`all[lastUser:]`, i.e. its assistant + tool rows too) made the ladder
		// NON-MONOTONE: D is entered only because C's smaller view did not fit,
		// and D's floor then sat ABOVE C's size, so the resolver shipped a view
		// larger than the one it had just rejected (measured on a 200-turn
		// fixture: C ~2000 tokens rejected at budget 356, D returned ~3075).
		// D's job is to give up FOLDED history, never to widen the tail.
		tail := TruncateOversizedMessages(all[max(lastUser, 0):min(lastUser+1, len(all))])
		folded := foldableRange(all, pairEnd, lastUser)

		// Precompute the drop sequence: variants[k] is the folded set after
		// giving up the k OLDEST turns. Building it copies message SLICES (cheap);
		// the expensive part is the megacave render, which must NOT run per step.
		//
		// The linear loop re-rendered the excerpt of the WHOLE remaining set on
		// every step — O(N^2) in characters. Measured: a 500-turn fixture spent
		// 147s in stage D; the live 10.5M-char conversation would spend minutes in
		// the request path. The fit predicate is monotone in rows dropped (megacave
		// ratio ~0.107, so total = W - 0.893*folded), so "smallest k that fits" is
		// a binary search over log2(N) renders instead of N of them.
		variants := [][]utils.Message{folded}
		for {
			prev := variants[len(variants)-1]
			next := dropOldestTurn(prev)
			if len(next) >= len(prev) {
				break // nothing left to give up (one turn, or only non-user rows)
			}
			variants = append(variants, next)
		}

		// Every variant is a suffix-shaped subset, so the verbatim tail is the
		// same for all of them; only the summary changes.
		candOf := func(k int) []utils.Message {
			var cand []utils.Message
			art := megacaveArtifactWithDropMarker(variants[k], "megacave_D", prior, len(folded)-len(variants[k]))
			if art != nil {
				cand = append(cand, art...)
			}
			return append(cand, tail...)
		}

		lo, hi, found := 0, len(variants)-1, -1
		for lo <= hi {
			mid := (lo + hi) / 2
			if fits(candOf(mid)) {
				found = mid
				hi = mid - 1
			} else {
				lo = mid + 1
			}
		}
		if found < 0 {
			// Nothing fits, not even the smallest summary. Keep the largest fold:
			// the live tail stays verbatim, the summary keeps what it can, and the
			// marker records the loss. (Never return an empty view — the caller's
			// `len(convTail) > 0` guard would then send the FULL history, defect
			// #161.)
			found = len(variants) - 1
		}
		droppedRows := len(folded) - len(variants[found])
		candidate = candOf(found)
		utils.Log("[Eco/stageD] probed %d drop steps -> kept %d of %d folded rows (dropped %d) -> view %d msgs ~%d tokens (budget %d, fits=%v)",
			len(variants), len(variants[found]), len(folded), droppedRows, len(candidate), approxTokens(candidate), budgetTokens, fits(candidate))
		id = 4
		foldFrom = lastUserIdxSafe(all)
		if approxTokens(candidate) > budgetTokens {
			utils.Log("[Eco] stage D: budget %d tokens — summary of %d rows + live tail (~%d) is the smallest foldable view; %d rows dropped from the summary",
				budgetTokens, len(variants[found]), approxTokens(tail), droppedRows)
		}
	}

	// Bound the view we are about to send, in EVERY stage, with the same
	// transform the estimator measures (boundedWireChars). Stages A/B/C/D keep
	// the newest user message verbatim, so an oversized newest message survives
	// every stage unchanged; without this the resolver returns a view the
	// preemptive context guard rejects, and the loop settles into
	// guard -> compact (folds 0) -> retry -> send on every single message.
	candidate = TruncateOversizedMessages(candidate)

	// Never fold the whole history and never fold the checkpoint pair: an empty
	// live tail is how the caller ends up shipping the FULL uncompressed
	// history (its `len(convTail) > 0` guard), and folding the pair destroys the
	// rolling summary. foldFrom must land strictly after the pair and at or
	// before the newest user turn.
	if floor := lastUserIdxSafe(all); foldFrom > floor {
		foldFrom = floor
	}
	if foldFrom >= len(all) {
		foldFrom = lastUserIdxSafe(all)
	}
	if pairEnd >= 0 && foldFrom <= pairEnd {
		foldFrom = pairEnd + 1
	}
	if foldFrom < 0 {
		foldFrom = 0
	}
	utils.Log("[Eco] resolved staged compaction (stage %d, %d messages, ~%d tokens / budget %d, folding %d of %d, pair ends at %d)",
		id, len(candidate), approxTokens(candidate), budgetTokens, foldFrom, len(all), pairEnd)
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
