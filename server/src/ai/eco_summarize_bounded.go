package ai

// eco_summarize_bounded.go — the fold summariser's RECOVERY path.
//
// The primary path (buildSummaryRequestFromView) replays the conversation's
// exact chat request so the summariser reads the same cached prefix the chat
// turn just wrote. That is the right design while the prefix fits.
//
// It stops working on exactly the conversations that need the summariser most.
// The fold fires when the view has grown to the trigger, so the replayed prefix
// is by construction near the window; summaryOutputTokens subtracts that prefix
// plus the tool schemas from the window, floors at minSummaryOutputTokens, and
// attemptFoldSummary refuses the call. Measured in production:
//
//	[Eco] machine-only FALLBACK fold (LLM summariser unavailable: summary
//	budget collapsed (max_tokens=1024 for a 262144-token window): the prefix
//	already fills it): 1240 rows, folded=2824666 chars
//
// 2.8 MB of history folded with no LLM checkpoint, because the builder never
// sent anything. The summariser is a normal upstream chat call and it works —
// it was being asked to fit a 262144-token window inside itself.
//
// The recovery drops what the fold does not need:
//
//   - the tool schemas. The summariser answers with text; summaryInstruction
//     opens "SYSTEM TASK — DURABLE CONTEXT CHECKPOINT (not a reply to the
//     user)". It never calls a tool, and the schemas are the largest single
//     block of the prefix on this deployment — measured by the preemptive
//     guard's own accounting at 28349 tokens.
//   - the live tail. The fold's subject is the EVICTED range; the rows that
//     stay in context are the model's context next turn already.
//
// What it keeps: the system rows (identity, style, the roll-up banner
// vocabulary) and the evicted rows verbatim, in batches sized so the input plus
// the output budget always fit the window. Batching is what makes the path
// unconditional — no shape of conversation can floor the budget — so the LLM
// summary is attempted on every fold and the machine-only render goes back to
// being the fallback for a real failure (proxy down, deadline, empty answer).
//
// The batch count is bounded, and the batches are the NEWEST evicted rows:
// summaryInstruction already says "cover the newest evicted rows first (what
// you are about to lose is the freshest work)". The machine render covers the
// whole evicted range verbatim, so rows the batch bound leaves out are
// represented, just not by the LLM. A 2.8 MB fold therefore costs a bounded
// number of round-trips, not one per 60k tokens.

