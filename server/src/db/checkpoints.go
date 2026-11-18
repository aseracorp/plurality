package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// EcoCheckpointToolName is the function name used on the assistant tool call
// that represents an eco-mode rolling-checkpoint summary. The matching tool
// result message carries the actual summary text in its Content.
//
// A single conversation has at most one checkpoint pair at any time. The pair
// sits in the messages table at the seq boundary between the summarised
// turns (older) and the live tail (newer). It is invisible to the client —
// getConversationFromDB filters it out — and only flows to the LLM when eco
// mode is enabled (see ai/eco.go::resolveEcoContextCompaction).
// The string is defined in utils (utils.EcoCheckpointToolName) because the
// sender, the estimator and the attachment pipeline all need the same rule and
// utils cannot import db. This alias keeps db.EcoCheckpointToolName as the name
// persistence call sites already use.
const EcoCheckpointToolName = utils.EcoCheckpointToolName

// CheckpointPair locates and carries the contents of a conversation's
// rolling checkpoint, if any. AssistantID is the row id of the assistant
// message that owns the tool call; ToolID is the row id of the matching
// tool-role result. InsertSeq is the seq of the assistant row (the tool
// row sits at InsertSeq+1).
type CheckpointPair struct {
	AssistantID  int64
	ToolID       int64
	AssistantSeq int64
	Summary      string
}

// IsCheckpointMessage reports whether the given message represents one half
// of an eco checkpoint pair. The assistant half carries a single tool call
// whose function name is EcoCheckpointToolName; the tool half carries that
// name in msg.Name.
func IsCheckpointMessage(msg utils.Message) bool {
	// Deliberately narrower than utils.IsCheckpointSummaryCarrier: this names the
	// PERSISTED pair (tool name EcoCheckpointToolName / assistant tool call of the
	// same name). The staged in-memory view carries the same roll-up under a
	// "megacave_<stage>" name and is never a DB row, so widening this predicate
	// to it would change fold-boundary arithmetic for no benefit.
	if msg.Role == "tool" && msg.Name == EcoCheckpointToolName {
		return true
	}
	if msg.Role == "assistant" {
		for _, tc := range msg.ToolCalls {
			if tc.Function.Name == EcoCheckpointToolName {
				return true
			}
		}
	}
	return false
}

