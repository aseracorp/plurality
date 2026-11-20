package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/textsafe"
	"github.com/azukaar/plurality/src/utils"
)

// EcoCheckpointToolName is the function name used on the assistant tool call
// that represents an eco-mode rolling-checkpoint summary. The matching tool
// result message carries the actual summary text in its Content.
//
// A single conversation has at most one checkpoint pair at any time. The pair
// sits in the messages table ahead of the live tail: durable compaction mints
// it once and then rewrites its summary in place (see
// CompactConversationDurable). It is invisible to the client —
// GetConversationById strips it via FilterOutCheckpoints — and only flows to
// the LLM when eco mode is enabled (see ai/eco.go::filterCheckpointsForRequest).
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

// ToolResultByCallID returns the FULL, uncompressed body of the tool result
// identified by its tool_call_id — the recovery path for caveman "read less".
//
// Keyed on tool_call_id rather than seq on purpose: inserting an eco checkpoint
// shifts existing rows by +2, so a seq baked into a stored banner would point
// at the wrong message after compaction. tool_call_id is immutable.
//
// Compression is non-destructive: the projection lives in compressed_content
// and the tool's original output stays in `content`, so the model can always
// pull back exactly what the tool printed. found=false when no tool row has
// that call id in this conversation.
func ToolResultByCallID(ctx context.Context, conversationID, toolCallID string) (body string, name string, found bool, err error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return "", "", false, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return "", "", false, err
	}
	if toolCallID == "" {
		return "", "", false, nil
	}
	var contentJSON *string
	var namePtr *string
	scanErr := d.QueryRowContext(ctx,
		`SELECT content, name FROM messages
		  WHERE conversation_id = ? AND tool_call_id = ? AND role = 'tool'
		  ORDER BY seq DESC LIMIT 1`,
		conversationID, toolCallID,
	).Scan(&contentJSON, &namePtr)
	if scanErr == sql.ErrNoRows {
		return "", "", false, nil
	}
	if scanErr != nil {
		return "", "", false, scanErr
	}
	if namePtr != nil {
		name = *namePtr
	}
	return unmarshalMessageContent(contentJSON).TextContent(), name, true, nil
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

