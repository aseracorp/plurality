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
// The only transform left on the normal path is truncateOversizedTail's
// PER-MESSAGE bound: each row is bounded as a pure function of that stored
// row (its own length + its own truncation banner), never of the slice
// total or a window position, so it is byte-stable across turns.
//
// The system prompt is prepended elsewhere (in SendChatCompletion) and is
// not part of `messages` here.
func filterCheckpointsForRequest(messages []utils.Message, ecoOn bool) []utils.Message {
	if !ecoOn {
		return db.FilterOutCheckpoints(messages)
	}

	// Find the LAST checkpoint pair (assistant half) — the rolling design
	// keeps only one, but defensive against any stale extras.
	lastCheckpointIdx := -1
	for i, m := range messages {
		if m.Role == "assistant" && db.IsCheckpointMessage(m) {
			lastCheckpointIdx = i
		}
	}
	// No checkpoint yet: this conversation has never been compacted, so
	// there is no frozen head to protect and NOTHING bounds the history.
	// A giant raw history must not be shipped whole — that is exactly how
	// "WordPress Security Review" (1866 msgs, ~770k tokens) kept dying with
	// context_length_exceeded. So the BOOTSTRAP case keeps the budget-capped
	// tail: bounded on every request, at the cost of a moving head, until
	// the first durable fold mints the pair (after which the frozen path
	// below takes over). The overflow path in tool_loop.go is what forces
	// that first fold for a conversation that never crossed the eco trigger.
	if lastCheckpointIdx < 0 {
		return capTail(messages)
	}

	// Frozen prefix: the pair, then every live row after it. No window, no
	// total-size-dependent cap — see the doc comment. The tradeoff is
	// explicit: an unbounded tail CAN exceed the model's window. That is not
	// recovered per-request by sliding the head (which would destroy the
	// cache); it is recovered by a DURABLE fold from the context-length error
	// path (tool_loop.go), and only as a last resort by the in-memory trim
	// there. The preemptive guard in index.go (SentViewTokens / estTokens)
	// turns "about to exceed" into that durable fold before shipping.
	tail := messages[lastCheckpointIdx:]
	return truncateOversizedTail(tail)
}

// maxTailChars is the character budget for the live tail that is sent to
// the provider. ~100k chars ≈ 25-30k tokens, safe for any 128k-context
// model, leaving room for system + output.
//
// It is NO LONGER the per-request bound of an eco-compacted conversation:
// the frozen-prefix path in filterCheckpointsForRequest applies no window at
// all, and durable compaction is what bounds the view. maxTailChars now only
// bounds the BOOTSTRAP case (a conversation that has never been compacted,
// so it has no frozen head whose stability is worth protecting).
const maxTailChars = 100_000

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
// capTailWindow computes the START index of the live window capTail keeps,
// and the char total of the rows from that index on.
//
// The accounting uses SentViewRowChars — the size the row ACTUALLY ships
// (per-message bound included, the same clamp truncateOversizedTail applies
// and the same measure the eco trigger uses) — not TotalContentChars. The
// old walker summed PRE-truncation sizes, so the log claimed a 100k budget
// while the window shipped far more (measured live on this very
// conversation: "kept 13 msgs / 1189684 chars" against maxTailChars=100000),
// and one 890KB tool result pushed the walker past every older row, evicting
// 98 rows of context in a single turn (start idx 1509 -> 1607).
//
// Rows bounded at maxCompactMessageChars each, budget maxTailChars: when the
// newest row ALONE exceeds the budget the window is that one row — it ships
// truncated (truncateOversizedTail bounds it), never an empty window.
//
// Returns the start index; 0 when the whole slice fits the budget.
func capTailWindow(tail []utils.Message) int {
	chars := 0
	start := len(tail)
	for i := len(tail) - 1; i >= 0; i-- {
		c := SentViewRowChars(tail[i])
		if chars+c > maxTailChars {
			if start == len(tail) {
				// The newest row alone exceeds the budget: keep it (it ships
				// bounded); an empty window answers nothing.
				start = i
			}
			break
		}
		chars += c
		start = i
	}
	if start == len(tail) {
		// Empty tail.
		return 0
	}
	return start
}