// FilterOutCheckpoints returns a copy of messages with every checkpoint
// message (assistant half + tool half) removed. Used when returning a
// conversation to the client — the user never sees the synthetic pair.
func FilterOutCheckpoints(messages []utils.Message) []utils.Message {
	out := make([]utils.Message, 0, len(messages))
	for _, m := range messages {
		if IsCheckpointMessage(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// GetCheckpoint returns the current checkpoint pair for a conversation, or
// (nil, nil) if no checkpoint exists. The assistant row is identified by
// having a tool_calls JSON entry whose function.name equals
// EcoCheckpointToolName; the tool row is matched by tool_call_id.
func GetCheckpoint(ctx context.Context, conversationID string) (*CheckpointPair, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, errors.New("user ID not found in request context")
	}
	db, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}

	// Find the assistant row. The tool_calls column is a JSON array; the
	// LIKE filter is a cheap pre-filter, the canonical check happens after
	// we unmarshal and compare function names.
	rows, err := db.QueryContext(ctx,
		`SELECT id, seq, tool_calls
		 FROM messages
		 WHERE conversation_id = ? AND role = 'assistant' AND compacted = 0 AND tool_calls LIKE ?
		 ORDER BY seq ASC`,
		conversationID, "%"+EcoCheckpointToolName+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assistantID, assistantSeq int64
	var toolCallID string
	found := false
	for rows.Next() {
		var id, seq int64
		var tcJSON *string
		if err := rows.Scan(&id, &seq, &tcJSON); err != nil {
			return nil, err
		}
		calls := unmarshalToolCalls(tcJSON)
		for _, tc := range calls {
			if tc.Function.Name == EcoCheckpointToolName {
				assistantID = id
				assistantSeq = seq
				toolCallID = tc.ID
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	// Find the matching tool row.
	var toolID int64
	var contentJSON *string
	err = db.QueryRowContext(ctx,
		`SELECT id, content
		 FROM messages
		 WHERE conversation_id = ? AND role = 'tool' AND tool_call_id = ?
		 LIMIT 1`,
		conversationID, toolCallID,
	).Scan(&toolID, &contentJSON)
	if errors.Is(err, sql.ErrNoRows) {
		// Orphaned assistant half — treat as no checkpoint and let the next
		// summary pass overwrite it. We still report the assistant row id so
		// the caller can clean it up if it wants to.
		return &CheckpointPair{
			AssistantID:  assistantID,
			ToolID:       0,
			AssistantSeq: assistantSeq,
			Summary:      "",
		}, nil
	}
	if err != nil {
		return nil, err
	}

	pair := &CheckpointPair{
		AssistantID:  assistantID,
		ToolID:       toolID,
		AssistantSeq: assistantSeq,
		Summary:      unmarshalMessageContent(contentJSON).TextContent(),
	}
	return pair, nil
}

// MessageSeqByID resolves a message's stable DB row id (utils.Message.ID,
// populated by getConversationFromDB from messages.id) to its seq.
//
// Identity-based boundary resolver, replacing the positional OFFSET lookup for
// the durable fold boundary. The positional lookup is valid only for a FULL
// history slice; the durable path gets its slice from
// GetConversationByIdInternal, which EXCLUDES compacted = 1 rows and INJECTS
// the synthetic checkpoint pair, so slice index and SQL OFFSET count different
// row sets and diverge further on every compaction. Observed on live traffic:
//
//	folding 1035 of 1036 -> durable removed 24
//	folding 1037 of 1062 -> durable removed 0
//	folding 1065 of 1070 -> durable removed 26
//	folding 1067 of 1074 -> durable removed 0   (turn dies at the window)
//
// Identity names a specific row regardless of which view produced the slice.
//
// Returns (0, nil) for absent/unparseable ids (legacy rows, synthetic
// checkpoint pairs) so callers can fall back.
func MessageSeqByID(ctx context.Context, conversationID string, id string) (int64, error) {
	if id == "" {
		return 0, nil
	}
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return 0, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return 0, err
	}
	var rowID int64
	if _, err := fmt.Sscanf(id, "%d", &rowID); err != nil || rowID <= 0 {
		return 0, nil // tool_call_id fallback / checkpoint pair
	}
	var seq int64
	err = d.QueryRowContext(ctx,
		`SELECT seq FROM messages WHERE conversation_id = ? AND id = ?`,
		conversationID, rowID,
	).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// MessageSeqAt returns the seq value of the (sliceOffset)-th message of a
// conversation in seq-ascending order.
//
// CONTRACT: valid ONLY for a slice that is the FULL ordered history (every row
// including compacted ones), i.e. getConversationByIdIncludeCompacted. NOT
// valid for the LLM-facing view from GetConversationByIdInternal, which omits
// compacted rows and carries the injected checkpoint pair. Prefer
// MessageSeqByID whenever the message carries a DB id.
func MessageSeqAt(ctx context.Context, conversationID string, sliceOffset int) (int64, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return 0, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return 0, err
	}
	var seq int64
	// The slice this resolves belongs to the FULL ordered message history of
	// the conversation (the caller passes conv.Messages, which includes
	// already-`compacted` rows via GetConversationById). Using `compacted = 0`
	// here made the OFFSET overshoot when prior compactions had marked rows
	// compacted — the in-memory offset indexed the full history but the query
	// returned only the non-compacted subset, so `sql: no rows` came back for
	// any offset past that subset, MessageSeqAt returned (0,nil), and durable
	// compaction bailed with `folded 0` on EVERY overflow turn (observed live:
	// DBG_boundary firstKeptIdx=2071 return 0 while the conversation had 13k
	// messages). Index into the FULL ordered history by seq.
	err = d.QueryRowContext(ctx,
		`SELECT seq FROM messages WHERE conversation_id = ?
		 ORDER BY seq ASC LIMIT 1 OFFSET ?`,
		conversationID, sliceOffset,
	).Scan(&seq)
	if err != nil {
		// When the slice offset points past the end of the non-compacted rows
		// (e.g. the in-memory view carried checkpoint pairs / attachments
		// whose rows are not counted in this query, or an index arithmetic
		// skew), return NO error with seq=0 so callers treat it as "no
		// boundary" instead of aborting durable compaction. Observed live:
		// "durable compaction failed; falling back to in-memory staged
		// collapse : sql: no rows in result set" on EVERY overflow turn,
		// because this error aborted CompactConversationDurable before it could
		// write any checkpoint. Treat sql.ErrNoRows as empty, propagate real
		// errors.
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return seq, nil
}

// ReplaceCheckpoint atomically removes the previous checkpoint pair (if any)
// and inserts a new one at insertBeforeSeq, shifting live messages above
// that boundary by +2 to make room.
//
// oldPairIDs may be empty (first checkpoint for this conversation). The
// caller is responsible for assembling the assistant + tool messages with
// the correct ToolCallID linkage and EcoCheckpointToolName names.
func ReplaceCheckpoint(
	ctx context.Context,
	conversationID string,
	oldPairIDs []int64,
	assistantMsg utils.Message,
	toolMsg utils.Message,
	insertBeforeSeq int64,
) error {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return errors.New("user ID not found in request context")
	}
	db, err := GetUserDB(userID)
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if len(oldPairIDs) > 0 {
		// NON-DESTRUCTIVE: mark the superseded checkpoint pair as compacted
		// instead of deleting it. The rows stay in the DB (client view keeps
		// them); GetCheckpoint and the LLM view skip compacted rows.
		placeholders := strings.Repeat("?,", len(oldPairIDs))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]interface{}, 0, len(oldPairIDs)+1)
		args = append(args, conversationID)
		for _, id := range oldPairIDs {
			args = append(args, id)
		}
		_, err = tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE messages SET compacted = 1 WHERE conversation_id = ? AND id IN (%s)`, placeholders),
			args...,
		)
		if err != nil {
			return err
		}
	}

	// Shift live messages at or after insertBeforeSeq by +2 to free two
	// consecutive seq slots for the new pair. Done in two passes via
	// negation so the (conversation_id, seq) UNIQUE constraint is never
	// transiently violated mid-update.
	_, err = tx.ExecContext(ctx,
		`UPDATE messages SET seq = -(seq + 2)
		 WHERE conversation_id = ? AND seq >= ?`,
		conversationID, insertBeforeSeq,
	)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE messages SET seq = -seq
		 WHERE conversation_id = ? AND seq < 0`,
		conversationID,
	)
	if err != nil {
		return err
	}

	if _, err := insertMessage(tx, conversationID, insertBeforeSeq, assistantMsg); err != nil {
		return err
	}
	if _, err := insertMessage(tx, conversationID, insertBeforeSeq+1, toolMsg); err != nil {
		return err
	}

	return tx.Commit()
}

// --- Durable context compaction ---------------------------------------------
//
// CompactConversationDurable physically shrinks a conversation's persisted
// history when the LLM context window is exhausted, so the shrink survives
// process restarts and later DB reloads. The previous behavior compacted
// only the in-memory []utils.Message in the LLM loop; the very next
// GetConversationByIdInternal reload pulled the full history back from the
// DB, so every turn re-hit the context-length guard (observed:
// conversation 319ce9283cac61666757b88a — 839 persisted messages, one
// checkpoint stuck at seq 154, a fresh "estimated 134k tokens vs 131072"
// error every turn).
//
// Strategy: fold the messages before `foldFrom` into the conversation's
// existing eco checkpoint summary (creating one if none exists), then mark
// those rows compacted. The
// The summary is UNCAPPED. Every row this fold marks `compacted = 1`
// disappears from the LLM view, so every one of them has to be represented in
// the summary. A bounded summary input meant rows got folded away and were
// simply gone: the old budget walk `continue`d past rows that did not fit,
// `break`ed at exhaustion, then hard-truncated the stored text at 80_000 chars
// — keeping the newest tail and dropping the OLDEST content with no marker.
// Fitting the request to the context window is the CALLER's job:
// ai.resolveEcoContextCompaction picks the boundary so that
// prior summary + new excerpt + live tail fits, and folds exactly that much.
// Deleting triggers the FTS and vector cleanup triggers already defined in
// schema (messages_fts_delete / messages_vec_delete).

// composeDurableSummary builds the checkpoint summary from scratch: the machine
// excerpt of the folded rows (mechanical raw text when no producer is wired),
// carried forward APPEND-ONLY on the previous summary.
//
// The previous shape built the prior text into a string that the machine path
// then threw away, so every fold lost the summary of everything folded before
// it — "context lost on restart" with no error anywhere. Appending (never
// rewriting, never capping) keeps the stored text a strict prefix-extension of
// the previous one: the checkpoint pair is the first content of the prompt after
// the system block, so its head bytes stay byte-identical and the backend KV
// prefix cache keeps reusing them instead of re-prefilling every turn.
func composeDurableSummary(machineSummary func([]utils.Message) string, priorSummary string, folded []utils.Message) string {
	summary := ""
	if machineSummary != nil {
		summary = strings.TrimSpace(machineSummary(folded))
		if summary == "" {
			utils.Log("[DurableCompact] machine excerpt empty — using raw text")
		}
	}
	if summary == "" {
		var input strings.Builder
		writeDurableSummaryText(&input, folded)
		summary = strings.TrimSpace(input.String())
		if summary == "" {
			return ""
		}
		summary = "[compacted by Plurality — machine excerpt unavailable; raw excerpt follows]\n" + summary
	}
	return strings.TrimSpace(carryForwardSummary(priorSummary, summary))
}

// rowsBeyondBoundary returns the rows this fold marks compacted that sit AT OR
// AFTER the boundary the caller summarized: the resolver summarized
// messages[:foldFrom], the clamp can move firstKeptIdx forward.
func rowsBeyondBoundary(messages []utils.Message, foldFrom, firstKeptIdx int) []utils.Message {
	if foldFrom < 0 {
		foldFrom = 0
	}
	if firstKeptIdx > len(messages) {
		firstKeptIdx = len(messages)
	}
	if foldFrom >= firstKeptIdx {
		return nil
	}
	out := make([]utils.Message, 0, firstKeptIdx-foldFrom)
	for i := foldFrom; i < firstKeptIdx; i++ {
		if IsCheckpointMessage(messages[i]) {
			continue
		}
		out = append(out, messages[i])
	}
	return out
}

// representExtraRows renders the rows the stored summary did not cover, so the
// fold cannot mark a row compacted (removing it from every future LLM view)
// without saying what it contained.
func representExtraRows(machineSummary func([]utils.Message) string, extra []utils.Message) string {
	text := ""
	if machineSummary != nil {
		text = strings.TrimSpace(machineSummary(extra))
	}
	if text == "" {
		var input strings.Builder
		writeDurableSummaryText(&input, extra)
		text = strings.TrimSpace(input.String())
	}
	if text == "" {
		return fmt.Sprintf("[compacted by Plurality — %d messages were folded by the boundary clamp and are not represented in this summary]", len(extra))
	}
	return fmt.Sprintf("[folded by the boundary clamp, rendered here so nothing is lost]\n%s", text)
}

// lastUserIdx returns the index of the newest non-checkpoint user message,
// used as the floor that durable compaction never folds past. Returns 0 when
// the conversation has no user row, which the caller treats as "nothing can
// be folded" (foldFrom <= 0 is a no-op).
func lastUserIdx(messages []utils.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && !IsCheckpointMessage(messages[i]) {
			return i
		}
	}
	return 0
}

// CompactConversationDurable folds the oldest messages of a conversation into
// its rolling checkpoint summary (creating one if absent) and marks the folded
// rows compacted (never deleted). `messages` must be the conversation's full
// message list in seq order. `keep` is the number of newest messages preserved
// verbatim. `machineSummary` renders the folded messages into the excerpt that
// becomes the summary; compaction is MACHINE-ONLY (#147) so this must be a pure
// transform — it must never call an LLM. When it is nil, or returns an empty
// string, a bounded mechanical raw dump is stored instead. Returns the number of
// messages folded.
//
// The former `summaryFn func(text string) (string, error)` parameter is REMOVED:
// #147 made compaction machine-only and left it accepted, documented and
// plumbed through every call site but never invoked. Tests passing a summariser
// there silently got the mechanical fallback, which is how
// TestCompactConversationDurable_MovesCheckpointForward came to fail.
func CompactConversationDurable(
	ctx context.Context,
	conversationID string,
	messages []utils.Message,
	foldFrom int,
	machineSummary func([]utils.Message) string,
) (int, error) {
	return CompactConversationDurableWithSummary(ctx, conversationID, messages, foldFrom, machineSummary, "")
}

// CompactConversationDurableWithSummary marks the rows before foldFrom compacted
// and stores `summary` as the new checkpoint summary VERBATIM.
//
// The request path must use this variant. ai.resolveEcoContextCompaction picks
// the fold boundary AND sizes the summary to fit the context window (stage D
// shrinks the carried-forward roll-up to what the budget leaves). When the
// durable fold recomposed the summary itself - prior + UNCAPPED excerpt - it
// stored something strictly larger than the request the resolver had just
// measured as fitting, so the next load came back BIGGER than the request that
// was sent. Measured in the closed-loop test (budget 40000 est tokens):
//
//	round 5: sent ~33987 -> reload ~34001, summary 74523 chars
//	round 6: sent ~34090 -> reload ~40223, summary 93187 chars  (+18664)
//
// The fold reports progress while the request re-inflates every round, which is
// the "compaction never kicks in, the conversation grew to the context size and
// failed to continue" report - and the unbounded summary growth that left the
// live conversation with 16 accumulated checkpoint pairs.
//
// An empty summary keeps the self-composing behaviour (db tests and any caller
// without a staged view).
func CompactConversationDurableWithSummary(
	ctx context.Context,
	conversationID string,
	messages []utils.Message,
	foldFrom int,
	machineSummary func([]utils.Message) string,
	summary string,
) (int, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return 0, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return 0, err
	}
	// foldFrom is the index where the LIVE tail begins: the staged resolver
	// (ai.resolveEcoContextCompaction) chose it, so it already represents
	// "fold exactly what the stages dropped to fit the context window". A
	// non-positive value means the history already fits and there is nothing
	// to fold — never fold the whole conversation, which is how a caller ends
	// up shipping the FULL uncompressed history.
	if foldFrom <= 0 {
		return 0, nil
	}
	if foldFrom > len(messages) {
		foldFrom = len(messages)
	}

	// Locate the current checkpoint pair (assistant half carries the
	// eco_checkpoint tool call; the tool half carries the summary).
	var oldToolID int64
	priorSummary := ""
	rows, err := d.QueryContext(ctx,
		`SELECT id, tool_calls FROM messages
		 WHERE conversation_id = ? AND role = 'assistant' AND compacted = 0 AND tool_calls LIKE ?
		 ORDER BY seq ASC`, conversationID, "%"+EcoCheckpointToolName+"%")
	if err != nil {
		return 0, err
	}
	var foundAsstID int64
	var foundToolCallID string
	for rows.Next() {
		var id int64
		var tcJSON *string
		if err := rows.Scan(&id, &tcJSON); err != nil {
			rows.Close()
			return 0, err
		}
		calls := unmarshalToolCalls(tcJSON)
		for _, tc := range calls {
			if tc.Function.Name == EcoCheckpointToolName {
				foundAsstID = id
				foundToolCallID = tc.ID
				break
			}
		}
		if foundAsstID != 0 {
			break
		}
	}
	rows.Close()
	if foundAsstID != 0 {
		var tcContent *string
		err := d.QueryRowContext(ctx,
			`SELECT id, content FROM messages
			 WHERE conversation_id = ? AND role = 'tool' AND tool_call_id = ? LIMIT 1`,
			conversationID, foundToolCallID).Scan(&oldToolID, &tcContent)
		if err == nil && tcContent != nil {
			priorSummary = unmarshalMessageContent(tcContent).TextContent()
		}
	}

	// Turn-anchored keep boundary. The kept window must ALWAYS start on a
	// user message: the newest user turn is the floor (its in-flight tool
	// chain stays verbatim), and the previous user turn is included when it
	// fits within `keep`. Without this, a long tool chain can push the user's
	// actual ask into the summary and leave a pure tool/assistant wall in
	// the live tail — exactly the "context lost on restart" shape observed
	// on conversation e1c3b48c63eb077f0985b83a (100+ assistant/tool pairs
	// after the checkpoint, zero user messages, so every budgeting pass had
	// no user anchor and the model lost the thread).
	// Clamp the resolver's boundary to a legal live-window start. Never start
	// on a tool row (its parent assistant/tool_call would be folded away,
	// leaving an orphan tool message — a hard API error for strict providers),
	// and always leave a non-empty live tail anchored on a user message, so
	// compaction can never collapse the conversation to nothing.
	firstKeptIdx := foldFrom
	for firstKeptIdx < len(messages) && messages[firstKeptIdx].Role == "tool" {
		firstKeptIdx++
	}
	if firstKeptIdx >= len(messages) {
		firstKeptIdx = lastUserIdx(messages)
		for firstKeptIdx < len(messages) && messages[firstKeptIdx].Role == "tool" {
			firstKeptIdx++
		}
	}
	if firstKeptIdx >= len(messages) || firstKeptIdx <= 0 {
		// Nothing can be folded without emptying the live window.
		return 0, nil
	}

	// Render EVERY compactable message before the boundary into the summary
	// input, in chronological order. No budget: the rows listed here are exactly
	// the rows this transaction can mark `compacted = 1`, so anything left out of
	// the summary is context that silently disappears from every future request.
	// Size is bounded by the boundary the CALLER chose, not by a constant here.
	//
	// Rows durable compaction has ALREADY folded are excluded: the request path
	// loads with includeCompacted = true, so the slice carries them, and the
	// UPDATE below can only mark compacted = 0 rows. Rendering them into the new
	// summary would duplicate history the prior roll-up already represents (the
	// summary grows toward the trigger, which is the #177 re-inflation shape),
	// and it would make the "these are the rows this marks" claim false. Skipping
	// them makes the render set, the fold set and the UPDATE's reach ONE set.
	var folded []utils.Message
	for i := 0; i < firstKeptIdx; i++ {
		m := messages[i]
		if m.Compacted || IsCheckpointMessage(m) {
			continue
		}
		folded = append(folded, m)
	}

	// Compaction is MACHINE-ONLY: it never calls an LLM. The folded history
	// becomes the deterministic megacave excerpt (Caveman /caveman-compress
	// style) when a producer is supplied, and a mechanical raw excerpt
	// otherwise. When the CALLER already has a summary (the staged resolver's
	// shipped view) it is stored as-is: it is the text the request was budgeted
	// against, see CompactConversationDurableWithSummary.
	if strings.TrimSpace(summary) == "" {
		summary = composeDurableSummary(machineSummary, priorSummary, folded)
	}
	if strings.TrimSpace(summary) == "" {
		// Nothing to say about the folded rows — do not move the pair.
		return 0, nil
	}

	// The rows this transaction marks compacted must ALL be represented by the
	// summary it stores. The boundary can be clamped forward past tool rows
	// (an orphan tool result at the head of the live tail is a hard API error),
	// and then the fold marks MORE rows than the caller's summary describes.
	// Saying so out loud is cheaper than deleting history with no trace.
	if extra := rowsBeyondBoundary(messages, foldFrom, firstKeptIdx); len(extra) > 0 {
		summary = strings.TrimSpace(summary) + "\n\n" + representExtraRows(machineSummary, extra)
	}

	// ROW-EXACT FOLD SET. The fold marks compacted EXACTLY the rows listed in
	// `folded` — the rows the shipped summary renders — and nothing else.
	//
	// The boundary used to be a SEQ, and the write was
	//   UPDATE messages SET compacted = 1 WHERE conversation_id = ? AND seq < ?
	// That marks EVERY row below the boundary, including rows the summary never
	// covered, and it re-marks rows earlier folds already compacted. Measured on
	// the live annihilation (conversation 9658ae45a32388ed1dafaac1,
	// 2026-11-17 19:41:30): the shipped summary was ~1384 tokens and the fold
	// marked 232 rows — 230 rows removed from every future request with no
	// representation anywhere. The summary is the only representation of a
	// compacted row, so "summary covers X, fold marks everything below N" is the
	// annihilation. A seq can express a range; it cannot express a SET.
	//
	// The slice is the LLM-facing view (GetConversationByIdInternal), which
	// carries the DB row id in msg.ID for every row, so the set resolves by
	// STABLE IDENTITY. Rows with no DB id (the synthetic pair the view injects,
	// legacy rows) are skipped: they are not fold targets here (the pair is
	// handled by the summary rewrite below) and a positional OFFSET fallback is
	// exactly what diverged from the slice after prior compactions — the
	// observed `durable removed 0`. Skipping is safe: a fold set with no real
	// rows folds nothing and the caller leaves the pair untouched.
	for firstKeptIdx < len(messages) && IsCheckpointMessage(messages[firstKeptIdx]) {
		firstKeptIdx++
	}
	if firstKeptIdx > len(messages) {
		firstKeptIdx = len(messages)
	}
	foldIDs := foldSetRowIDs(messages, folded)

	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// NON-DESTRUCTIVE compaction: mark folded rows with compacted=1 instead of
	// deleting them. The full history stays in the DB and is served to the
	// client (GetConversationById); only the LLM-facing view
	// (GetConversationByIdInternal) skips these rows, with the checkpoint
	// summary standing in for them. This is the fix for "conversations lose
	// messages": compaction shapes requests to the LLM, it never destroys
	// stored conversation data.
	//
	// The set is written by ROW ID, so it is exactly the rows the summary
	// renders — no range, no overreach, no re-marking of rows earlier folds
	// compacted (they are compacted = 0 in the slice by construction, so they
	// cannot be in the set).
	if len(foldIDs) == 0 {
		utils.Log("[DurableCompact] fold set resolves to 0 DB rows (sliceIdx=%d sliceLen=%d foldSet=%d conv=%s) — leaving the checkpoint pair untouched (history NOT shrinking)",
			firstKeptIdx, len(messages), len(folded), conversationID)
		return 0, nil
	}
	ph := strings.Repeat("?,", len(foldIDs))
	ph = ph[:len(ph)-1]
	args := make([]interface{}, 0, len(foldIDs)+1)
	args = append(args, conversationID)
	for _, id := range foldIDs {
		args = append(args, id)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET compacted = 1 WHERE conversation_id = ? AND compacted = 0 AND id IN (`+ph+`)`, args...)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if removed == 0 {
		// Nothing was folded, so there is nothing new to summarise — and moving
		// the pair would be pure damage. The checkpoint pair sits at the HEAD of
		// the LLM view (GetConversationByIdInternal returns compacted=0 rows,
		// pair first), so re-inserting it rewrites the first ~65k characters of
		// every prompt: the backend KV prefix cache can then only reuse the
		// stable system block (observed on strata: reuse 32049 -> 17116, and on
		// the live strata-swift deployment prefill reuse stuck at ~24k of a
		// ~95k prompt, i.e. the whole history re-prefilled every turn).
		//
		// This is the wedge the loud log below describes: conversation
		// 08611c509032e6831ba6031f fired it on EVERY message
		// ("durable compaction folded 0 messages at boundary 2"), rewriting the
		// prompt head each turn while shrinking nothing. Roll the transaction
		// back instead of churning the head.
		utils.Log("[DurableCompact] fold set of %d ids matched 0 compactable rows (sliceIdx=%d sliceLen=%d conv=%s) — leaving the checkpoint pair untouched (history NOT shrinking)",
			len(foldIDs), firstKeptIdx, len(messages), conversationID)
		return 0, nil
	}

	// PERSIST THE SUMMARY BY REWRITING THE EXISTING PAIR IN PLACE (option B).
	//
	// The old shape minted a NEW pair at the boundary: mark the old pair
	// compacted, shift EVERY live row at or after the boundary by +2 to open two
	// seq slots, insert a fresh [assistant, tool] pair. Three separate costs:
	//   - the +2 shift touches the seq of every live message on every fold;
	//   - each fold leaves a superseded pair behind (the live conversation had
	//     16 accumulated pairs, 61-68 KB each, every one of them a row in the
	//     client view and a write in the WAL);
	//   - a brand-new pair row is brand-new bytes at offset 0 of the prompt
	//     (new tool_call_id, new timestamp, content moved), so backend KV prefix
	//     reuse collapses to the system block.
	//
	// Rewriting the summary text on the EXISTING tool row keeps the row id, the
	// seq, the tool_call_id and the assistant half, and touches the one column
	// that actually changed. #183 already proved this is the cheap shape for a
	// summary change (UpdateCheckpointSummaryShrink does exactly this for the
	// stage-D shrink); a fold is the same write with a longer text.
	//
	// "Moves the checkpoint forward" is preserved where it matters: the pair
	// stays the FIRST live row of the view, and the rows it stands for advance
	// every fold, because the fold set is the rows this summary renders. The
	// seq of the pair does not need to track the boundary — every row below the
	// boundary is compacted = 1, so the pair is at the frontier by definition.
	// The e1c3b48c63eb077f0985b83a wedge ("stuck at seq 119/120 with 100+ rows
	// behind and no user anchor") is a boundary-choice bug, fixed by the
	// turn-anchored clamp above, not by where the pair row physically sits.
	if oldToolID == 0 {
		// No pair to rewrite: create the first pair. The slot is MIN(seq) - 2
		// over EVERY row of the conversation (compacted included): rows are never
		// renumbered, so the two seqs immediately below the current minimum are
		// free by definition, and they put the pair FIRST in the seq-ordered LLM
		// view — the invariant every consumer of the pair relies on. This path
		// runs at most once per conversation: once the pair exists, folds
		// rewrite it in place.
		headSeq := int64(1)
		var minSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MIN(seq) FROM messages WHERE conversation_id = ?`,
			conversationID).Scan(&minSeq); err == nil && minSeq.Valid {
			headSeq = minSeq.Int64 - 2
		}
		if err := insertCheckpointPairInTx(ctx, tx, conversationID, headSeq, summary); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return int(removed), nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET content = ? WHERE conversation_id = ? AND id = ?`,
		marshalMessageContent(utils.NewTextContent(summary)), conversationID, oldToolID); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(removed), nil
}

// insertCheckpointPairInTx writes a fresh [assistant, tool] checkpoint pair at
// seq/seq+1 inside the fold transaction. Used ONLY when the conversation has
// no pair yet; folds after that rewrite the existing pair in place.
func insertCheckpointPairInTx(ctx context.Context, tx *sql.Tx, conversationID string, seq int64, summary string) error {
	toolCallID := "eco_" + GenerateID()
	now := time.Now().Format(time.RFC3339)
	assistantMsg := utils.Message{
		Role:      "assistant",
		Content:   utils.MessageContent{},
		Timestamp: now,
		ToolCalls: []utils.ToolCall{{
			ID:   toolCallID,
			Type:  "function",
			Function: utils.FunctionCall{Name: EcoCheckpointToolName, Arguments: "{}"},
		}},
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(summary),
		ToolCallID: toolCallID,
		Name:       EcoCheckpointToolName,
		Timestamp:  now,
	}
	if _, err := insertMessage(tx, conversationID, seq, assistantMsg); err != nil {
		return err
	}
	_, err := insertMessage(tx, conversationID, seq+1, toolMsg)
	return err
}

// foldSetRowIDs maps the fold set (the messages the shipped summary renders) to
// their DB row ids, in the slice's chronological order. Rows without a DB id
// are skipped — see CompactConversationDurableWithSummary.
//
// The two slices are matched by IDENTITY, not by position: `messages` is the
// LLM view, `folded` is the subset the resolver selected (it excludes the pair
// and any already-compacted row). A positional zip would drift the moment those
// two views differ, which is the class of bug this whole function exists to
// kill.
func foldSetRowIDs(messages, folded []utils.Message) []int64 {
	byContent := make(map[string][]int64, len(messages))
	for _, m := range messages {
		id := dbRowID(m.ID)
		if id == 0 {
			continue
		}
		key := m.Role + "\x00" + m.Name + "\x00" + m.ToolCallID + "\x00" + m.Content.TextContent()
		byContent[key] = append(byContent[key], id)
	}
	out := make([]int64, 0, len(folded))
	used := map[int64]bool{}
	for _, m := range folded {
		if id := dbRowID(m.ID); id != 0 {
			if !used[id] {
				used[id] = true
				out = append(out, id)
			}
			continue
		}
		// No stable id (legacy / synthetic): match the earliest unconsumed row
		// with identical identity fields, so the fold set stays exact.
		key := m.Role + "\x00" + m.Name + "\x00" + m.ToolCallID + "\x00" + m.Content.TextContent()
		for _, id := range byContent[key] {
			if !used[id] {
				used[id] = true
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// dbRowID parses the DB row id the view exposes in msg.ID ("" / a
// tool_call_id fallback -> 0).
func dbRowID(id string) int64 {
	var n int64
	if _, err := fmt.Sscanf(id, "%d", &n); err != nil || n <= 0 {
		return 0
	}
	return n
}

// UpdateCheckpointSummaryShrink rewrites ONLY the summary text carried by the
// tool half of the LIVE checkpoint pair, in place: same row id, same seq, same
// tool_call_id, same assistant half, no compacted flag touched, no other row
// touched. Returns whether the row was written.
//
// WHY IN PLACE. CompactConversationDurableWithSummary persists a fold by MOVING
// the pair: mark the old pair compacted, shift every row >= boundarySeq by +2,
// insert a fresh pair. That is the right shape for a FOLD — the boundary moved,
// so the pair has to sit at the new boundary. It is the wrong shape for a
// ROLL-UP SHRINK, where nothing moved: stage D shrank the summary text, the
// fold removed 0 rows, and the pair is already the first live row. Re-inserting
// it there rewrites the first tens of KB of every prompt and shifts the seq of
// every live row, so the backend KV prefix cache can only reuse the stable
// system block. Updating the one column that changed leaves the prompt head a
// pure function of the text.
//
// WHY SHRINK-ONLY (this IS the latch). The write is accepted only when the new
// text is STRICTLY SHORTER than the stored text. Callers reach this from a
// request path that re-runs the shrink every turn against a live tail that
// grows every turn, so an unguarded writer would (a) re-inflate the stored
// summary — the #177 shape, where the durable state comes back BIGGER than the
// request that was just measured as fitting, so compaction never converges —
// and (b) move the prompt head on every turn that adds a byte. Refusing to grow
// makes the stored roll-up a monotone non-increasing step function: identical
// bytes turn after turn inside a shrink band, so prefix reuse extends THROUGH
// the summary (measured ~55k tokens on the live conversation) instead of
// stalling at the 24576 checkpoint floor.
//
// Empty text is refused. The pair row is the ONLY representation of every row
// durable compaction marked compacted = 1 — the client still sees the full
// history, GetConversationByIdInternal does not — so blanking it silently
// deletes that history from every future request.
//
// No FTS/vector maintenance is needed: messages_fts is populated only for
// user/assistant rows (see insertMessage) and vectors are keyed by message id,
// which does not change. The row is a synthetic tool row and is never in the
// search index.
//
// The read-then-write is not wrapped in a transaction: the shrink guard compares
// against the text it just read, so two concurrent writers to the SAME
// conversation could in principle both pass and the longer one land last.
// Requests are serialised per conversation (one active request per conversation
// id) and the pair row is synthetic, so no path writes it concurrently; if that
// serialisation ever goes away, do the compare inside the write transaction.
func UpdateCheckpointSummaryShrink(ctx context.Context, conversationID string, summary string) (bool, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return false, nil
	}
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return false, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return false, err
	}

	// Locate the live pair exactly the way the fold does: the assistant row
	// owning an eco_checkpoint tool call, then the tool row answering that call
	// id. compacted = 0 on both — a superseded pair must never be revived.
	var toolCallID string
	var assistantID int64
	rows, err := d.QueryContext(ctx,
		`SELECT id, tool_calls FROM messages
		 WHERE conversation_id = ? AND role = 'assistant' AND compacted = 0 AND tool_calls LIKE ?
		 ORDER BY seq ASC`, conversationID, "%"+EcoCheckpointToolName+"%")
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		var tcJSON *string
		if err := rows.Scan(&id, &tcJSON); err != nil {
			rows.Close()
			return false, err
		}
		for _, tc := range unmarshalToolCalls(tcJSON) {
			if tc.Function.Name == EcoCheckpointToolName {
				assistantID = id
				toolCallID = tc.ID
				break
			}
		}
		if assistantID != 0 {
			break
		}
	}
	rows.Close()
	if assistantID == 0 || toolCallID == "" {
		return false, nil
	}

	var toolID int64
	var stored *string
	err = d.QueryRowContext(ctx,
		`SELECT id, content FROM messages
		 WHERE conversation_id = ? AND role = 'tool' AND tool_call_id = ? AND compacted = 0
		 LIMIT 1`, conversationID, toolCallID).Scan(&toolID, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		// An assistant half with no live tool half is a half-pair the fold path
		// cannot represent either; there is nothing to latch onto.
		return false, nil
	}
	if err != nil {
		return false, err
	}

	oldText := strings.TrimSpace(unmarshalMessageContent(stored).TextContent())
	if oldText != "" && len(summary) >= len(oldText) {
		// Not a shrink. Monotonicity is the whole point of the latch, and the
		// equal-length case must be a real no-op: rewriting the row with the
		// bytes it already carries dirties a WAL page every turn for zero gain.
		return false, nil
	}

	// Rewrite the content column, preserving the row's other columns. The
	// summary is a single text part, so re-marshalling NewTextContent matches
	// the shape insertMessage wrote.
	if _, err := d.ExecContext(ctx, `UPDATE messages SET content = ? WHERE id = ?`,
		marshalMessageContent(utils.NewTextContent(summary)), toolID); err != nil {
		return false, err
	}
	utils.Log("[DurableCompact] checkpoint summary shrunk in place: %d -> %d chars (id=%d seq untouched, conv=%s)",
		len(oldText), len(summary), toolID, conversationID)
	return true, nil
}

// priorSummaryBanner is the marker the durable summary uses to separate the
// carried-forward summary from the newly folded messages.
const priorSummaryBanner = "PRIOR CHECKPOINT:"

// carryForwardSummary composes the stored checkpoint summary as
//
//	<cumulative roll-up>\n\n[newer history, machine-compacted]\n<new excerpt>
//
// i.e. APPEND-ONLY. The result starts with the previous stored summary
// byte-for-byte, and the checkpoint pair is the first content of the prompt
// after the system block, so the prompt head stays stable and the backend KV
// prefix cache keeps reusing it instead of re-prefilling the whole history
// every turn. Nesting generations of "PRIOR CHECKPOINT:" banners (measured
// live: 15 nested banners, +~105 chars per turn) is what broke that, so the
// head marker is no longer written at all — see deNestPriorSummary for the
// legacy cleanup.
func carryForwardSummary(prior, excerpt string) string {
	prior = deNestPriorSummary(prior)
	excerpt = strings.TrimSpace(excerpt)
	if prior == "" {
		return excerpt
	}
	if excerpt == "" {
		return prior
	}
	return prior + "\n\n" + newerHistoryBanner + "\n" + excerpt
}

// newerHistoryBanner separates the carried-forward roll-up from the excerpt of
// the newest fold. It is a section marker, NOT a nesting marker: a stored
// summary carries exactly one priorSummaryBanner at its head.
const newerHistoryBanner = "[newer history, machine-compacted]"

// deNestPriorSummary returns the cumulative roll-up carried by a stored
// summary, with the head banner and any generations of nested banners
// collapsed. Legacy summaries stored banner+text under a new banner every
// round; stripping the repeated markers keeps the count at one so the head of
// the prompt stops moving.
func deNestPriorSummary(prior string) string {
	prior = strings.TrimSpace(prior)
	if prior == "" {
		return ""
	}
	// Drop every occurrence of the legacy head marker (and its "NEW MESSAGES:"
	// companion), keeping the content that followed them. This is a ONE-TIME
	// rewrite for pre-existing conversations whose stored summary nested the
	// banner for generations; after the first fold the summary this package
	// writes carries no head marker at all, so de-nesting is a no-op and the
	// text stays append-only.
	for {
		k := strings.Index(prior, priorSummaryBanner)
		if k < 0 {
			break
		}
		head := strings.TrimSpace(prior[:k])
		tail := strings.TrimSpace(strings.TrimPrefix(prior[k+len(priorSummaryBanner):], "\n"))
		if head == "" {
			prior = tail
		} else {
			prior = head + "\n" + tail
		}
	}
	prior = strings.TrimSpace(strings.TrimPrefix(prior, "NEW MESSAGES:"))
	// NOTE: newerHistoryBanner is deliberately NOT stripped. It is a section
	// marker of the current format; rewriting/removing it would move bytes in
	// the middle of the stored summary — and the pair is the first content of
	// the prompt after the system block, so mid-text movement invalidates the
	// KV prefix exactly like nesting did.
	return strings.TrimSpace(prior)
}

// writeDurableSummaryText renders a message slice for the durable-compaction
// summary input (same shape as the eco MachineExcerptMegacave output).
func writeDurableSummaryText(b *strings.Builder, msgs []utils.Message) {
	for _, m := range msgs {
		switch m.Role {
		case "user":
			b.WriteString("USER:\n")
			for _, p := range m.ContentParts() {
				if (p.Type == "text" || p.Type == "snippet") && p.Text != "" {
					if p.Filename != "" {
						b.WriteString("[attachment: ")
						b.WriteString(p.Filename)
						b.WriteString("]\n")
					}
					b.WriteString(p.Text)
					b.WriteString("\n")
				}
			}
			b.WriteString("\n")
		case "assistant":
			b.WriteString("ASSISTANT:\n")
			for _, p := range m.ContentParts() {
				if p.Type == "text" && p.Text != "" {
					b.WriteString(p.Text)
					b.WriteString("\n")
				}
			}
			for _, tc := range m.ToolCalls {
				b.WriteString("[tool_call ")
				b.WriteString(tc.Function.Name)
				b.WriteString("] args=")
				b.WriteString(tc.Function.Arguments)
				b.WriteString("\n")
			}
			b.WriteString("\n")
		case "tool":
			name := m.Name
			if name == "" {
				name = "tool"
			}
			b.WriteString("TOOL_RESULT ")
			b.WriteString(name)
			b.WriteString(":\n")
			for _, p := range m.ContentParts() {
				if p.Type == "text" && p.Text != "" {
					b.WriteString(p.Text)
					b.WriteString("\n")
				}
			}
			b.WriteString("\n")
		}
	}
}