// --- Durable context compaction (ultracave frozen compaction) ---------------
//
// CompactConversationDurable shrinks a conversation's LLM-facing history
// PHYSICALLY, so the shrink survives process restarts and later DB reloads.
// In-memory compaction alone re-inflates on the next GetConversationByIdInternal
// reload, which is how a conversation ends up re-hitting the context-length
// guard every single turn.
//
// Shape (this is the whole contract):
//
//   - NON-DESTRUCTIVE. Folded rows are FLAGGED (`compacted = 1`), never
//     DELETEd, and their seq is never renumbered. The client keeps seeing the
//     full history; only the LLM view skips them, with the checkpoint summary
//     standing in for them.
//   - ROW-EXACT. The fold marks compacted EXACTLY the rows the stored summary
//     renders, addressed by stable DB row id. The destructive seq-range shape
//     (`UPDATE ... WHERE seq < ?`) marks every row below a boundary — including
//     rows no summary ever covered — and re-marks rows earlier folds compacted.
//     A seq can express a range; it cannot express a SET.
//   - ONE PAIR, REWRITTEN IN PLACE. A conversation has at most one checkpoint
//     pair, minted once. Every later fold rewrites ONLY the content column of
//     the existing tool row: same row id, same seq, same tool_call_id, same
//     assistant half. The pair is the first content of the prompt after the
//     system block, so a fold that mints a fresh pair (new tool_call_id, new
//     timestamp, +2 seq shift of every live row) rewrites the head of every
//     prompt and collapses backend KV prefix reuse to the system block.
//   - ANTI-CHURN. A fold that can remove no row returns 0 and leaves the pair
//     byte-identical, so a no-progress turn never rewrites the prompt head.
//   - MACHINE-ONLY. `machineSummary` is a PURE transform of the folded rows;
//     it must never call an LLM. nil or an empty result stores a bounded
//     mechanical excerpt instead.
//
// Returns the number of rows folded.
func CompactConversationDurable(
	ctx context.Context,
	conversationID string,
	messages []utils.Message,
	foldFrom int,
	machineSummary func([]utils.Message) string,
) (int, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return 0, errors.New("user ID not found in request context")
	}
	d, err := GetUserDB(userID)
	if err != nil {
		return 0, err
	}

	// foldFrom is the index at which the LIVE tail begins, chosen by the caller.
	// A non-positive value means "the history already fits": folding from index
	// 0 would fold the WHOLE conversation, which is how a caller ends up
	// shipping an empty live window. Never do that.
	if foldFrom <= 0 {
		return 0, nil
	}
	if foldFrom > len(messages) {
		foldFrom = len(messages)
	}

	// ---- Clamp the boundary to a legal live-window start -----------------
	//
	// Never start the live window on a `tool` row: its parent assistant row
	// (the tool_calls side of the pair) would sit in the fold set, leaving an
	// orphan tool result at the head of the request — a hard provider error for
	// strict providers, not a warning.
	firstKeptIdx := foldFrom
	for firstKeptIdx < len(messages) && IsCheckpointMessage(messages[firstKeptIdx]) {
		firstKeptIdx++
	}
	for firstKeptIdx < len(messages) && messages[firstKeptIdx].Role == "tool" {
		firstKeptIdx++
	}

	// The live tail must stay anchored on a USER row. A long tool chain that
	// pushes the user's actual ask into the summary leaves a pure
	// assistant/tool wall in the live tail: every budgeting pass then has no
	// user anchor and the model loses the thread. The newest user row is the
	// floor — the fold never crosses it.
	if lu := lastUserIdx(messages); lu <= 0 {
		// No user row at all (or the very first row is the only one): there is
		// no safe live window, so nothing may be folded.
		return 0, nil
	} else if firstKeptIdx > lu {
		firstKeptIdx = lu
	}
	if firstKeptIdx >= len(messages) || firstKeptIdx <= 0 {
		// Folding anything would empty the live window.
		return 0, nil
	}

	// ---- RENDER SET = FOLD SET = the reach of the UPDATE, ONE set ---------
	//
	// Every compactable row strictly before the boundary goes into the summary
	// input, in chronological order, and is the ONLY thing the fold can mark.
	// Rows already folded by an earlier pass are skipped: the caller's slice
	// (includeCompacted) carries them, the UPDATE below can only touch
	// compacted = 0 rows, and rendering them again would duplicate history the
	// stored roll-up already represents (that re-inflation is what makes
	// compaction never converge). Checkpoint rows are skipped: the pair is the
	// summary carrier, not a fold target.
	var folded []utils.Message
	for i := 0; i < firstKeptIdx; i++ {
		m := messages[i]
		if m.Compacted || IsCheckpointMessage(m) {
			continue
		}
		folded = append(folded, m)
	}
	if len(folded) == 0 {
		return 0, nil
	}

	// ---- Locate the live checkpoint pair (if any) ------------------------
	oldToolID, priorSummary, priorRawContent := findLiveCheckpoint(ctx, d, conversationID)

	// ---- Compose the summary: prior VERBATIM + this fold's excerpt --------
	summary := composeDurableSummary(machineSummary, priorSummary, folded)
	if summary == "" {
		return 0, nil
	}

	// ---- NO-GROWTH GUARD: a byte-identical roll-up is not a write --------
	//
	// The recovery fold recomputes the roll-up on the LLM view the caller just
	// reloaded, so the same rows can be rendered twice. composeDurableSummary is
	// a PURE function of (priorSummary, folded) — no clock, no random, no LLM in
	// the storage layer — so when the composition comes back byte-for-byte equal
	// to what the pair already stores, there is nothing new to represent, and the
	// UPDATE below would be pure damage: the pair is the first content of the
	// prompt after the system block, so rewriting the same bytes still touches the
	// row and collapses backend KV prefix reuse to the system block (the observed
	// trace: cache_read 26048 -> 16384, flat for 20 turns).
	//
	// Pure byte comparison of the exact column payload the write would put in the
	// tool half — no hashing, no new column, no clock. Returns 0: callers already
	// treat 0 rows as "no durable fold" and bound the request in memory, leaving
	// the pair untouched (see ai/ultracaveDurableFoldForLoop).
	if oldToolID != 0 {
		newContent := marshalMessageContent(utils.NewTextContent(summary))
		if derefContent(newContent) == derefContent(priorRawContent) {
			utils.Log("[DurableCompact] composed roll-up is byte-identical to the stored checkpoint pair (%d chars, conv=%s) — skipping the write, 0 rows folded (the prompt head stays byte-identical)",
				len(summary), conversationID)
			return 0, nil
		}
	}

	// ---- Resolve the fold set to DB row ids ------------------------------
	//
	// The slice is the LLM-facing view, which exposes the DB row id in msg.ID,
	// so the set resolves by STABLE IDENTITY. A positional OFFSET fallback is
	// exactly what diverged from the slice after prior compactions (the
	// observed "durable removed 0"), so rows with no id are skipped instead.
	foldIDs := foldSetRowIDs(messages, folded)
	if len(foldIDs) == 0 {
		utils.Log("[DurableCompact] fold set of %d rows resolves to 0 DB row ids (boundary=%d sliceLen=%d conv=%s) — leaving the checkpoint pair untouched (history NOT shrinking)",
			len(folded), firstKeptIdx, len(messages), conversationID)
		return 0, nil
	}

	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	ph := strings.Repeat("?,", len(foldIDs))
	ph = ph[:len(ph)-1]
	args := make([]interface{}, 0, len(foldIDs)+1)
	args = append(args, conversationID)
	for _, id := range foldIDs {
		args = append(args, id)
	}
	// `compacted = 0` makes re-marking an earlier fold's rows impossible by
	// construction, and the explicit id list means no row the summary does not
	// render can be removed from any future request.
	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET compacted = 1
		 WHERE conversation_id = ? AND compacted = 0 AND id IN (`+ph+`)`, args...)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if removed == 0 {
		// Nothing was folded, so there is nothing new to represent — and moving
		// the pair would be pure damage (prompt-head rewrite, KV prefix reuse
		// collapse). Roll the transaction back instead of churning the head.
		utils.Log("[DurableCompact] fold set of %d ids matched 0 compactable rows (boundary=%d sliceLen=%d conv=%s) — leaving the checkpoint pair untouched (history NOT shrinking)",
			len(foldIDs), firstKeptIdx, len(messages), conversationID)
		return 0, nil
	}

	if oldToolID == 0 {
		// No pair to rewrite: mint the ONE pair this conversation will ever
		// have. The slot is MIN(seq) - 2 over EVERY row (compacted included):
		// rows are never renumbered, so the two seqs immediately below the
		// current minimum are free by definition, and they place the pair FIRST
		// in the seq-ordered LLM view — the invariant every consumer relies on.
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

	// Rewrite ONLY the content column of the existing tool half: same row id,
	// same seq, same tool_call_id, same assistant half, no compacted flag
	// touched, no other row touched. This is the one column that changed.
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

// findLiveCheckpoint returns the row id of the LIVE checkpoint tool half (the
// row carrying the summary text), the summary text it stores, and the RAW
// content-column payload of that row (the exact bytes a rewrite would replace),
// or (0, "", nil) when the conversation has no live pair. `compacted = 0` on
// both halves: a superseded pair must never be revived or rewritten.
//
// The raw payload is what the no-growth guard compares against: comparing the
// serialized column (not just the decoded text) is what makes the comparison the
// exact claim "this UPDATE would write the bytes that are already there".
func findLiveCheckpoint(ctx context.Context, d *sql.DB, conversationID string) (toolRowID int64, summary string, rawContent *string) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, tool_calls FROM messages
		 WHERE conversation_id = ? AND role = 'assistant' AND compacted = 0 AND tool_calls LIKE ?
		 ORDER BY seq ASC`, conversationID, "%"+EcoCheckpointToolName+"%")
	if err != nil {
		return 0, "", nil
	}
	var toolCallID string
	for rows.Next() {
		var id int64
		var tcJSON *string
		if err := rows.Scan(&id, &tcJSON); err != nil {
			break
		}
		for _, tc := range unmarshalToolCalls(tcJSON) {
			if tc.Function.Name == EcoCheckpointToolName {
				toolCallID = tc.ID
				break
			}
		}
		if toolCallID != "" {
			break
		}
	}
	rows.Close()
	if toolCallID == "" {
		return 0, "", nil
	}
	var contentJSON *string
	err = d.QueryRowContext(ctx,
		`SELECT id, content FROM messages
		 WHERE conversation_id = ? AND role = 'tool' AND tool_call_id = ? AND compacted = 0
		 LIMIT 1`, conversationID, toolCallID).Scan(&toolRowID, &contentJSON)
	if err != nil {
		// An assistant half with no live tool half is a half-pair this fold
		// path cannot represent: treat it as no pair.
		return 0, "", nil
	}
	return toolRowID, unmarshalMessageContent(contentJSON).TextContent(), contentJSON
}