import (
	"context"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// summaryBatchTokens is the history one summariser round-trip carries on the
// recovery path, expressed in tokens, and it is EXACTLY the summariser
// renderer's own input capacity: renderMessagesForSummary walks the range
// backwards and stops at maxSummaryInputChars, emitting an "older messages
// omitted" NOTE when it has to. A batch bigger than that budget is silently
// truncated by the renderer, so the batch IS the renderer's capacity — every
// batch this path builds renders VERBATIM, with no NOTE, and the model sees the
// whole range it is asked to checkpoint. summaryBatchChars is the char form and
// is the number the splitter bounds against, so the equality is exact (deriving
// it by dividing and multiplying by chars-per-token loses the remainder).
//
// The size is well under the window, which is what makes the path
// unconditional: on a 262144-token window, system rows (~40k) + one batch
// (~27k) + the reply reserve leaves over 150k of room, clamped to
// maxSummaryOutputTokens. No shape of conversation can floor the budget.
const summaryBatchTokens = maxSummaryInputChars / summaryCharsPerToken

// summaryMaxBatches bounds the fold's LLM cost. Each round-trip is a chat
// completion on a self-hosted model measured at ~3.5 min for a full window, so
// an unbounded batch loop would stall a turn for an hour on a 2.8 MB eviction.
// Three batches is ~240k chars of LLM coverage per fold; the machine render
// covers the whole eviction verbatim regardless.
const summaryMaxBatches = 3

func summaryBatchChars() int { return maxSummaryInputChars }

// summaryBatchCharBudget sizes one batch for a specific model window.
//
// The ceiling is the renderer's own input capacity (maxSummaryInputChars): a
// batch bigger than that is silently truncated by renderMessagesForSummary
// behind its "older messages omitted" NOTE. The floor is the window's room for
// history: system rows, the reply reserve and a minimum output budget are all
// claimed first, so a small window gets a small batch and its output budget
// stays above the collapse floor instead of being floored by an oversized
// input. On the deployed 262144-token window the ceiling wins (room ≈ 215k
// tokens → 645k chars, capped to 80k); on an 8k window the room wins.
func summaryBatchCharBudget(windowTokens int) int {
	if windowTokens <= 0 {
		return maxSummaryInputChars
	}
	room := windowTokens - promptOverheadTokens - guardReplyReserve - minSummaryOutputTokens
	if room <= 0 {
		// The prompt fills the window: keep the smallest honest batch so the
		// fold still gets LLM coverage of the freshest rows.
		return minSummaryOutputTokens * summaryCharsPerToken
	}
	if r := room * summaryCharsPerToken; r < maxSummaryInputChars {
		return r
	}
	return maxSummaryInputChars
}

// splitEvictedBatches packs evicted rows into at most `maxBatches` batches of
// at most `batchCharBudget` shipped chars each, returned in CHRONOLOGICAL order.
//
// Selection is newest-first: the walk starts at the newest evicted row, so when
// the eviction exceeds maxBatches batches, the rows that get the LLM checkpoint
// are the freshest work — the priority summaryInstruction states, and the
// machine render covers the rest.
//
// Pure function of the stored slice: same rows in, same batches out.
func splitEvictedBatches(evicted []utils.Message, batchCharBudget, maxBatches int) [][]utils.Message {
	if len(evicted) == 0 || batchCharBudget <= 0 || maxBatches <= 0 {
		return nil
	}

	// Walk backwards, so `newest` accumulates the newest batch first.
	newest := make([][]utils.Message, 0, maxBatches)
	var cur []utils.Message
	size := 0
	for i := len(evicted) - 1; i >= 0; i-- {
		c := shippedRowChars(evicted[i])
		if len(cur) > 0 && size+c > batchCharBudget {
			newest = append(newest, reverseRows(cur))
			cur = nil
			size = 0
			if len(newest) == maxBatches {
				break
			}
		}
		cur = append(cur, evicted[i])
		size += c
	}
	if len(cur) > 0 && len(newest) < maxBatches {
		newest = append(newest, reverseRows(cur))
	}

	// newest[0] is the newest batch; the summariser must read oldest first.
	batches := make([][]utils.Message, 0, len(newest))
	for b := len(newest) - 1; b >= 0; b-- {
		batches = append(batches, newest[b])
	}
	return batches
}

func reverseRows(rows []utils.Message) []utils.Message {
	out := make([]utils.Message, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i])
	}
	return out
}

// reqContentText is the string body of a rendered row: the builder renders text
// rows as strings, and a non-string content (multimodal parts) contributes no
// text to the checkpoint.
func reqContentText(c interface{}) string {
	if s, ok := c.(string); ok {
		return s
	}
	return ""
}

// buildBoundedSummaryRequest assembles ONE summariser round-trip: the
// conversation's system rows, the batch's evicted rows verbatim, and the
// checkpoint instruction. No tools, no live tail — see the file header.
//
// The system rows come from a full replay of the conversation, so the
// attachment index and the important_memory block are the same bytes the chat
// request renders (they are numbered by scan order over the WHOLE slice;
// replaying a shorter slice renames them). Only the HISTORY rows are dropped.
func buildBoundedSummaryRequest(ctx context.Context, conv utils.Conversation, model utils.Model,
	batch []utils.Message, priorSummary string, payload ...ChatPayload) (StandardChatRequest, error) {
	if model.Name == "" {
		model = chatModelForConversation(conv)
	}
	if model.Name == "" {
		return StandardChatRequest{}, fmt.Errorf("no chat model resolved for conversation %s", conv.ID)
	}
	p := chatPayloadForConversation(conv)
	if len(payload) > 0 {
		p = payload[0]
	}
	if p.ConversationID == "" {
		p.ConversationID = conv.ID
	}

	parts, err := buildChatRequestParts(ctx, model, conv, p)
	if err != nil {
		return StandardChatRequest{}, fmt.Errorf("cannot replay the system prompt for the bounded fold summary: %w", err)
	}

	// System rows only, clock stripped: the per-turn mutation of the prefix.
	system := make([]StandardMessageReq, 0, 4)
	for _, m := range stripCurrentTimeTail(parts.request.Messages) {
		if m.Role == "system" {
			system = append(system, m)
		}
	}

	body := renderEvictedRowsForSummary(batch)
	if strings.TrimSpace(body) == "" {
		return StandardChatRequest{}, fmt.Errorf("the bounded fold summary has no evicted text to summarise")
	}
	msgs := append(append([]StandardMessageReq(nil), system...),
		StandardMessageReq{Role: "user", Content: "The messages about to be evicted from your context, verbatim:\n\n" + body},
		StandardMessageReq{Role: "user", Content: summaryInstruction(priorSummary, len(batch))},
	)

	req := StandardChatRequest{
		Model:       model.Name,
		Messages:    msgs,
		Stream:      false,
		Tools:       nil, // the summariser answers with text; the schemas cost 28k tokens
		CachePrompt: parts.request.CachePrompt,
		NKeep:       parts.request.NKeep,
	}
	// Input = system rows + batch + instruction, counted at chars/3 — the same
	// rate SentViewTokens uses, so the trigger, the guard and this budget agree
	// about what is being sent. renderedRowsTokens covers every row because
	// these are RENDERED rows (StandardMessageReq), not stored ones.
	input := renderedRowsTokens(msgs)
	req.MaxTokens = boundedSummaryOutputTokens(model.Name, input)
	return req, nil
}

