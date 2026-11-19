package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// --- frozen-compaction test harness ----------------------------------------
//
// Every test drives the REAL storage path: rows are written with insertMessage
// (the same choke point PushMessage and the tool loop use), the fold is
// CompactConversationDurable, and the views are read back through
// GetConversationById (client) / GetConversationByIdInternal (LLM).

// openFrozenDB registers a per-user file DB in the package cache and returns
// it, mirroring what GetUserDB does in production (MaxOpenConns(1), WAL).
func openFrozenDB(t *testing.T, userID string) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	oldPath := userDataPath
	userDataPath = dir
	t.Cleanup(func() { userDataPath = oldPath })

	os.MkdirAll(filepath.Join(dir, userID), 0755)
	db, err := sql.Open("sqlite3", filepath.Join(dir, userID, "conversations.db")+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	userDBs.Store(userID, db)
	t.Cleanup(func() { userDBs.Delete(userID); db.Close() })
	return db
}

func frozenCtx(userID string) context.Context {
	return context.WithValue(context.Background(), "userID", userID)
}

func seedConv(t *testing.T, db *sql.DB, convID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, last_message_at) VALUES (?, 'frozen', '2026-01-01T00:00:00Z')`,
		convID); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
}

// appendRows inserts msgs at the next free seqs and returns them the way the
// loader exposes them (ID = DB row id as a string).
func appendRows(t *testing.T, db *sql.DB, convID string, msgs ...utils.Message) []utils.Message {
	t.Helper()
	var maxSeq sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(seq) FROM messages WHERE conversation_id = ?`, convID).Scan(&maxSeq); err != nil {
		t.Fatalf("maxseq: %v", err)
	}
	next := int64(0)
	if maxSeq.Valid {
		next = maxSeq.Int64 + 1
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	out := make([]utils.Message, 0, len(msgs))
	for i, m := range msgs {
		id, err := insertMessage(tx, convID, next+int64(i), m)
		if err != nil {
			tx.Rollback()
			t.Fatalf("insert: %v", err)
		}
		m.ID = fmt.Sprintf("%d", id)
		out = append(out, m)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return out
}

func userMsg(text string) utils.Message {
	return utils.Message{Role: "user", Content: utils.NewTextContent(text)}
}

func asstMsg(text string, toolCallID, toolName string) utils.Message {
	m := utils.Message{Role: "assistant", Content: utils.NewTextContent(text)}
	if toolCallID != "" {
		m.ToolCalls = []utils.ToolCall{{
			ID:       toolCallID,
			Type:     "function",
			Function: utils.FunctionCall{Name: toolName, Arguments: "{}"},
		}}
	}
	return m
}

func toolMsg(name, text, callID string) utils.Message {
	return utils.Message{
		Role:       "tool",
		Name:       name,
		Content:    utils.NewTextContent(text),
		ToolCallID: callID,
	}
}

// compactedRowIDs returns the DB row ids currently marked compacted = 1.
func compactedRowIDs(t *testing.T, db *sql.DB, convID string) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM messages WHERE conversation_id = ? AND compacted = 1 ORDER BY id`, convID)
	if err != nil {
		t.Fatalf("query compacted: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

// checkpointRows locates the checkpoint pair rows (id, seq, role) plus the
// summary text the tool half carries.
type frozenPair struct {
	AssistantID  int64
	AssistantSeq int64
	ToolID       int64
	ToolSeq      int64
	ToolCallID   string
	Summary      string
	RawAssistant string
	RawTool      string
	Count        int
}

func checkpointRows(t *testing.T, db *sql.DB, convID string) frozenPair {
	t.Helper()
	rows, err := db.Query(
		`SELECT id, seq, role, tool_calls, tool_call_id, name, content, compacted
		 FROM messages WHERE conversation_id = ? ORDER BY seq`, convID)
	if err != nil {
		t.Fatalf("query pair: %v", err)
	}
	defer rows.Close()
	var p frozenPair
	for rows.Next() {
		var id, seq int64
		var role string
		var tcJSON, callID, name, content *string
		var compacted int
		if err := rows.Scan(&id, &seq, &role, &tcJSON, &callID, &name, &content, &compacted); err != nil {
			t.Fatalf("scan pair: %v", err)
		}
		var m utils.Message
		m.Role = role
		m.ToolCalls = unmarshalToolCalls(tcJSON)
		m.Content = unmarshalMessageContent(content)
		if callID != nil {
			m.ToolCallID = *callID
		}
		if name != nil {
			m.Name = *name
		}
		if !IsCheckpointMessage(m) {
			continue
		}
		if compacted != 0 {
			t.Fatalf("checkpoint row id=%d seq=%d marked compacted — a fold must never fold the pair", id, seq)
		}
		p.Count++
		switch role {
		case "assistant":
			p.AssistantID, p.AssistantSeq, p.RawAssistant = id, seq, derefStr(tcJSON)
			for _, tc := range m.ToolCalls {
				p.ToolCallID = tc.ID
			}
		case "tool":
			p.ToolID, p.ToolSeq, p.RawTool = id, seq, derefStr(content)
			p.Summary = m.Content.TextContent()
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return p
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// seqByRowID snapshots the seq of every row, so tests can assert the fold
// never renumbers a live row (the seq = -(seq+2) renumbering shape).
func seqByRowID(t *testing.T, db *sql.DB, convID string) map[int64]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id, seq FROM messages WHERE conversation_id = ?`, convID)
	if err != nil {
		t.Fatalf("query seqs: %v", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = seq
	}
	return out
}

