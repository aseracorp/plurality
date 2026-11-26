package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/textsafe"
	"github.com/azukaar/plurality/src/utils"
)

// summaryInFlight is the global single-flight gate for eco-mode summary
// jobs. Only one summary runs across the whole server at any time;
// conversations that miss their window pick up on the next user turn.
var summaryInFlight atomic.Bool

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
//   - ecoOn=true (the frozen-prefix path): keep the ONE checkpoint pair
//     verbatim and append every live row after it. NO sliding char cap on
//     this path — durable compaction is what bounds the view now.
//
// Why the sliding cap had to go: capTail walked backwards from the newest
// row, accumulated ~100k chars and then backed up to a user row, so the
// START index of the sent history moved on (almost) every turn. KV prompt
// caching (llama.cpp/avalon/strata, and the cloud providers' prefix caches)
// matches the longest common PREFIX of the previous request's token stream,
// so a head that starts one message later every turn invalidates the whole
// cached prefix: cache reuse collapsed to the system block and each agent
// turn re-prefilled tens of thousands of tokens. The pair is the first
// content after the system block, so keeping it + everything after it,
// unbounded, is what makes the prefix stable: rows are only ever APPENDED
// between folds, and a fold is a durable, infrequent, machine-written event.
//
// The only transform left on the normal path is the per-row bound inside the
// fold render — the request path applies none:
// PER-MESSAGE bound: each row is bounded as a pure function of that stored
// row (its own length + its own truncation banner), never of the slice
// total or a window position, so it is byte-stable across turns.
//
// The system prompt is prepended elsewhere (in SendChatCompletion) and is
// not part of `messages` here.
// windowTokens is the selected model's context window (0 = unknown), resolved
// by the caller for the preemptive guard and the durable fold. It no longer
// sizes a per-request cap: the sent view is bounded per MESSAGE here, and the
// guard turns "this view no longer fits the window" into a durable fold.
func filterCheckpointsForRequest(messages []utils.Message, ecoOn bool, windowTokens ...int) []utils.Message {
	// AUTO-COMPACTION OFF: the user asked for the raw history, so the pair is
	// dropped and the model reads every stored row. Compaction is off — no tick,
	// no preemptive guard.
	//
	// One exception, and it is data loss: a pair EXISTS only if this conversation
	// has already been folded, and it gets folded even with auto-compaction off,
	// because the context-overflow fallback must fire on a request the provider
	// rejects (the alternative is a dead turn, every turn). The LLM loader excludes
	// rows flagged compacted, so dropping the pair here removes the ONLY record of
	// those rows: the sent view shrinks by the size of the fold and the model
	// answers with the folded history simply gone, no summary replacing it. That
	// is silent context loss — the exact failure this subsystem exists to prevent.
	//
	// So: no pair -> raw history, as asked. Pair -> ship the frozen view
	// (pair + live rows), because that pair is what the folded rows became.
	if !ecoOn {
		if !hasCheckpointPair(messages) {
			return dropOrphanToolRows(db.FilterOutCheckpoints(messages))
		}
		utils.Log("[Eco] conv has eco mode off but a durable fold exists: shipping the frozen view, "+
			"because the %d rows that fold flagged compacted are excluded by the loader and the pair is "+
			"their only remaining representation", countCompactedRows(messages))
	}

	// THE SLICE IS NOT THE LLM VIEW. Every caller of this function hands it a
	// slice, and the row set behind that slice is NOT guaranteed to exclude the
	// rows durable compaction folded (compacted = 1). It is for the loop's
	// reload path (db.GetConversationByIdInternal adds "AND compacted = 0"),
	// it is NOT for the path that STARTS a turn: handleChat pushes the user
	// message with db.PushMessage, which reloads through
	// getConversationFromDB's DEFAULT row set — folded rows INCLUDED — and that
	// slice is what RunLLMLoop ships on the turn's first request.
	//
	// The frozen branch slices from the pair, and the pair sits at seq -2/-1
	// while folded rows carry seq 0..N, i.e. folded rows sit AFTER the pair.
	// Slicing from the pair therefore re-shipped the entire folded history:
	//
	//	21:58:35 PERF msgs=557 tok_before=241327 cache_read=21521
	//	  (the post-fold LLM view of this conversation is 171 rows)
	//	21:59:50 [Eco] lastPT=241327 -> 241649 > trigger=216000 — compacting
	//	21:59:50 [Eco] cutoff=171 (of 175 messages) — folds 169 of 175
	//	22:00:35 [Eco] LLM view 111080 -> 34125 tokens (view rows 6)
	//
	// Three consequences, all of them the bug being reported: the prompt
	// re-inflates to the full history, so lastPT (the provider's own count)
	// measures a view the frozen path never intends to ship and the eco trigger
	// fires on a phantom; findCutoffIndex's cumulative-PT arithmetic is then
	// taken against rows whose prompt_tokens came from those fat requests, so
	// targetDrop sits below the whole PT floor and the cutoff lands on the
	// newest row — fold everything; and the fold rewrites the pair, which is
	// the head of the prompt, so the KV prefix collapses to
	// cache_read=21521 (system block + tool schemas).
	//
	// Dropping folded rows HERE, at the one request-path choke point, makes the
	// frozen view correct for every slice shape (client view, internal view,
	// post-push view) and keeps the measure the trigger uses (SentViewTokens
	// skips m.Compacted too) the same measure the request ships.
	live := make([]utils.Message, 0, len(messages))
	for _, m := range messages {
		if m.Compacted {
			continue
		}
		live = append(live, m)
	}
	messages = live

	// Find the LAST checkpoint pair (assistant half) — the rolling design
	// keeps only one, but defensive against any stale extras.
	lastCheckpointIdx := -1
	for i, m := range messages {
		if m.Role == "assistant" && db.IsCheckpointMessage(m) {
			lastCheckpointIdx = i
		}
	}
	// No checkpoint pair: this conversation has never been compacted, so there
	// is no frozen head to protect, and NOTHING on this path bounds the
	// history as a whole. That is deliberate.
	//
	// This branch used to cap the slice to a char budget (capTail). capTail
	// sliced tail[start:] and returned: no db write, no compacted flag, no
	// summary, no log naming what it removed. The client keeps those rows, so
	// the user watched the full transcript while the model answered from the
	// newest slice - the history loss eco exists to eliminate, and silent.
	// It also broke eco's own trigger: capping the prompt held lastPT below
	// the trigger forever, so the conversation that most needed compaction was
	// the one the trigger could never see.
	//
	// The no-pair path is the frozen path with no pair: EVERY live row, whole.
	// Nothing on this path rewrites a row any more. The per-message truncation
	// it used to apply is deleted: a truncated row's bytes change between passes
	// (the banner re-counts the banner, measured 100000+104=100104 over its own
	// bound), and a row whose bytes change discards the KV prefix behind it.
	//
	// "This view no longer fits the window" is handled where it belongs: the
	// preemptive guard in index.go measures this exact slice (SentViewTokens of
	// the value returned here) and returns the context-length error the LLM loop
	// recovers from with a DURABLE fold. That fold mints the pair, bounds the
	// view through the fold render + the model's checkpoint, and tells the user
	// it happened. Nothing is silently cut and the head never slides.
	if lastCheckpointIdx < 0 {
		return dropOrphanToolRows(messages)
	}

	// Frozen prefix: the pair, then every live row after it. No window, no
	// total-size-dependent cap — see the doc comment. The tradeoff is
	// explicit: an unbounded tail CAN exceed the model's window. That is not
	// recovered per-request by sliding the head (which would destroy the
	// cache); it is recovered by a DURABLE fold from the context-length error
	// path (tool_loop.go), and only as a last resort by the durable roll-up
	// refine there. The preemptive guard in index.go (SentViewTokens / estTokens)
	// turns "about to exceed" into that durable fold before shipping.
	return dropOrphanToolRows(messages[lastCheckpointIdx:])
}

// dropOrphanToolRows removes "tool" rows whose ToolCallID has no surviving
// assistant tool_call in the shipped slice.
//
// This is NOT the size-based capping this file deleted: it fires at any size,
// and what it removes is not context the model can use. OpenAI-compatible
// providers reject a tool message with no preceding tool_call, so shipping one
// costs a hard 400 on EVERY later request — a durable fold cannot fix a
// legality error, so the conversation is dead until someone edits the rows by
// hand. An orphan (left by an interrupted stream, a cancelled tool, a partial
// flush) carries no call, no arguments and no turn boundary: it is a malformed
// row, not information.
//
// Pure and idempotent on the stored slice — the decision depends only on which
// assistant calls are in the slice, so the row set is byte-stable across passes
// and the KV prefix behind it survives.
func dropOrphanToolRows(msgs []utils.Message) []utils.Message {
	calls := make(map[string]bool, 8)
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			for _, tc := range msgs[i].ToolCalls {
				calls[tc.ID] = true
			}
		}
	}
	out := make([]utils.Message, 0, len(msgs))
	for i := range msgs {
		m := msgs[i]
		if m.Role == "tool" && m.ToolCallID != "" && !calls[m.ToolCallID] {
			utils.Log("[Eco] dropping orphan tool row (%s, %d chars): no surviving assistant call, so the provider would reject the request",
				m.Name, m.Content.TotalContentChars())
			continue
		}
		out = append(out, m)
	}
	// THE WINDOW MUST NEVER BE EMPTY. Every row in the slice can be an orphan at
	// once — an interrupted stream leaves a run of tool results with no
	// assistant half. An empty view is not a degraded request: the model gets the
	// system prompt, the tool schemas and nothing else, appendCurrentTimeTail
	// takes its len(msgs)==0 branch and invents a clock-only prompt. Observed
	// live: a turn arrived as ONLY "The current time is ...", and one stored tool
	// row carried 18 copies of it, the model narrating the timestamp into its own
	// tool output.
	//
	// So the legality repair yields to the history invariant: keep the newest
	// row. A malformed row is a 400 someone can fix by folding; a history-less
	// prompt is a conversation answering from thin air while the user watches the
	// full transcript.
	if len(out) == 0 && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		out = append(out, last)
		utils.Log("[Eco] every row was an orphan tool result — keeping the newest row (role=%s, %d chars) so the request is never history-less",
			last.Role, last.Content.TotalContentChars())
	}
	return out
}

// charsPerToken is the estimator divisor used across the eco path
// (SentViewTokens divides by it), so a char budget built with it is consistent
// with the token math the preemptive guard and the eco trigger use.
const charsPerToken = 3

// promptOverheadTokens is the room the bootstrap budget and the compaction
// policy reserve for what rides in FRONT of the history on every request:
//
//	 24000  the system prompt — measured on the running server, this product's
//	        system prompt is over 20000 tokens on its own
//	+16000  the tool schemas, which the preemptive guard adds to the history
//	        measure separately (preemptiveGuardEstimate) and which ride on every
//	        tool-capable request
//
// The split matters because the two budgets must not double-count and must not
// leave a hole: the guard fires at window - guardReplyReserve against
// history + schemas, and the provider rejects above the window. Reserving only
// the system prompt leaves a schemas-sized hole — the guard calls a history
// legal that the provider answers 400, which is the overflow path firing every
// turn.
const promptOverheadTokens = 40_000