// derefContent flattens a content-column payload for the byte comparison
// (nil — an empty/zero MessageContent — compares as "").
func derefContent(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// lastUserIdx returns the index of the newest non-checkpoint user message, the
// floor that durable compaction never folds past. Returns 0 when the
// conversation has no user row (or its only user row is the first row), which
// the caller treats as "nothing can be folded".
func lastUserIdx(messages []utils.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && !IsCheckpointMessage(messages[i]) {
			return i
		}
	}
	return 0
}

// maxDurableSummaryChars bounds the stored checkpoint summary. The bound is a
// constant, so the write cannot grow the roll-up without limit and re-inflate
// the request that the caller just measured as fitting.
const maxDurableSummaryChars = 100_000

// minDurableExcerptChars is the floor of the room left for the NEW excerpt
// after the prior roll-up is paid for. Without a floor, a prior summary near
// half the budget leaves almost nothing, and the newest folded rows are
// rendered down to a stub.
const minDurableExcerptChars = 8_000

// MaxDurableSummaryChars exposes the storage bound so the CALLER side (the ai
// fold path) caps the excerpt it renders at the SAME number the storage layer
// truncates at. Two different caps fighting each other is how a fold ends up
// rendering an excerpt the storage then slices mid-sentence without the
// renderer's own truncation marker ever appearing.
const MaxDurableSummaryChars = maxDurableSummaryChars

