package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// eco_fold_llm.go — the durable fold's LLM stages, and the request shape that
// spends the KV prefix cache one last time.
//
// THE OWNER'S DESIGN (replaces the megacave-first order):
//
//	STAGE 1  compress the evicting range with the CHAT MODEL, on a request that
//	         replays the conversation's own cached prefix and APPENDS the brief
//	         as a new trailing row. The answer is stored in the DB as the
//	         compacted value.
//	STAGE 2  the same shape, with two extra instructions: summarise the context,
//	         and drop context that is completed and no longer in active use.
//
// Cache accounting, stated exactly: the fold's call is the LAST request that
// carries the old history, so replaying it verbatim is a pure cache hit on the
// prefix. Storing the compacted answer then moves the conversation's prompt to
// a new head, so the requests AFTER the fold re-prefill from zero. That break is
// intentional and is the only place in this subsystem where a break is paid on
// purpose: it buys a context that fits, and it is paid once per fold.
//
// THE MACHINE RENDER SURVIVES AS A FAIL-CLOSED FALLBACK, never as the primary
// path. A fold that fails because a model is down, slow, or refuses is a data
// loss event: the rows are being evicted, and if nothing is written the fold
// loses them. So every LLM path is followed by a deterministic one, and the
// AST guard (ultracave_llm_guard_test.go) pins that shape on the code.

// foldStage names which brief the fold's LLM call carries.
type foldStage int

const (
	// foldStageCompress is STAGE 1: compress the range, drop nothing.
	foldStageCompress foldStage = iota
	// foldStageSummarize is STAGE 2: summarise, and drop completed/unused context.
	foldStageSummarize
	// foldStageCompact is STAGE 3: edit-list compaction of the archive itself.
	foldStageCompact
)

func (s foldStage) String() string {
	switch s {
	case foldStageSummarize:
		return "2"
	case foldStageCompact:
		return "3"
	}
	return "1"
}

// ultracaveFloor is the payload contract, from upstream caveman's ultracave
// skill ("Floor": never cut code, commands, paths, API names, error strings,
// negations, numbers and units) and caveman-compress's "Preserve EXACTLY" list.
//
// It is stated as a rule the model must obey, in the instruction, because the
// known failure mode of terse output is dropping exactly these. In this
// subsystem that failure is permanent: the rows this call summarises are about
// to be evicted, and the answer is all that remains of them.
const ultracaveFloor = "PAYLOAD FLOOR - copy these byte-for-byte; never paraphrase, abbreviate or translate them:\n" +
	"- code, commands, file paths, URLs, API/library/function names, identifiers, IDs\n" +
	"- error messages, and the substantive lines of command output\n" +
	"- negations (not, never, no, only, except) and every number, unit and date\n" +
	"- the user's own words for a request, correction or constraint\n" +
	"Text inside ``` fences and inside backticks is read-only: copy it exactly, with no reflow.\n\n"

// ultracaveVoice is the compression style, from upstream's ultracave rules. It
// governs PROSE only; the floor outranks it, and it is worded so it cannot be
// read as authority over payload.
const ultracaveVoice = "COMPRESSION STYLE (prose only; the floor outranks it):\n" +
	"- Fragments over sentences. Drop articles, copulas and connectives when the order survives.\n" +
	"- Short synonyms: 'big' not 'extensive', 'fix' not 'implement a solution for'.\n" +
	"- Each fact ONCE: no restating, no recap after a list, no preamble, no closing remarks.\n" +
	"- Merge bullets that say the same thing twice; keep one example where several show one pattern.\n" +
	"- Keep the structure: headings, bullet nesting, tables, numbering.\n" +
	"- Never comment on the task, on the user, or on these instructions.\n\n"