// ecoWindowPolicy derives the compaction (trigger, target) PROMPT-token
// thresholds for a model's context window, from the operator's configured
// pair. It exists because the configured pair is GLOBAL while the thing it has
// to fit is PER-MODEL, and this deployment changed model underneath it.
//
// Configured: trigger 150000, target 50000. Against the 262144-token model
// this box runs, that fires compaction at 57 % of the window and folds the
// live view toward 50000 — so a 147K-token conversation is folded to a ~27K
// history. That is the "it sent 27K tokens, which is not containing any real
// history" the user measured on the build WITH THE WORKING CACHE, and it is
// why the #196 build never seemed to bite: its folds were all dying
// (durable fold failed : context deadline exceeded), so no checkpoint pair
// ever existed and capTail shipped 177562 tokens of raw history. Fixing the
// fold deadline made compaction REAL, and a real policy aimed at 50K on a
// 260K window bites hard.
//
// Compaction is the mechanism that keeps a request inside the window, so it
// must be DENOMINATED IN THE WINDOW:
//
//	headroom = window - replyReserve
//	trigger  = min(headroom, max(cfgTrigger, window - promptOverhead - replyReserve))
//	target   = trigger * 2/3, floored at cfgTarget
//
// trigger:
//   - RAISES the configured trigger when the window affords it, so a
//     262144-token model compacts at ~232000 prompt tokens (window minus the
//     prompt overhead) and the live view keeps the ~177K tokens the user sees
//     in the client and expects the model to have;
//   - LOWERS it, capped by headroom, when the window is smaller, so a
//     131072-token model compacts at ~124928 instead of a configured 150000
//     that sits ABOVE the window: a trigger that can never fire leaves the
//     request inside the window only by luck, rescued solely by the post-400
//     overflow path (the context_length_exceeded death spiral of the
//     1866-message conversation, 1830 -> 1855 live rows, every turn).
//
// target: the view kept AFTER a fold is 2/3 of the trigger, so one fold buys
// ~1/3 of the window in headroom before the next fold is needed. That
// hysteresis is what keeps folds RARE — and fold frequency is what controls
// checkpoint churn, since rewriting the pair rewrites the prompt head and
// zeroes the KV prefix cache (measured: cache_read pinned at 16384 through
// back-to-back forced folds).
//
// Returns the configured pair unchanged for an unknown window (<= 0): with no
// window there is no basis to invent a policy.
func ecoWindowPolicy(cfgTrigger, cfgTarget, windowTokens int) (trigger, target int) {
	trigger, target = cfgTrigger, cfgTarget
	if windowTokens <= 0 {
		// Unknown window: the configured pair governs, unchanged. The ONE thing
		// that must not survive is target >= trigger, for the same reason the
		// clamp exists below — a fold with nothing to remove is refused by the
		// hysteresis floor, so the conversation churns the pair every turn.
		if target >= trigger || target <= 0 {
			target = trigger * 2 / 3
		}
		return
	}
	headroom := windowTokens - guardReplyReserve
	if headroom <= 0 {
		return
	}
	if raised := windowTokens - promptOverheadTokens - guardReplyReserve; raised > trigger {
		trigger = raised
	}
	if trigger > headroom {
		trigger = headroom
	}
	if t := trigger * 2 / 3; t > target {
		target = t
	}
	if target <= 0 || target >= trigger {
		// A target at/above the trigger leaves the fold nothing to remove:
		// every fold is refused by the hysteresis floor and the conversation
		// churns the pair every turn.
		target = trigger * 2 / 3
	}
	return
}

// ecoConversationWindow resolves the context window of the model a
// conversation is configured to use, for the compaction policy. The text
// model is the one compaction protects; an unresolvable model yields 0, which
// ecoWindowPolicy reads as "use the configured pair".
func ecoConversationWindow(conv *utils.Conversation) int {
	if conv == nil {
		return 0
	}
	// The SAME resolution the request path uses (index.go: SelectModel over
	// conv.ModelSelected). Reading ModelSelected.Text directly leaves a hole:
	// SelectModel also resolves through Vision, and a conversation with only
	// Vision set would yield window=0 here — the policy's fallback is the
	// CONFIGURED pair (trigger 150000 / target 50000), so the compaction tick
	// would fold a 148K-token conversation toward 50K on a 262144-token model.
	// That is the "sent 27K tokens, not containing any real history" failure,
	// reachable through a nil-Text ModelSelected. Resolving the model the way
	// the request does makes the policy and the shipped request agree by
	// construction: one model, one window, one policy.
	name := chatModelForConversation(*conv).Name
	if name == "" {
		return 0
	}
	return Models.DeclaredContextWindow(name)
}

// bootstrapCutoff is the char-based fold boundary for a conversation with NO
// checkpoint pair: the oldest user-anchored boundary whose REMAINING sent
// view fits the eco target. findCutoffIndex's arithmetic is cumulative
// prompt_tokens, and a bootstrap conversation's lastPT is capped by the
// sliding window (~33k tokens), so it can sit BELOW the target even while
// the stored view is far over it — the cutoff lands at -1 and the bootstrap
// fold skips, leaving the self-lock intact. The view the frozen path ships
// is measured in chars (SentViewTokens), so the bootstrap cutoff uses the
// same measure: fold the oldest rows until the rest fits the target.
//
// Returns -1 when the view already fits the target (nothing to fold). The
// boundary is a user row, and the newest user row is the floor (the same
// rule CompactConversationDurable enforces), so the live window never
// empties.
//
// The newest user row is the FLOOR, not a guarantee: when the newest turn
// alone exceeds the target (a single >300k-char tool result), no cutoff can
// land the view under target. This then folds everything older — strictly
// better than the pre-fix code, which shipped the over-target window every
// turn forever. The provider's context window remains the preemptive guard's
// job (index.go); compaction's job is to stop the churn, which it does at
// any target.
func bootstrapCutoff(messages []utils.Message, targetTokens int) int {
	lu := -1
	for i, m := range messages {
		if m.Role == "user" && !m.Compacted && !db.IsCheckpointMessage(m) {
			lu = i
		}
	}
	if lu <= 0 {
		return -1
	}
	// Suffix sum, walked forward: suffix is the char total of messages[i:]
	// at each step. A prefix sum inside the loop would make this O(n^2) on
	// the 30k-row conversations this path exists for.
	suffix := 0
	for i := range messages {
		suffix += sentViewRowCharsLive(messages[i])
	}
	for i := 0; i <= lu; i++ {
		if messages[i].Role == "user" && !messages[i].Compacted {
			fits := suffix/3 <= targetTokens
			if i == 0 {
				if fits {
					return -1 // the whole view fits; folding 0 rows is a no-op
				}
			} else if fits {
				return i
			}
		}
		suffix -= sentViewRowCharsLive(messages[i])
	}
	// Nothing up to the newest user row fits: fold everything older than it.
	// A fold that leaves only the newest user turn is the floor; the storage
	// layer clamps the same way.
	return lu
}

