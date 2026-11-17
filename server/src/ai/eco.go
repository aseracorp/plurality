package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/textsafe"
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

// shippedSummary returns the summary text the staged resolver put into the view
// it decided to send, or "" when the view carries no checkpoint artifact.
//
// The durable fold must store THIS text, not one it composes itself. The
// resolver sizes the carried-forward roll-up to what the content budget leaves
// (stage D shrinks it); the fold recomposes prior + UNCAPPED excerpt, which is
// strictly larger than the request the resolver measured as fitting. Storing the
// self-composed text makes the conversation come back from the DB BIGGER than
// the request that was just sent, so the fold reports progress while every later
// request re-inflates — the "eco compaction never kicks in, the conversation
// grew to the context size and then failed to continue" report, plus the
// unbounded checkpoint growth (16 pairs, 61-68 KB each) seen on the live
// conversation.
func shippedSummary(view []utils.Message) string {
	for _, m := range view {
		if m.Role == "tool" && strings.HasPrefix(m.Name, "megacave_") {
			return strings.TrimSpace(m.TextContent())
		}
	}
	return ""
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
	carried := strings.TrimSpace(carryForward(prior, newExcerpt))
	if droppedRows > 0 {
		marker := strings.TrimSpace(oldestDroppedMarker(droppedRows))
		if carried == "" {
			// Nothing to summarise AND nothing carried, yet rows were given up.
			// The marker is the whole content, so it unavoidably sits at offset
			// 0 — acceptable because there is no roll-up whose prefix the backend
			// has already prefilled, and the pair must exist: the durable fold
			// marks those rows compacted = 1, so a nil artifact here deletes them
			// with no trace.
			carried = marker
		} else {
			carried = carried + "\n\n" + marker
		}
	}
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
	const toolOverheadTokens = 40 // noise margin for the injected excerpt pair

	fits := func(msgs []utils.Message) bool {
		return approxTokens(msgs)+toolOverheadTokens <= budgetTokens
	}

	// Whole history within budget: return it UNCHANGED. This keeps the
	// below-trigger path byte-stable across turns (KV prompt cache hits).
	if fits(all) {
		utils.Log("[Eco] staged resolver: full history fits (≈%d / %d tokens) — unchanged, stable prefix", approxTokens(all), budgetTokens)
		return TruncateOversizedMessages(all), 0
	}

	// The checkpoint pair heads the LLM view and carries the rolling summary.
	// It is NEVER a fold target: folding it destroys the summary of everything
	// it stands for and rewrites the first ~65 KB of every prompt, collapsing
	// backend KV prefix reuse to the system block.
	pairEnd := checkpointPairEnd(all)
	prior := priorSummaryFromView(all, pairEnd)

	// THE LIVE TAIL IS NOT A FOLD TARGET.
	//
	// The agentic LLM-facing view is [pair, user, assistant, tool, assistant,
	// tool, ...]: ONE user row per user message, then a wall of assistant/tool
	// rows. The newest user message therefore sits at index 2, and every row
	// AFTER it is the in-flight working context the agent is reasoning over.
	//
	// Stages B/C/D used to define the tail as "the newest user message" and
	// send that single row. That is the collapse-to-floor measured on
	// conversation 6751a6d5ac78c156d07fec21: a 125807-token request became a
	// flat 24289-token request and stayed there for 97 consecutive turns —
	// system + tool schemas + the stale checkpoint + ONE user row, zero live
	// history. The resolver logged "3 messages, ≈20151 tokens ... folding 2 of
	// 166": it folded the PAIR and threw away 164 rows of working context.
	//
	// Every stage therefore pins the tail to the start of the newest user turn.
	// Only rows strictly between the pair and that boundary are foldable, and
	// when that range is empty the shrinkable object is the summary — never the
	// tail (see stage D for the graded tail fallback).
	tailStart := startOfLastTurn(all)
	if tailStart >= 0 && tailStart <= pairEnd {
		tailStart = pairEnd + 1
	}
	if tailStart < 0 {
		tailStart = 0
	}
	// Bound the tail the same way the sender does (TruncateOversizedMessages):
	// measuring raw bytes of a row the send cap will clamp anyway demands a
	// fold that cannot shrink it (the 991440-char pasted-logfile wedge).
	tail := TruncateOversizedMessages(all[tailStart:])
	tailTokens := approxTokens(tail)
	foldedAll := foldableRange(all, pairEnd, tailStart)

	// THE FOLD TARGET — and the wedge this closes.
	//
	// tailStart is only a LEGAL durable boundary when something actually sits
	// between the pair and the live tail. On the agentic shape the checkpoint
	// pair IS the first live row, so foldableRange(pairEnd, tailStart) is EMPTY
	// and tailStart is the newest user row, with the pair at tailStart-2. The
	// durable fold resolves the boundary to that user row, and
	//   UPDATE messages SET compacted = 1 WHERE seq < boundarySeq AND compacted = 0
	// can then only match ZERO rows: everything below the pair is already
	// compacted = 1, and the pair itself is excluded by pairIDs. Measured live:
	// 216 + 51 consecutive "DurableCompact ... folded 0 rows" lines, one per
	// message, across the 29 of 47 conversations whose live pair heads the view.
	//
	// A fold that cannot remove a row must not be requested at all, so the
	// stages report foldTarget = 0: the durable fold is never called, so the
	// pair keeps its row id, its seq and its position, no live row is shifted
	// by +2, and the prompt head is not rewritten.
	//
	// This closes the CHURN, not the fit. When the range is empty the roll-up is
	// the only shrinkable object, so stage D shrinks it and the shipped view
	// carries text SHORTER than what is stored. Until that shrink is persisted
	// the head still moves every turn (see persistRolledUpShrinkIfNoProgress,
	// change (ii)) — reporting 0 alone would only make the log quieter.
	foldTarget := 0
	if len(foldedAll) > 0 {
		foldTarget = tailStart
	}

	var candidate []utils.Message
	id := 0
	foldFrom = 0
	// represented = how many rows of the FOLD SET the shipped summary renders.
	// Stage A renders all of them; every later stage renders a subset, and a
	// summary that renders a strict subset is NOT authorised to fold the rows it
	// left out (see the gate at the end of the resolver).
	represented := 0

	// STAGE A — newest user turn verbatim + UNCAPPED megacave of everything
	// between the pair and that turn, carried forward on the prior roll-up.
	{
		art := megacaveArtifact(foldedAll, "megacave_A", prior)
		candidate = append(append([]utils.Message{}, art...), tail...)
		id = 1
		foldFrom = foldTarget
		// Stage A renders the WHOLE fold set — the fully-represented shape.
		represented = len(foldedAll)
		utils.Log("[Eco/stageA] folded %d rows -> view %d msgs ~%d tokens (budget %d, tail %d rows ~%d tokens, fits=%v)",
			len(foldedAll), len(candidate), approxTokens(candidate), budgetTokens, len(tail), tailTokens, fits(candidate))
	}

	countUserRows := func(msgs []utils.Message) int {
		n := 0
		for i := range msgs {
			if msgs[i].Role == "user" {
				n++
			}
		}
		return n
	}

	// searchVariants picks the SMALLEST number of oldest-folded-turns dropped
	// that makes the view fit, and renders it. variants[k] is the folded set
	// after giving up the k oldest turns; each is a suffix-shaped subset, so the
	// verbatim tail is identical for all of them and only the summary changes.
	//
	// The fit predicate is monotone in rows dropped (the megacave transform runs
	// at ratio ~0.107 on a real 10.5M-char conversation, so total = W - 0.893 *
	// folded), which makes "smallest k that fits" a binary search over log2(N)
	// megacave renders. The linear loop re-rendered the excerpt of the WHOLE
	// remaining set on every step — O(N^2) in characters: a 500-turn fixture
	// spent 147 s in stage D, and the live conversation would spend minutes in
	// the request path.
	// allowEmpty: false keeps at least one folded turn in the summary (stage B), so
	// stage C (which reduces the SAME set further, by dropping tool rows)
	// remains reachable at a smaller budget. Without it B's floor equals C's floor,
	// C can never fit, and the stage silently dies.
	// Returns the view for the SMALLEST number of dropped turns that fits, and
	// whether any variant fit at all. When none fits it returns the FLOOR
	// variant (the smallest allowed one) and false — never nil. A nil return
	// here means a view with no summary artifact, and the caller still has
	// foldFrom = tailStart, so the durable fold would mark every folded row
	// compacted = 1 with NOTHING representing them: the context-loss bug wearing
	// a fallback costume.
	searchVariants := func(folded []utils.Message, tag string, allowEmpty bool, reduce func([]utils.Message) []utils.Message) ([]utils.Message, bool, int) {
		variants := [][]utils.Message{folded}
		for {
			prev := variants[len(variants)-1]
			next := dropOldestTurn(prev)
			if len(next) >= len(prev) {
				break // nothing left to give up (one turn, or only non-user rows)
			}
			variants = append(variants, next)
		}
		if reduce != nil {
			// Secondary axis: shrink each variant by dropping tool rows from the
			// excerpt (stage C). Applied to EVERY variant so the ladder stays
			// monotone in k, and the variant list is de-duplicated (the reduction
			// of a one-turn set and of its empty tail can coincide).
			seen := map[string]bool{}
			reduced := make([][]utils.Message, 0, len(variants))
			for _, v := range variants {
				r := reduce(v)
				key := fmt.Sprintf("%d|%d", len(r), countUserRows(r))
				if seen[key] {
					continue
				}
				seen[key] = true
				reduced = append(reduced, r)
			}
			if len(reduced) > 0 {
				variants = reduced
			}
		}
		// candOf renders variant k and RECORDS how many fold-set rows that
		// variant's summary represents (the rows it renders, on the same basis as
		// the variant ladder). The authorization gate at the end of the resolver
		// compares this against len(foldedAll): a summary that renders a strict
		// subset of the fold set is not authorised to fold the rows it left out.
		candOf := func(k int, record *int) []utils.Message {
			art := megacaveArtifactWithDropMarker(variants[k], tag, prior, len(folded)-len(variants[k]))
			if record != nil {
				*record = len(variants[k])
			}
			return append(append([]utils.Message{}, art...), tail...)
		}
		hi := len(variants) - 1
		if !allowEmpty && hi > 0 {
			hi-- // keep at least one folded turn represented
		}
		// fullRender is what the FULL fold set renders — the representation the
		// fold is authorised to persist. Every variant renders the same prior
		// roll-up, so comparing a chosen variant's excerpt length against it is a
		// direct measure of how much of the fold set the shipped summary covers.
		var fullRender int
		candOf(0, &fullRender)
		lo, found := 0, -1
		for lo <= hi {
			mid := (lo + hi) / 2
			if fits(candOf(mid, nil)) {
				found = mid
				hi = mid - 1
			} else {
				lo = mid + 1
			}
		}
		chosen := found
		if found < 0 {
			chosen = hi // floor: smallest allowed variant, no fit
		}
		view := candOf(chosen, &represented)
		if found < 0 {
			return view, false, represented
		}
		return view, true, represented
	}

	if !fits(candidate) {
		// STAGE B — same tail, give up the OLDEST folded turns until the summary
		// fits. B differs from A only in how much folded history the summary
		// keeps; it does not touch the tail. Giving the tail up is what
		// destroyed the context, and it never rescued the budget either: on the
		// agentic shape the fold range is empty and the summary is the only
		// large object left.
		cand, fitB, repB := searchVariants(foldedAll, "megacave_B", false, nil)
		candidate = cand
		represented = repB
		_ = fitB
		id = 2
		foldFrom = foldTarget
		utils.Log("[Eco/stageB] folded set %d rows -> view %d msgs ~%d tokens (budget %d, tail %d rows ~%d tokens, fits=%v)",
			len(foldedAll), len(candidate), approxTokens(candidate), budgetTokens, len(tail), tailTokens, fits(candidate))
	}
	if !fits(candidate) {
		// STAGE C — summary reduced to user/assistant rows only, same tail.
		//
		// The newest user message stays VERBATIM in the tail. Megacaving every
		// user row into a tool message (the old C shape) leaves the view with
		// zero role:"user" rows and every chat template rejects that: strata
		// answers 400 "No user query found in messages". Measured end-to-end
		// against the live engine, stages A/B/D cache at 98-99.9 % reuse and
		// stage C alone was an outright request failure — the stage-C unit tests
		// passed because they only asserted an artifact exists. See
		// TestResolver_EveryStageIsSendable.
		candC, _, repC := searchVariants(foldedAll, "megacave_C", true, userOnlyWithLastAssistant)
		candidate = candC
		represented = repC
		id = 3
		foldFrom = foldTarget
		utils.Log("[Eco/stageC] summary reduced to user/assistant rows -> view %d msgs ~%d tokens (budget %d, fits=%v)",
			len(candidate), approxTokens(candidate), budgetTokens, fits(candidate))
	}
	if !fits(candidate) {
		// STAGE D — the last resort, in three graded steps, TAIL LAST.
		//
		// 1. Give up folded turns (same search as B, which can reach the
		//    prior-roll-up-only floor).
		// 2. Shrink the carried-forward roll-up by keeping its NEWEST bytes and
		//    deleting the OLDEST ones off the FRONT. This is the only lever left
		//    on the agentic shape, where the fold range between the pair and the
		//    newest user turn is empty and the summary is the only large object.
		//
		//    Naming note (#182 kept the tested direction; this comment stops
		//    lying about it): tailOfSummary keeps the NEWEST bytes, so it cuts
		//    the FRONT of the text. That is the opposite of "cutting the END",
		//    which this comment used to claim — and the claim is what made the
		//    shrink look cache-cheap by construction. Cutting the front of the
		//    object that sits at offset 0 of the prompt DOES move the prompt
		//    head. The shrink is cache-cheap only when it is (a) bounded per
		//    pass and (b) quantised (#182), AND (c) PERSISTED, so the same bytes
		//    ship turn after turn — see persistRolledUpShrinkIfNoProgress.
		//    Unpersisted, the shrink is recomputed against a tail that grows
		//    every turn, so the head moves once per ~12 KiB of growth and the
		//    quantisation becomes a re-prefill cadence instead of a brake.
		// 3. Only if the LIVE TAIL alone is larger than the budget: keep the
		//    newest user row plus the newest complete tool pairs. This degrades
		//    the tail GRADUALLY, never to "one user row and nothing else".
		//
		// Capping the excerpt is NOT the contract and cannot work: the excerpt
		// saturates at the cap, so `fits` can never go true below that floor.
		// The old greedy loop drained the slice to nil, megacaveArtifact(nil)
		// returned nil, and the resolver emitted an EMPTY history, which the
		// caller's `len(convTail) > 0` guard then replaced with the FULL
		// UNCOMPACTED history (defect #161).
		var droppedChars int
		var droppedTailRows int
		var keptTail []utils.Message
		var keptPrior = prior
		if cand, fitD, repD := searchVariants(foldedAll, "megacave_D", true, nil); fitD {
			candidate = cand
			represented = repD
		} else {
			// Step 2: fit the view. On the agentic shape the fold range between the
			// pair and the newest user row is EMPTY, so the only two objects left
			// are the roll-up and the live tail — and they are NOT equal levers:
			// trimming the TAIL is reversible (those rows stay compacted = 0 and
			// return next request), shrinking the ROLL-UP is permanent (it is the
			// only representation of every row earlier folds marked compacted = 1).
			//
			// The old loop spent the permanent lever FIRST, by arithmetic, with no
			// floor. Live proof, conversation 141c6b2d33616fed6f2dd8dc 15:37:09:
			//
			//	[Eco/stageD] ... summary -101522 chars, tail -0 rows, fits=false
			//
			// 101522 of a 105366-char roll-up (96 %) deleted, tail untouched, and
			// the request STILL missed the budget: permanent loss that bought
			// nothing. Cutting the FRONT of the roll-up also moves the first bytes
			// after the system block, and the engine for that turn logged
			// `prompt 133973 = 24576 reused + 11093 read` (24576 = the system-block
			// floor) with 14-82 s of re-prefill per turn.
			//
			// The live tail stays the LAST resort (step 3 trims it only when the
			// tail ALONE is over budget — trimming it is what made a request
			// answer with "only 27K tokens"). The roll-up is shrunk here, and
			// the fix changes only HOW: bound the loss per pass, and quantise
			// the cut so the prompt head is a step function instead of moving
			// every turn.
			const dropMarkerAllowance = 120 // room for the loss notice appended later
			render := func(kept string, keptTail []utils.Message) []utils.Message {
				return append(append([]utils.Message{},
					mustCheckpointArtifact(foldedAll, "megacave_D", kept,
						len(foldedAll), len(prior)-len(kept), len(tail)-len(keptTail))...), keptTail...)
			}
			// 2a. Shrink the roll-up: loss bounded PER PASS, cut quantised.
			keptPrior, keptTail = prior, tail
			candidate = render(keptPrior, tail)
			if !fits(candidate) && len(keptPrior) > 0 {
				for pass := 0; pass < 8; pass++ {
					if fits(candidate) {
						break
					}
					want := len(keptPrior) - (approxTokens(candidate)-budgetTokens)*3
					maxDelete := len(keptPrior) * rollupLossPerPassPercent / 100
					if len(keptPrior)-want > maxDelete {
						want = len(keptPrior) - maxDelete
					}
					want -= want % shrinkQuantumChars
					if want >= len(keptPrior) {
						want = len(keptPrior) - shrinkQuantumChars
					}
					if want <= 0 {
						break
					}
					shrunk := tailOfSummary(keptPrior, want)
					if len(shrunk) >= len(keptPrior) {
						break
					}
					keptPrior = shrunk
					droppedChars = len(prior) - len(keptPrior)
					candidate = render(keptPrior, tail)
				}
			}
			keptTail = tail

			// Step 3 gate: the LIVE TAIL ALONE, beside the SMALLEST artifact the
			// resolver can emit (the empty-roll-up banner), is over budget. That
			// is a physical constraint — and the only situation in which trimming
			// the tail buys the request anything.
			minArt := mustCheckpointArtifact(foldedAll, "megacave_D", "", 0, 0, 0)
			tailAlone := approxTokens(append(append([]utils.Message{}, minArt...), tail...))
			if len(tail) > 1 && tailAlone+toolOverheadTokens > budgetTokens {
				// Step 3: the LIVE TAIL alone is over budget. Keep the newest
				// user row and as many recent rows as fit — a GRADED trim, never
				// the collapse to "one user row and nothing else". The trimmed
				// rows are NOT durable-folded (they stay compacted = 0), so this
				// is ephemeral: the next turn re-includes them. Keeping the prior
				// summary intact matters here: it is the only representation of
				// everything folded in earlier rounds, and trimming the tail
				// already gives the model the room it needs.
				artOnly := megacaveArtifact(nil, "megacave_D", keptPrior)
				// The summary must be shrunk to what the SHIPPED tail leaves, not
				// to what the full tail leaves. Step 2 sized the summary against
				// the whole tail; here the tail is about to be trimmed, and if the
				// summary is left at its step-2 size a small budget ships an
				// oversized request: measured at budget 200 with a 30000-char
				// roll-up, step 2 computed roomChars <= 0 (the tail alone is over
				// budget) so it skipped the shrink, step 3 trimmed the tail to the
				// anchor and the view left the resolver at ~10078 tokens / budget
				// 200 — the guard -> compact -> send-oversized loop this stage
				// exists to close. Size the roll-up against the ANCHOR (the newest
				// user row, the smallest tail that can ship), then spend what is
				// left on the tail.
				anchorTokens := approxTokens(tail[:1])
				sumRoomChars := (budgetTokens - toolOverheadTokens - dropMarkerAllowance - anchorTokens) * 3
				if sumRoomChars < 0 {
					sumRoomChars = 0
				}
				if len(keptPrior) > sumRoomChars {
					// Quantised: the summary is the first content after the
					// system block, and this path runs on EVERY turn above
					// budget. Cutting an exact-fit amount moved the head every
					// single turn, so prefix reuse collapsed to the system block
					// on every turn (measured live: `reused 24576` of a ~130k
					// prompt, 32-82 s of re-prefill each). Snapping the room to a
					// quantum makes the head a step function: it moves once per
					// band, and the turns inside a band are byte-identical.
					keptPrior = tailOfSummary(keptPrior, sumRoomChars-sumRoomChars%shrinkQuantumChars)
					droppedChars = len(prior) - len(keptPrior)
				}
				artOnly = megacaveArtifact(nil, "megacave_D", keptPrior)
				roomTokens := budgetTokens - toolOverheadTokens - dropMarkerAllowance - approxTokens(artOnly)
				keptTail, droppedTailRows = tailBudgeted(tail, roomTokens)
				art := mustCheckpointArtifact(foldedAll, "megacave_D", keptPrior, len(foldedAll), droppedChars, droppedTailRows)
				candidate = append(append([]utils.Message{}, art...), keptTail...)
				// Converge: tailBudgeted is a step function in rows and the marker
				// text grows with the counts it names, so the last measurement can
				// still overshoot. Give the overshoot back to the summary, bounded
				// (each pass strictly shrinks it, so this terminates).
				for pass := 0; pass < 4 && len(keptPrior) > 0; pass++ {
					over := approxTokens(candidate) - budgetTokens
					if over <= 0 {
						break
					}
					shrunk := tailOfSummary(keptPrior, len(keptPrior)-over*3)
					if len(shrunk) >= len(keptPrior) {
						break
					}
					keptPrior = shrunk
					droppedChars = len(prior) - len(keptPrior)
					art = mustCheckpointArtifact(foldedAll, "megacave_D", keptPrior, len(foldedAll), droppedChars, droppedTailRows)
					candidate = append(append([]utils.Message{}, art...), keptTail...)
				}
				utils.Log("[Eco] stage D step 3: the live tail ALONE (~%d tokens with the smallest artifact) is over budget %d — keeping the newest user row + %d of %d tail rows, roll-up shrunk to %d chars (ephemeral tail trim, history intact)",
					tailAlone, budgetTokens, len(keptTail), len(tail), len(keptPrior))
			}
		}
		id = 4
		foldFrom = foldTarget
		utils.Log("[Eco/stageD] view %d msgs ~%d tokens (budget %d, tail %d rows ~%d tokens, summary -%d chars, tail -%d rows, fits=%v)",
			len(candidate), approxTokens(candidate), budgetTokens, len(tail), tailTokens, droppedChars, droppedTailRows, fits(candidate))
	}

	// THE AUTHORIZATION GATE — the constraint the design was missing.
	//
	// A stage's shape may be PERSISTED only when the summary it ships
	// represents every row the durable fold is about to mark compacted. The
	// stored summary is the ONLY representation of a compacted row: the client
	// still sees the full history, GetConversationByIdInternal does not. So a
	// fold that marks rows the shipped summary does not render is not
	// compaction, it is deletion from every future request.
	//
	// Live proof (conversation 9658ae45a32388ed1dafaac1, 2026-11-17 19:41:30):
	//
	//	stageA folded 232 rows -> ~174088 tokens (fits=false)
	//	stageB folded set 232 rows -> ~174088 tokens (fits=false)
	//	stageC summary reduced to user/assistant rows -> ~1384 tokens (fits=true)
	//	resolved staged compaction (stage 3, folding 232 of 233)
	//	durable compaction folded 232 messages
	//
	// Stage C's reduction is sanctioned by the spec for the REQUEST (95 %). It is
	// not sanctioned as a DURABLE state: the fold marked 232 rows compacted from
	// a ~1384-token summary, i.e. 230 rows removed with no representation
	// anywhere. That is the reported annihilation, and it is legal under every
	// rule the resolver had — which is exactly why the rule has to be here.
	//
	// So: when the shipped summary renders a strict subset of the fold set, the
	// fold is NOT authorised. foldFrom goes to 0, the caller ships the staged
	// view EPHEMERALLY (this request only, the rows stay compacted = 0 and come
	// back next turn) and the durable state is left exactly as it was. The
	// request still gets its budget; the history is not destroyed to buy it.
	//
	// This makes the destructive path a deliberate choice instead of a side
	// effect, and it is the safety net that lets stages A-D be built out freely.
	if foldFrom > 0 && represented < len(foldedAll) {
		utils.Log("[Eco] authorization gate: shipped summary represents %d of %d fold-set rows (stage %d) — shipping EPHEMERAL, NOT folding (folding %d rows would delete %d unrepresented rows)",
			represented, len(foldedAll), id, len(foldedAll), len(foldedAll)-represented)
		foldFrom = 0
	}

	// Bound the view we are about to send, in EVERY stage, with the same
	// transform the estimator measures (boundedWireChars). Stages keep the
	// newest user message verbatim, so an oversized newest message survives
	// every stage unchanged; without this the resolver returns a view the
	// preemptive context guard rejects, and the loop settles into
	// guard -> compact (folds 0) -> retry -> send on every single message.
	candidate = TruncateOversizedMessages(candidate)

	// Never fold the whole history and never fold the checkpoint pair: an empty
	// live tail is how the caller ends up shipping the FULL uncompressed
	// history (its `len(convTail) > 0` guard), and folding the pair destroys the
	// rolling summary. foldFrom must land strictly after the pair and at the
	// start of the live tail.
	// The clamps below exist to keep a REAL fold legal: strictly after the pair,
	// never past the newest user turn, never the whole history. They must not
	// re-inflate a deliberate 0 ("nothing foldable") back into pairEnd+1 — that
	// is exactly the boundary that folds 0 rows, so the clamp would re-arm the
	// wedge the caller just closed.
	if foldFrom > 0 {
		if floor := lastUserIdxSafe(all); foldFrom > floor {
			foldFrom = floor
		}
		if foldFrom >= len(all) {
			foldFrom = lastUserIdxSafe(all)
		}
		if pairEnd >= 0 && foldFrom <= pairEnd {
			foldFrom = pairEnd + 1
		}
	}
	if foldFrom < 0 {
		foldFrom = 0
	}

	utils.Log("[Eco] resolved staged compaction (stage %d, %d messages, ~%d tokens / budget %d, folding %d of %d, pair ends at %d)",
		id, len(candidate), approxTokens(candidate), budgetTokens, foldFrom, len(all), pairEnd)
	return candidate, foldFrom
}