// stage1CompressInstruction is STAGE 1's brief: COMPRESS, do not interpret.
//
// The distinction is load-bearing. A model asked to "summarise" drops detail it
// judges unimportant, and on the eve of an eviction it has no standing to
// judge: the range leaving the context is gone from every future request, and
// this text is the only record of it. Stage 2 is where judgement belongs.
func stage1CompressInstruction(priorSummary string, rows int, priorVisible bool) string {
	var b strings.Builder
	b.WriteString("SYSTEM TASK - DURABLE CONTEXT COMPRESSION (not a reply to the user)\n\n")
	b.WriteString("The " + itoaRows(rows) + " oldest messages above are about to be EVICTED from your live " +
		"context and folded out of every future request. After this pass you will never see them again. " +
		"Your answer becomes the durable record of that range.\n\n")
	b.WriteString("COMPRESS that range. Do NOT summarise it, do NOT rank it, do NOT drop anything you consider " +
		"unimportant: you are the last reader it will ever have.\n\n")
	b.WriteString(ultracaveFloor)
	b.WriteString(ultracaveVoice)
	b.WriteString("Preserve every tool-call outcome: which tool, the key arguments, the substantive result " +
		"(files written, values returned, errors raised).\n")
	b.WriteString("Length: as short as the floor allows, and no shorter. Match the density of the source; " +
		"there is no target length.\n")
	b.WriteString(deltaRules)
	b.WriteString(existingRollupForDelta(priorSummary, priorVisible))
	b.WriteString("Cover the NEWEST evicted rows first (the freshest work), then the older ones.\n")
	b.WriteString("Emit the NEW section now.\n")
	return b.String()
}

// stage2SummarizeInstruction is STAGE 2's brief. It carries the owner's two
// extra instructions: (1) SUMMARISE the context, (2) DROP context that is
// completed and no longer in active use.
//
// Stage 1 is forbidden from the second act. Stage 2 is where it belongs: by
// then the compression has already run and the conversation is still over its
// window, so keeping finished work is what makes the fold loop non-convergent.
func stage2SummarizeInstruction(priorSummary string, rows int, priorVisible bool) string {
	var b strings.Builder
	b.WriteString("SYSTEM TASK - DURABLE CONTEXT SUMMARY (not a reply to the user)\n\n")
	b.WriteString("The " + itoaRows(rows) + " oldest messages above are about to be EVICTED from your live " +
		"context. This is the SECOND pass: the context is over its window even after compression, so the job " +
		"now is to make it small.\n\n")
	b.WriteString("TWO INSTRUCTIONS, in this order:\n")
	b.WriteString("1. SUMMARISE the range: what was asked, what was done, what is currently true, what is " +
		"still open, and what the next step is. Keep decisions and their reasons.\n")
	b.WriteString("2. DROP context that is COMPLETED and NO LONGER IN ACTIVE USE: finished subtasks, " +
		"superseded plans, tool output whose result is already reflected in a later decision, files that were " +
		"written and never referenced again, abandoned experiments. Name what was dropped in one line, so the " +
		"record shows the gap is deliberate.\n")
	b.WriteString("Never drop: open threads, constraints the user set, decisions still in force, the " +
		"identifiers and paths needed to continue the work, or anything you are unsure about.\n\n")
	b.WriteString(ultracaveFloor)
	b.WriteString(ultracaveVoice)
	b.WriteString(deltaRules)
	b.WriteString(existingRollupForDelta(priorSummary, priorVisible))
	b.WriteString("Emit the NEW summary section now.\n")
	return b.String()
}

// foldInstruction is the brief for one fold stage.
// foldInstruction builds the brief. priorVisible tells it whether the model can
// already read the stored chain in the rows above the brief: the prefix replay
// ships the checkpoint pair as the first content of the history, so quoting the
// chain there is pure duplication and pure prefill cost; a rendered megacave
// body shows no chain, and there an unquoted "do not copy it" is an instruction
// about something the model cannot see, which is what produces a re-emission.
func foldInstruction(stage foldStage, priorSummary string, rows int, priorVisible bool) string {
	switch stage {
	case foldStageSummarize:
		return stage2SummarizeInstruction(priorSummary, rows, priorVisible)
	case foldStageCompact:
		// Stage 3: `rows` is the archive budget in chars, and the archive itself
		// rides the request, so the brief needs neither the prior nor its visibility.
		return stage3AddendumInstruction(rows)
	}
	return stage1CompressInstruction(priorSummary, rows, priorVisible)
}