// tickNeedsFold is the eco tick's gate, as a pure decision so it is testable.
//
// It fires on EITHER measure: the provider's own count (lastPT) or the sent
// view. lastPT alone cannot gate this path.
//
// lastPT is the provider's prompt count of the last SUCCESSFUL request. The
// preemptive guard in index.go refuses an oversized request BEFORE it is sent,
// so the provider never counts it, so lastPT stays pinned at the last small
// request while the view that will not fit keeps growing. The trigger can never
// fire. Live, conv 9f0ab2352bf2bdc40e77384b, /app/data/server.log, 2026-11-25,
// on the build that removed the sliding tail cap:
//
//	06:13:52 preemptive context guard: estimated 707728 tokens (sent view
//	         679333 + schemas 28395) vs window 262144, 1201 live rows
//	06:13:52 durable fold: 1196 rows folded, history 2439 -> 5 messages
//	         (view ~34042 tokens, fold #1)
//	06:55:10 lastPT=94602  below trigger=216000 (sent view 98148)  - skip
//	08:00:21 lastPT=111325 below trigger=216000 (sent view 131383) - skip
//
// That is the self-lock the tail cap used to cause, moved: the cap used to
// clamp the prompt so lastPT could not climb to the trigger; now the guard
// refuses the request so lastPT is never recorded. Same lock, same gate.
//
// The tick is the path that folds at the TARGET, on the policy's cadence, and
// tells the user. Skipping it hands compaction to overflow recovery, which must
// fit the WINDOW and leave room for the roll-up it writes (durableFold budgets
// the roll-up at (target - liveTail) * 3), so it cuts toward half the target —
// from 679333 tokens that is a 95 % shed in one turn, with no pair minted by
// the tick and no warning until the collapse.
//
// The sent view is the honest measure for a conversation whose requests the
// guard blocks: SentViewTokens skips folded rows and caps each row at the
// per-message bound, so it is what the frozen path ships.
func tickNeedsFold(lastPT, sentViewTokens, triggerTokens int) bool {
	return lastPT > triggerTokens || sentViewTokens > triggerTokens
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
		return -1
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
// The budget keeps the NEWEST messages (the most recent context the live
// model will lose) up to the cap, so the summary preserves the most
// actionable recent history when the compacted range is enormous; a banner
// notes that older messages were omitted. Never returns a context-busting
// blob to the summary model.
func renderMessagesForSummary(messages []utils.Message) string {
	var b strings.Builder
	budget := maxSummaryInputChars
	// Walk backwards so a huge range keeps its most recent turns; then
	// reverse the kept slice so the output stays chronological.
	var kept []utils.Message
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if db.IsCheckpointMessage(m) {
			continue
		}
		if int64(budget) <= 0 {
			break
		}
		// Estimate this message's rendered size and stop before exceeding
		// the budget (per-message cap keeps one giant message from blowing
		// the whole budget).
		mBytes := m.Content.TotalContentChars()
		if mBytes > perMsgBudget {
			mBytes = perMsgBudget
		}
		if mBytes > budget {
			continue
		}
		budget -= mBytes
		kept = append(kept, m)
	}
	if len(kept) < len(messages) {
		b.WriteString("[NOTE: older messages omitted to fit the summary input budget — the following is the most recent portion of the compacted range; earlier context was dropped by the context-safety cap]\n\n")
	}
	for i := len(kept) - 1; i >= 0; i-- {
		m := kept[i]
		switch m.Role {
		case "user":
			b.WriteString("USER:\n")
			writeContentPartsBudget(&b, m.ContentParts())
			b.WriteString("\n")
		case "assistant":
			b.WriteString("ASSISTANT:\n")
			writeContentPartsBudget(&b, m.ContentParts())
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
			writeContentPartsBudget(&b, m.ContentParts())
			b.WriteString("\n")
		}
	}
	return b.String()
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
				b.WriteString(textsafe.HeadBytes(p.Text, perMsgBudget))
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
// When all gates pass it folds the oldest turns into the conversation's ONE
// checkpoint pair via db.CompactConversationDurable: the rows are FLAGGED
// (never deleted, never renumbered) and the roll-up is a deterministic
// MACHINE excerpt.
//
// The fold is LLM-FIRST (see durableFold): the chat model that owns the
// conversation writes the durable checkpoint of the rows it is about to lose,
// on a request that replays its own cached prefix, and the result is megacaved
// before it is stored. Its failure CANNOT lose a fold — durableFold falls back
// to the deterministic machine-only render. What stayed forbidden through all
// of this: the FAST model (its KV cache is not warm for this conversation and
// it never saw the history), SendChatCompletion (a fold must never take a
// chat turn), the clock, db.ReplaceCheckpoint, and anything else that moves
// the prompt head. The AST guard in ultracave_llm_guard_test.go enforces it.
//
// The prior roll-up is carried forward VERBATIM by the storage layer
// (composeDurableSummary concatenates prior + the new excerpt); the caller
// must NOT prepend it.
//
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
	// and future compactions are not starved. Compaction is machine-only, so
	// this deadline bounds pure DB work: there is no LLM round-trip left on
	// the path.
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
	// Compaction thresholds are denominated in the conversation's MODEL window.
	// A global 150000/50000 pair folds a 262144-token model's 147K conversation
	// down to a ~27K history (the context loss the user reported) and sits ABOVE
	// the window of a 128K model, where it can never fire at all.
	convWindow := ecoConversationWindow(conv)
	ecoTrigger, ecoTarget := ecoWindowPolicy(cfg.TriggerTokens, cfg.TargetTokens, convWindow)
	if ecoTrigger != cfg.TriggerTokens || ecoTarget != cfg.TargetTokens {
		utils.Log("[Eco] conv %s window policy: configured trigger=%d target=%d, model window=%d -> trigger=%d target=%d",
			conversationID, cfg.TriggerTokens, cfg.TargetTokens, convWindow, ecoTrigger, ecoTarget)
	}

	// Locate the existing checkpoint (if any) BEFORE the trigger gate: it tells
	// the fold whether it is minting the first pair or rolling an existing one,
	// and the no-pair boundary rules read it (see tickFoldBoundary).
	// Read the CURRENT checkpoint pair straight out of the loaded LLM view
	// (it carries the pair) instead of db.GetCheckpoint: that helper issues
	// its second query while the first result set is still open, which
	// wedges the single-connection SQLite pool (SetMaxOpenConns(1)) forever
	// as soon as a pair exists. Reading the slice is also cheaper, and it is
	// the exact view the fold will be computed from, so the pair index and
	// the roll-up text cannot disagree with the slice being folded.
	existingCheckpointEndIdx := -1
	var priorSummary string
	for i, m := range conv.Messages {
		if !db.IsCheckpointMessage(m) {
			continue
		}
		if m.Role == "tool" {
			priorSummary = m.TextContent()
			continue
		}
		// assistant half: the tool half follows immediately.
		existingCheckpointEndIdx = i + 1
	}

	lastPT := lastAssistantPromptTokens(conv.Messages)
	if lastPT == 0 {
		utils.Log("[Eco] conv %s has no assistant prompt_tokens yet — skip", conversationID)
		return
	}
	sentView := SentViewTokens(conv.Messages)
	// ONE trigger for both paths. The trigger measures lastPT = the provider's
	// own prompt count of the newest turn, and with the bootstrap tail cap
	// gone the sent view IS the prompt, so that measure is honest for a
	// conversation with no checkpoint pair too.
	//
	// It used to be self-locking: capTail capped the prompt at a char budget,
	// so lastPT could never climb to a trigger far above the cap, and the
	// conversation that most needed compaction was the one the trigger could
	// never see. The gate needed a second arm (bootstrapFoldNeeded: fold when
	// the view outgrows the budget the cap would apply) to break that lock.
	// The lock is gone with the cap, so the arm is gone with it - keeping it
	// would fold a view that fits the window comfortably, which is not
	// compacting safely, it is losing context the model had room for.
	//
	// "This request will not fit the window" is not the tick's job: the
	// preemptive guard in index.go measures the exact slice this turn ships
	// and raises the context-length error the LLM loop answers with a durable
	// fold, so a conversation that jumps the window in one turn is compacted
	// durably at the moment it happens, with the user told.
	if !tickNeedsFold(lastPT, sentView, ecoTrigger) {
		utils.Log("[Eco] conv %s lastPT=%d and sent view %d tokens both below trigger=%d - skip",
			conversationID, lastPT, sentView, ecoTrigger)
		return
	}
	if existingCheckpointEndIdx < 0 {
		utils.Log("[Eco] conv %s lastPT=%d > trigger=%d - compacting toward target=%d (this fold mints the first checkpoint pair)",
			conversationID, lastPT, ecoTrigger, ecoTarget)
	} else {
		utils.Log("[Eco] conv %s lastPT=%d > trigger=%d - compacting toward target=%d",
			conversationID, lastPT, ecoTrigger, ecoTarget)
	}

	// THE NO-PAIR RULE + THE HYSTERESIS FLOOR, as ONE pure function
	// (tickFoldBoundary): the boundary of a conversation with NO checkpoint
	// pair comes ONLY from the char-based bootstrapCutoff — never from
	// findCutoffIndex's cumulative prompt_tokens arithmetic — and ANY
	// boundary that buys less than the hysteresis floor is refused. Both
	// rules exist because of the live trace below; see the function's doc
	// comment.
	cutoff := tickFoldBoundary(conv.Messages, lastPT, ecoTarget, existingCheckpointEndIdx)
	if cutoff < 0 {
		// tickFoldBoundary logged WHY (the view already fits the target, the
		// PT arithmetic cannot cut, or the fold is under the hysteresis
		// floor). No boundary means NO write, so the checkpoint pair stays
		// byte-identical — the documented 0-row contract.
		utils.Log("[Eco] conv %s tick fold refused: no usable boundary (lastPT=%d target=%d, %d messages, checkpoint end=%d) — pair stays byte-identical",
			conversationID, lastPT, ecoTarget, len(conv.Messages), existingCheckpointEndIdx)
		return
	}
	utils.Log("[Eco] conv %s cutoff=%d (of %d messages), prior checkpoint end=%d", conversationID, cutoff, len(conv.Messages), existingCheckpointEndIdx)

	// Rows this fold renders: everything strictly between the existing
	// checkpoint pair and the cutoff. The storage layer re-derives the fold
	// set from the slice and clamps the boundary itself; this range is the
	// caller-side budget arithmetic only.
	startIdx := 0
	if existingCheckpointEndIdx >= 0 {
		startIdx = existingCheckpointEndIdx + 1
	}
	if startIdx >= cutoff {
		utils.Log("[Eco] conv %s fold range is empty (start=%d cutoff=%d) — skip", conversationID, startIdx, cutoff)
		return
	}

	// Machine-only fold. `foldFrom = cutoff` — findCutoffIndex's arithmetic
	// (targetDrop = lastPromptTokens - targetTokens, land on a clean turn
	// boundary past the existing pair) is unchanged; only its RESULT is used
	// as a fold boundary now, and it feeds a durable fold instead of an
	// LLM-written checkpoint replacement.
	// The CHAT model of this conversation (conv.ModelSelected, NOT the fast
	// shortcut) plus the payload shape the loop's requests carried, so the
	// fold's summary request replays the cached prefix.
	chatModel := chatModelForConversation(*conv)
	ecoPayload := chatPayloadForConversation(*conv)
	if chatModel.Name == "" {
		utils.Log("[Eco] conv %s has no chat model resolved — the fold will use the machine-only render", conversationID)
	}

	before := SentViewTokens(conv.Messages)
	folded, err := durableFold(dbCtx, conversationID, conv.Messages, cutoff, priorSummary, chatModel, ecoPayload)
	if err != nil {
		utils.Error("[Eco] durable fold failed", err)
		return
	}
	if folded == 0 {
		utils.Log("[Eco] conv %s durable fold removed 0 rows (cutoff=%d of %d) — the view is NOT shrinking; leaving the checkpoint pair byte-identical", conversationID, cutoff, len(conv.Messages))
		return
	}
	// Reload the LLM-facing view so the logged size is the size the NEXT
	// request will actually carry (folded rows skipped, pair kept), not an
	// estimate from the pre-fold slice.
	logFoldResult(dbCtx, conversationID, folded, before, lastPT, ecoTarget)
}

// logFoldResult reports, in one line, exactly what a fold did: rows folded
// and the resulting LLM-view size. The view is re-read from the DB so the
// number is the one the next request ships.
func logFoldResult(dbCtx context.Context, conversationID string, folded, beforeTokens, lastPT, targetTokens int) {
	if conv, err := db.GetConversationByIdInternal(dbCtx, conversationID); err == nil {
		after := SentViewTokens(conv.Messages)
		utils.Log("[Eco] conv %s compacted: folded %d rows, LLM view %d -> %d tokens (lastPT=%d, target=%d, view rows %d)",
			conversationID, folded, beforeTokens, after, lastPT, targetTokens, len(conv.Messages))
		return
	}
	utils.Log("[Eco] conv %s compacted: folded %d rows (view re-read failed, size not measured)", conversationID, folded)
}

// SentViewTokens estimates the prompt size of the history that is ACTUALLY
// SENT to the provider for this conversation: rows durable compaction has
// folded (m.Compacted) are skipped, every other row is counted at its wire
// char length / 3, each row clamped at the per-message cap the sender itself
// applies.
//
// Why the trigger must count the SENT view and not every stored row:
// db.GetConversationByIdInternal already excludes folded rows, so a folded
// row is not in the slice at all — but the older in-memory slices (and the
// eco gate's own history) do carry them. If the estimator counted folded
// rows, lastAssistantPromptTokens (the provider's own count, which never
// includes them) plus the raw slice size would stay ABOVE the trigger
// FOREVER after the first fold: every user turn would fire compaction again,
// each pass folding a few more rows and rewriting the checkpoint summary,
// i.e. the head of the prompt would change on every turn — exactly the
// cache-killer this subsystem exists to prevent. Folding converges only if
// the measure shrinks by what was folded.
//
// The measure is the RAW stored body and the sender ships the RAW stored body:
// the same bytes, so the guard and the wire cannot disagree.
func SentViewTokens(messages []utils.Message) int {
	chars := 0
	for i := range messages {
		if messages[i].Compacted {
			continue
		}
		chars += SentViewRowChars(messages[i])
	}
	return chars / 3
}