// capTail bounds a message slice to the live-tail budget so a conversation
// can never ship a context-busting history to the provider. Applied ONLY to
// the bootstrap case (no checkpoint pair yet — see
// filterCheckpointsForRequest): a never-compacted conversation with a huge
// raw history must be bounded on every request, or every turn dies with
// context_length_exceeded. Once a durable pair exists the frozen path takes
// over and this cap must NOT run (it moves the prompt head and kills KV
// prefix reuse — the contract pinned by ultracave_prefix_test.go).
//
// Two rules the implementation must keep:
//
//   - BUDGET = WHAT SHIPS: the walker sums SentViewRowChars (post-bound), so
//     the kept window provably fits maxTailChars and the log line is honest.
//   - ANCHOR FORWARD: the window must open on a user row so no tool result
//     is orphaned at the head (its parent call sits in the dropped section).
//     The old code backed UP to the newest user row, re-adding rows the
//     walker had just excluded — measured live, a 98-row drag that shipped
//     1.19M chars on a 100k budget. Walking FORWARD to the first user row
//     drops the OLD side of the window instead, which costs history the
//     eviction is about to fold anyway and can never orphan a tool result.
//     Only when the slice ENDS in a tool/assistant wall (no user row at or
//     after the walker's start) does the anchor fall back to the newest user
//     row — bounded by that tool chain, the same floor
//     CompactConversationDurable uses.
func capTail(tail []utils.Message) []utils.Message {
	if len(tail) == 0 {
		return tail
	}
	start := capTailWindow(tail)
	if start > 0 {
		anchor := start
		for anchor < len(tail) && tail[anchor].Role != "user" {
			anchor++
		}
		if anchor < len(tail) {
			start = anchor
		} else {
			for start > 0 && tail[start].Role != "user" {
				start--
			}
		}
	}
	if start > 0 {
		// Re-measure the ANCHORED window for the log (the anchor may have
		// moved start; the walker's total covers the pre-anchor set). The
		// log line is the operator's only view of what the cap shipped —
		// after the bug this printed "100k" while shipping 1.19M chars.
		kept := 0
		for i := start; i < len(tail); i++ {
			kept += SentViewRowChars(tail[i])
		}
		utils.Log("[Eco] capping tail by budget: kept %d msgs / %d chars (budget %d, start idx %d) for context safety",
			len(tail)-start, kept, maxTailChars, start)
		tail = tail[start:]
	}
	// The budget bounds whole rows; a single oversized row that is KEPT
	// (e.g. the lone newest row over the budget) still needs its per-message
	// bound applied. truncateOversizedTail is a pure function of each stored
	// row, so it is byte-stable across turns.
	return truncateOversizedTail(tail)
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
		if m.Role == "user" {
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
		suffix += SentViewRowChars(messages[i])
	}
	for i := 0; i <= lu; i++ {
		if messages[i].Role == "user" {
			fits := suffix/3 <= targetTokens
			if i == 0 {
				if fits {
					return -1 // the whole view fits; folding 0 rows is a no-op
				}
			} else if fits {
				return i
			}
		}
		suffix -= SentViewRowChars(messages[i])
	}
	// Nothing up to the newest user row fits: fold everything older than it.
	// A fold that leaves only the newest user turn is the floor; the storage
	// layer clamps the same way.
	return lu
}

// bootstrapFoldNeeded reports whether runEcoSummary must fold a conversation
// that has NOT crossed the prompt-tokens trigger.
//
// The trigger measures lastPT = the provider's prompt count of the last
// turn. For a conversation with NO checkpoint pair, capTail caps that prompt
// at maxTailChars (~33k tokens) on every turn, so lastPT can NEVER reach a
// trigger far above the cap — the conversation that most needs compaction
// is the one the trigger can never see. Self-locking, observed live: 98 rows
// evicted in one turn at lastPT=85121 < trigger=150000, the eviction storm
// repeating every turn, no pair ever minted, the sliding window running
// forever. The sliding window evicting rows IS compaction pressure: hand it
// to the durable fold, which mints the pair, after which the frozen path
// takes over and the trigger measures the real (unslid) view.
//
// Pure decision (no I/O) so the gate itself is testable.
func bootstrapFoldNeeded(existingCheckpointEndIdx, lastPT, triggerTokens, sentViewTokens int) bool {
	if lastPT > triggerTokens {
		return true
	}
	return existingCheckpointEndIdx < 0 && sentViewTokens*3 > maxTailChars
}