// persistRolledUpShrinkIfNoProgress latches the stage-D roll-up shrink into the
// stored checkpoint pair when the durable fold made NO progress.
//
// THE CADENCE PROBLEM THIS CLOSES. Above the eco trigger, stage D shrinks the
// carried-forward roll-up so the request fits. The shrink is computed against
// the LIVE TAIL, which grows every turn, so the amount that has to be cut moves
// every turn — and until now the shrunk text was thrown away at the end of the
// request. The next turn therefore re-loaded the FULL stored roll-up and
// re-shrank it from scratch, which moves the front of the object sitting at
// offset 0 of the prompt (tailOfSummary keeps the NEWEST bytes). The backend can
// then only reuse the prefix up to the last checkpoint before the cut — 8192 /
// 16384 / 24576, exactly the floor family in the engine log — so the shrink
// turns into a re-prefill cadence: one full re-prefill per ~12 KiB of tail
// growth.
//
// Persisting the shrink makes the stored roll-up a monotone step function: once
// cut, those bytes stay cut, so the SAME bytes ship turn after turn and prefix
// reuse extends THROUGH the summary instead of stalling at 24576.
//
// GATE — the latch runs ONLY when the fold made no progress (foldFrom == 0,
// folded == 0). When a fold did happen, CompactConversationDurableWithSummary
// already stored the shipped summary VERBATIM, including the rowsBeyondBoundary
// material appended by representExtraRows; the roll-up body is only PART of
// that text, so latching it after a real fold would delete rows the stored
// summary represents. That is context loss, and it is the one thing this
// function must never do.
//
// The write is advisory: a failure costs the cache, never the request, so the
// error is logged and swallowed.
func persistRolledUpShrinkIfNoProgress(ctx context.Context, conversationID string, view []utils.Message, foldFrom, folded int) {
	if foldFrom != 0 || folded != 0 {
		return
	}
	body := rollupBodyFromShippedView(view)
	if body == "" {
		return
	}
	written, err := db.UpdateCheckpointSummaryShrink(ctx, conversationID, body)
	if err != nil {
		utils.Error("[Eco] roll-up shrink latch failed (request unaffected, cache cost only)", err)
		return
	}
	if written {
		utils.Log("[Eco] roll-up shrink latched in place (fold made no progress: %d rows at %d) — stored summary is now the shrunk text, prompt head stable",
			folded, foldFrom)
	}
}