// sentViewRowChars is the per-row char count SentViewTokens sums.
//
//   - The body is the TOTAL across all text parts (TextContent() sees only
//     the first part and hid a 4.9MB blob behind a short preamble).
//   - A row is counted WHOLE. There is no per-message cap any more: the
//     request path ships the stored bytes, so counting anything less would
//     under-count the wire and let an oversized request ship un-flagged.
//
// It counts the RAW stored body, not the stored caveman projection.
// convertMessagesToOpenAI ships that projection only for caveman-enabled
// models (cavemanCompressApplies), so subtracting it would make the TRIGGER
// measure model-dependent, and the eco trigger is evaluated per conversation
// (whose next turn may use any model). Counting the raw body is the
// conservative direction for the trigger: a compressed model folds marginally
// early, and the estimator can never under-count and let an oversized request
// ship.
//
// The preemptive context guard is NOT that estimator. It decides, per request,
// whether THIS request fits THIS model's window, and it has the model in
// hand — so it must measure what that request ships. See
// SentViewTokensShipped.
// It is a pure function of the stored row: no clock, no slice position, no
// total-size dependence. Same row in, same number out, every turn.
func SentViewRowChars(m utils.Message) int {
	chars := m.Content.TotalContentChars()
	// The assistant half of a tool round ships its tool_calls: the function
	// name and the JSON ARGUMENTS (file writes, shell commands, patch bodies).
	// Content alone undercounts an agent turn by the factor that made every
	// bound in this subsystem fiction — live, capTail logged "kept 228 msgs /
	// 117987 chars" for a request the proxy measured at tok_before=177562.
	// The preemptive guard adds the tool SCHEMAS on top of this measure, so
	// the row measure must include the row's own tool calls or the guard
	// under-estimates and a context-busting request ships.
	for _, tc := range m.ToolCalls {
		chars += len(tc.Function.Arguments) + len(tc.Function.Name) + len(tc.ID)
	}
	return chars
}

// sentViewRowCharsLive is SentViewRowChars for a row the wire ACTUALLY carries.
//
// A row an earlier fold marked compacted is NOT on the wire: the LLM-facing
// view is loaded with `AND compacted = 0` (db.getConversationFromDB), so the
// row exists only in the raw slice the fold indexes positionally. Counting it
// measures history the request stopped shipping turns ago.
//
// Returning 0 (rather than skipping the index) keeps the suffix walks in
// bootstrapCutoff / bootstrapCutoffRefined exact: a compacted row adds 0 to
// the running suffix and subtracts 0 from it, so suffix at index i is the
// live sent view of messages[i:] — the number the wire ships.
func sentViewRowCharsLive(m utils.Message) int {
	if m.Compacted {
		return 0
	}
	return SentViewRowChars(m)
}

// liveFoldSet is the render set of a fold starting at foldFrom: the rows the
// stored roll-up will actually represent.
//
// db.CompactConversationDurable builds this same set internally (skip rows an
// earlier fold marked compacted, skip the checkpoint pair) and renders exactly
// it. The ai layer sized its budget off messages[:foldFrom] instead, so the
// two layers disagreed about how many rows one fold covered — see
// durableFoldBudget.
func liveFoldSet(messages []utils.Message) []utils.Message {
	out := make([]utils.Message, 0, len(messages))
	for i := range messages {
		if messages[i].Compacted || db.IsCheckpointMessage(messages[i]) {
			continue
		}
		out = append(out, messages[i])
	}
	return out
}

// SentViewRowCharsShipped is SentViewRowChars measured on the bytes THIS model
// ships: a tool row with a stored caveman projection ships the projection
// (index.go substitutes msg.CompressedContent for the raw body when
// cavemanCompressApplies), so the raw body must not be counted for it.
//
// The min() matters. The projection is a compression, so it is normally
// shorter, but the transform ADDS labels and an elision banner and on a short
// row it can come out LONGER (measured ratio ~1.045 on prose+tool content).
// Taking the smaller of the two keeps this measure an under-count of the raw
// measure and never an over-count of the wire, which is the direction that can
// ship a context-busting request un-flagged.
func SentViewRowCharsShipped(m utils.Message, st cavemanStoneSet) int {
	chars := m.Content.TotalContentChars()
	if m.Role == "tool" && m.CompressedContent != "" && cavemanCompressApplies(st, m.Name) {
		if n := len(m.CompressedContent); n < chars {
			chars = n
		}
	}
	for _, tc := range m.ToolCalls {
		chars += len(tc.Function.Arguments) + len(tc.Function.Name) + len(tc.ID)
	}
	return chars
}

// SentViewTokensShipped is SentViewTokens for a model whose caveman stones are
// known — the measure the preemptive context guard must use.
//
// WHY THE GUARD NEEDS IT (2026-11-25, conversation 9f0ab2352bf2bdc40e77384b):
// the guard fired on a sent view of 679333 tokens against a 262144 window, and
// the fold it forced left the provider measuring the NEXT request at 24683
// prompt_tokens, of which ~21521 is the system block and tool schemas. The
// model received ~3160 tokens of history and 1196 rows of conversation were
// gone. The raw measure counts tool bodies the wire never carries, so it
// over-counts the very request the guard is measuring, and it fires a fold on
// a request that fits.
//
// The eco TRIGGER keeps the raw measure on purpose (see SentViewRowChars): it
// is per-conversation and model-agnostic. This function is per-request and
// takes the stones as an argument, so the caller must have the model.
func SentViewTokensShipped(messages []utils.Message, st cavemanStoneSet) int {
	chars := 0
	for i := range messages {
		if messages[i].Compacted {
			continue
		}
		chars += SentViewRowCharsShipped(messages[i], st)
	}
	return chars / 3
}

// megacaveFitsCeiling is STAGE 1's fit test: is the machine megacave enough, so
// the fold needs no model call?
//
// THREE conditions, all required:
//
//   - non-empty: an empty render means the fold set rendered nothing, and an
//     empty section must never be stored (the storage layer would refuse it and
//     the fold would be lost);
//   - within the ceiling: the render was taken UNCEILED (maxTotalChars = 0), so
//     this is a real measurement. Rendering with the ceiling and measuring the
//     result is vacuously true — capMegacaveExcerpt enforces the ceiling by
//     truncating, so a capped render is always <= ceiling;
//   - not at the per-row floor: MegacaveRowBudget floors at
//     minMegacaveRowShowChars, and a fold whose rows all sit at that floor is
//     rows x (64 + label) of elision MARKERS. That is numerically in-window and
//     content-free — the failure mode the live incident produced (1240 rows /
//     2824666 chars folded into a checkpoint that described nothing). A fold with
//     no room for content escalates: the model can compress the same history into
//     a checkpoint that is short AND meaningful.
//
// The floor condition is the one that fires in practice: the per-row budget is
// DERIVED from the ceiling, so an uncapped render lands near the ceiling by
// construction and the size clause is a boundary guard, not the discriminator.
func megacaveFitsCeiling(megacaved string, ceiling int, atRowFloor bool) bool {
	if strings.TrimSpace(megacaved) == "" {
		return false
	}
	if atRowFloor {
		return false
	}
	return len(megacaved) <= ceiling
}

// PriorSummaryFromView returns the roll-up text the conversation's LIVE
// checkpoint pair carries, read straight out of a loaded LLM-facing view
// (that view includes the pair), and "" when there is no pair.
//
// Deliberately NOT db.GetCheckpoint: that helper runs its second query while
// the first result set is still open, which wedges the single-connection
// SQLite pool (SetMaxOpenConns(1)) indefinitely as soon as a pair exists
// (observed in the ultracave tests: the fold hung forever at the second
// prior-summary lookup). Reading the slice is also cheaper, and it is the
// exact slice the fold is computed from, so the roll-up text cannot disagree
// with the rows being folded.
//
// The text feeds the excerpt BUDGET only. The storage layer carries it
// forward VERBATIM and does the concatenation — callers must not re-prepend
// it, and must not re-render it (it is the only representation of every row
// an earlier fold marked compacted).
func PriorSummaryFromView(messages []utils.Message) string {
	for _, m := range messages {
		if m.Role == "tool" && db.IsCheckpointMessage(m) {
			return m.TextContent()
		}
	}
	return ""
}

// durableFoldRoomChars is the roll-up's room in CHARS for one fold: the part
// of the fold budget the live tail did not spend, at summaryCharsPerToken
// chars/token, minus the prior roll-up it must carry forward and the label
// overhead the renderer adds per row.
//
// THE COUPLING IS THE WHOLE POINT. The boundary and the excerpt budget come
// from ONE number (overflowFoldBudgetTokens), so a boundary that leaves a fat
// live tail leaves the roll-up the rest of the window, and a fold that
// summarises hard leaves the tail room to grow. Denominating the two
// separately is what produced the 2026-11-25 collapse: the caller cut toward
// 72000 while the summariser budgeted against 144000, the room went negative,
// and the stored checkpoint lost everything it summarised.
//
// The chain's own markers are paid for too (durableChainOverheadChars). The
// storage layer enforces the bound on prior + excerpt + markers, so an excerpt
// ceiling that ignores the markers leaves a roll-up that overshoots the bound
// by exactly that much — and an overshoot is what fires the escape valve.
func durableFoldRoomChars(budgetTokens, tailTokens, priorChars, overheadChars int) int {
	return overflowFoldRollupRoomChars(budgetTokens, tailTokens) - priorChars - 2*overheadChars - durableChainOverheadChars
}

// durableChainOverheadChars is what db.composeDurableSummary reserves on top of
// the prior text and this fold's excerpt: the ONE section banner, the blank line
// around it, and the escape-valve marker (its digits plus the storage layer's
// own slack). The constants live in db; reading them here keeps the two layers
// on one number instead of two hand-copied guesses.
const durableChainOverheadChars = len(db.NewerHistoryBanner) + 4 + 64 + 128

// durableRollupBound is the bound on the WHOLE stored roll-up (prior + this
// fold's excerpt), in chars — the number handed to db.CompactConversationDurable
// as summaryBound, which is what db.composeDurableSummary enforces.
//
// It is the roll-up's share of the fold budget BEFORE the prior is subtracted.
// durableFoldRoomChars subtracts the prior because it answers a different
// question: how much room the NEW excerpt has. The two are not the same number
// and passing the second as the bound is a self-inflicted escape valve: the
// excerpt is rendered up to the full limit, so prior + excerpt always exceeds
// it, so the prior is megacaved and re-emitted on EVERY fold. The prior sits at
// the head of the prompt, so the KV prefix diverges there every fold — the
// rolling summary, and the cache_read=0 that follows it.
func durableRollupBound(budgetTokens, tailTokens int) int {
	room := overflowFoldRollupRoomChars(budgetTokens, tailTokens)
	if room < 0 {
		return 0
	}
	return room
}