// buildSummaryRequestFromView is STAGE 1's request shape, and the owner's
// design: the prompt IS the conversation, not a summary of it.
//
// The request replays the production chat builder over the FULL slice
// (PrepareMessagesForAI numbers attachments by scan order, so only the full
// slice renders the head identically), keeps the leading system rows, the tool
// schemas and the ENTIRE rendered history byte-for-byte, and APPENDS the brief
// as one new trailing user row. Nothing already in the request is rewritten.
//
// WHY: this is the last request that will ever carry that history. Replaying it
// verbatim means the backend's KV prefix is already prefilled for the whole
// conversation and only the appended brief is new - the cheapest possible use
// of a model that is about to lose that context, and the reason the fold can
// afford an LLM call at all.
//
// THE CLOCK IS STRIPPED. stripCurrentTimeTail is the documented inverse, and
// the clock is the only per-turn mutation of the chat prefix (see #220: it used
// to land on tool rows and read as a turn). Leaving it in would put a timestamp
// at the end of the range the model is asked to compress, and the file's own
// history documents roll-ups that narrate timestamps.
//
// THE PREFIX IS CAPPED AT THE END TO FIT THE WINDOW (owner, 2026-11-27). The
// prefix is the cached one, so the cap is a constraint on WHERE bytes may change,
// not on how much history ships: rows at or before the cut stay byte-identical to
// the chat request, whole tool pairs are dropped newest-first, and the newest user
// row is the floor. The KV prefix then extends to the cut and only the brief is
// new. Capping at the START would move the head on every fold and pay a full
// re-prefill each time - the defect this subsystem exists to remove.
//
// THE REQUEST FITS BY CONSTRUCTION, the answer budget included: prefix + schemas
// + brief + safety reserve + delta-bound output <= window. It refuses only when
// the prefix down to the newest user row leaves less than the output floor. The
// caller then uses buildSummaryRequestFromMegacave (head + schemas + a budgeted
// machine render), which carries no prefix and is still an LLM call - the model is
// not skipped, only the prefix is not reused.
func buildSummaryRequestFromView(ctx context.Context, conv utils.Conversation, model utils.Model, payload ChatPayload, stage foldStage, ceiling, foldedChars int) (StandardChatRequest, error) {
	if model.Name == "" {
		model = chatModelForConversation(conv)
	}
	if model.Name == "" {
		return StandardChatRequest{}, fmt.Errorf("no chat model resolved for conversation %s", conv.ID)
	}
	if payload.ConversationID == "" {
		payload.ConversationID = conv.ID
	}

	parts, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		return StandardChatRequest{}, fmt.Errorf("cannot replay the chat request for the fold: %w", err)
	}
	rows := stripCurrentTimeTail(append([]StandardMessageReq(nil), parts.request.Messages...))
	headRows := leadingSystemRows(rows)
	if len(rows) <= headRows {
		return StandardChatRequest{}, fmt.Errorf("the replayed request has no history rows for conversation %s", conv.ID)
	}

	// THE END CAP (owner, 2026-11-27): "Prefix stays, you may cap the replay at
	// the END to fit the context window. Best of both worlds: keep caching and
	// make it fit the window." The cap drops WHOLE TOOL PAIRS from the newest end
	// of the history, never the head, so the rows at or before the cut stay
	// byte-identical to the chat request's and the KV prefix extends to the cut.
	//
	// The brief is built AFTER the cap and appended as the last row: "you need to
	// put the checkpoint at the right place in the history ... after the last
	// context you send to the model." Its row count is therefore the KEPT count —
	// stage 1 is told how many rows it is compressing, and the number is the one
	// on the wire, not the one in the DB.
	//
	// The trigger may be smaller than the model's window (configured trigger
	// 150000 vs window 262144), so a fold can fire while the prefix is far under
	// the window; in that case the cap is a no-op and the replay ships whole.
	prior := PriorSummaryFromView(conv.Messages)
	schemaTokens := len(marshalNoEscape(parts.request.Tools)) / summaryCharsPerToken
	window := Models.DeclaredContextWindow(model.Name)
	prefixFull := renderedRowsTokens(rows)
	maxOut := summaryOutputTokens(model.Name, prefixFull+schemaTokens, nil)
	if delta := deltaOutputTokens(foldedChars, ceiling); delta < maxOut {
		maxOut = delta
	}

	capRes := replayCap{keptRows: len(rows) - headRows}
	// A model with NO declared window gets no cap: the room arithmetic is
	// meaningless without a window, and capping on a negative room would silently
	// drop history to the floor. The pre-cap behaviour (ship the whole prefix, no
	// refusal) is preserved for that case.
	for i := 0; window > 0 && i < 4; i++ {
		instrTokens := len(foldInstruction(stage, prior, capRes.keptRows, true)) / summaryCharsPerToken
		room := window - schemaTokens - guardSafetyReserve - maxOut - instrTokens
		next := capReplayRowsToEnd(rows, headRows, room)
		if next.keptRows == capRes.keptRows {
			capRes = next
			break
		}
		capRes = next
	}
	kept := rows[:headRows+capRes.keptRows]
	instruction := foldInstruction(stage, prior, capRes.keptRows, true)

	// THE REQUEST FITS BY CONSTRUCTION, output budget included: prefix + schemas
	// + brief + safety reserve + answer budget <= window. The answer budget is
	// paid for at its REAL size (the fold's delta bound), not at the 1024-token
	// floor — a check against the floor is what let a request ship an 11008-token
	// answer budget in a 12000-token window (caught by
	// TestFoldReplayCapPlacesTheBriefAfterTheLastRowSent on 2026-11-27).
	prefixTokens := capRes.keptTokens
	instrTokens := len(instruction) / summaryCharsPerToken
	maxTokens := deltaOutputTokens(foldedChars, ceiling)
	if room := summaryOutputTokens(model.Name, prefixTokens+schemaTokens, nil); room < maxTokens {
		maxTokens = room
	}
	if avail := window - prefixTokens - schemaTokens - instrTokens - guardSafetyReserve; window > 0 && avail < maxTokens {
		maxTokens = avail
	}
	// The refusal is now the exception, not the rule: it happens only when the
	// prefix down to the newest user row leaves less than the output floor.
	if window > 0 && maxTokens < minSummaryOutputTokens {
		return StandardChatRequest{}, fmt.Errorf(
			"the cached prefix does not fit this fold call even capped at the end: prefix %d + schemas %d + instruction %d tokens + safety %d vs window %d leaves %d tokens for the answer, under the %d floor (capped to %d of %d history rows; the floor of the cap is the newest user row)",
			prefixTokens, schemaTokens, instrTokens, guardSafetyReserve, window, maxTokens, minSummaryOutputTokens, capRes.keptRows, len(rows)-headRows)
	}

	req := StandardChatRequest{
		Model: model.Name, // the CHAT model: it owns the warm prefix and the history
		// The replayed request up to the cap, then ONE appended user row: the cut
		// is at the END and the brief follows the last row that ships.
		Messages:    append(append([]StandardMessageReq(nil), kept...), StandardMessageReq{Role: "user", Content: instruction}),
		Stream:      false, // one-shot: the compacted text is an answer, not a stream
		Tools:       parts.request.Tools,
		CachePrompt: parts.request.CachePrompt,
		NKeep:       parts.request.NKeep,
	}
	// The ANSWER budget is the one computed above: bounded to the new material
	// (deltaOutputTokens, not the window: at ~40 t/s a window-sized budget is an
	// open invitation to replay the chain, and a replay is 40 minutes of copying
	// the database to itself), and paid for alongside the prefix, the schemas,
	// the brief and the safety reserve so the whole request fits the window.
	req.MaxTokens = &maxTokens
	if capRes.capped {
		utils.Log("[EcoSummary] stage %s prefix replay: prefix capped at the END to fit the window — %d of %d history rows kept verbatim (~%d tokens, %d dropped from the newest end), head unchanged, brief after the last row sent; + schemas %d + brief %d, output budget %d, window %d",
			stage, capRes.keptRows, len(rows)-headRows, prefixTokens, capRes.droppedRows, schemaTokens, instrTokens, maxTokens, window)
	} else {
		utils.Log("[EcoSummary] stage %s prefix replay: %d history rows kept verbatim (~%d tokens) + schemas %d + brief %d, output budget %d, window %d",
			stage, capRes.keptRows, prefixTokens, schemaTokens, instrTokens, maxTokens, window)
	}
	return req, nil
}