// tailBudgeted degrades the LIVE TAIL gradually for stage D step 3: keep the
// newest user row (the task anchor) plus the most recent rows that fit.
//
// It never returns "one user row and nothing else" as a first choice — that is
// the collapse-to-floor shape (97 consecutive turns at 24289 tokens,
// conversation 6751a6d5ac78c156d07fec21). Trimming here is EPHEMERAL: the
// caller durable-folds only up to foldFrom, so the dropped tail rows stay
// compacted=0 in the DB and the next turn re-includes them. Returns the kept
// view and the number of tail rows given up (for the summary's drop marker).
func tailBudgeted(tail []utils.Message, roomTokens int) ([]utils.Message, int) {
	if len(tail) == 0 {
		return nil, 0
	}
	anchor := approxTokens(tail[:1])
	if len(tail) == 1 || roomTokens <= anchor {
		return tail[:1], len(tail) - 1
	}
	// toks[i] = tokens of the suffix tail[i:], computed in one backward pass so
	// the search stays linear (approxTokens per suffix on a 5000-row tail would
	// be quadratic).
	toks := make([]int, len(tail)+1)
	for i := len(tail) - 1; i >= 0; i-- {
		toks[i] = toks[i+1] + approxTokens(tail[i:i+1])
	}
	if toks[len(tail)-1]+anchor > roomTokens {
		// Not even the last row fits next to the anchor: keep the anchor alone.
		return tail[:1], len(tail) - 1
	}
	i := 1
	for i < len(tail) && toks[i]+anchor > roomTokens {
		i++
	}
	if i >= len(tail) {
		// Nothing beyond the anchor fits.
		return tail[:1], len(tail) - 1
	}
	// Never start the kept suffix on a tool row: its parent assistant tool_call
	// has to be in the same view or strict providers reject the request as an
	// orphan tool result. Stepping back costs a little size — a small budget
	// overrun is cheaper than a 400.
	for i > 1 && tail[i].Role == "tool" {
		i--
	}
	kept := append(append([]utils.Message{}, tail[:1]...), tail[i:]...)
	return kept, i - 1
}

