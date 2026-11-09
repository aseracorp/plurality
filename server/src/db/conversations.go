package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/azukaar/plurality/src/search"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

// LiteLLMBaseURL is set by the main package at startup so we can call embeddings.
var LiteLLMBaseURL string

// PushMessage creates a new conversation or appends a message to an existing one.
// Returns the updated conversation, whether it was newly created, and any error.
func PushMessage(ctx context.Context, conversation utils.Conversation, message utils.Message) (utils.Conversation, bool, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return utils.Conversation{}, false, errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return utils.Conversation{}, false, err
	}

	currentTime := time.Now()

	if conversation.ID == "" {
		// New conversation
		conversation.ID = GenerateID()
		conversation.LastMessageAt = currentTime
		conversation.UserID = userID
		conversation.Messages = append(conversation.Messages, message)

		tx, err := db.Begin()
		if err != nil {
			return utils.Conversation{}, false, err
		}
		defer tx.Rollback()

		_, err = tx.Exec(
			`INSERT INTO conversations (id, title, last_message_at, model_selected, state, mini_app, folder, icon)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			conversation.ID,
			conversation.Title,
			currentTime.Format(time.RFC3339Nano),
			marshalJSON(conversation.ModelSelected),
			string(conversation.State),
			marshalMiniApp(conversation.MiniApp),
			conversation.Folder,
			conversation.Icon,
		)
		if err != nil {
			return utils.Conversation{}, false, err
		}

		msgID, err := insertMessage(tx, conversation.ID, 0, message)
		if err != nil {
			return utils.Conversation{}, false, err
		}

		if err := tx.Commit(); err != nil {
			return utils.Conversation{}, false, err
		}

		// Async embedding for searchable messages — serialized through the
		// single embed worker (never a bare goroutine: N concurrent embeds
		// on the SetMaxOpenConns(1) pool wedge the whole backend).
		if message.Role == "user" || message.Role == "assistant" {
			search.EnqueueEmbed(db, LiteLLMBaseURL, "message", fmt.Sprintf("%d", msgID), message.TextContent())
		}

		utils.Log("Created new conversation ID: %s for user ID: %s", conversation.ID, userID)
		return conversation, true, nil
	}

	// Existing conversation — push message
	utils.Debug("Pushing message to conversation ID: %s for user ID: %s", conversation.ID, userID)

	tx, err := db.Begin()
	if err != nil {
		return utils.Conversation{}, false, err
	}
	defer tx.Rollback()

	// Get next sequence number
	var maxSeq sql.NullInt64
	err = tx.QueryRow(`SELECT MAX(seq) FROM messages WHERE conversation_id = ?`, conversation.ID).Scan(&maxSeq)
	if err != nil {
		return utils.Conversation{}, false, err
	}
	nextSeq := int64(0)
	if maxSeq.Valid {
		nextSeq = maxSeq.Int64 + 1
	}

	msgID, err := insertMessage(tx, conversation.ID, nextSeq, message)
	if err != nil {
		return utils.Conversation{}, false, err
	}

	// Build dynamic SET clause
	setClauses := "last_message_at = ?"
	args := []interface{}{currentTime.Format(time.RFC3339Nano)}

	if conversation.Title != "" {
		setClauses += ", title = ?"
		args = append(args, conversation.Title)
	}
	if conversation.ModelSelected.Text != nil {
		setClauses += ", model_selected = ?"
		args = append(args, marshalJSON(conversation.ModelSelected))
	}

	args = append(args, conversation.ID)
	_, err = tx.Exec(`UPDATE conversations SET `+setClauses+` WHERE id = ?`, args...)
	if err != nil {
		return utils.Conversation{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return utils.Conversation{}, false, err
	}

	// Async embedding for searchable messages — serialized through the
	// single embed worker (never a bare goroutine: N concurrent embeds
	// on the SetMaxOpenConns(1) pool wedge the whole backend).
	if message.Role == "user" || message.Role == "assistant" {
		search.EnqueueEmbed(db, LiteLLMBaseURL, "message", fmt.Sprintf("%d", msgID), message.TextContent())
	}

	// Reload the conversation in LLM-FACING form: rows folded into the rolling
	// checkpoint (compacted=1) are excluded, and the live checkpoint pair is
	// included. This is the view every LLM request must be built from (#167/#168);
	// shoving the full non-destructive history here is what made the context
	// guard reject requests that compaction had already made to fit (observed:
	// 741 messages shipped while only 198 were live, 464 folded). The client
	// still gets the full history via GetConversationById.
	updated, err := getConversationFromDB(db, conversation.ID, false)
	if err != nil {
		return utils.Conversation{}, false, err
	}
	updated.UserID = userID

	utils.Debug("Updated conversation: %s for user ID: %s", updated.ID, userID)
	return *updated, false, nil
}

// ListConversations returns all conversations for the current user, sorted by
// LastMessageAt descending, with Messages set to nil.
func ListConversations(ctx context.Context) ([]utils.Conversation, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(
		`SELECT id, title, last_message_at, model_selected, state, mini_app, folder, icon, trigger_type, trigger_id
		 FROM conversations ORDER BY last_message_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conversations []utils.Conversation
	for rows.Next() {
		conv, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		conv.UserID = userID
		conv.Messages = nil
		conversations = append(conversations, conv)
	}

	return conversations, rows.Err()
}

// GetConversationById returns the full client-facing conversation.
// COMPACTION IS NON-DESTRUCTIVE: rows that were folded into the rolling
// checkpoint summary are marked compacted but NEVER deleted, and they are
// included here together with the checkpoint pair (whose tool-role half
// carries the human-readable summary). The client therefore never loses
// history — it sees every message that was ever stored, plus a visible
// summary marker where older turns were folded.
//
// Use GetConversationByIdInternal for the LLM-facing view, which skips
// the compacted rows (the summary replaces them in the request).
func GetConversationById(ctx context.Context, id string) (*utils.Conversation, error) {
	// Include the compacted rows too — the client must see the full history.
	return getConversationByIdIncludeCompacted(ctx, id)
}

// getConversationByIdIncludeCompacted loads a conversation including rows
// marked compacted (the full non-destructive history) plus any checkpoint
// pair. This is the client-facing view.
func getConversationByIdIncludeCompacted(ctx context.Context, id string) (*utils.Conversation, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, errors.New("user ID not found in request context")
	}
	db, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}
	conv, err := getConversationFromDB(db, id, true)
	if err != nil {
		return nil, err
	}
	conv.UserID = userID
	return conv, nil
}