// durableFold is the single durable fold entry point, shared by the eco
// trigger and the context-length recovery path in tool_loop.go.
//
// The budget arithmetic (per the renderer's own doc comments): the excerpt
// may cost at most `cfg.TargetTokens` minus the live tail it has to leave
// room for, minus the prior roll-up it has to carry, minus the artifact
// overhead. MegacaveRowBudget then splits THAT over the rows of the fold set,
// which is the only way the megacave ratio stays < 1 as a property of the
// transform: on realistic prose+tool content the renderer runs at ratio
// ~1.045 against the FIXED cap (it adds labels/newlines), so a fixed cap
// makes the "compaction" ADD bytes to the prompt.
//
// LLM-FIRST, THEN MEGACAVE. The rows this fold is about to evict are the last
// thing the chat model will ever see, so they are summarised by the CHAT MODEL
// itself, on the request that replays the conversation's own cached prefix
// (buildSummaryRequestFromView) — the cheapest possible use of a model that is
// about to lose that context. The LLM's text is then megacaved (the same
// deterministic lossy-compress operation the fallback uses), so the stored
// roll-up stays a CHAIN of megacaved summaries within the same ceiling.
//
// THE LLM CAN NEVER LOSE A FOLD. Any failure — proxy down, non-200, empty
// answer, budget collapse, context deadline — falls through to the machine-only
// renderer (machineOnlyFoldRenderer), which is byte-for-byte the render this
// path produced before the summariser was reintroduced. The machine-only render
// also stays deterministic: same fold set, same bytes. The clock never enters
// the stored roll-up (the summary request strips it, see stripCurrentTimeTail).
//
// `model` is the conversation's CHAT model (never the fast shortcut) and
// `payload` the chat payload whose system prompt / tool schemas the last real
// request carried, so the replayed prefix is the cached one.
func durableFold(ctx context.Context, conversationID string, messages []utils.Message, foldFrom int, priorSummary string, model utils.Model, payload ChatPayload) (int, error) {
	// The fold's target, in the CHAT model's window. durableFold must agree with
	// its caller (durableCompact computes the boundary from ecoTarget): if the
	// summariser writes an excerpt for a 50000-token target while the caller cut
	// toward 154666, the roll-up summarises away 100K tokens of context the
	// window can hold — the "not containing any real history" complaint,
	// reproduced by the summariser instead of the cap.
	// The fold's CEILING, in the CHAT model's window. durableFold must agree
	// with its caller (UltracaveOverflowFoldFrom computes the boundary from
	// overflowFoldBudgetTokens): if the caller leaves a live tail at 2/3 of
	// the window budget while the summariser sizes the excerpt against
	// ecoTargetTokens, the room below goes NEGATIVE the moment the tail
	// reaches the target, `chars` collapses to 0, the ceiling collapses to
	// the per-row floor, and the stored roll-up is marker mush — the
	// "not containing any real history" complaint, reproduced by the
	// summariser instead of the cap.
	budget := overflowFoldBudgetTokens(Models.DeclaredContextWindow(model.Name))
	if budget <= 0 {
		// No DECLARED window (unknown model, or the tick's no-selection path):
		// A built-in window table or a 128k default would inflate the roll-up
		// room for a model that may be far smaller, so the configured policy
		// governs, unchanged.
		budget = ecoFoldTarget(model.Name)
	}
	// Live tail this fold leaves in place, and the roll-up it carries
	// forward, both in chars — the budget is what is LEFT for the new
	// excerpt after both are paid for.
	tailTokens := SentViewTokens(messages[foldFrom:])
	priorChars := len(priorSummary)
	overhead := megacaveLabelOverheadChars
	room := durableFoldRoomChars(budget, tailTokens, priorChars, overhead)

	// The render set, NOT messages[:foldFrom]. db.CompactConversationDurable
	// builds its fold set by skipping rows an earlier fold marked compacted
	// and the checkpoint pair, and renders EXACTLY that set — so every number
	// the ai layer derives here must be measured on the same set, or the
	// caller budgets for rows the fold will never represent.
	foldable := liveFoldSet(messages[:foldFrom])
	// The room is a CEILING, not a licence. A fold that renders the folded
	// rows at their own size is not a compaction: the megacave transform
	// runs at ratio ~1.045 on prose+tool content (it ADDS labels and the
	// elision banner), so whenever the caller's room is bigger than the
	// folded content — a conversation below the eco target, an overflow fold
	// asked to keep a fat live tail — an unconstrained budget makes the
	// "compaction" ADD bytes to the prompt (measured: 20589 -> 21762 view
	// tokens with 81 rows folded). So the excerpt is also capped at HALF the
	// wire size of the rows it replaces: every fold then provably shrinks the
	// representation, on any content shape, and the per-row budget lands
	// near the average row size so large rows are the ones elided.
	// Measuring the whole prefix counts history the wire stopped shipping
	// turns ago: live, conv 9f0ab2352bf2bdc40e77384b at 14:13:09 logged
	// folded=7052851 chars for a fold that rendered 112 rows / ~900000 chars —
	// a 7.8x over-count. That number IS the excerpt's ceiling
	// (chars = min(room, foldedChars/2)), so the over-count inflates the
	// ceiling the roll-up is capped at, and the same measure is the
	// hysteresis floor, so it lets a fold that removes nothing pass.
	foldedChars := 0
	for i := range foldable {
		foldedChars += SentViewRowChars(foldable[i])
	}
	chars := room
	if half := foldedChars / 2; half < chars {
		chars = half
	}
	if chars < 0 {
		// No room: MegacaveRowBudget renders at its floor so the fold is at
		// least the smallest honest size (see its doc comment).
		chars = 0
	}
	// THE CEILING. The per-row split (MegacaveRowBudget) bounds each row's
	// CONTENT, but it cannot bound the FINISHED excerpt: the floor
	// minMegacaveRowShowChars x rows saturates the total, so on a 1467-row
	// conversation MegacaveRowBudget(90000, 1467) -> show=64 rendered
	// 1,458,172 chars, 16x over the budget. The caller knows the total, so the
	// total is passed explicitly: MachineExcerptMegacaveCapped truncates at
	// `chars` and appends one marker naming the original size. `chars` is
	// passed to the storage layer as THIS fold's bound, so the ai layer and the
	// storage layer agree on ONE number — the renderer's ceiling is the number
	// that gets applied, not the storage backstop slicing the text after the
	// fact.
	//
	// IT IS NOT db.MaxDurableSummaryChars. A fold's roll-up bound is a number in
	// the fold's OWN budget: the stored roll-up plus the live tail the fold
	// leaves is what the next request ships, and `room` above IS that budget
	// (target − live tail, in chars). Clamping it to the global 100k-char
	// constant (≈33333 tokens) throws away the headroom the fold just bought:
	// on this 262144-token deployment the target is 144000 tokens, so every fold
	// stored a roll-up at a FIFTH of the room it had, the post-fold view landed
	// at 34125 tokens, the view crossed the trigger again within turns, and each
	// re-fold rewrote the pair — the head of the prompt (cache_read pinned at
	// 21521). The bound stays a CEILING: room is bounded by the model's window,
	// so the write can still never grow the roll-up without limit.
	floor := (minMegacaveRowShowChars + megacaveLabelOverheadChars) * len(foldable)
	// THE ROLL-UP BOUND IS NOT THE EXCERPT CEILING. `chars` bounds THIS fold's
	// excerpt. The stored roll-up is prior + excerpt, and db.composeDurableSummary
	// truncates the PRIOR to `limit - excerpt` when prior + excerpt > limit — the
	// escape valve, whose marker is the "rolling summary" the owner named.
	// Passing the excerpt ceiling as the roll-up bound therefore guarantees the
	// valve fires: the excerpt is rendered up to the whole limit, so the prior can
	// never fit next to it, and every fold rewrites the head of the prompt.
	//
	// Live, conv 9f0ab2352bf2bdc40e77384b, 2026-11-25 14:13:09: room=545051,
	// prior=102805, excerpt rendered at 545051 -> 647856 > 545051 -> valve ->
	// 14:14:06 cache_read=0, then 14:17:22 [PrefixWatch] CACHE BREAK at byte 6001
	// (region: HISTORY). The bound is the room the ROLL-UP has, and the excerpt
	// ceiling is what is left of it after the prior is paid for.
	summaryBound := durableRollupBound(budget, tailTokens)
	if summaryBound < floor {
		// No room: the renderer lands on its floor, so the bound must cover the
		// floor, or the storage layer truncates text its own renderer produced.
		summaryBound = floor
	}
	// The ceiling is passed EXPLICITLY, so "no ceiling" (0 = unlimited in the
	// renderer) must never reach it: a fold with no room renders at the smallest
	// honest size — the floor the row budget uses — so the fit test FAILS and the
	// resolver escalates, and the excerpt stays bounded either way.
	ceiling := chars
	if ceiling <= 0 {
		ceiling = floor
	}
	// The machine render’s per-row budget: computed here because it is the size of
	// the fail-closed fallback, and because the verbatim user rows are paid for
	// first (megacave never compresses them), so the budget splits over the rows
	// it CAN compress. Splitting over all rows hands the verbatim ones a share
	// they never spend and starves the tool rows that need it.
	verbatim := MegacaveIncompressibleChars(foldable) + MegacaveAttachmentPointerChars(foldable)
	show := MegacaveRowBudget(chars-verbatim, MegacaveCompressibleRows(foldable))

	// ---- STAGE 1: LLM COMPRESSION, the primary path -------------------------
	//
	// THE ORDER IS LLM-FIRST. The rows this fold evicts are the last thing the
	// chat model will ever read, so they are COMPRESSED by that model, on a
	// request that replays the conversation's own cached prefix
	// (buildSummaryRequestFromView) and APPENDS the brief as a new trailing row.
	// The cache is spent exactly once, here; the compacted answer is stored in
	// the DB, so the requests after the fold re-prefill from zero on the new,
	// smaller context. That break is the one this subsystem pays on purpose,
	// because the context was running out and the fold is the event that fixes it.
	//
	// STAGE 1 MUST NOT JUDGE THE HISTORY. It compresses it and drops nothing:
	// deciding what is "no longer needed" is stage 2's job, and stage 2 only runs
	// when compression alone left the conversation over its window. A model asked
	// to summarise on the eve of an eviction drops detail it cannot see the
	// consequences of — the rows are gone from every future request, and this
	// text is all that remains of them.
	//
	// THE SUMMARISER'S OWN DEADLINE. durableFold is handed the tick's dbCtx,
	// whose 45s bound exists to keep the SQLite path off a wedged
	// single-connection pool. That budget is DB work ONLY: the summariser is a
	// chat round-trip on a large prompt, measured at ~3.5 min. Inheriting the 45s
	// bound killed it on every fold of the live incident, so no conversation ever
	// got an LLM checkpoint. context.WithoutCancel keeps the userID claim (the
	// write path needs it) and detaches the caller's deadline; summaryHTTPTimeout
	// is the summariser's own hard ceiling. The DB write keeps its own fresh
	// bound (durableWriteContext, created AFTER this call).
	summariserCtx, cancelSummariser := summariserCallContext(ctx)
	defer cancelSummariser()
	convForSummary := utils.Conversation{
		ID:            conversationID,
		Messages:      messages,
		ModelSelected: payload.ModelSelected,
	}
	if uid, ok := ctx.Value("userID").(string); ok {
		convForSummary.UserID = uid
	}

	// Stage 1 tries the prefix replay first (cache hit, the model reads the real
	// rows) and falls back to the budgeted machine render (fits by construction)
	// when the replayed prefix cannot fit the declared window — the common case,
	// since the fold runs because the context is large. Both are LLM calls.
	llmText, llmErr := attemptFoldCompress(summariserCtx, convForSummary, model, foldable, payload)

	// ---- STAGE 2: SUMMARISE + DROP, when compression is not enough ----------
	//
	// Stage 2 carries the owner's two extra instructions: (1) summarise the
	// context, (2) drop context that is completed and no longer in active use.
	// It runs when stage 1 produced nothing, or a checkpoint that does not fit
	// the fold's ceiling - i.e. the conversation is over its window even
	// compressed, so finished work is what has to go.
	if llmErr == nil && strings.TrimSpace(llmText) != "" && len(llmText) <= ceiling {
		utils.Log("[Eco] stage 1 LLM compression: %d rows, folded %d chars -> checkpoint %d chars (ceiling %d, per-row show %d, chat model %s)",
			len(foldable), foldedChars, len(llmText), ceiling, show, model.Name)
		dbWriteCtx, cancelWrite := durableWriteContext(ctx)
		defer cancelWrite()
		return db.CompactConversationDurable(dbWriteCtx, conversationID, messages, foldFrom,
			llmFoldSummaryRenderer(llmText, ceiling), summaryBound)
	}

	stage1Reason := llmErr
	if stage1Reason == nil {
		stage1Reason = fmt.Errorf("the stage-1 checkpoint is %d chars, over the %d-char ceiling", len(llmText), ceiling)
	}
	utils.Log("[Eco] stage 1 not enough (%v) - escalating to stage 2 SUMMARY+DROP", stage1Reason)

	if txt2, err2 := attemptFoldSummaryFromMegacave(summariserCtx, convForSummary, model, foldable, payload); err2 == nil && strings.TrimSpace(txt2) != "" {
		utils.Log("[Eco] stage 2 LLM summary (summarise + drop completed): %d rows -> checkpoint %d chars (ceiling %d, chat model %s)",
			len(foldable), len(txt2), ceiling, model.Name)
		dbWriteCtx, cancelWrite := durableWriteContext(ctx)
		defer cancelWrite()
		return db.CompactConversationDurable(dbWriteCtx, conversationID, messages, foldFrom,
			llmFoldSummaryRenderer(txt2, ceiling), summaryBound)
	} else {
		stage2Reason := err2
		if stage2Reason == nil {
			stage2Reason = fmt.Errorf("the summariser answered with an empty checkpoint")
		}
		// FAIL-CLOSED, and it is load-bearing: the rows are being evicted, so
		// writing nothing loses them outright. With both model paths unavailable
		// (proxy down, non-200, deadline, empty answer) the fold stores the
		// deterministic machine render, bounded by the same ceiling and
		// announcing any truncation. Lower quality than a model checkpoint;
		// never a lost fold.
		utils.Log("[Eco] BOTH LLM STAGES FAILED, storing the machine render (stage1: %v; stage2: %v): %d rows, room=%d chars, folded=%d chars, ceiling=%d, per-row show=%d",
			stage1Reason, stage2Reason, len(foldable), room, foldedChars, ceiling, show)
		dbWriteCtx, cancelWrite := durableWriteContext(ctx)
		defer cancelWrite()
		return db.CompactConversationDurable(dbWriteCtx, conversationID, messages, foldFrom,
			machineOnlyFoldRenderer(show, ceiling), summaryBound)
	}
}

