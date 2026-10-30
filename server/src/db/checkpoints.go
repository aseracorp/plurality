package db

import (
	"context"
	"database/sql"
	"encoding/json"
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
// mode is enabled (see ai/eco.go::filterCheckpointsForRequest).
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
		 WHERE conversation_id = ? AND role = 'assistant' AND tool_calls LIKE ?
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
		`SELECT seq FROM messages WHERE conversation_id = ?
		 ORDER BY seq ASC LIMIT 1 OFFSET ?`,
		conversationID, sliceOffset,
	).Scan(&seq)
	if err != nil {
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
		placeholders := strings.Repeat("?,", len(oldPairIDs))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]interface{}, 0, len(oldPairIDs)+1)
		args = append(args, conversationID)
		for _, id := range oldPairIDs {
			args = append(args, id)
		}
		_, err = tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM messages WHERE conversation_id = ? AND id IN (%s)`, placeholders),
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
) (int, error) {
	if keep <= 0 {
		keep = durableKeepDefault
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
		 WHERE conversation_id = ? AND role = 'assistant' AND tool_calls LIKE ?
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

	// Render the oldest (len(messages)-keep) messages into the summary input.
	// Walk newest-first with a budget, then reverse for chronological order.
	var folded []utils.Message
	budget := maxDurableSummaryChars
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if IsCheckpointMessage(m) {
			continue
		}
		if i >= len(messages)-keep {
			continue // newest `keep` stay verbatim, not folded
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

	summary := ""
	if summaryFn != nil {
		summary, err = summaryFn(text)
		if err != nil {
			utils.Error("[DurableCompact] summary model call failed — falling back to raw text", err)
			summary = ""
		}
	}
	if summary == "" {
		summary = "[compacted by Plurality — summary unavailable; raw excerpt follows]\n" + text
		if len(summary) > maxDurableSummaryChars {
			summary = summary[len(summary)-maxDurableSummaryChars:]
		}
	}
	summary = strings.TrimSpace(summary)

	// Resolve DB seq of the first kept (newest `keep` + optional pair) row so
	// we can delete everything before it. The `messages` slice is seq-ordered;
	// the first kept message is at index len(messages)-keep (after skipping
	// checkpoint messages, which the slice includes). Walk the slice to find
	// the first non-checkpoint message at or after that index, then look up
	// its seq.
	firstKeptIdx := len(messages) - keep
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

	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Delete rows with seq < boundarySeq, preserving the checkpoint pair ids.
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
		`DELETE FROM messages WHERE conversation_id = ? AND seq < ?`+excludeSQL, args...)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()

	// Persist the new summary: update the existing tool row, or insert a
	// fresh checkpoint pair if none existed.
	if oldToolID != 0 {
		newContent, _ := json.Marshal(summary)
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET content = ? WHERE id = ?`, string(newContent), oldToolID); err != nil {
			return 0, err
		}
	} else {
		// Shift rows >= boundarySeq by +2 and insert a fresh pair at the
		// boundary, mirroring ReplaceCheckpoint.
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
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(removed), nil
}

// writeDurableSummaryText renders a message slice for the durable-compaction
// summary input (same shape as the eco renderMessagesForSummary output).
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