// priorSummaryBanner is the LEGACY head marker older builds wrote. It is
// stripped, never written: a stored summary carries exactly ONE banner, so
// generations of nesting cannot accumulate.
const priorSummaryBanner = "PRIOR CHECKPOINT:"

// newerHistoryBanner is the single section marker separating the carried-
// forward roll-up from the newest folded excerpt. Exported so the ai layer can
// reference the SAME marker in the summarisation instruction it sends (the
// model is told not to re-summarise what that section already covers) — one
// string, two layers, no drift.
const NewerHistoryBanner = "[newer history, machine-compacted]"

// newerHistoryBanner is the package-internal spelling of the marker.
const newerHistoryBanner = NewerHistoryBanner

// chainCompressionMarker announces the escape valve: the prior roll-up did not
// fit next to the new section, so it was compressed by the registered chain
// compressor (megacave) rather than dropped. ONE marker, emitted once, and it
// MUST say what happened, because a roll-up that silently shrank is a
// "history vanished with no trace" defect the model cannot see.
const chainCompressionMarker = "[prior roll-up megacaved down to fit the durable summary bound: "

// chainCompressorSlack is the head-room reserved on top of the compressor's
// room, so the bound holds even when a compressor's announcing marker is longer
// than the compressor's own estimate of it. 128 chars buys every realistic
// "compressed N chars -> M chars" marker.
const chainCompressorSlack = 128

// summaryChainCompressor is the escape valve, injected by the ai layer (ai
// imports db, so db cannot import the megacave renderer itself). It compresses
// a roll-up text into at most maxChars chars and MUST announce the compression
// inside its own output. nil = no compressor registered: the outer backstop
// (boundDurableExcerpt) keeps the bound, with its own marker, so the bound is
// never met by dropping content silently.
var (
	chainMu           sync.RWMutex
	chainCompressorFn func(text string, maxChars int) string
)