// GetConversationByIdInternal returns the raw conversation including any
// eco checkpoint pair. For server-internal use only — never hand the result
// to a client. Rows marked compacted are EXCLUDED: the LLM sees the summary
// checkpoint pair standing in for them (non-destructive compaction).
func GetConversationByIdInternal(ctx context.Context, id string) (*utils.Conversation, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}

	conv, err := getConversationFromDB(db, id, false)
	if err != nil {
		return nil, err
	}
	conv.UserID = userID
	return conv, nil
}

// DeleteConversation deletes a single conversation and its messages.
func DeleteConversation(ctx context.Context, id string) error {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return errors.New("user ID not found in request context")
	}

	utils.Debug("Deleting conversation ID %s for user ID %s", id, userID)

	db, err := GetUserDB(userID)
	if err != nil {
		return err
	}

	res, err := db.Exec(`DELETE FROM conversations WHERE id = ?`, id)
	if err != nil {
		return err
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// UpdateConversationMetadata updates the title and/or icon of a conversation.
func UpdateConversationMetadata(ctx context.Context, id string, title string, image string) error {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return err
	}

	setClauses := ""
	var args []interface{}

	if title != "" {
		setClauses += "title = ?"
		args = append(args, title)
	}
	if image != "" {
		if setClauses != "" {
			setClauses += ", "
		}
		setClauses += "icon = ?"
		args = append(args, image)
	}

	if setClauses == "" {
		return nil
	}

	args = append(args, id)
	res, err := db.Exec(`UPDATE conversations SET `+setClauses+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// UpdateConversationFolder updates the folder of a conversation.
func UpdateConversationFolder(ctx context.Context, id string, folder string) error {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return err
	}

	res, err := db.Exec(`UPDATE conversations SET folder = ? WHERE id = ?`, folder, id)
	if err != nil {
		return err
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// ListConversationsByTrigger returns conversations matching a (trigger_type,
// trigger_id) pair. Used by the sub-agent management tool to find its children
// (trigger_type='sub_agent', trigger_id=<parent_conv_id>). Messages are not loaded.
func ListConversationsByTrigger(ctx context.Context, triggerType, triggerID string) ([]utils.Conversation, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(
		`SELECT id, title, last_message_at, model_selected, state, mini_app, folder, icon, trigger_type, trigger_id
		 FROM conversations WHERE trigger_type = ? AND trigger_id = ? ORDER BY last_message_at DESC`,
		triggerType, triggerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conversations []utils.Conversation
	for rows.Next() {
		conv, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		conv.UserID = userID
		conversations = append(conversations, conv)
	}

	return conversations, rows.Err()
}

// SetConversationTrigger records what triggered a conversation (e.g.
// triggerType="cron", triggerID=<cron uuid>). Used by the cron and webhook
// packages — kept generic so a third trigger type doesn't add a column.
func SetConversationTrigger(ctx context.Context, conversationID, triggerType, triggerID string) error {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return err
	}

	res, err := db.Exec(
		`UPDATE conversations SET trigger_type = ?, trigger_id = ? WHERE id = ?`,
		triggerType, triggerID, conversationID,
	)
	if err != nil {
		return err
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// UpdateConversationState sets the processing state of a conversation.
func UpdateConversationState(ctx context.Context, conversationID string, state utils.ConversationState) error {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return err
	}

	res, err := db.Exec(`UPDATE conversations SET state = ? WHERE id = ?`, string(state), conversationID)
	if err != nil {
		return err
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// GetActiveConversationsForUser returns conversations that are not idle.
func GetActiveConversationsForUser(ctx context.Context) ([]utils.Conversation, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, errors.New("user ID not found in request context")
	}

	db, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(
		`SELECT id, title, last_message_at, model_selected, state, mini_app, folder, icon, trigger_type, trigger_id
		 FROM conversations WHERE state != ?`, string(utils.StateIdle),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conversations []utils.Conversation
	for rows.Next() {
		conv, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		conv.UserID = userID
		conversations = append(conversations, conv)
	}

	return conversations, rows.Err()
}

// DeleteAllConversations deletes all conversations for a user and cleans up files.
func DeleteAllConversations(ctx context.Context, userID string) (int64, error) {
	if userID == "" {
		return 0, errors.New("user ID cannot be empty")
	}

	utils.Log("[DeleteAllConversations] Deleting all conversations for user ID: %s", userID)

	db, err := GetUserDB(userID)
	if err != nil {
		return 0, err
	}

	// Count first
	var count int64
	db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count)

	// Delete all
	_, err = db.Exec(`DELETE FROM conversations`)
	if err != nil {
		utils.Error("[DeleteAllConversations] Error deleting user conversations", err)
		return 0, err
	}

	utils.Log("[DeleteAllConversations] Deleted %d conversations for user ID: %s", count, userID)

	// Clean up all attachment files for this user
	if cleanErr := storage.DeleteUserFiles(userID); cleanErr != nil {
		utils.Error("[DeleteAllConversations] Error cleaning up user files", cleanErr)
	}

	return count, nil
}

// --- Internal helpers ---

// insertMessage inserts a single message row into the messages table
// and populates the FTS index for user/assistant messages with plain text.
// Returns the auto-incremented message ID.
func insertMessage(tx *sql.Tx, conversationID string, seq int64, msg utils.Message) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO messages (conversation_id, seq, role, content, tool_calls, tool_call_id, name, timestamp, total_tokens, prompt_tokens, completion_tokens, response_cost, compression_ratio, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conversationID,
		seq,
		msg.Role,
		marshalMessageContent(msg.Content),
		marshalToolCalls(msg.ToolCalls),
		nullString(msg.ToolCallID),
		nullString(msg.Name),
		nullString(msg.Timestamp),
		msg.TotalTokens,
		msg.PromptTokens,
		msg.CompletionTokens,
		msg.ResponseCost,
		msg.CompressionRatio,
		marshalModel(msg.Model),
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()

	// Populate FTS with plain text (not JSON) for searchable roles
	if (msg.Role == "user" || msg.Role == "assistant") && msg.TextContent() != "" {
		tx.Exec(
			`INSERT INTO messages_fts(conversation_id, message_id, content) VALUES (?, ?, ?)`,
			conversationID, id, msg.TextContent(),
		)
	}

	return id, nil
}

// scanConversation reads a conversation row (without messages) from a rows scanner.
func scanConversation(rows *sql.Rows) (utils.Conversation, error) {
	var conv utils.Conversation
	var lastMessageAt string
	var modelSelectedJSON string
	var stateStr string
	var miniAppJSON *string
	var triggerType *string
	var triggerID *string

	err := rows.Scan(
		&conv.ID,
		&conv.Title,
		&lastMessageAt,
		&modelSelectedJSON,
		&stateStr,
		&miniAppJSON,
		&conv.Folder,
		&conv.Icon,
		&triggerType,
		&triggerID,
	)
	if err != nil {
		return conv, err
	}

	conv.LastMessageAt, _ = time.Parse(time.RFC3339Nano, lastMessageAt)
	conv.ModelSelected = unmarshalModelSelected(modelSelectedJSON)
	conv.State = utils.ConversationState(stateStr)
	conv.MiniApp = unmarshalMiniApp(miniAppJSON)
	if triggerType != nil {
		conv.TriggerType = *triggerType
	}
	if triggerID != nil {
		conv.TriggerID = *triggerID
	}

	return conv, nil
}

// getConversationFromDB loads a full conversation with messages from SQLite.
func getConversationFromDB(db *sql.DB, id string, includeCompacted bool) (*utils.Conversation, error) {
	// Load conversation row
	var conv utils.Conversation
	var lastMessageAt string
	var modelSelectedJSON string
	var stateStr string
	var miniAppJSON *string
	var triggerType *string
	var triggerID *string

	err := db.QueryRow(
		`SELECT id, title, last_message_at, model_selected, state, mini_app, folder, icon, trigger_type, trigger_id
		 FROM conversations WHERE id = ?`, id,
	).Scan(
		&conv.ID,
		&conv.Title,
		&lastMessageAt,
		&modelSelectedJSON,
		&stateStr,
		&miniAppJSON,
		&conv.Folder,
		&conv.Icon,
		&triggerType,
		&triggerID,
	)
	if err != nil {
		return nil, err
	}

	conv.LastMessageAt, _ = time.Parse(time.RFC3339Nano, lastMessageAt)
	conv.ModelSelected = unmarshalModelSelected(modelSelectedJSON)
	conv.State = utils.ConversationState(stateStr)
	conv.MiniApp = unmarshalMiniApp(miniAppJSON)
	if triggerType != nil {
		conv.TriggerType = *triggerType
	}
	if triggerID != nil {
		conv.TriggerID = *triggerID
	}

	// Load messages
	compactedFilter := ""
	if !includeCompacted {
		compactedFilter = " AND compacted = 0"
	}
	msgRows, err := db.Query(
		`SELECT id, role, content, tool_calls, tool_call_id, name, timestamp, total_tokens, prompt_tokens, completion_tokens, response_cost, compression_ratio, model, compacted
		 FROM messages WHERE conversation_id = ?`+compactedFilter+` ORDER BY seq`, id,
	)
	if err != nil {
		return nil, err
	}
	defer msgRows.Close()

	for msgRows.Next() {
		var msg utils.Message
		var msgID int64
		var contentJSON *string
		var toolCallsJSON *string
		var toolCallID *string
		var name *string
		var timestamp *string
		var modelJSON *string
		var compacted int

		err := msgRows.Scan(
			&msgID,
			&msg.Role,
			&contentJSON,
			&toolCallsJSON,
			&toolCallID,
			&name,
			&timestamp,
			&msg.TotalTokens,
			&msg.PromptTokens,
			&msg.CompletionTokens,
			&msg.ResponseCost,
			&msg.CompressionRatio,
			&modelJSON,
			&compacted,
		)
		if err != nil {
			return nil, err
		}

		msg.Content = unmarshalMessageContent(contentJSON)
		msg.ToolCalls = unmarshalToolCalls(toolCallsJSON)
		if toolCallID != nil {
			msg.ToolCallID = *toolCallID
		}
		if name != nil {
			msg.Name = *name
		}
		if timestamp != nil {
			msg.Timestamp = *timestamp
		}
		msg.Model = unmarshalModel(modelJSON)
		msg.Compacted = compacted != 0

		// Expose the stable DB id to clients for dedupe/merge. Fall back to the
		// tool_call_id for tool messages (legacy rows) — those are still unique
		// per assistant turn. Non-tool legacy rows keep an empty ID, which the
		// client treats as "cannot dedupe, append if content differs".
		msg.ID = fmt.Sprintf("%d", msgID)
		if msg.ID == "0" || msg.ID == "" {
			msg.ID = msg.ToolCallID
		}

		conv.Messages = append(conv.Messages, msg)
	}

	if err := msgRows.Err(); err != nil {
		return nil, err
	}

	return &conv, nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}