// durableWriteContext is the durable fold's DB-write context: detached from
// the caller's deadline, with its own fresh 45s bound. It is created AFTER
// the summariser round-trip has finished, never before it.
//
// The pre-fix code derived the write's context from the eco goroutine's
// 45s DB budget BEFORE starting the summariser, so a slow LLM consumed the
// write's entire budget and the fold died at the write:
//
//	20:14:34 [Eco] conv a75308439f290ce744a5e7f1 BOOTSTRAP compaction: sent view
//	           679865 tokens over tail budget (100000 chars) with no checkpoint pair
//	20:15:19 [Eco] machine-only FALLBACK fold (LLM summariser unavailable: summary
//	           request failed: Post "http://127.0.0.1:8787/v1/chat/completions":
//	           context deadline exceeded): 700 rows, room=149124 chars, folded=2038862 chars
//	20:15:19 [ERROR] [Eco] durable fold failed : context deadline exceeded
//
// 45s to the second, on a 2 MB fold — the write never ran. That is the root
// cause of the caching regression: the fold MINTS the checkpoint pair, and
// the pair hands a conversation to the frozen path. With every fold lost, no
// conversation ever minted a pair, so capTail ran the sliding window on EVERY
// turn (live: start idx 655 → 702 across one afternoon, 33 msgs / 178176
// chars on a 100000-char budget), the prompt head moved every turn, cache_read
// stayed pinned flat and ttfb climbed to 83s. A lost fold is not a missed
// optimisation — it is the trigger for the whole churn.
//
// context.WithoutCancel keeps the userID claim the storage layer needs and
// drops the caller's deadline (a cancelled turn must not abort the write);
// the fresh 45s keeps the single-connection bound. The summariser's own
// ceiling (summaryHTTPTimeout) bounds the LLM round-trip, so the two
// deadlines can no longer eat each other. The 45s applies to the write alone,
// which is pure SQLite work: the bound stays honest.
func durableWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
}

// summariserCallContext gives the fold's summarisation round-trip a deadline
// of its OWN. See durableFold: the tick's 45s DB budget is not a budget for a
// chat completion, and inheriting it turned every fold into a machine-only
// fallback. WithoutCancel keeps the request-scoped values (userID), detaches
// the caller's deadline, and applies summaryHTTPTimeout.
func summariserCallContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), summaryHTTPTimeout)
}

// ecoFoldTarget is THE fold target for a model: the size of the live view a
// fold keeps, in prompt tokens. Every consumer of the target must call this,
// because the target is the denominator of a budget split:
//
//	UltracaveOverflowFoldFrom bounds the LIVE TAIL at half the target, and
//	durableFold sizes the new roll-up excerpt at (target - liveTail) * 3.
//
// The two halves only sum to the whole when they come from the SAME number.
// When durableFold moved to the window-derived target and the boundary side
// kept the configured one, the halves were drawn from different wholes and the
// chained roll-up grew past its production ceiling (92598 chars against the
// 75000-char ceiling at the 50000-token default) — the roll-up is what re-
// inflates the next reload above the eco trigger, so a fat roll-up rewrites the
// prompt head every turn and zeroes the KV prefix cache. One function, called
// everywhere, is the fix.
func ecoFoldTarget(modelName string) int {
	_, target := ecoWindowPolicy(ecoConfiguredTrigger(), ecoTargetTokens(),
		Models.DeclaredContextWindow(modelName))
	return target
}

// ecoConfiguredTrigger is the operator's configured eco trigger, for callers
// with cfg-free signatures that need the (trigger, target) PAIR to derive a
// window policy. See ecoTargetTokens, which is the same idea for the target.
func ecoConfiguredTrigger() int {
	cfg := auth.GetConfig().Eco
	if cfg.TriggerTokens > 0 {
		return cfg.TriggerTokens
	}
	return 150_000
}

// ecoTargetTokens is the eco target window (chars budget denominator) with a
// sane fallback when config is unavailable/uninitialised.
func ecoTargetTokens() int {
	cfg := auth.GetConfig().Eco
	if cfg.TargetTokens > 0 {
		return cfg.TargetTokens
	}
	return defaultEcoTargetTokensFallback
}

// defaultEcoTargetTokensFallback mirrors auth's default target so a fold
// before the config is loaded cannot compute a negative excerpt budget.
const defaultEcoTargetTokensFallback = 50_000

// CompactConversationMachineOnly is the durable compaction entry point: pick
// the fold boundary with the SAME arithmetic the eco trigger uses, then fold
// durably. Used by the context-length recovery path in tool_loop.go and by the
// ultracave tests.
//
// `priorSummary` is the CURRENT stored roll-up text; it only feeds the
// budget arithmetic. The storage layer carries it forward VERBATIM and does
// the concatenation itself - passing it in twice would double the roll-up,
// and re-rendering it through the megacave transform would destroy the one
// representation of every row an earlier fold marked compacted.
//
// The trailing variadic `target` carries the conversation's CHAT model + chat
// payload for the LLM-first attempt inside durableFold. It is variadic so every
// existing 5-arg call site keeps compiling: omitted target means "no chat model
// to replay against", i.e. the summariser is skipped and the fold is the
// machine-only render (the previous behaviour). Production call sites MUST pass
// the target.
func CompactConversationMachineOnly(ctx context.Context, conversationID string, messages []utils.Message, priorSummary string, keep int, target ...foldSummaryTarget) (int, error) {
	// The chat model rides in `target`; its window sizes the fold target, so
	// the live-tail bound chosen HERE and the roll-up excerpt durableFold writes
	// come from ONE number (see ecoFoldTarget). Omitting the window here while
	// durableFold used the model's window is what let the chained roll-up reach
	// 92598 chars against a 75000-char ceiling.
	foldWindow := 0
	if len(target) > 0 {
		foldWindow = Models.DeclaredContextWindow(target[0].model.Name)
	}
	foldFrom := UltracaveOverflowFoldFrom(messages, keep, foldWindow)
	if foldFrom <= 0 {
		return 0, nil
	}
	chatModel := utils.Model{}
	payload := ChatPayload{ConversationID: conversationID}
	if len(target) > 0 {
		chatModel = target[0].model
		payload = target[0].payload
		payload.ConversationID = conversationID
	}
	return durableFold(ctx, conversationID, messages, foldFrom, priorSummary, chatModel, payload)
}

// ultracaveMinFoldProgressPercent is the HYSTERESIS of the overflow fold, as
// a percentage of the eco target window: a fold that cannot remove at least
// this much of the SENT VIEW does not fold at all.
//
// WHY A FLOOR ON PROGRESS (measured live, 2026-11-20, conversation
// c363b2b3, /app/data/server.log): the context-length recovery fold ran 20
// times in one day on a 1828-row conversation and the live row count went
// 1828 -> 1855 — UP — because every fold removed ~2 rows and the loop counted
// that as progress. The running `folded=` total grew only ~23k chars across
// all 20 folds. Each of those folds rewrites the checkpoint pair, i.e. the
// HEAD of the prompt, so each one also costs KV prefix reuse: the churn is
// expensive and buys nothing, so the loop can never converge. A fold that
// removes a fraction of a percent of the view is not compaction, and the
// recovery path must be allowed to say "no" and fall through to the ephemeral
// in-memory trim (the documented last resort).
//
// WHY 5% OF THE TARGET: the recovery path gets ultracaveMaxFoldsPerTurn folds
// per turn, so two folds must be enough to move a conversation materially
// away from the window — 2 x 5% of the target does that, and the observed
// churn (~23k chars over 20 folds, ~2 rows each) is rejected by three orders
// of magnitude (5% of the 50k-token default target = 2_500 tokens = 7_500
// wire chars, vs ~2 rows ≈ tens of chars). It is a FRACTION OF THE TARGET,
// not a row count, because the view's cost is the sent view (chars/3): 2 rows
// of 100 chars and 2 rows of 100k chars are not the same fold.
const ultracaveMinFoldProgressPercent = 5