// renderedRowsTokens counts rendered rows in the sent-view measure.
func renderedRowsTokens(rows []StandardMessageReq) int {
	chars := 0
	for _, r := range rows {
		chars += len(reqContentText(r.Content))
	}
	return chars / summaryCharsPerToken
}

// boundedSummaryOutputTokens is the recovery path's output budget: window minus
// the input, minus the reply reserve, clamped to
// [minSummaryOutputTokens, maxSummaryOutputTokens].
//
// Unlike summaryOutputTokens it takes NO schema allowance, because this request
// ships no tools. That is the point of the recovery: the primary path's budget
// collapsed on the schemas plus a full-window prefix, and the fold needs a
// shape that cannot collapse.
func boundedSummaryOutputTokens(modelName string, inputTokens int) *int {
	window := Models.MaxContextTokensForPrefixed(modelName)
	budget := window - inputTokens - guardReplyReserve
	if budget > maxSummaryOutputTokens {
		budget = maxSummaryOutputTokens
	}
	if budget < minSummaryOutputTokens {
		budget = minSummaryOutputTokens
	}
	return &budget
}

// summariseEvictedInBatches is the recovery driver: one bounded summariser call
// per batch, oldest batch first, each call seeing the roll-up the previous call
// produced. The batch summaries are returned concatenated in chronological
// order, so the caller's renderer sees one checkpoint text — the same shape the
// primary path hands it.
//
// Any call failing aborts the recovery and returns the error: a partial
// checkpoint of only the oldest batches would silently drop the freshest work,
// and the caller's machine-only render covers the whole eviction.
func summariseEvictedInBatches(ctx context.Context, conv utils.Conversation, model utils.Model,
	evicted []utils.Message, payload ...ChatPayload) (string, error) {
	batches := splitEvictedBatches(evicted, summaryBatchCharBudget(Models.MaxContextTokensForPrefixed(model.Name)), summaryMaxBatches)
	if len(batches) == 0 {
		return "", fmt.Errorf("no evicted rows to summarise")
	}

	var out []string
	prior := ""
	for i, batch := range batches {
		req, err := buildBoundedSummaryRequest(ctx, conv, model, batch, prior, payload...)
		if err != nil {
			return "", err
		}
		text, err := GenerateCheckpointSummaryFromRequest(ctx, req)
		if err != nil {
			return "", fmt.Errorf("bounded summary batch %d/%d failed: %w", i+1, len(batches), err)
		}
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("bounded summary batch %d/%d returned no text", i+1, len(batches))
		}
		utils.Log("[EcoSummary] bounded batch %d/%d: %d evicted rows, output budget %d tokens, checkpoint %d chars",
			i+1, len(batches), len(batch), *req.MaxTokens, len(text))
		out = append(out, text)
		prior = text // the next batch continues this roll-up
	}
	return strings.Join(out, "\n\n"), nil
}