func idSet(ids []int64) map[int64]bool {
	s := make(map[int64]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestDurableFrozenFoldMarksOnlyRenderedRows is the row-exact contract: the set of
// rows the summary renders is EXACTLY the set the fold marks compacted.
//
// The slice handed to the fold deliberately OMITS one row that sits below the
// boundary (the staged LLM view drops rows to fit the context window, so slice
// index and seq do not correspond). The legacy seq-range write
// (`UPDATE ... WHERE seq < boundarySeq`) marks that unrendered row too — an
// annihilation: a row removed from every future request with no representation
// anywhere. Marking by ID set cannot.
func TestDurableFrozenFoldMarksOnlyRenderedRows(t *testing.T) {
	const user = "fz_rows"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_rows"
	seedConv(t, db, convID)

	rows := appendRows(t, db, convID,
		userMsg("U1"),
		asstMsg("A1", "call_1", "shell_exec"),
		toolMsg("shell_exec", "T1 out", "call_1"),
		asstMsg("A2", "call_2", "shell_exec"), // <- the row the slice omits
		toolMsg("shell_exec", "T2 out", "call_2"),
		userMsg("U2"),
		asstMsg("A3", "", ""),
		userMsg("U3"),
	)

	// Slice the caller would pass: the LLM view, WITHOUT seq3 (row index 3).
	var slice []utils.Message
	for i, m := range rows {
		if i == 3 {
			continue
		}
		slice = append(slice, m)
	}
	unrenderedID := dbRowID(rows[3].ID)

	var rendered []int64
	machine := func(msgs []utils.Message) string {
		for _, m := range msgs {
			rendered = append(rendered, dbRowID(m.ID))
		}
		var b strings.Builder
		writeDurableSummaryText(&b, msgs)
		return b.String()
	}

	folded, err := CompactConversationDurable(ctx, convID, slice, 4, machine)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded != len(rendered) {
		t.Fatalf("folded=%d, summary rendered=%d — the fold must mark exactly what it renders", folded, len(rendered))
	}

	marked := compactedRowIDs(t, db, convID)
	if len(marked) != len(rendered) {
		t.Fatalf("marked %d rows, summary renders %d", len(marked), len(rendered))
	}
	renderedSet, markedSet := idSet(rendered), idSet(marked)
	for id := range renderedSet {
		if !markedSet[id] {
			t.Fatalf("row %d was rendered into the summary but not folded", id)
		}
	}
	for id := range markedSet {
		if !renderedSet[id] {
			t.Fatalf("row %d was folded WITHOUT being rendered — summary-less history loss", id)
		}
	}
	if markedSet[unrenderedID] {
		t.Fatalf("the row the summary never renders (id=%d) was marked compacted — seq-range shape", unrenderedID)
	}
	// Everything at or after the live boundary stays live.
	for _, m := range slice[4:] {
		if markedSet[dbRowID(m.ID)] {
			t.Fatalf("live-tail row %d was folded", dbRowID(m.ID))
		}
	}
}

// TestDurableFrozenFoldKeepsExactlyOnePairAcrossFolds: 3 successive folds must leave
// the conversation with exactly TWO checkpoint rows, and the SAME pair — same
// row ids, same seqs, same tool_call_id, same assistant half — every time.
// Pair accumulation (16 pairs on the live conversation) and pair re-minting
// (prompt-head rewrite, KV prefix reuse collapse) are both killed here.
func TestDurableFrozenFoldKeepsExactlyOnePairAcrossFolds(t *testing.T) {
	const user = "fz_pair"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_pair"
	seedConv(t, db, convID)

	appendRows(t, db, convID,
		userMsg("U1"),
		asstMsg("A1", "call_1", "shell_exec"),
		toolMsg("shell_exec", "T1", "call_1"),
		userMsg("U2"),
		asstMsg("A2", "call_2", "shell_exec"),
		toolMsg("shell_exec", "T2", "call_2"),
		userMsg("U3"),
	)

	machine := func(msgs []utils.Message) string {
		var b strings.Builder
		writeDurableSummaryText(&b, msgs)
		return b.String()
	}

	var first frozenPair
	for round := 0; round < 3; round++ {
		if round > 0 {
			appendRows(t, db, convID,
				asstMsg(fmt.Sprintf("A after %d", round), fmt.Sprintf("call_r%d", round), "shell_exec"),
				toolMsg("shell_exec", fmt.Sprintf("T after %d", round), fmt.Sprintf("call_r%d", round)),
				userMsg(fmt.Sprintf("U turn %d", round)),
			)
		}
		conv, err := GetConversationByIdInternal(ctx, convID)
		if err != nil {
			t.Fatalf("round %d load: %v", round, err)
		}
		boundary := lastUserIdx(conv.Messages)
		if boundary <= 0 {
			t.Fatalf("round %d: no user anchor in %d rows", round, len(conv.Messages))
		}
		folded, err := CompactConversationDurable(ctx, convID, conv.Messages, boundary, machine)
		if err != nil {
			t.Fatalf("round %d fold: %v", round, err)
		}
		if folded == 0 {
			t.Fatalf("round %d: fold removed 0 rows, expected progress", round)
		}
		p := checkpointRows(t, db, convID)
		if p.Count != 2 {
			t.Fatalf("round %d: %d checkpoint rows, expected exactly 2 (pair accumulation)", round, p.Count)
		}
		if p.Summary == "" {
			t.Fatalf("round %d: pair carries no summary", round)
		}
		if round == 0 {
			first = p
			continue
		}
		if p.AssistantID != first.AssistantID || p.ToolID != first.ToolID {
			t.Fatalf("round %d: pair rows changed (%d/%d -> %d/%d) — folds must rewrite the pair in place",
				round, first.AssistantID, first.ToolID, p.AssistantID, p.ToolID)
		}
		if p.AssistantSeq != first.AssistantSeq || p.ToolSeq != first.ToolSeq {
			t.Fatalf("round %d: pair seqs moved (%d/%d -> %d/%d)",
				round, first.AssistantSeq, first.ToolSeq, p.AssistantSeq, p.ToolSeq)
		}
		if p.ToolCallID != first.ToolCallID {
			t.Fatalf("round %d: tool_call_id changed (%s -> %s) — prompt-head churn",
				round, first.ToolCallID, p.ToolCallID)
		}
		if p.RawAssistant != first.RawAssistant {
			t.Fatalf("round %d: the assistant half was rewritten", round)
		}
	}
}

// TestDurableFrozenFoldNoRenumberingOfLiveRows: the fold never touches a live row's
// seq. The legacy shape shifted every live row at or after the boundary by +2
// via `seq = -(seq+2)`.
func TestDurableFrozenFoldNoRenumberingOfLiveRows(t *testing.T) {
	const user = "fz_seq"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_seq"
	seedConv(t, db, convID)

	appendRows(t, db, convID,
		userMsg("U1"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
		userMsg("U2"), asstMsg("A2", "call_2", "shell_exec"), toolMsg("shell_exec", "T2", "call_2"),
		userMsg("U3"),
	)
	before := seqByRowID(t, db, convID)

	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := CompactConversationDurable(ctx, convID, conv.Messages, lastUserIdx(conv.Messages), nil); err != nil {
		t.Fatalf("fold: %v", err)
	}
	after := seqByRowID(t, db, convID)
	for id, seq := range before {
		if after[id] != seq {
			t.Fatalf("row %d renumbered %d -> %d", id, seq, after[id])
		}
	}
	// The single minted pair sits immediately below the old minimum: MIN(seq)-2.
	minOld := int64(0)
	for _, s := range before {
		if s < minOld {
			minOld = s
		}
	}
	p := checkpointRows(t, db, convID)
	if p.AssistantSeq != minOld-2 || p.ToolSeq != minOld-1 {
		t.Fatalf("pair minted at %d/%d, expected %d/%d", p.AssistantSeq, p.ToolSeq, minOld-2, minOld-1)
	}
}

// TestDurableFrozenFoldViews: folded rows stay visible to the CLIENT view (with
// Compacted set) and invisible to the LLM view. Compaction shapes requests, it
// never removes the user's history.
func TestDurableFrozenFoldViews(t *testing.T) {
	const user = "fz_views"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_views"
	seedConv(t, db, convID)

	rows := appendRows(t, db, convID,
		userMsg("U1 keep-me-visible"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
		userMsg("U2 keep-me-visible"), asstMsg("A2", "call_2", "shell_exec"), toolMsg("shell_exec", "T2", "call_2"),
		userMsg("U3 live anchor"),
	)
	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	folded, err := CompactConversationDurable(ctx, convID, conv.Messages, lastUserIdx(conv.Messages), nil)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatalf("expected progress")
	}

	client, err := GetConversationById(ctx, convID)
	if err != nil {
		t.Fatalf("client load: %v", err)
	}
	byID := map[string]utils.Message{}
	for _, m := range client.Messages {
		byID[m.ID] = m
		if IsCheckpointMessage(m) {
			t.Fatalf("client view leaked the synthetic checkpoint pair")
		}
	}
	if len(client.Messages) != len(rows) {
		t.Fatalf("client view has %d rows, expected the full %d (compaction deleted history)", len(client.Messages), len(rows))
	}
	var foldedSeen, liveSeen int
	for _, m := range rows {
		cm, ok := byID[m.ID]
		if !ok {
			t.Fatalf("client view lost row %s", m.ID)
		}
		if dbRowID(m.ID) <= 0 {
			continue
		}
		if cm.Compacted {
			foldedSeen++
		} else {
			liveSeen++
		}
	}
	if foldedSeen != folded {
		t.Fatalf("client view flags %d folded rows, fold removed %d", foldedSeen, folded)
	}
	if liveSeen == 0 {
		t.Fatalf("client view flags no live rows")
	}

	llm, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("llm load: %v", err)
	}
	for _, m := range llm.Messages {
		if m.Compacted {
			t.Fatalf("LLM view served a folded row (id=%s)", m.ID)
		}
	}
	if len(llm.Messages) != len(rows)-folded+2 {
		t.Fatalf("LLM view has %d rows, expected %d (live tail + the checkpoint pair)",
			len(llm.Messages), len(rows)-folded+2)
	}
	if !IsCheckpointMessage(llm.Messages[0]) || !IsCheckpointMessage(llm.Messages[1]) {
		t.Fatalf("the checkpoint pair is not the first content of the LLM view")
	}
}

// TestDurableFrozenFoldNoProgressLeavesPairByteIdentical: a fold that can remove no
// row must return 0 and rewrite NOTHING. Moving the pair on a no-progress fold
// rewrites the head of every prompt while shrinking nothing — the wedge that
// fired on every message of a live conversation.
func TestDurableFrozenFoldNoProgressLeavesPairByteIdentical(t *testing.T) {
	const user = "fz_nochurn"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_nochurn"
	seedConv(t, db, convID)

	appendRows(t, db, convID,
		userMsg("U1"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
		userMsg("U2"), asstMsg("A2", "call_2", "shell_exec"), toolMsg("shell_exec", "T2", "call_2"),
		userMsg("U3 live anchor"),
	)
	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	boundary := lastUserIdx(conv.Messages)
	if _, err := CompactConversationDurable(ctx, convID, conv.Messages, boundary, nil); err != nil {
		t.Fatalf("first fold: %v", err)
	}
	before := checkpointRows(t, db, convID)
	seqsBefore := seqByRowID(t, db, convID)

	// Same slice again: every compactable row before the boundary is already
	// folded, so this fold can remove 0 rows.
	folded, err := CompactConversationDurable(ctx, convID, conv.Messages, boundary, nil)
	if err != nil {
		t.Fatalf("second fold: %v", err)
	}
	if folded != 0 {
		t.Fatalf("folded=%d, expected 0", folded)
	}
	after := checkpointRows(t, db, convID)
	if after.RawTool != before.RawTool {
		t.Fatalf("no-progress fold rewrote the summary row:\n--- before ---\n%s\n--- after ---\n%s", before.RawTool, after.RawTool)
	}
	if after.RawAssistant != before.RawAssistant || after.AssistantID != before.AssistantID ||
		after.ToolID != before.ToolID || after.AssistantSeq != before.AssistantSeq || after.ToolSeq != before.ToolSeq {
		t.Fatalf("no-progress fold moved the pair: %+v -> %+v", before, after)
	}
	afterSeqs := seqByRowID(t, db, convID)
	if len(afterSeqs) != len(seqsBefore) {
		t.Fatalf("no-progress fold changed the row count: %d -> %d", len(seqsBefore), len(afterSeqs))
	}
	for id, seq := range seqsBefore {
		if afterSeqs[id] != seq {
			t.Fatalf("no-progress fold renumbered row %d: %d -> %d", id, seq, afterSeqs[id])
		}
	}

	// A foldFrom of 0 must be a no-op too: never fold the whole conversation.
	if n, err := CompactConversationDurable(ctx, convID, conv.Messages, 0, nil); err != nil || n != 0 {
		t.Fatalf("foldFrom=0 -> (%d, %v), expected (0, nil)", n, err)
	}
	// ...and with no pair yet: it must not mint one.
	convID2 := "c_nopair"
	seedConv(t, db, convID2)
	appendRows(t, db, convID2, userMsg("U1"), asstMsg("A1", "", ""), userMsg("U2"))
	conv2, err := GetConversationByIdInternal(ctx, convID2)
	if err != nil {
		t.Fatalf("load2: %v", err)
	}
	if n, err := CompactConversationDurable(ctx, convID2, conv2.Messages, 0, nil); err != nil || n != 0 {
		t.Fatalf("foldFrom=0 (no pair) -> (%d, %v), expected (0, nil)", n, err)
	}
	if p := checkpointRows(t, db, convID2); p.Count != 0 {
		t.Fatalf("foldFrom=0 minted %d checkpoint rows", p.Count)
	}
}

// TestDurableFrozenFoldCarriesPriorSummaryVerbatim: the stored summary is the prior
// roll-up VERBATIM plus one section marker plus the new excerpt. The prior text
// is never re-rendered, and banner generations never nest.
func TestDurableFrozenFoldCarriesPriorSummaryVerbatim(t *testing.T) {
	const user = "fz_prior"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_prior"
	seedConv(t, db, convID)

	machine := func(msgs []utils.Message) string {
		var b strings.Builder
		writeDurableSummaryText(&b, msgs)
		return b.String()
	}

	appendRows(t, db, convID,
		userMsg("U1 first ask"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
		userMsg("U2"), asstMsg("A2", "call_2", "shell_exec"), toolMsg("shell_exec", "T2", "call_2"),
		userMsg("U3 anchor"),
	)
	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := CompactConversationDurable(ctx, convID, conv.Messages, lastUserIdx(conv.Messages), machine); err != nil {
		t.Fatalf("fold 1: %v", err)
	}
	s1 := checkpointRows(t, db, convID).Summary
	if !strings.Contains(s1, "U1 first ask") {
		t.Fatalf("fold 1 summary lost the folded text: %q", s1)
	}

	for round := 2; round <= 3; round++ {
		appendRows(t, db, convID,
			asstMsg(fmt.Sprintf("A%d", round), fmt.Sprintf("call_%d", round), "shell_exec"),
			toolMsg("shell_exec", fmt.Sprintf("tool output %d", round), fmt.Sprintf("call_%d", round)),
			userMsg(fmt.Sprintf("U anchor %d", round)),
		)
		conv, err := GetConversationByIdInternal(ctx, convID)
		if err != nil {
			t.Fatalf("load %d: %v", round, err)
		}
		boundary := lastUserIdx(conv.Messages)
		if _, err := CompactConversationDurable(ctx, convID, conv.Messages, boundary, machine); err != nil {
			t.Fatalf("fold %d: %v", round, err)
		}
		s := checkpointRows(t, db, convID).Summary
		// The PRIOR roll-up survives verbatim (banner stripped, text intact).
		priorText := stripSummaryBanners(s1)
		if priorText == "" {
			t.Fatalf("fold %d: prior roll-up is empty", round)
		}
		if !strings.Contains(stripSummaryBanners(s), priorText) {
			t.Fatalf("fold %d dropped the prior roll-up:\n--- prior ---\n%s\n--- stored ---\n%s", round, priorText, s)
		}
		// The newest folded text is present.
		if !strings.Contains(s, fmt.Sprintf("tool output %d", round)) {
			t.Fatalf("fold %d summary missing the newest folded text", round)
		}
		// Exactly ONE banner, no nesting.
		for _, banner := range []string{priorSummaryBanner, "NEW MESSAGES:", newerHistoryBanner} {
			if n := strings.Count(s, banner); n > 1 {
				t.Fatalf("fold %d: banner %q appears %d times (nesting): %q", round, banner, n, head(s, 200))
			}
		}
		if len(s) > 3*maxDurableSummaryChars {
			t.Fatalf("fold %d: summary is %d chars, unbounded growth", round, len(s))
		}
		s1 = s
	}
}

// TestDurableFrozenFoldIsDeterministic: the stored summary is a pure function of the
// folded rows. Two conversations with identical history store byte-identical
// summaries — no clock, no LLM, no random, so the prompt head never churns.
func TestDurableFrozenFoldIsDeterministic(t *testing.T) {
	machine := func(msgs []utils.Message) string {
		var b strings.Builder
		writeDurableSummaryText(&b, msgs)
		return b.String()
	}
	seed := func(t *testing.T, user, convID string) (context.Context, *sql.DB) {
		t.Helper()
		db := openFrozenDB(t, user)
		ctx := frozenCtx(user)
		seedConv(t, db, convID)
		appendRows(t, db, convID,
			userMsg("U1"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
			userMsg("U2"), asstMsg("A2", "call_2", "shell_exec"), toolMsg("shell_exec", "T2", "call_2"),
			userMsg("U3"), asstMsg("A3", "call_3", "shell_exec"), toolMsg("shell_exec", "T3", "call_3"),
			userMsg("U4 anchor"),
		)
		return ctx, db
	}

	ctxA, dbA := seed(t, "fz_det_a", "c_det")
	ctxB, dbB := seed(t, "fz_det_b", "c_det")

	// Same conversation, folded at the same boundaries on two separate DBs.
	for _, pair := range []struct {
		ctx context.Context
		db  *sql.DB
	}{{ctxA, dbA}, {ctxB, dbB}} {
		for round := 0; round < 2; round++ {
			conv, err := GetConversationByIdInternal(pair.ctx, "c_det")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if _, err := CompactConversationDurable(pair.ctx, "c_det", conv.Messages, lastUserIdx(conv.Messages), machine); err != nil {
				t.Fatalf("fold: %v", err)
			}
			appendRows(t, pair.db, "c_det",
				asstMsg("A new", "call_new", "shell_exec"),
				toolMsg("shell_exec", "T new", "call_new"),
				userMsg("U anchor new"),
			)
		}
	}

	sa := checkpointRows(t, dbA, "c_det").Summary
	sb := checkpointRows(t, dbB, "c_det").Summary
	if sa == "" {
		t.Fatalf("no summary stored")
	}
	if sa != sb {
		t.Fatalf("summary is not deterministic:\n--- A ---\n%s\n--- B ---\n%s", head(sa, 400), head(sb, 400))
	}
}

// TestDurableFrozenFoldNilMachineSummaryStoresBoundedFallback: machineSummary is
// MACHINE-only. nil, or an empty result, stores a bounded mechanical excerpt —
// never an LLM call, never an empty pair.
func TestDurableFrozenFoldNilMachineSummaryStoresBoundedFallback(t *testing.T) {
	const user = "fz_fallback"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_fallback"
	seedConv(t, db, convID)

	huge := strings.Repeat("long tool output ", 40_000)
	appendRows(t, db, convID,
		userMsg("U1"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", huge, "call_1"),
		userMsg("U2 anchor"),
	)
	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	empty := func([]utils.Message) string { return "" }
	folded, err := CompactConversationDurable(ctx, convID, conv.Messages, lastUserIdx(conv.Messages), empty)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatalf("expected progress")
	}
	p := checkpointRows(t, db, convID)
	if p.Count != 2 {
		t.Fatalf("pair rows = %d, expected 2", p.Count)
	}
	if len(p.Summary) > maxDurableSummaryChars+200 {
		t.Fatalf("fallback summary is %d chars, bound is %d", len(p.Summary), maxDurableSummaryChars)
	}
	if !strings.Contains(p.Summary, "long tool output") {
		t.Fatalf("fallback summary stores no representation of the folded rows")
	}
}

// TestDurableFrozenFoldBoundaryNeverStartsOnToolRow: the live window cannot begin on
// a tool row (its parent assistant half would be folded away, leaving an
// orphan tool result at the head of the request — a hard provider error), and
// the tail stays anchored on a user row.
func TestDurableFrozenFoldBoundaryNeverStartsOnToolRow(t *testing.T) {
	const user = "fz_tool"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_tool"
	seedConv(t, db, convID)

	appendRows(t, db, convID,
		userMsg("U1"), asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
		userMsg("U2"), asstMsg("A2", "call_2", "shell_exec"),
		toolMsg("shell_exec", "T2a", "call_2"), toolMsg("shell_exec", "T2b", "call_2"),
		userMsg("U3 anchor"),
	)
	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Boundary points AT a tool row (index 5).
	boundary := 5
	if conv.Messages[boundary].Role != "tool" {
		t.Fatalf("fixture: expected a tool row at %d, got %s", boundary, conv.Messages[boundary].Role)
	}
	folded, err := CompactConversationDurable(ctx, convID, conv.Messages, boundary, nil)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded == 0 {
		t.Fatalf("expected the clamped boundary to fold the prefix")
	}
	llm, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var firstLive utils.Message
	seenLive := false
	liveCount := 0
	for _, m := range llm.Messages {
		if IsCheckpointMessage(m) {
			continue
		}
		if !seenLive {
			firstLive = m
			seenLive = true
		}
		liveCount++
	}
	if !seenLive {
		t.Fatalf("compaction emptied the live window")
	}
	if firstLive.Role == "tool" {
		t.Fatalf("live window starts on an orphan tool row (name=%s)", firstLive.Name)
	}
	if firstLive.Role != "user" {
		t.Fatalf("live window starts on %q, expected the user anchor", firstLive.Role)
	}
	if liveCount == 0 {
		t.Fatalf("no live rows left")
	}
}

// TestDurableFrozenFoldNeverFoldsPastTheLastUserRow: a boundary at the very end of
// the slice is clamped back to the newest user row, so the live tail can never
// collapse to nothing (the "100 assistant/tool rows behind the checkpoint with
// zero user messages" wedge).
func TestDurableFrozenFoldNeverFoldsPastTheLastUserRow(t *testing.T) {
	const user = "fz_floor"
	db := openFrozenDB(t, user)
	ctx := frozenCtx(user)
	convID := "c_floor"
	seedConv(t, db, convID)

	appendRows(t, db, convID,
		userMsg("U1 anchor"),
		asstMsg("A1", "call_1", "shell_exec"), toolMsg("shell_exec", "T1", "call_1"),
		asstMsg("A2", "call_2", "shell_exec"), toolMsg("shell_exec", "T2", "call_2"),
	)
	conv, err := GetConversationByIdInternal(ctx, convID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	folded, err := CompactConversationDurable(ctx, convID, conv.Messages, len(conv.Messages), nil)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded != 0 {
		t.Fatalf("folded=%d: the newest user row is the floor, nothing may be folded past it", folded)
	}
	if p := checkpointRows(t, db, convID); p.Count != 0 {
		t.Fatalf("a fold with no legal boundary minted %d checkpoint rows", p.Count)
	}
}