// rollupLossPerPassPercent bounds how much of the roll-up ONE shrink pass may
// delete. The roll-up is the only representation of every row earlier folds
// marked compacted = 1, so deleting it is permanent context loss.
//
// Live proof (conversation 141c6b2d33616fed6f2dd8dc, 2026-11-16 15:37:09):
//
//	[Eco/stageD] ... summary -101522 chars, tail -0 rows, fits=false
//
// one pass deleted 101522 of a 105366-char roll-up (96 %) while the request
// missed the budget anyway — permanent loss that bought nothing.
//
// An ABSOLUTE floor is not the answer: a conversation whose live tail alone
// approaches the budget physically cannot carry 75 % of a 100 KB roll-up, so
// flooring the total makes the resolver ship an OVERSIZED request that dies on
// "exceeds the context window" — the failure this path exists to prevent.
// Measured with a 25 % absolute floor in the closed-loop fixture: the fold
// stopped converging and the reload sat at ~43201 tokens against a 40000
// budget.
//
// Per-pass is the enforceable, honest shape: no single fold guts the summary,
// degradation spreads across rounds, and 8 passes still reach any size the
// window physically requires.
const rollupLossPerPassPercent = 10

// shrinkQuantumChars coarsens the roll-up shrink so the PROMPT HEAD moves
// rarely. The summary is the first content after the system block, and the
// shrink path runs on every turn above budget: computing an exact-fit cut each
// time moved the head every single turn, so backend prefix reuse collapsed to
// the system block (measured live: `prompt 133973 tokens = 24576 reused +
// 11093 read`, i.e. the whole ~130k history re-prefilled, 32-82 s per turn).
// Snapping the cut to a quantum makes the head a step function — turns inside
// one band are byte-identical and keep their cache; the head moves at most
// once per band.
const shrinkQuantumChars = 12 * 1024

