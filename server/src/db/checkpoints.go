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

// MessageSeqAt returns the seq value of the (sliceOffset)-th message of a
// conversation in seq-ascending order. Used by the eco summary code to
// resolve a slice index (from an in-memory load) back to the actual DB
// seq, which may differ if prior checkpoints have created gaps.
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
	err = d.QueryRowContext(ctx,
		`SELECT seq FROM messages WHERE conversation_id = ? AND compacted = 0
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
// Strategy: fold the oldest (len(messages) − keep) messages into the
// conversation's existing eco checkpoint summary (creating one if none
// exists), then physically DELETE those rows inside one transaction. The
// summary input is bounded (see maxDurableSummaryChars below) so a huge
// history cannot blow the fast model's window; when the summary model call
// fails we fall back to a bounded raw dump so durable progress is still
// made. Deleting triggers the FTS and vector cleanup triggers already
// defined in schema (messages_fts_delete / messages_vec_delete).

const maxDurableSummaryChars = 80_000 // mirrors ai perMsgBudget family; summary input bound
const durableKeepDefault = 30         // newest messages kept verbatim

// CompactConversationDurable folds the oldest messages of a conversation
// into its rolling checkpoint summary (creating one if absent) and deletes
// them from the DB. `messages` must be the conversation's full message list
// in seq order (as returned by GetConversationByIdInternal). `keep` is the
// number of newest messages preserved verbatim. `summaryFn` summarises the
// input text; when it returns an error or empty string a bounded raw dump
// is stored instead. Returns the number of messages removed.
func CompactConversationDurable(
	ctx context.Context,
	conversationID string,
	messages []utils.Message,
	keep int,
	summaryFn func(text string) (string, error),
	machineSummaryFn ...func([]utils.Message) string,
) (int, error) {
	if keep <= 0 {
		keep = durableKeepDefault
	}
	var machineSummary func([]utils.Message) string
	if len(machineSummaryFn) > 0 && machineSummaryFn[0] != nil {
		machineSummary = machineSummaryFn[0]
	}
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return 0, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return 0, err
	}
	// Need at least keep+1 non-checkpoint messages to make progress.
	if len(messages) <= keep+1 {
		return 0, nil
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
	firstKeptIdx := turnAnchoredFirstKeptIdx(messages, keep)
	// The turn-anchored floor keeps the WHOLE newest user turn even when that
	// turn is a massive tool/assistant wall (hundreds of messages — observed
	// conversation 4aa674fc88d9de92b6f8af15: only 2 user messages followed by
	// ~200 tool/assistant rows). Keeping all of it means there is nothing to
	// fold: "durable removed 0" every step, the in-memory trim is undone by
	// the next DB reload, and the context guard re-fires on every turn. When
	// the kept window alone exceeds a sane size (2x keep), apply a HARD cap:
	// keep only the newest `keep` messages (aligned off a tool row so
	// tool_calls/results pairing stays intact) and fold the rest of the
	// oversized turn too, so durable compaction always makes real progress.
	if len(messages)-firstKeptIdx > keep*2 {
		if hard := hardFirstKeptIdx(messages, keep); hard > firstKeptIdx {
			firstKeptIdx = hard
		}
	}

	// Render the oldest (firstKeptIdx) messages into the summary input.
	// Walk newest-first with a budget, then reverse for chronological order.
	var folded []utils.Message
	budget := maxDurableSummaryChars
	for i := firstKeptIdx - 1; i >= 0; i-- {
		m := messages[i]
		if IsCheckpointMessage(m) {
			continue
		}
		if budget <= 0 {
			break
		}
		mBytes := m.Content.TotalContentChars()
		if mBytes > maxDurableSummaryChars {
			mBytes = maxDurableSummaryChars
		}
		if mBytes > budget {
			continue
		}
		budget -= mBytes
		folded = append(folded, m)
	}
	for i, j := 0, len(folded)-1; i < j; i, j = i+1, j-1 {
		folded[i], folded[j] = folded[j], folded[i]
	}

	var input strings.Builder
	if priorSummary != "" {
		input.WriteString("PRIOR CHECKPOINT:\n")
		input.WriteString(priorSummary)
		input.WriteString("\n\nNEW MESSAGES:\n")
	}
	writeDurableSummaryText(&input, folded)
	text := strings.TrimSpace(input.String())
	if text == "" {
		return 0, nil
	}

	// Compaction is MACHINE-ONLY: it must never call an LLM. The folded history
	// is replaced by the deterministic megacave excerpt (Caveman
	// /caveman-compress-style; no model call) when a producer is supplied, and
	// otherwise by a mechanical raw excerpt of the folded text. Either way this
	// is a pure history transform for the future conversation workflow — no
	// request is ever sent to a chat model (the old LLM "summary" fallback
	// blocked the retry for minutes on a slow self-hosted model and meant
	// compaction was oddly doing model inference at all).
	summary := ""
	if machineSummary != nil {
		summary = strings.TrimSpace(machineSummary(folded))
		if summary == "" {
			utils.Log("[DurableCompact] machine excerpt empty — using raw text")
		}
	}
	if summary == "" {
		// Mechanical fallback (no LLM): mark it clearly so a resumed workflow
		// knows the older messages were rolled up, then cap the size.
		summary = "[compacted by Plurality — summary unavailable; raw excerpt follows]\n" + text
		if len(summary) > maxDurableSummaryChars {
			summary = summary[len(summary)-maxDurableSummaryChars:]
		}
	}
	summary = strings.TrimSpace(summary)

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
	boundarySeq, err := MessageSeqAt(ctx, conversationID, firstKeptIdx)
	if err != nil {
		return 0, err
	}
	if boundarySeq == 0 {
		// MessageSeqAt reports "no boundary" when the offset points past the
		// non-compacted rows (see MessageSeqAt). Cannot determine where to
		// insert the checkpoint — skip this round without aborting so the
		// request still proceeds; durable compaction will succeed on a later
		// turn when the view lines up with the DB.
		return 0, nil
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

// turnAnchoredFirstKeptIdx computes the slice index where the kept window
// starts so the live tail is always anchored on a complete user turn.
//
//   - The newest user message and everything after it (its in-flight turn)
//     are ALWAYS kept — the floor. A tail that starts mid-turn orphans tool
//     results whose parent assistant call was cut and gives the model no
//     request to answer.
//   - The previous user turn is included when it fits within `keep`
//     messages, so the model also sees the turn that led into the current
//     one. `keep` remains a soft upper bound on the window, not a hard
//     message count: when the newest user turn alone is longer than `keep`
//     messages (a long tool chain), the whole turn is kept anyway — a
//     coherent tail beats a fixed count.
//   - Conversations with NO user message at all (pure background-agent
//     tool loops) fall back to the newest `keep` messages, but never
//     starting on a "tool" row: the window is pushed forward to the first
//     assistant boundary so the model still sees a complete turn head.
func turnAnchoredFirstKeptIdx(messages []utils.Message, keep int) int {
	n := len(messages)
	if n == 0 {
		return 0
	}
	// Floor: the newest user message and its in-flight turn.
	floor := n - 1
	for floor > 0 && messages[floor].Role != "user" {
		floor--
	}
	if messages[floor].Role == "user" {
		// Keep the previous user turn too when it fits within the window
		// (the floor itself is at the very start of the slice, so there is
		// no previous turn to include).
		prev := floor - 1
		for prev > 0 && messages[prev].Role != "user" {
			prev--
		}
		if prev >= 0 && messages[prev].Role == "user" && floor-prev <= keep {
			return prev
		}
		return floor
	}
	// No user message anywhere: fall back to the newest `keep` messages but
	// never start on a tool row (that would orphan its parent call).
	start := n - keep
	if start < 0 {
		start = 0
	}
	for start < n && messages[start].Role == "tool" {
		start++
	}
	if start >= n {
		start = n - 1
	}
	return start
}

// hardFirstKeptIdx returns the slice index that keeps ONLY the newest `keep`
// messages of a conversation, aligned so the kept window never starts on a
// "tool" row (which would orphan its parent assistant tool_call). Used as the
// hard cap when the turn-anchored window is itself an oversized tool wall, so
// durable compaction can always fold a meaningful chunk and make progress.
func hardFirstKeptIdx(messages []utils.Message, keep int) int {
	n := len(messages)
	if n == 0 {
		return 0
	}
	if keep <= 0 {
		keep = durableKeepDefault
	}
	start := n - keep
	if start < 0 {
		start = 0
	}
	for start < n && messages[start].Role == "tool" {
		start++
	}
	if start >= n {
		start = n - 1
	}
	return start
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