// ultracaveMinFoldProgressTokens is the hysteresis threshold in sent-view
// tokens (the same measure SentViewTokens uses).
// ultracaveMinFoldProgressTokens is the hysteresis floor for a fold. Pass the
// fold's target to keep the floor consistent with the boundary arithmetic
// (ecoFoldTarget); omitted, it uses the configured target, which is what the
// tests pin.
func ultracaveMinFoldProgressTokens(target ...int) int {
	t := ecoTargetTokens()
	if len(target) > 0 && target[0] > 0 {
		t = target[0]
	}
	return t * ultracaveMinFoldProgressPercent / 100
}

// ultracaveFoldProgressTokens is what a fold at `foldFrom` BUYS, in sent-view
// tokens: the wire size of the live rows it removes, divided by the estimator's
// char/3.
//
// The checkpoint pair's rows are EXCLUDED on purpose. The storage layer carries
// the prior roll-up forward VERBATIM (see PriorSummaryFromView), so folding the
// pair's rows removes NOTHING from the sent view — counting them would let a
// fold that only shuffles the pair pass the hysteresis floor. The new excerpt
// the fold writes is bounded by the budget arithmetic in durableFold (at most
// HALF the wire size of the rows it replaces), so rows removed here is the
// honest measure of progress.
func ultracaveFoldProgressTokens(messages []utils.Message, foldFrom int) int {
	removed := 0
	for i := 0; i < foldFrom && i < len(messages); i++ {
		if db.IsCheckpointMessage(messages[i]) || messages[i].Compacted {
			continue
		}
		removed += SentViewRowChars(messages[i])
	}
	return removed / 3
}

// windowTokensOr unwraps the variadic window argument (0 = unknown).
func windowTokensOr(windowTokens []int) int {
	if len(windowTokens) > 0 {
		return windowTokens[0]
	}
	return 0
}

// overflowFoldBudgetTokens is the CEILING on everything a fold leaves in the
// prompt: live tail + roll-up, in tokens, for a model with this window.
//
// WHY THE WINDOW AND NOT THE ECO TARGET (2026-11-25, conversation
// 9f0ab2352bf2bdc40e77384b): the overflow fold's arithmetic was denominated in
// ecoTargetTokens (144000 on this deployment) and then HALVED for the tail
// (72000), and durableFold's roll-up room was (target - live tail) * 3. With a
// prior roll-up already at 105805 chars the room went NEGATIVE, `chars`
// collapsed to 0, the ceiling collapsed to the per-row floor, and the stored
// roll-up was clamped to the 100000-char global constant (~33333 tokens) —
// which is the "34K" the owner named (the fold logged a 34042-token view). The
// window never entered the arithmetic at all, so a 262144-token model was
// compacted as if it had 72000.
//
// The budget reserves the prompt overhead (system block + tool schemas,
// measured 21521-28395 tokens live, bounded by promptOverheadTokens) and the
// reply reservation, so the post-fold request fits the window with room for
// the answer.
func overflowFoldBudgetTokens(windowTokens int) int {
	if windowTokens <= 0 {
		return 0
	}
	budget := windowTokens - promptOverheadTokens - guardReplyReserve
	if budget <= 0 {
		return 0
	}
	return budget
}

// overflowFoldTailBudgetTokens is the live-tail share of the fold budget. The
// remaining third is what the roll-up summarises, so the split is what keeps
// durableFold's room positive: a tail at the FULL budget leaves the roll-up
// nothing, the render collapses to its per-row floor, and the stored
// checkpoint loses everything it summarised.
func overflowFoldTailBudgetTokens(budgetTokens int) int {
	if budgetTokens <= 0 {
		return 0
	}
	return budgetTokens * 2 / 3
}

// overflowFoldRollupRoomChars is durableFold's roll-up room in CHARS for a
// fold that leaves this live tail: the third of the budget the tail did not
// use, at summaryCharsPerToken chars/token. It must clear
// db.MaxDurableSummaryChars, the global clamp — when the clamp binds, the
// stored roll-up is ~33333 tokens on a 262144-token window, which is the
// 2026-11-25 failure.
func overflowFoldRollupRoomChars(budgetTokens, tailTokens int) int {
	if budgetTokens <= 0 || tailTokens >= budgetTokens {
		return 0
	}
	return (budgetTokens - tailTokens) * summaryCharsPerToken
}

// bootstrapCutoffRefined cuts INSIDE the newest user turn when no user
// boundary fits.
//
// bootstrapCutoff's contract is "cut at a user row", and in an agent
// conversation a user turn is ONE user row followed by hundreds of tool rows.
// Measured on the live conversation, the sent-view suffix at user boundaries:
//
//	idx 2677  249602 tokens
//	idx 2690  233063 tokens
//	idx 2793   53273 tokens   <- ~100 rows, no user row in between
//	idx 2818   30797 tokens
//
// There is no boundary between 233063 and 53273, so a target of 72000 has no
// legal cut and bootstrapCutoff returns the newest user row: the fold takes
// the ENTIRE conversation (observed: 1196 rows folded, history 2439 -> 5
// messages). Cutting at a tool row is legal for the window (the sender's
// tool_call backfill repairs the orphaned call), so the refinement walks to
// the first index of ANY role whose suffix fits, then snaps FORWARD to a legal
// window start — forward, because that only shrinks the tail and can never
// break the fit.
//
// Returns -1 when there is nothing to refine: the view already fits, the fit
// point is the newest user row itself, or no legal start exists below it.
func bootstrapCutoffRefined(messages []utils.Message, targetTokens int, lu int) int {
	if lu <= 0 || targetTokens <= 0 {
		return -1
	}
	suffix := 0
	for i := range messages {
		suffix += SentViewRowChars(messages[i])
	}
	for i := 0; i < lu; i++ {
		if suffix/summaryCharsPerToken <= targetTokens {
			b := i
			for b < lu && (messages[b].Role == "tool" || messages[b].Compacted || db.IsCheckpointMessage(messages[b])) {
				b++
			}
			if b <= 0 || b >= lu {
				return -1
			}
			return b
		}
		suffix -= sentViewRowCharsLive(messages[i])
	}
	return -1
}

// UltracaveOverflowFoldFrom picks the durable fold boundary for the
// context-length overflow path (tool_loop.go), using the SAME arithmetic the
// eco trigger uses: findCutoffIndex walks PromptTokens toward
// cfg.TargetTokens and lands on a clean turn boundary.
//
// Returns the fold boundary (>0), or -1 when nothing safe can be folded:
//   - the newest user turn is the FLOOR (the fold never crosses it, so the
//     user's live ask is never summarized away);
//   - the boundary may never sit on a `tool` row (its parent assistant call
//     would be folded, orphaning the result at the head of the request), nor
//     on the checkpoint pair (folding the pair rewrites the prompt head);
//   - folding from 0 would fold the whole conversation;
//   - HYSTERESIS: on a view already over the eco target — the churn regime —
//     a fold that buys less than ultracaveMinFoldProgressTokens is refused.
//
// A -1 means "no durable fold": the caller falls through to the ephemeral
// in-memory trim, and because no write happens the checkpoint pair stays
// BYTE-IDENTICAL. That is the existing 0-row contract this path relies on.
//
// BOUNDARY PRIORITY, in the order the two measures are trusted:
//  1. findCutoffIndex (cumulative provider prompt_tokens) — the eco trigger's
//     own arithmetic, so a fold and a trigger tick agree.
//  2. bootstrapCutoff (the SENT VIEW in chars, SentViewRowChars) — the measure
//     the frozen path actually ships. This is the fix for the 2-row churn:
//     findCutoffIndex's lastPT is the provider's count of the LAST request,
//     which the sliding window caps (~33k tokens), so on a 1855-row / 41k-token
//     eco view it returns NOTHING (lastPT ≤ target) and the old code fell to
//     rule 3 and folded 2 rows. The char boundary folds until the REST fits the
//     target, so one fold removes a meaningful fraction of the conversation.
//  3. keep the newest `keep` rows — reached ONLY when the sent view already
//     fits the target, i.e. the conversation is not in the churn regime and the
//     caller's explicit "keep N" is the whole request. Snapped to the same
//     legal boundaries.
//
// `windowTokens` (variadic, so existing call sites keep the configured
// semantics) is the conversation model's context window. It sizes the fold
// BUDGET (see overflowFoldBudgetTokens), which bounds the live tail and,
// through the same number, the roll-up the fold writes. Production passes the
// model's window so the tail bound and durableFold's excerpt budget are drawn
// from ONE number.
//
// When no user boundary leaves a tail inside that budget — the agent-turn
// cliff, see bootstrapCutoffRefined — the boundary is refined INSIDE the
// newest turn rather than folding the whole conversation.