func tailOfSummary(prior string, maxChars int) string {
	if maxChars <= 0 || prior == "" {
		return ""
	}
	if len(prior) <= maxChars {
		return prior
	}
	cut := textsafe.TailBytes(prior, maxChars)
	// Never start mid-multibyte-character, and never start mid-line.
	if i := strings.Index(cut, "\n"); i >= 0 && i < 200 {
		cut = cut[i+1:]
	}
	return strings.TrimSpace(cut)
}

// summaryWithDropMarker renders the stage-D summary: the (possibly shrunk)
// roll-up, plus an out-loud notice of what was given up. The marker goes AFTER
// the roll-up, never in front of it — a marker whose text changes every round
// at the HEAD moves the prompt prefix and invalidates the backend KV cache
// exactly like the nested "PRIOR CHECKPOINT:" banners did.
func summaryWithDropMarker(keptPrior string, droppedRows int, droppedChars int, droppedTailRows int) string {
	keptPrior = strings.TrimSpace(keptPrior)
	var note string
	// The tail notice goes FIRST: it describes the most recent loss, and it is
	// the one that matters for continuing the current task.
	if droppedTailRows > 0 {
		note += tailTrimmedMarker(droppedTailRows)
	}
	if droppedChars > 0 {
		note += oldestCharsDroppedMarker(droppedChars)
	}
	if droppedRows > 0 {
		note += oldestDroppedMarker(droppedRows)
	}
	if keptPrior == "" {
		// The roll-up is entirely gone. The pair row is the summary of
		// everything folded in earlier rounds, so it must SAY it is empty —
		// a tail-trim notice alone leaves the model reading a checkpoint that
		// silently represents nothing (the fold marks those rows compacted=1,
		// so nothing else in the conversation carries them).
		if note == "" {
			return "[earlier history rolled into this checkpoint]"
		}
		return "[this checkpoint's roll-up of earlier history was dropped in full: the request cannot fit it]\n\n" + note
	}
	if note == "" {
		return keptPrior
	}
	return keptPrior + "\n\n" + note
}