// SetSummaryChainCompressor registers the roll-up chain compressor. Called
// once from the ai package's init; tests inject a deterministic stand-in.
func SetSummaryChainCompressor(fn func(text string, maxChars int) string) {
	chainMu.Lock()
	defer chainMu.Unlock()
	chainCompressorFn = fn
}

// summaryChainCompressor returns the registered compressor, or nil.
func summaryChainCompressor() func(text string, maxChars int) string {
	chainMu.RLock()
	defer chainMu.RUnlock()
	return chainCompressorFn
}

// truncatedExcerptBanner announces that the stored excerpt was CUT to
// maxDurableSummaryChars. ONE marker, appended, never nested: the reader of the
// roll-up must be able to see that history was dropped instead of discovering
// it by the text ending mid-sentence.
const truncatedExcerptBanner = "[... durable summary truncated at "

// composeDurableSummary builds the stored summary: the carried-forward prior
// roll-up VERBATIM, then ONE separator, then this fold's excerpt.
//
// The prior text is carried, never re-rendered — it is the only representation
// of every row earlier folds marked compacted, so re-summarising it would
// destroy that history — and it is bounded so the roll-up cannot grow without
// limit. Banners are stripped from the prior text and re-emitted once, so the
// stored summary carries exactly one occurrence of any banner no matter how
// many folds have run (nested banner generations were what moved the head of
// the prompt every turn).
//
// The output is a pure function of (priorSummary, folded): no clock, no
// random, no LLM. Repeated folds of the same input store byte-identical text.
func composeDurableSummary(machineSummary func([]utils.Message) string, priorSummary string, folded []utils.Message) string {
	excerpt := ""
	if machineSummary != nil {
		excerpt = strings.TrimSpace(machineSummary(folded))
		if excerpt == "" {
			utils.Log("[DurableCompact] machine excerpt empty — using mechanical fallback")
		}
	}
	if excerpt == "" {
		var b strings.Builder
		writeDurableSummaryText(&b, folded)
		excerpt = strings.TrimSpace(b.String())
		if excerpt == "" {
			return ""
		}
		excerpt = boundDurableExcerpt(excerpt)
		excerpt = "[compacted by Plurality — machine excerpt unavailable; raw excerpt follows]\n" + excerpt
	} else {
		// THE SUCCESS PATH IS BOUNDED TOO. The fallback path truncated the excerpt
		// and the success path did not, so one fold stored a 1,444,347-char roll-up
		// (~481k tokens) measured on a real 1467-row conversation. The stored
		// roll-up is what re-inflates the NEXT reload: an unbounded roll-up keeps
		// the view above the eco trigger, so compaction fires again immediately and
		// rewrites the prompt head every turn — the checkpoint pair is the first
		// content after the system block, so every rewrite collapses backend KV
		// prefix reuse to the system block and it never accumulates. The renderer
		// already honours a caller-supplied ceiling
		// (ai.MachineExcerptMegacaveCapped via eco.durableFold); this is the outer
		// backstop that keeps that promise true for ANY machineSummary, at the same
		// number the caller was asked to respect.
		excerpt = boundDurableExcerpt(excerpt)
	}
	// THE CHAIN: the prior roll-up is the accumulated record of every row
	// earlier folds marked compacted, carried forward VERBATIM (never
	// re-summarised — an LLM pass over it would destroy the one representation
	// those rows have left). Banners are stripped and re-emitted once, so
	// generations never nest.
	prior := stripSummaryBanners(priorSummary)
	if prior == "" {
		return strings.TrimSpace(excerpt)
	}

	// Cost the composition must reserve on top of the two texts.
	const markerSlack = 64 // digits inside the escape-valve marker
	overhead := len(newerHistoryBanner) + 4

	// APPEND-ONLY, ONCE THE CHAIN EXISTS. The prior already carries the ONE
	// section banner, sitting at the boundary of the FIRST append. Stripping it
	// here and re-emitting it in front of THIS fold's excerpt moves every byte
	// between the old and the new boundary — the bytes before the append point
	// are no longer a byte-for-byte prefix, and the pair is the first content of
	// the prompt, so the KV prefix cache diverges there every single turn.
	// A chain that has a banner is therefore EXTENDED at the tail and touched
	// nowhere else: banner count stays exactly 1 (the chain never nests) and the
	// stored text is a pure append of the newest section.
	if strings.Contains(priorSummary, newerHistoryBanner) {
		if len(priorSummary)+len(excerpt)+2 <= maxDurableSummaryChars {
			return strings.TrimSpace(priorSummary + "\n" + excerpt)
		}
	} else if len(prior)+len(excerpt)+overhead <= maxDurableSummaryChars {
		// It FITS onto a chain with NO banner yet (a first append, or a legacy
		// prior): carry the prior verbatim, emit the ONE section banner, append.
		return strings.TrimSpace(prior + "\n\n" + newerHistoryBanner + "\n" + excerpt)
	}

	// THE ESCAPE VALVE. The total does not fit, and dropping content silently is
	// not allowed. The PRIOR is what gets compressed (megacaved by the
	// registered chain compressor — the deterministic lossy-compress operation),
	// never the newest section: the newest folded rows are the freshest facts the
	// live model is about to lose, and the prior has already survived one
	// compaction pass.
	markerCost := len(chainCompressionMarker) + markerSlack + 2
	priorRoom := maxDurableSummaryChars - len(excerpt) - overhead - markerCost
	if priorRoom < minPriorRollupChars {
		// The new section is fat: give the prior a floor and bound the excerpt
		// to what is left, announcing the truncation (boundDurableExcerpt keeps
		// the TAIL, i.e. the newest folded rows, and names the drop).
		priorRoom = minPriorRollupChars
		excerpt = boundDurableExcerpt(excerpt, maxDurableSummaryChars-overhead-priorRoom-markerCost)
	}
	if compressed := compressPriorRollup(prior, priorRoom); compressed != "" {
		marker := chainCompressionMarker + strconv.Itoa(len(prior)) + " chars -> " +
			strconv.Itoa(len(compressed)) + " chars, nothing dropped silently]"
		combined := strings.TrimSpace(marker + "\n" + compressed + "\n\n" + newerHistoryBanner + "\n" + excerpt)
		if len(combined) <= maxDurableSummaryChars {
			return combined
		}
		// The compressor overshot its room: re-run it at the exact remainder so
		// the bound holds with the marker in place. One retry is enough because
		// the second room is strictly smaller than the first.
		retry := priorRoom - (len(combined) - maxDurableSummaryChars)
		if again := compressPriorRollup(prior, retry); again != "" {
			marker = chainCompressionMarker + strconv.Itoa(len(prior)) + " chars -> " +
				strconv.Itoa(len(again)) + " chars, nothing dropped silently]"
			combined = strings.TrimSpace(marker + "\n" + again + "\n\n" + newerHistoryBanner + "\n" + excerpt)
			if len(combined) <= maxDurableSummaryChars {
				return combined
			}
		}
	}

	// NO COMPRESSOR REGISTERED (a db-only build/test), or the compressor
	// produced nothing: keep the bound with the outer backstop, keeping the
	// prior's TAIL and ANNOUNCING the compression. The newest section is
	// preserved in full: the prior is the thing that shrinks.
	room := maxDurableSummaryChars - len(excerpt) - overhead - markerCost
	if room < minPriorRollupChars {
		room = minPriorRollupChars
		excerpt = boundDurableExcerpt(excerpt, maxDurableSummaryChars-overhead-room)
	}
	prior = boundDurableExcerpt(prior, room)
	return strings.TrimSpace(chainCompressionMarker + strconv.Itoa(len(priorSummary)) +
		" chars -> " + strconv.Itoa(len(prior)) + " chars (no chain compressor registered)]\n" +
		prior + "\n\n" + newerHistoryBanner + "\n" + excerpt)
}