func UltracaveOverflowFoldFrom(messages []utils.Message, keep int, windowTokens ...int) int {
	if len(messages) == 0 {
		return -1
	}
	lu := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && !db.IsCheckpointMessage(messages[i]) {
			lu = i
			break
		}
	}
	if lu <= 0 {
		// No user row (or the only user row is the first): nothing safe to fold.
		return -1
	}
	if keep <= 0 {
		keep = 24
	}
	target := ecoTargetTokens()
	if len(windowTokens) > 0 {
		_, target = ecoWindowPolicy(ecoConfiguredTrigger(), target, windowTokens[0])
	}

	// The SENT VIEW, the measure the frozen path ships and the measure the
	// hysteresis floor is a fraction of. `overTarget` is the churn regime: the
	// view is already bigger than compaction's target, so a fold here must buy
	// a real fraction of the target, not 2 rows.
	viewTokens := SentViewTokens(messages)
	overTarget := viewTokens > target
	cand := -1
	if lastPT := lastAssistantPromptTokens(messages); lastPT > 0 {
		ckptEnd := -1
		for i, m := range messages {
			if m.Role == "assistant" && db.IsCheckpointMessage(m) {
				ckptEnd = i + 1
				break
			}
		}
		cand = findCutoffIndex(messages, lastPT, target, ckptEnd)
	}
	if cand <= 0 && overTarget {
		// FIX for the 2-row churn: fall back to the CHAR boundary (the same
		// measure the frozen path ships), not to `keep`. FindCutoffIndex is
		// cumulative prompt_tokens and the sliding window caps lastPT, so it
		// returns nothing exactly when the stored view is far over the target
		// — and `keep 24` on a 1855-row conversation is then a fold of 2 rows
		// after the floor/clamp. bootstrapCutoff folds the oldest rows until
		// the REST fits the target, on a user boundary.
		//
		// The target is halved on purpose. durableFold sizes the new
		// roll-up at (target - liveTail) * 3, so a boundary that leaves
		// the live tail AT the target leaves the roll-up no room at all:
		// the render collapses to its per-row floor (observed: 90 rows,
		// folded 113930 chars, budget 7416) and the stored checkpoint
		// loses almost everything it summarized. Halving keeps the live
		// tail under half the target and the roll-up under the other
		// half, which is the split the eco config itself implies
		// (TriggerTokens = 2 x TargetTokens).
		// The tail budget is the window's, not half the eco target. Halving
		// the target was a proxy for "leave the roll-up room", and on a
		// 262144-token window it asked for a 72000-token tail: the fold cut
		// to a FIFTH of the window and the model answered from a fragment
		// (observed: 24683 prompt_tokens, ~3160 of them history). The split
		// is now explicit and window-derived: tail = 2/3 of the fold budget,
		// roll-up = the rest (see overflowFoldTailBudgetTokens).
		tailBudget := overflowFoldTailBudgetTokens(overflowFoldBudgetTokens(windowTokensOr(windowTokens)))
		if tailBudget <= 0 {
			// No window known: keep the configured semantics, unchanged —
			// half the eco target, the split the config itself implies
			// (TriggerTokens = 2 x TargetTokens).
			tailBudget = target / 2
		}
		cand = bootstrapCutoff(messages, tailBudget)
		if cand >= lu && lu > 0 {
			// THE CLIFF. bootstrapCutoff cuts only at user rows, and an agent
			// turn is one user row followed by hundreds of tool rows: the
			// suffix jumps from 233063 tokens straight to 53273 across one
			// boundary, so NOTHING up to the newest user row fits and the
			// cutoff lands on the floor — a fold of the WHOLE conversation.
			// Refine inside the turn instead.
			if refined := bootstrapCutoffRefined(messages, tailBudget, lu); refined > 0 {
				utils.Log("[Eco] overflow fold: no user boundary fits %d tokens (cliff at the newest user row %d) — refining inside the turn to %d",
					tailBudget, lu, refined)
				cand = refined
			}
		}
	}
	if cand <= 0 {
		// The view is NOT over target and the token arithmetic gave nothing:
		// honour the caller's explicit "keep the newest `keep` rows" (the
		// pre-existing behaviour, and the shape of a fold requested right after
		// a provider 500 with no prompt_tokens yet).
		cand = len(messages) - keep
	}
	// Never fold past the newest user turn, and never start the live window
	// on an orphan tool row / the checkpoint pair.
	if cand > lu {
		cand = lu
	}
	for cand > 0 && cand < len(messages) && (messages[cand].Role == "tool" || db.IsCheckpointMessage(messages[cand])) {
		cand--
	}
	if cand <= 0 || cand >= len(messages) {
		return -1
	}
	// HYSTERESIS. In the churn regime a fold must buy at least
	// ultracaveMinFoldProgressTokens of sent view; below that the fold is
	// refused, which costs NO write, so the checkpoint pair stays byte-identical
	// and the caller falls through to the ephemeral in-memory trim (the
	// documented 0-row contract, and the only way the 20-folds-for-2-rows loop
	// observed on 2026-11-20 can terminate).
	if overTarget {
		if bought := ultracaveFoldProgressTokens(messages, cand); bought < ultracaveMinFoldProgressTokens(target) {
			utils.Log("[Eco] overflow fold refused (hysteresis): view %d tokens over target %d, boundary %d buys only %d tokens (< %d, %d%% of target) — keeping the prompt head and letting the caller trim in memory",
				viewTokens, target, cand, bought, ultracaveMinFoldProgressTokens(), ultracaveMinFoldProgressPercent)
			return -1
		}
	}
	return cand
}

// tickFoldBoundary picks the durable fold boundary for the eco TICK
// (runEcoSummary) — the fold every conversation runs on, and for a
// conversation with NO checkpoint pair it is the fold EVERY NEW CONVERSATION
// runs on. Pure: no I/O, no clock, no LLM, so both gates below are testable
// directly.
//
// RULE 1 — THE NO-PAIR RULE: with no pair (existingCheckpointEndIdx < 0) the
// boundary comes ONLY from bootstrapCutoff, the char-based measure of the view
// the frozen path actually ships. findCutoffIndex is NEVER consulted, and a
// view that already fits the target folds NOTHING.
//
// Why: findCutoffIndex's arithmetic is targetDrop = lastPT - target, and
// lastPT is the provider's prompt count of the newest turn — system prompt +
// tool schemas + the whole window, i.e. mostly NOT history. Live trace, conv
// a44a43258742259a60be242d (a NEW conversation, 68 rows, NO checkpoint pair,
// max prompt_tokens 52709), /app/data/server.log 2026-11-21 10:51:02:
//
//	BOOTSTRAP compaction: sent view 46331 tokens over tail budget (100000
//	  chars) with no checkpoint pair, lastPT=52709 below trigger=150000
//	  — compacting toward target=50000
//	cutoff=67 (of 68 messages), prior checkpoint end=-1
//
// targetDrop = 52709 - 50000 = 2709: the LAST assistant crossing 2709
// cumulative tokens is the second-to-last row, so cutoff = 67 of 68 = fold
// EVERYTHING older than the newest turn, on a brand-new conversation, toward
// a target (50000) the sent view (46331) ALREADY meets. The fold then removed
// 0 rows and left the pair byte-identical — pure churn of the prompt head,
// and with the cache trace below it is not free: cache_read=24137 FLAT across
// 8 turns while tok_before grew 31949 -> 107780, ttfb_ms 4475 -> 74034.
//
// RULE 2 — THE HYSTERESIS FLOOR: any boundary, pair or no-pair, that buys
// less than ultracaveMinFoldProgressTokens of sent view is REFUSED. That is
// the floor UltracaveOverflowFoldFrom already enforces (its 2026-11-20 trace:
// 20 folds removing ~2 rows each, live row count 1828 -> 1855, UP) — the tick
// fold must obey it too, or the same conversation rewrites the prompt head
// every turn for ~2 rows. A refused fold writes NOTHING, so the pair stays
// byte-identical: the documented 0-row contract.
//
// RULE 3 — the boundaries the storage layer enforces, applied here so the
// caller-side log is honest: the newest user row is the FLOOR, the live window
// never starts on a `tool` row, never on the checkpoint pair, never empty.
//
// Returns a boundary (> 0) or -1 = NO FOLD, logging the specific reason.
func tickFoldBoundary(messages []utils.Message, lastPT, target, existingCheckpointEndIdx int) int {
	viewTokens := SentViewTokens(messages)
	lu := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && !db.IsCheckpointMessage(messages[i]) {
			lu = i
			break
		}
	}
	if lu <= 0 {
		utils.Log("[Eco] tick fold refused: no non-checkpoint user row past index 0 (view %d tokens, %d messages, lastPT=%d) — a fold would leave no live window; pair stays byte-identical",
			viewTokens, len(messages), lastPT)
		return -1
	}

	cand := -1
	if existingCheckpointEndIdx < 0 {
		// NO-PAIR RULE, in both directions.
		if viewTokens <= target {
			utils.Log("[Eco] tick fold refused: sent view %d tokens already fits target %d (lastPT=%d, %d messages, NO checkpoint pair) — no fold needed, folding here would only churn the prompt head; pair stays byte-identical",
				viewTokens, target, lastPT, len(messages))
			return -1
		}
		cand = bootstrapCutoff(messages, target)
		if cand <= 0 {
			utils.Log("[Eco] tick fold refused: bootstrapCutoff gave no boundary (view %d tokens, target %d, lastPT=%d, %d messages) — pair stays byte-identical",
				viewTokens, target, lastPT, len(messages))
			return -1
		}
	} else {
		// THE VIEW-FITS GATE, PAIR BRANCH. The no-pair branch refuses a view
		// that already fits the target; this branch had no such gate, so it
		// folded on lastPT alone — and lastPT is the provider's prompt count
		// of the last request, which measures the slice that request shipped.
		// Live, conv af46ee1c387e35a6e5467822 (2026-11-22 21:59:50): the sent
		// view was 111080 tokens, the target 144000 — the view FIT — and
		// lastPT=241649 (inflated by folded rows the request re-shipped) drove
		// targetDrop=97649 below the prompt_tokens floor of every live row, so
		// every assistant "crossed", the cutoff landed on the newest row and
		// 169 of 175 rows folded: 111080 -> 34125 tokens. A fold that lands
		// under the target on a view that already fits is not compaction, it
		// is context loss, and it rewrites the prompt head to do it.
		if viewTokens <= target {
			utils.Log("[Eco] tick fold refused: sent view %d tokens already fits target %d (lastPT=%d, %d messages, checkpoint end=%d) — lastPT measures the request, not the frozen view; folding here would ship a view under the target and rewrite the prompt head for nothing; pair stays byte-identical",
				viewTokens, target, lastPT, len(messages), existingCheckpointEndIdx)
			return -1
		}
		cand = findCutoffIndex(messages, lastPT, target, existingCheckpointEndIdx)
		if cand <= 0 {
			// A pair exists, so the frozen path owns the view and the token
			// arithmetic is the trigger's own measure; when it cannot cut,
			// fall to the char boundary (the measure the view ships).
			cand = bootstrapCutoff(messages, target)
		}
		if cand <= 0 {
			utils.Log("[Eco] tick fold refused: no boundary from prompt_tokens (lastPT=%d target=%d) nor from the sent view (%d tokens, target %d, %d messages) — pair stays byte-identical",
				lastPT, target, viewTokens, target, len(messages))
			return -1
		}
	}

	// Boundary legality (the storage layer clamps identically; snapping here
	// keeps the logged boundary the one the fold will use).
	if cand > lu {
		cand = lu
	}
	for cand > 0 && cand < len(messages) && (messages[cand].Role == "tool" || db.IsCheckpointMessage(messages[cand])) {
		cand--
	}
	if cand <= 0 || cand >= len(messages) {
		utils.Log("[Eco] tick fold refused: boundary snapped out of range (cand=%d of %d messages, newest user row %d, view %d tokens) — pair stays byte-identical",
			cand, len(messages), lu, viewTokens)
		return -1
	}

	// THE HYSTERESIS FLOOR.
	if bought := ultracaveFoldProgressTokens(messages, cand); bought < ultracaveMinFoldProgressTokens(target) {
		utils.Log("[Eco] tick fold refused (hysteresis): view %d tokens, target %d, boundary %d of %d buys only %d tokens (< floor %d, %d%% of target) — not compaction; pair stays byte-identical",
			viewTokens, target, cand, len(messages), bought, ultracaveMinFoldProgressTokens(), ultracaveMinFoldProgressPercent)
		return -1
	}
	return cand
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

// hasCheckpointPair reports whether the slice carries an eco checkpoint pair.
func hasCheckpointPair(messages []utils.Message) bool {
	for i := range messages {
		if messages[i].Role == "assistant" && db.IsCheckpointMessage(messages[i]) {
			return true
		}
	}
	return false
}

// countCompactedRows counts the rows durable compaction flagged in this slice.
// Zero when the slice came from the LLM loader (which excludes them), which is
// itself worth logging: it says whether the caller's slice is the LLM view.
func countCompactedRows(messages []utils.Message) int {
	n := 0
	for i := range messages {
		if messages[i].Compacted {
			n++
		}
	}
	return n
}