// emptyRollUpBanners are the banners mustCheckpointArtifact renders when the
// roll-up is gone. They are meta-text ABOUT the checkpoint, never roll-up
// content, so they must never be latched into the pair row.
var emptyRollUpBanners = []string{
	"[earlier history rolled into this checkpoint]",
	"dropped in full",
}

// dropMarkerPrefixes are the leading texts of the out-loud loss notices
// summaryWithDropMarker appends AFTER the roll-up. They are derived from the
// marker formatters themselves, so a reworded marker cannot silently drift out
// of sync with the parser that has to find it.
var dropMarkerPrefixes = []string{
	markerHead(tailTrimmedMarker(0)),
	markerHead(oldestCharsDroppedMarker(0)),
	markerHead(oldestDroppedMarker(0)),
}

func markerHead(marker string) string {
	if i := strings.Index(marker, ":"); i >= 0 {
		return marker[:i+1]
	}
	return marker
}

// summaryCarrierText returns the summary text the view ships, from whichever
// carrier it uses: the staged megacave artifact when a stage built one,
// otherwise the carried checkpoint pair row (utils.EcoCheckpointToolName). A
// view that needs no compaction ships the pair row verbatim, so the latch must
// be able to read it — on that turn body == stored and the shrink guard refuses
// the write as an exact no-op. The staged artifact wins when both are present
// because it is what the request actually carries.
func summaryCarrierText(view []utils.Message) string {
	for i := range view {
		m := view[i]
		if m.Role == "tool" && strings.HasPrefix(m.Name, "megacave_") {
			return strings.TrimSpace(m.TextContent())
		}
	}
	for i := range view {
		m := view[i]
		if m.Role == "tool" && m.Name == utils.EcoCheckpointToolName {
			return strings.TrimSpace(m.TextContent())
		}
	}
	return ""
}

