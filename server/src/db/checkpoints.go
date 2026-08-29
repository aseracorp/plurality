package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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

// DeleteStaleSyntheticPairs prunes old long_task-reminder and wait-resume
// synthetic pairs from a conversation. These are machine-generated
// assistant(tool_call) + tool(result) pairs injected by the LLM loop
// (injectLongTaskReminder / pauseForWait); across a long tool-heavy
// conversation dozens of them can accumulate, and every one carries the
// full task list / resume payload back into the LLM context on every turn.
//
// It keeps the most recent pair of each kind (long_task / wait) so the
// last known state stays visible to the model, and deletes every older
// pair. Pairs are always removed as a unit (assistant half + matching tool
// half) so the OpenAI "a tool message must immediately follow its
// assistant tool_call" invariant is never violated. Eco checkpoints are
// untouched. Returns the number of message rows deleted.
func DeleteStaleSyntheticPairs(ctx context.Context, conversationID string) (int, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return 0, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return 0, err
	}

	// Walk the conversation newest-first and find, for each synthetic kind,
	// the assistant tool_call row that should be kept. Everything older for
	// that kind is stale. We match the assistant half by tool_calls JSON
	// containing the tool name; the matching tool row is found by
	// tool_call_id.
	rows, err := d.QueryContext(ctx,
		`SELECT id, tool_calls FROM messages
		 WHERE conversation_id = ? AND role = 'assistant' AND tool_calls IS NOT NULL
		 ORDER BY seq DESC`,
		conversationID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type pair struct{ assistantID, toolID int64 }
	kept := map[string]pair{} // toolName -> most recent pair to keep
	var staleCandidateIDs []int64
	var staleToolCallIDs []string

	for rows.Next() {
		var id int64
		var tcJSON *string
		if err := rows.Scan(&id, &tcJSON); err != nil {
			return 0, err
		}
		if tcJSON == nil {
			continue
		}
		calls := unmarshalToolCalls(tcJSON)
		for _, tc := range calls {
			name := tc.Function.Name
			if !isSyntheticToolName(name) {
				continue
			}
			if _, ok := kept[name]; ok {
				// Already have the newest pair for this kind — this older
				// assistant half is stale.
				staleCandidateIDs = append(staleCandidateIDs, id)
				if tc.ID != "" {
					staleToolCallIDs = append(staleToolCallIDs, tc.ID)
				}
			} else {
				kept[name] = pair{assistantID: id}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(staleCandidateIDs) == 0 {
		return 0, nil
	}

	// Resolve tool halves for the stale assistant calls. A single DELETE of
	// the assistant rows plus a second DELETE of their tool rows is
	// sufficient and avoids shifting seq values (which the eco checkpoint
	// machinery relies on). Tool rows are matched by tool_call_id.
	deleted := 0
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if len(staleCandidateIDs) > 0 {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE conversation_id = ? AND id IN (`+placeholders(len(staleCandidateIDs))+`)`,
			append([]interface{}{conversationID}, intsToInterfaces(staleCandidateIDs)...)...,
		)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}

	if len(staleToolCallIDs) > 0 {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE conversation_id = ? AND role = 'tool' AND tool_call_id IN (`+placeholders(len(staleToolCallIDs))+`)`,
			append([]interface{}{conversationID}, strsToInterfaces(staleToolCallIDs)...)...,
		)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// isSyntheticToolName reports whether a tool name corresponds to a
// machine-generated synthetic pair injected by the LLM loop. These are the
// long_task end-of-turn reminder and the wait/resume timer signal. The wait
// name is the literal registry ToolID ("wait") — kept as a string here to
// avoid an import cycle (ai_tools imports db).
func isSyntheticToolName(name string) bool {
	switch name {
	case "long_task", "wait":
		return true
	}
	return false
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func intsToInterfaces(v []int64) []interface{} {
	out := make([]interface{}, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

func strsToInterfaces(v []string) []interface{} {
	out := make([]interface{}, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
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