// minPriorRollupChars is the floor of the room left for the PRIOR roll-up when
// the new section is fat. Without a floor, a huge new excerpt squeezes the
// chain to nothing and every row an earlier fold compacted loses its only
// representation.
const minPriorRollupChars = 8_000

// compressPriorRollup runs the prior roll-up through the registered chain
// compressor at `room` chars. Returns "" when no compressor is registered, the
// room is unusable, or the compressor produced nothing (the caller then falls
// back to the truncating backstop, which announces what it did).
func compressPriorRollup(prior string, room int) string {
	fn := summaryChainCompressor()
	if fn == nil || room <= 0 {
		return ""
	}
	out := strings.TrimSpace(fn(prior, room))
	if out == "" || len(out) > room {
		// A compressor that ignores its room is not usable for the bound: the
		// caller's retry math assumes `room` is respected.
		return ""
	}
	return out
}

// boundDurableExcerpt caps this fold's excerpt to maxDurableSummaryChars on a
// rune boundary and appends ONE marker naming the original size, so a truncated
// roll-up announces itself. Truncating at the END (keeping the head) mirrors the
// excerpt's own chronological order: the oldest folded rows are what the next
// reload reads first.
func boundDurableExcerpt(excerpt string, maxChars ...int) string {
	limit := maxDurableSummaryChars
	if len(maxChars) > 0 && maxChars[0] > 0 {
		limit = maxChars[0]
	}
	if len(excerpt) <= limit {
		return excerpt
	}
	marker := truncatedExcerptBanner + strconv.Itoa(len(excerpt)) + " chars ...]"
	keep := limit - len(marker) - 1
	if keep < 0 {
		return marker
	}
	// Keep the TAIL: the excerpt is chronological, so the head is the OLDEST
	// folded rows. Keeping the head preserves a conversation's first hours and
	// discards its most recent work — invisible to the model, which reads a
	// summary that appears to end early. The marker names what was dropped.
	return marker + "\n" + textsafe.TailBytes(excerpt, keep)
}