// rollupBodyFromShippedView returns the roll-up body the resolver decided to
// ship, with the out-loud loss markers stripped, or "" when the view carries no
// checkpoint artifact / the roll-up is gone.
//
// The durable latch must persist THIS, never shippedSummary(view). The markers
// describe a PER-REQUEST shape: tailTrimmedMarker counts live-tail rows left
// out of THIS request (they are back next turn) and oldestCharsDroppedMarker
// counts characters removed in THIS pass. Writing them into the pair row makes
// an ephemeral loss permanent and duplicates a banner that grows every turn,
// inside the single row that is the only representation of every row earlier
// folds marked compacted = 1.
//
// The markers are always appended after the roll-up — a marker at the HEAD
// moves the prompt prefix — so the body is the text before the LAST marker.
func rollupBodyFromShippedView(view []utils.Message) string {
	text := summaryCarrierText(view)
	if text == "" {
		return ""
	}
	for _, b := range emptyRollUpBanners {
		if strings.Contains(text, b) {
			return ""
		}
	}
	// The EARLIEST marker, not the last: summaryWithDropMarker appends the
	// notices in order (tail notice first), so the first marker is where the
	// marker BLOCK starts. Taking the last index would keep the earlier markers
	// inside the body — and tailTrimmedMarker counts rows dropped from THIS
	// request only, so that is exactly the per-request text that must never
	// reach the pair row.
	best := -1
	for _, p := range dropMarkerPrefixes {
		if i := strings.Index(text, "\n\n"+p); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		return text
	}
	return strings.TrimSpace(text[:best])
}

