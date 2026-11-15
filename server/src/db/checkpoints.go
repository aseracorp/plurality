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
const EcoCheckpointToolName = "eco_checkpoint"

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
	var pairIDs []int64
	if foundAsstID != 0 {
		pairIDs = append(pairIDs, foundAsstID)
		var tcContent *string
		err := d.QueryRowContext(ctx,
			`SELECT id, content FROM messages
			 WHERE conversation_id = ? AND role = 'tool' AND tool_call_id = ? LIMIT 1`,
			conversationID, foundToolCallID).Scan(&oldToolID, &tcContent)
		if err == nil {
			if oldToolID != 0 {
				pairIDs = append(pairIDs, oldToolID)
			}
			if tcContent != nil {
				priorSummary = unmarshalMessageContent(tcContent).TextContent()
			}
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

	// Render EVERY message before the boundary into the summary input, in
	// chronological order. No budget and no skipping: the rows listed here are
	// exactly the rows this transaction marks `compacted = 1`, so anything left
	// out of the summary is context that silently disappears from every future
	// request. Size is bounded by the boundary the CALLER chose, not by a
	// constant here.
	var folded []utils.Message
	for i := 0; i < firstKeptIdx; i++ {
		m := messages[i]
		if IsCheckpointMessage(m) {
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

	// Resolve DB seq of the first kept row so we can delete everything before
	// it. The `messages` slice is seq-ordered; the first kept message is at
	// the turn-anchored firstKeptIdx computed above. Skip any checkpoint
	// messages (the slice includes the pair), then look up its seq.
	for firstKeptIdx < len(messages) && IsCheckpointMessage(messages[firstKeptIdx]) {
		firstKeptIdx++
	}
	if firstKeptIdx >= len(messages) {
		return 0, nil
	}
	// Resolve the boundary by STABLE IDENTITY first. The slice here comes from
	// the LLM-facing view (GetConversationByIdInternal), which omits compacted
	// rows and carries the injected checkpoint pair, so a positional OFFSET
	// lookup into the FULL history indexes a different row set and diverges
	// after every compaction — the observed `durable removed 0`.
	boundarySeq, err := MessageSeqByID(ctx, conversationID, messages[firstKeptIdx].ID)
	if err != nil {
		return 0, err
	}
	if boundarySeq == 0 {
		// Legacy rows / synthetic pair with no DB id: fall back to positional.
		boundarySeq, err = MessageSeqAt(ctx, conversationID, firstKeptIdx)
		if err != nil {
			return 0, err
		}
	}
	if boundarySeq == 0 {
		utils.Log("[DurableCompact] cannot resolve fold boundary: id=%q sliceIdx=%d sliceLen=%d conv=%s — NOT compacting",
			messages[firstKeptIdx].ID, firstKeptIdx, len(messages), conversationID)
		return 0, fmt.Errorf("durable compaction: unresolved fold boundary at slice index %d", firstKeptIdx)
	}

	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// NON-DESTRUCTIVE compaction: mark folded rows with compacted=1 instead
	// of deleting them. The full history stays in the DB and is served to
	// the client (GetConversationById); only the LLM-facing view
	// (GetConversationByIdInternal) skips these rows, with the checkpoint
	// summary standing in for them. This is the fix for "conversations lose
	// messages": compaction shapes requests to the LLM, it never destroys
	// stored conversation data.
	args := []interface{}{conversationID, boundarySeq}
	excludeSQL := ""
	if len(pairIDs) > 0 {
		ph := strings.Repeat("?,", len(pairIDs))
		ph = ph[:len(ph)-1]
		excludeSQL = " AND id NOT IN (" + ph + ")"
		for _, id := range pairIDs {
			args = append(args, id)
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET compacted = 1 WHERE conversation_id = ? AND seq < ? AND compacted = 0`+excludeSQL, args...)
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
		utils.Log("[DurableCompact] boundary seq=%d (sliceIdx=%d sliceLen=%d conv=%s) folded 0 rows — leaving the checkpoint pair untouched (history NOT shrinking)",
			boundarySeq, firstKeptIdx, len(messages), conversationID)
		return 0, nil
	}

	// Persist the new summary by MOVING the checkpoint forward: delete the
	// old pair (if any) and insert a fresh pair at the boundary, mirroring
	// ReplaceCheckpoint. The previous behaviour updated the existing tool row
	// IN PLACE, which froze the checkpoint at its original seq while the live
	// tail grew behind it — after a restart the tail had no user anchor and
	// the rolling summary never advanced (observed: conversation
	// e1c3b48c63eb077f0985b83a, checkpoint stuck at seq 119/120 while 100+
	// pure tool/assistant messages piled up behind it, so every turn
	// re-compacted the same stuck boundary and the model lost the thread).
	if len(pairIDs) > 0 {
		// Mark the old pair as compacted too (non-destructive) — it is
		// superseded by the fresh pair inserted below.
		placeholders := strings.Repeat("?,", len(pairIDs))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]interface{}, 0, len(pairIDs)+1)
		args = append(args, conversationID)
		for _, id := range pairIDs {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE messages SET compacted = 1 WHERE conversation_id = ? AND id IN (%s)`, placeholders),
			args...); err != nil {
			return 0, err
		}
	}
	// Shift rows >= boundarySeq by +2 and insert a fresh pair at the
	// boundary, mirroring ReplaceCheckpoint. Done in two passes via negation
	// so the (conversation_id, seq) UNIQUE constraint is never transiently
	// violated mid-update.
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET seq = -(seq + 2) WHERE conversation_id = ? AND seq >= ?`,
		conversationID, boundarySeq); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET seq = -seq WHERE conversation_id = ? AND seq < 0`,
		conversationID); err != nil {
		return 0, err
	}
	toolCallID := "eco_" + GenerateID()
	now := time.Now().Format(time.RFC3339)
	assistantMsg := utils.Message{
		Role:      "assistant",
		Content:   utils.MessageContent{},
		Timestamp: now,
		ToolCalls: []utils.ToolCall{{
			ID:   toolCallID,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      EcoCheckpointToolName,
				Arguments: "{}",
			},
		}},
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(summary),
		ToolCallID: toolCallID,
		Name:       EcoCheckpointToolName,
		Timestamp:  now,
	}
	if _, err := insertMessage(tx, conversationID, boundarySeq, assistantMsg); err != nil {
		return 0, err
	}
	if _, err := insertMessage(tx, conversationID, boundarySeq+1, toolMsg); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(removed), nil
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