// maxKeepTailChars bounds how many characters of a tail message are kept
// when a single message is too large to fit the budget whole. It preserves
// the head of the message so the model still sees what the text was about.
// (The tail-cap walker above uses the same overall budget; this is the
// per-message bound so one giant message can never blow the context window
// on its own.)
const maxKeepTailChars = 100_000

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
func truncateOversizedTail(tail []utils.Message) []utils.Message {
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
		if total <= maxKeepTailChars {
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
		if truncLen > maxKeepTailChars {
			truncLen = maxKeepTailChars
		}
		trunc := textsafe.HeadBytes(txt, truncLen)
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

	// Locate the existing checkpoint (if any) BEFORE the trigger gate: the
	// bootstrap case (no pair) has its own gate — see bootstrapFoldNeeded.
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
	if !bootstrapFoldNeeded(existingCheckpointEndIdx, lastPT, cfg.TriggerTokens, sentView) {
		utils.Log("[Eco] conv %s lastPT=%d below trigger=%d — skip", conversationID, lastPT, cfg.TriggerTokens)
		return
	}
	if lastPT > cfg.TriggerTokens {
		utils.Log("[Eco] conv %s lastPT=%d > trigger=%d — compacting toward target=%d", conversationID, lastPT, cfg.TriggerTokens, cfg.TargetTokens)
	} else {
		// The self-locking trigger, broken: no pair exists, the sliding
		// capTail window is over budget every turn, and lastPT can never
		// reach the trigger because the window caps the prompt. The window
		// evicting rows IS compaction pressure — fold it durably now, which
		// mints the pair and hands the conversation to the frozen path.
		utils.Log("[Eco] conv %s BOOTSTRAP compaction: sent view %d tokens over tail budget (%d chars) with no checkpoint pair, lastPT=%d below trigger=%d — compacting toward target=%d",
			conversationID, sentView, maxTailChars, lastPT, cfg.TriggerTokens, cfg.TargetTokens)
	}

	cutoff := findCutoffIndex(conv.Messages, lastPT, cfg.TargetTokens, existingCheckpointEndIdx)
	if cutoff < 0 && existingCheckpointEndIdx < 0 {
		// Bootstrap: the PT arithmetic cannot cut (lastPT is window-capped,
		// often below target while the stored view is far over it). Use the
		// char-based cutoff so the self-lock cannot survive in this shape.
		cutoff = bootstrapCutoff(conv.Messages, cfg.TargetTokens)
	}
	if cutoff < 0 {
		utils.Log("[Eco] no satisfying cutoff for conv %s (lastPT=%d target=%d, %d messages) — skip", conversationID, lastPT, cfg.TargetTokens, len(conv.Messages))
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
	logFoldResult(dbCtx, conversationID, folded, before, lastPT, cfg.TargetTokens)
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
// The per-message clamp matters for the same reason: it must match what
// truncateOversizedTail / TruncateOversizedMessages will do to that row, so
// the estimate never over-counts a blob the sender is going to bound anyway
// (which would fire the guard on a conversation that fits).
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
//   - A row over the per-message cap is counted AT the cap, because that is
//     what the sender ships: the cap is maxCompactMessageChars, the bound
//     TruncateOversizedMessages applies, and eco's tail bound
//     (maxKeepTailChars) is the same number. A test pins that equality.
//
// It deliberately does NOT subtract the stored caveman projection.
// convertMessagesToOpenAI ships that projection only for caveman-enabled
// models (cavemanCompressApplies), so subtracting it would make the trigger
// measure model-dependent, and the eco trigger is evaluated per conversation
// (whose next turn may use any model). Counting the raw body is the
// conservative direction: a compressed model folds marginally early, and the
// estimator can never under-count and let an oversized request ship.
//
// It is a pure function of the stored row: no clock, no slice position, no
// total-size dependence. Same row in, same number out, every turn.
func SentViewRowChars(m utils.Message) int {
	chars := m.Content.TotalContentChars()
	if chars > maxCompactMessageChars {
		chars = maxCompactMessageChars
	}
	return chars
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
	target := ecoTargetTokens()
	// Live tail this fold leaves in place, and the roll-up it carries
	// forward, both in chars — the budget is what is LEFT for the new
	// excerpt after both are paid for.
	tailTokens := SentViewTokens(messages[foldFrom:])
	priorChars := len(priorSummary)
	overhead := megacaveLabelOverheadChars
	room := (target-tailTokens)*3 - priorChars - 2*overhead

	foldable := messages[:foldFrom]
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
	// clamped to the storage bound (db.MaxDurableSummaryChars) so the ai layer
	// and the storage layer agree on ONE number — the renderer's ceiling is the
	// number that gets applied, not the storage backstop slicing the text after
	// the fact.
	if chars > db.MaxDurableSummaryChars {
		chars = db.MaxDurableSummaryChars
	}
	// The ceiling is passed EXPLICITLY, so "no ceiling" (0 = unlimited in the
	// renderer) must never reach it: a fold with no room renders at the smallest
	// honest size — the floor the row budget uses — so the fit test FAILS and the
	// resolver escalates, and the excerpt stays bounded either way.
	ceiling := chars
	if ceiling <= 0 {
		ceiling = (minMegacaveRowShowChars + megacaveLabelOverheadChars) * len(foldable)
	}
	show := MegacaveRowBudget(chars, len(foldable))

	// The durable WRITE gets its own bounded context, detached from the caller's
	// deadline. The eco goroutine caps its DB work at 45s (the single SQLite
	// connection must never be held by a wedged compaction), and that cap is
	// correct for DB work — but the summariser below can legitimately run for
	// minutes on a self-hosted model. If the write shared that 45s deadline, a
	// slow LLM would blow it and the fold would fail: the LLM's latency would
	// LOSE A FOLD, which is the one thing this design forbids. context.WithoutCancel
	// keeps the user claim, drops the caller's deadline; the fresh 45s keeps the
	// connection bound intact.
	dbWriteCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancelWrite()

	// ---- LLM FIRST: the last look at the rows about to be evicted ----------
	//
	// The evicted rows are rendered for the diagnostic (how much text the
	// checkpoint is standing in for), then the chat model is asked for the
	// durable checkpoint on a request that replays the conversation's own
	// cached prefix. SUCCESS feeds the LLM text into the megacave renderer, so
	// the stored section is itself megacaved and stays under `ceiling`.
	rendered := renderEvictedRowsForSummary(foldable)
	convForSummary := utils.Conversation{
		ID:            conversationID,
		Messages:      messages,
		ModelSelected: payload.ModelSelected,
	}
	if uid, ok := ctx.Value("userID").(string); ok {
		convForSummary.UserID = uid
	}
	if llmText, llmErr := attemptFoldSummary(ctx, convForSummary, model, foldable, payload); llmErr == nil && strings.TrimSpace(llmText) != "" {
		utils.Log("[Eco] LLM fold summary: %d rows, evicted text %d chars, LLM checkpoint %d chars, ceiling %d chars (chat model %s)",
			len(foldable), len(rendered), len(llmText), ceiling, model.Name)
		return db.CompactConversationDurable(dbWriteCtx, conversationID, messages, foldFrom,
			llmFoldSummaryRenderer(llmText, ceiling))
	} else if llmErr != nil {
		// The fallback must be LOUD: a fold that silently lost the LLM summary
		// is indistinguishable from a machine-only build.
		utils.Log("[Eco] machine-only FALLBACK fold (LLM summariser unavailable: %v): %d rows, room=%d chars, folded=%d chars, budget=%d chars, per-row show=%d",
			llmErr, len(foldable), room, foldedChars, chars, show)
	}
	// ---- FALLBACK: exactly the machine-only render, unchanged --------------
	return db.CompactConversationDurable(dbWriteCtx, conversationID, messages, foldFrom,
		machineOnlyFoldRenderer(show, ceiling))
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
	foldFrom := UltracaveOverflowFoldFrom(messages, keep)
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
func ultracaveMinFoldProgressTokens() int {
	return ecoTargetTokens() * ultracaveMinFoldProgressPercent / 100
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
		if db.IsCheckpointMessage(messages[i]) {
			continue
		}
		removed += SentViewRowChars(messages[i])
	}
	return removed / 3
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
func UltracaveOverflowFoldFrom(messages []utils.Message, keep int) int {
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
		cand = bootstrapCutoff(messages, target/2)
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
		if bought := ultracaveFoldProgressTokens(messages, cand); bought < ultracaveMinFoldProgressTokens() {
			utils.Log("[Eco] overflow fold refused (hysteresis): view %d tokens over target %d, boundary %d buys only %d tokens (< %d, %d%% of target) — keeping the prompt head and letting the caller trim in memory",
				viewTokens, target, cand, bought, ultracaveMinFoldProgressTokens(), ultracaveMinFoldProgressPercent)
			return -1
		}
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
