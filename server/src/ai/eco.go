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
func effectiveEcoTrigger(cfg auth.EcoConfig, modelName string) int {
	static := cfg.TriggerTokens
	if static <= 0 || cfg.ContextFraction <= 0 {
		return static
	}
	window := Models.MaxContextTokensFor(modelName)
	if window <= 0 {
		return static
	}
	// round(ctxFraction * window) without float: frac*100 * window / 100
	fractionBasis := int(cfg.ContextFraction * 100)
	if fractionBasis <= 0 || fractionBasis >= 100 {
		return static
	}
	dynamic := (fractionBasis * window) / 100
	if dynamic < static {
		return dynamic
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
	// never re-enters the context.
	tail = capCheckpointSummaryForWindow(tail, contextWindow, tailBudgetChars)

	return capTailWithBudget(tail, contextWindow, tailBudgetChars)
}

// capCheckpointSummaryForWindow bounds the checkpoint summary carried in
// the tool-role message of an eco checkpoint pair so it can never blow the
// model's context window BY ITSELF.
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

// maxTailChars returns the character budget for the live tail.
func maxTailChars(contextWindow int) int {
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
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

// maxTailCharsForOverhead returns the live-tail budget given the measured
// token overhead of the rest of the request.
func maxTailCharsForOverhead(contextWindow, overheadTokens int) int {
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
	if overheadTokens < 0 {
		overheadTokens = 0
	}
	const replyReserve = 4096 + 2048
	available := contextWindow - overheadTokens - replyReserve
	if available <= 0 {
		available = 8_192
	}
	budget := available * 3
	if budget > 100_000 {
		budget = 100_000
	}
	return budget
}

func capTail(tail []utils.Message, contextWindow int) []utils.Message {
	return capTailBudget(tail, maxTailChars(contextWindow), contextWindow)
}

func capTailWithBudget(tail []utils.Message, contextWindow, tailBudgetChars int) []utils.Message {
	return capTailBudget(tail, tailBudgetChars, contextWindow)
}

// capTailBudget bounds a message slice to an explicit character budget,
// walking backwards from the end.
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
	if start > len(tail)-1 {
		start = len(tail) - 1
	}
	for start > 0 && tail[start].Role != "user" {
		start--
	}
	if start > 0 {
		utils.Log("[Eco] capping tail by budget: kept %d msgs / %d chars (start idx %d) for context safety", len(tail)-start, chars, start)
		tail = tail[start:]
	}
	tail = trimTailToFitBudget(tail, tailBudgetChars)
	return truncateOversizedTail(tail, contextWindow)
}

// trimTailToFitBudget hard-caps the total content length of a tail slice to
// tailBudgetChars by dropping the OLDEST complete turns until the remainder
// fits, then shrinking the largest remaining messages.
func trimTailToFitBudget(tail []utils.Message, tailBudgetChars int) []utils.Message {
	if len(tail) == 0 || tailBudgetChars <= 0 {
		return tail
	}
	prefix := make([]int, len(tail)+1)
	for i := range tail {
		prefix[i+1] = prefix[i] + tail[i].Content.TotalContentChars()
	}
	if prefix[len(tail)] <= tailBudgetChars {
		return tail
	}

	floor := len(tail) - 1
	for floor > 0 && tail[floor].Role != "user" {
		floor--
	}

	keptStart := 0
	floorChars := tailBudgetChars / 2
	for {
		keptChars := prefix[len(tail)] - prefix[keptStart]
		if keptChars <= tailBudgetChars {
			break
		}
		next := keptStart + 1
		for next < len(tail) && tail[next].Role != "user" {
			next++
		}
		if next > floor {
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

// shrinkTailToFitBudget trims a turn-aligned tail by cutting the LARGEST
// messages first (keeping head and tail around an elision banner) until the
// window fits. Rows are never removed, so tool_calls/result pairing survives.
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
	out := make([]utils.Message, len(tail))
	copy(out, tail)
	tail = out

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
			continue
		}
		if sizes[idx] <= minShrinkableMsgChars {
			continue
		}
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

const minShrinkableMsgChars = 2_000

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
	shrunkMsg := m
	shrunkMsg.Content = utils.NewPartsContent(newParts)
	return shrunkMsg, len(full) - len(out)
}

func maxKeepTailChars(contextWindow int) int {
	if contextWindow <= 0 {
		contextWindow = defaultContextWindow
	}
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

func lastAssistantPromptTokens(messages []utils.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
			return messages[i].PromptTokens
		}
	}
	return 0
}

func findCutoffIndex(messages []utils.Message, lastPromptTokens, targetTokens int, existingCheckpointEndIdx int) int {
	if lastPromptTokens <= targetTokens {
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
	if lastCrossing >= 0 {
		crossingAsst = lastCrossing
	}

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

	cutoff := crossingAsst + 1
	for cutoff < len(messages) && messages[cutoff].Role == "tool" {
		cutoff++
	}

	if cutoff < minCutoff {
		return -1
	}
	if cutoff >= len(messages) {
		return -1
	}
	return cutoff
}

func findCharCutoffIndex(messages []utils.Message, targetTokens, existingCheckpointEndIdx int) int {
	if len(messages) == 0 || targetTokens <= 0 {
		return -1
	}
	targetChars := targetTokens * 4
	prefix := make([]int, len(messages)+1)
	for i := range messages {
		prefix[i+1] = prefix[i] + messages[i].Content.TotalContentChars()
	}
	if prefix[len(messages)] <= targetChars {
		return -1
	}
	floor := len(messages) - 1
	for floor > 0 && messages[floor].Role != "user" {
		floor--
	}
	keptStart := 0
	for {
		if prefix[len(messages)]-prefix[keptStart] <= targetChars {
			break
		}
		next := keptStart + 1
		for next < len(messages) && messages[next].Role != "user" {
			next++
		}
		if next > floor {
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

const maxSummaryInputChars = 80_000

// renderMessagesForSummary produces a plain-text dump of the given message
// slice suitable for feeding to the summarisation model, bounded to
// maxSummaryInputChars characters.
//
// It preserves BOTH ends of the compacted range: the OLDEST messages first
// (original task/goals/decisions — the part the live tail never re-shows),
// then spends the remaining budget on the NEWEST messages (recent state).
// This fixes "eco compaction wipes context": the old newest-only walk fed the
// summariser ONLY the recent tail on a large compaction, dropping the
// original goal before the summary model saw it, so the checkpoint discarded
// the conversation's core intent.
func renderMessagesForSummary(messages []utils.Message) string {
	var b strings.Builder
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

func messageInSlice(m utils.Message, slice []utils.Message) bool {
	for i := range slice {
		if slice[i].Content == m.Content && slice[i].Role == m.Role && slice[i].ToolCallID == m.ToolCallID {
			return true
		}
	}
	return false
}

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

func runEcoSummary(ctx context.Context, conversationID string) {
	utils.Log("[Eco] tick for conv %s", conversationID)
	if !summaryInFlight.CompareAndSwap(false, true) {
		utils.Log("[Eco] summary already in flight — skipping conv %s", conversationID)
		return
	}
	defer summaryInFlight.Store(false)

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

	modelName := ""
	if conv.ModelSelected.Text != nil {
		modelName = conv.ModelSelected.Text.Name
	} else if conv.ModelSelected.Vision != nil {
		modelName = conv.ModelSelected.Vision.Name
	}
	trigger := effectiveEcoTrigger(cfg, modelName)
	if trigger <= 0 {
		trigger = cfg.TriggerTokens
	}

	lastPT := lastAssistantPromptTokens(conv.Messages)
	if lastPT == 0 {
		utils.Log("[Eco] conv %s has no assistant prompt_tokens yet — skip", conversationID)
		return
	}
	if lastPT <= trigger {
		rawChars := 0
		for _, m := range conv.Messages {
			if db.IsCheckpointMessage(m) {
				continue
			}
			rawChars += m.Content.TotalContentChars()
		}
		if rawChars/4 <= trigger {
			utils.Log("[Eco] conv %s lastPT=%d below trigger=%d (model context-aware) — skip", conversationID, lastPT, trigger)
			return
		}
		utils.Log("[Eco] conv %s lastPT=%d below trigger=%d but raw history ≈%d chars → durable-compacting", conversationID, lastPT, trigger, rawChars)
	} else {
		utils.Log("[Eco] conv %s lastPT=%d > trigger=%d (model context-aware) — compacting toward target=%d", conversationID, lastPT, trigger, cfg.TargetTokens)
	}

	existingCheckpointEndIdx := -1
	var oldPairIDs []int64
	var priorSummary string
	if pair, err := db.GetCheckpoint(dbCtx, conversationID); err == nil && pair != nil {
		priorSummary = pair.Summary
		oldPairIDs = []int64{pair.AssistantID}
		if pair.ToolID != 0 {
			oldPairIDs = append(oldPairIDs, pair.ToolID)
		}
		for i, m := range conv.Messages {
			if m.Role == "assistant" && db.IsCheckpointMessage(m) {
				existingCheckpointEndIdx = i + 1
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

	startIdx := 0
	if existingCheckpointEndIdx >= 0 {
		startIdx = existingCheckpointEndIdx + 1
	}
	if startIdx >= cutoff {
		return
	}

	excerpt := renderMessagesForSummary(conv.Messages[startIdx:cutoff])
	var input string
	if priorSummary != "" {
		input = "PRIOR CHECKPOINT:\n" + priorSummary + "\n\nNEW MESSAGES:\n" + excerpt
	} else {
		input = excerpt
	}

	textModel, _ := fastShortcutModels()
	summary, err := GenerateCheckpointSummary(input, textModel)
	if err != nil {
		utils.Error("[Eco] checkpoint summary generation failed", err)
		return
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		utils.Error("[Eco] checkpoint summary returned empty", nil)
		return
	}

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

func tokensBeforeCutoff(messages []utils.Message, cutoff int) int {
	for i := cutoff; i < len(messages); i++ {
		if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
			return messages[i].PromptTokens
		}
	}
	return 0
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}