// stripSummaryBanners removes every occurrence of the summary banners, keeping
// the text around them. Collapsing banner generations is what keeps the stored
// summary at exactly one banner and keeps the roll-up's content verbatim.
func stripSummaryBanners(s string) string {
	s = strings.TrimSpace(s)
	for _, banner := range []string{priorSummaryBanner, "NEW MESSAGES:", newerHistoryBanner} {
		for {
			i := strings.Index(s, banner)
			if i < 0 {
				break
			}
			head := strings.TrimSpace(s[:i])
			tail := strings.TrimSpace(strings.TrimLeft(s[i+len(banner):], "\n"))
			switch {
			case head == "":
				s = tail
			case tail == "":
				s = head
			default:
				s = head + "\n" + tail
			}
		}
	}
	return strings.TrimSpace(s)
}

// insertCheckpointPairInTx writes a fresh [assistant, tool] checkpoint pair at
// seq/seq+1 inside the fold transaction. Used ONLY when the conversation has
// no pair yet; every later fold rewrites the existing pair in place, so the
// checkpoint row count stays 2 across any number of folds.
func insertCheckpointPairInTx(ctx context.Context, tx *sql.Tx, conversationID string, seq int64, summary string) error {
	_ = ctx // insertMessage is tx-scoped and takes no context on this branch
	toolCallID := "eco_" + GenerateID()
	now := time.Now().Format(time.RFC3339)
	assistantMsg := utils.Message{
		Role:      "assistant",
		Content:   utils.MessageContent{},
		Timestamp: now,
		ToolCalls: []utils.ToolCall{{
			ID:       toolCallID,
			Type:     "function",
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
	if _, err := insertMessage(tx, conversationID, seq+1, toolMsg); err != nil {
		return err
	}
	return nil
}

// foldSetRowIDs maps the fold set (the rows the stored summary renders) to
// their DB row ids, in the slice's chronological order. Rows without a DB id
// are skipped — they cannot be addressed, and a positional fallback is exactly
// the arithmetic that diverged from the slice after prior compactions.
//
// The two slices are matched by IDENTITY, not by position: `messages` is the
// caller's slice, `folded` is the subset this fold selected (it excludes the
// checkpoint pair and any already-compacted row). A positional zip drifts the
// moment those two views differ, which is the bug class this whole function
// exists to kill.
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
		// No stable id (legacy / synthetic row): match the earliest unconsumed
		// row with identical identity fields, so the fold set stays exact.
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

// writeDurableSummaryText renders a message slice into the mechanical fallback
// excerpt: plain chronological text, no clock, no model, byte-stable.
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
