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
func capTail(tail []utils.Message) []utils.Message {
	if len(tail) == 0 {
		return tail
	}
	count := 0
	chars := 0
	var start int
	start = len(tail) - 1
	for start >= 0 {
		chars += tail[start].Content.TotalContentChars()
		count++
		if chars > maxTailChars {
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
	return truncateOversizedTail(tail)
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
// Compaction NEVER calls an LLM. GenerateCheckpointSummary (the fast-model
// summariser) is no longer on this path: a model call costs minutes on a
// self-hosted summariser, its output is non-deterministic, and a
// non-deterministic summary rewrites the head of every prompt, which is the
// exact opposite of frozen compaction. The function is left defined for any
// other call site.
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

	lastPT := lastAssistantPromptTokens(conv.Messages)
	if lastPT == 0 {
		utils.Log("[Eco] conv %s has no assistant prompt_tokens yet — skip", conversationID)
		return
	}
	if lastPT <= cfg.TriggerTokens {
		utils.Log("[Eco] conv %s lastPT=%d below trigger=%d — skip", conversationID, lastPT, cfg.TriggerTokens)
		return
	}
	utils.Log("[Eco] conv %s lastPT=%d > trigger=%d — compacting toward target=%d", conversationID, lastPT, cfg.TriggerTokens, cfg.TargetTokens)

	// Locate the existing checkpoint (if any) and compute where the new
	// cutoff must land.
	existingCheckpointEndIdx := -1
	var priorSummary string
	// Read the CURRENT checkpoint pair straight out of the loaded LLM view
	// (it carries the pair) instead of db.GetCheckpoint: that helper issues
	// its second query while the first result set is still open, which
	// wedges the single-connection SQLite pool (SetMaxOpenConns(1)) forever
	// as soon as a pair exists. Reading the slice is also cheaper, and it is
	// the exact view the fold will be computed from, so the pair index and
	// the roll-up text cannot disagree with the slice being folded.
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

	cutoff := findCutoffIndex(conv.Messages, lastPT, cfg.TargetTokens, existingCheckpointEndIdx)
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
	before := SentViewTokens(conv.Messages)
	folded, err := durableFold(dbCtx, conversationID, conv.Messages, cutoff, priorSummary)
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

// durableFold is the single machine-only fold entry point, shared by the eco
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
// No LLM, no clock, no randomness: MachineExcerptMegacaveBudget is
// deterministic, so the same fold set always renders the same summary.
func durableFold(ctx context.Context, conversationID string, messages []utils.Message, foldFrom int, priorSummary string) (int, error) {
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
	show := MegacaveRowBudget(chars, len(foldable))
	utils.Log("[Eco] machine fold: %d rows, room=%d chars, folded=%d chars, budget=%d chars, per-row show=%d",
		len(foldable), room, foldedChars, chars, show)
	return db.CompactConversationDurable(ctx, conversationID, messages, foldFrom,
		func(m []utils.Message) string { return MachineExcerptMegacaveBudget(m, show) })
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

// CompactConversationMachineOnly is the machine-only compaction entry
// point: pick the fold boundary with the SAME arithmetic the eco trigger
// uses, then fold durably. NO LLM anywhere on the path. Used by the
// context-length recovery path in tool_loop.go and by the ultracave tests.
//
// `priorSummary` is the CURRENT stored roll-up text; it only feeds the
// budget arithmetic. The storage layer carries it forward VERBATIM and does
// the concatenation itself - passing it in twice would double the roll-up,
// and re-rendering it through the megacave transform would destroy the one
// representation of every row an earlier fold marked compacted.
func CompactConversationMachineOnly(ctx context.Context, conversationID string, messages []utils.Message, priorSummary string, keep int) (int, error) {
	foldFrom := UltracaveOverflowFoldFrom(messages, keep)
	if foldFrom <= 0 {
		return 0, nil
	}
	return durableFold(ctx, conversationID, messages, foldFrom, priorSummary)
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
//     would be folded, orphaning the result at the head of the request);
//   - folding from 0 would fold the whole conversation.
//
// When the token arithmetic yields no usable cutoff (e.g. a conversation
// with no prompt_tokens yet, which is exactly the case right after a
// provider 500), it falls back to keeping the newest `keep` messages,
// snapped to the same legal boundaries.
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
	cand := -1
	if lastPT := lastAssistantPromptTokens(messages); lastPT > 0 {
		ckptEnd := -1
		for i, m := range messages {
			if m.Role == "assistant" && db.IsCheckpointMessage(m) {
				ckptEnd = i + 1
				break
			}
		}
		cand = findCutoffIndex(messages, lastPT, ecoTargetTokens(), ckptEnd)
	}
	if cand <= 0 {
		// Token arithmetic gave nothing: keep the newest `keep` rows.
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