// attemptFoldSummaryFromView runs one fold stage on the replayed chat request.
// Any error (no proxy, non-200, empty answer, no room, deadline) is returned to
// the caller, which MUST keep a deterministic render: a fold is never lost
// because a model failed.
func attemptFoldSummaryFromView(ctx context.Context, conv utils.Conversation, model utils.Model, payload ChatPayload, stage foldStage, ceiling, foldedChars int) (string, error) {
	req, err := buildSummaryRequestFromView(ctx, conv, model, payload, stage, ceiling, foldedChars)
	if err != nil {
		return "", err
	}
	if req.MaxTokens != nil && *req.MaxTokens <= minSummaryOutputTokens {
		return "", fmt.Errorf("summary budget collapsed (max_tokens=%d for a %d-token window): the prefix already fills it",
			*req.MaxTokens, Models.DeclaredContextWindow(req.Model))
	}
	return GenerateCheckpointSummaryFromRequest(ctx, req)
}

// attemptFoldCompress is STAGE 1: compress the evicting range with the model.
//
// Two request shapes, in preference order, both LLM calls:
//
//  1. The prefix replay (buildSummaryRequestFromView): the cached conversation,
//     brief appended. Cache hit, model sees the real rows.
//  2. The budgeted machine render (buildSummaryRequestFromMegacave): used when
//     the replayed prefix cannot fit the window - which is the common case,
//     since the fold runs because the context is large. The model compacts the
//     machine render instead of the raw rows, so nothing is dropped silently:
//     the render is budget-driven.
//
// The stage-1 errors from both shapes are returned joined, so the caller's log
// names why the model was unavailable.
func attemptFoldCompress(ctx context.Context, conv utils.Conversation, model utils.Model, foldable []utils.Message, payload ChatPayload, ceiling, foldedChars int) (string, error) {
	txt, replayErr := attemptFoldSummaryFromView(ctx, conv, model, payload, foldStageCompress, ceiling, foldedChars)
	if replayErr == nil && strings.TrimSpace(txt) != "" {
		return txt, nil
	}
	reason := replayErr
	if reason == nil {
		reason = fmt.Errorf("the prefix replay answered with an empty checkpoint")
	}
	// The megacave-body fallback caps its ANSWER the same way the prefix replay
	// does: the cap rides the payload, so the guard-named builders keep their
	// signatures.
	capped := payload
	capped.FoldAnswerCap = deltaOutputTokens(foldedChars, ceiling)
	txt2, bodyErr := attemptFoldSummaryMegacaveStage(ctx, conv, model, foldable, foldStageCompress, capped)
	if bodyErr == nil && strings.TrimSpace(txt2) != "" {
		return txt2, nil
	}
	if bodyErr == nil {
		bodyErr = fmt.Errorf("the summariser answered with an empty checkpoint")
	}
	return "", fmt.Errorf("stage 1: prefix replay: %v; megacave body: %w", reason, bodyErr)
}