// mustCheckpointArtifact is checkpointArtifactMessages with a guarantee: the
// returned pair is NEVER nil, so a view that declares a fold (foldFrom > 0)
// always carries a summary row.
//
// This closes the worst shape the resolver can produce: the durable fold marks
// [pairEnd+1, foldFrom) compacted = 1, and if the artifact is nil there is
// nothing in the view that represents those rows. The conversation does not
// error, it just quietly forgets everything it folded — which is exactly what
// the 16 accumulated 61-68 KB checkpoint pairs and the 97-turn 24289-token
// flatline describe. An empty roll-up with rows given up renders as a banner
// that says so, which is honest and small.
func mustCheckpointArtifact(folded []utils.Message, tag, keptPrior string, droppedRows, droppedChars, droppedTailRows int) []utils.Message {
	text := summaryWithDropMarker(keptPrior, droppedRows, droppedChars, droppedTailRows)
	if strings.TrimSpace(text) == "" {
		text = "[earlier history rolled into this checkpoint]"
	}
	return checkpointArtifactMessages(folded, tag, text)
}

// tailTrimmedMarker states that the LIVE tail was trimmed for this request only.
//
// It is a separate sentence from oldestDroppedMarker on purpose: the folded rows
// are durably marked compacted=1 and are gone from the summary permanently,
// while trimmed tail rows stay compacted=0 in the DB and come back next turn.
// Conflating the two into one number is a false audit trail of context loss.
func tailTrimmedMarker(droppedTailRows int) string {
	return fmt.Sprintf("[live context trimmed for this request: the %d most recent tool/assistant rows of the current turn were left out of this request to fit the context window. They are still in the conversation and will be included again.]\n\n", droppedTailRows)
}

// oldestCharsDroppedMarker names how much of the ROLL-UP summary stage D had to
// drop. oldestDroppedMarker counts folded messages; when the fold range is
// empty the loss is measured in characters of the summary itself, and the model
// must be told it is reading a truncated roll-up.
func oldestCharsDroppedMarker(droppedChars int) string {
	return fmt.Sprintf("[oldest context dropped: the oldest %d characters of this conversation's summary were removed — the request cannot fit them. The remaining text is the NEWEST part of that summary.]\n\n", droppedChars)
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
//
// A set with NO user row is returned UNCHANGED. It used to return msgs[:0],
// which made the variant ladder collapse in ONE step: measured on the agentic
// shape (all user rows live in the newest user turn, so the foldable set is
// assistant/tool + the prior summary only), a 4-row set became a 0-row set, and
// stages C/D pass allowEmpty=true, so they landed on that empty variant, shipped
// a drop-marker-only summary (~1384 tokens), reported fits=true, and the durable
// fold marked all 232 rows compacted. That is the annihilation, and it came from
// this one line.
//
// "Nothing left to give up on this axis" is the caller's signal to stop (the
// ladder loop breaks when a variant is not strictly smaller), so it must be
// expressed as an unchanged slice, never as an empty one.
func dropOldestTurn(msgs []utils.Message) []utils.Message {
	firstUser := -1
	for i := range msgs {
		if msgs[i].Role == "user" {
			firstUser = i
			break
		}
	}
	if firstUser < 0 {
		return msgs
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
