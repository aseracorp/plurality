package db

import (
	"database/sql"
	"encoding/json"

	"github.com/azukaar/plurality/src/textsafe"
	"github.com/azukaar/plurality/src/utils"
)

// Storage constants shared with the AI context paths (eco.go and
// context_compaction.go use the same 100k bound). The banner TEXT is not a
// constant here: it comes from textsafe.Banner, so the on-disk form and the
// request path cannot drift. They used to be two different literals
// ("...exceeded the per-message context budget" here, "...across all content
// parts exceeded..." there), which is how the stored form ended up never
// matching what the context filter shipped.
const maintenanceMaxChars = 100_000

// RunStorageMaintenance heals a user database that accumulated oversized
// message blobs. Before the eco context filter (eco.go
// filterCheckpointsForRequest) and compaction path (context_compaction.go)
// truncated oversized messages in memory, the server persisted multi-MB
// tool results and pasted dumps verbatim. Those rows were never rewritten,
// so even though the LLM never saw beyond the first 100k chars, the
// database kept every megabyte (observed: a 4.2MB WordPress wp_get_site_info
// dump repeated 4x, plus ~25MB of >100k tool results across conversations).
//
// This pass is safe and idempotent:
//   - It only truncates rows whose JSON text body exceeds 100k chars, and
//     rewrites them with the exact same banner the in-memory paths use.
//   - It deletes vec_embeddings rows whose source_id no longer maps to a
//     messages row (no delete-trigger has ever existed for vectors, so
//     deleted messages left dangling vectors behind).
//   - It rebuilds the FTS index so search reflects the truncated content.
//
// Called once per user database on first open (GetUserDB). Errors are
// logged and swallowed — maintenance is best-effort and must never prevent
// the server from starting.
func RunStorageMaintenance(db *sql.DB) {
	// 1) Orphaned vectors first: cheapest and prevents the truncation pass
	//    (if it deletes anything) from leaving vectors behind. SQLite-vec
	//    stores row ids as text in vec_embeddings.source_id; messages.id is
	//    an integer, so compare via CAST.
	vecCleaned := false
	if _, err := db.Exec(`
		DELETE FROM vec_embeddings
		WHERE source_type = 'message'
		  AND source_id NOT IN (SELECT CAST(id AS TEXT) FROM messages)`); err != nil {
		utils.Debug("[Maintenance] orphan vector cleanup skipped: %v", err)
	} else if changes, _ := lastChangeCount(db); changes > 0 {
		utils.Log("[Maintenance] removed %d orphaned vector(s)", changes)
		vecCleaned = true
	}

	// 2) Truncate oversized message content / tool_calls (JSON-aware). Only
	//    rewrite rows that actually exceed the bound, so healthy databases
	//    are untouched (and thus never rewritten into new WAL pages).
	changed, err := truncateOversizedStoredContent(db)
	if err != nil {
		utils.Error("[Maintenance] oversized message truncation failed", err)
	}
	if changed == 0 && !vecCleaned {
		return // nothing to do — don't burn a full FTS rebuild every boot
	}

	// 3) Rebuild FTS only when something changed, so search reflects the
	//    truncated content and any deletes. Also collapses the shadow FTS
	//    tables, reclaiming pages that are otherwise copied into every
	//    checkpoint.
	if _, err := db.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('rebuild')`); err != nil {
		utils.Debug("[Maintenance] FTS rebuild skipped: %v", err)
	} else {
		utils.Log("[Maintenance] FTS index rebuilt")
	}
}

// truncateOversizedStoredContent rewrites stored message content / tool_calls
// JSON whose text exceeds maintenanceMaxChars, keeping the head + banner. It
// mirrors what utils.MessageContent and the in-memory truncation would do, so
// the on-disk representation stays exactly what the context filter ships. It
// returns the number of rows rewritten (0 when the database is already within
// bounds).
func truncateOversizedStoredContent(db *sql.DB) (int, error) {
	rows, err := db.Query(`SELECT id, content, tool_calls FROM messages`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type fix struct {
		id        int64
		content   *string
		toolCalls *string
	}
	var fixes []fix

	for rows.Next() {
		var id int64
		var content, toolCalls sql.NullString
		if err := rows.Scan(&id, &content, &toolCalls); err != nil {
			return 0, err
		}
		newContent, ch1 := truncateStoredContent(content)
		newTC, ch2 := truncateStoredToolCalls(toolCalls)
		if ch1 || ch2 {
			fixes = append(fixes, fix{id: id, content: newContent, toolCalls: newTC})
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, f := range fixes {
		if _, err := db.Exec(`UPDATE messages SET content = ?, tool_calls = ? WHERE id = ?`,
			nullableString(f.content), nullableString(f.toolCalls), f.id); err != nil {
			return len(fixes), err
		}
	}
	if len(fixes) > 0 {
		utils.Log("[Maintenance] truncated %d oversized stored message blob(s)", len(fixes))
	}
	return len(fixes), nil
}

// lastChangeCount reads sqlite3_changes for the just-executed statement so
// the maintenance loop knows whether a DELETE actually removed rows.
func lastChangeCount(db *sql.DB) (int64, error) {
	var n int64
	if err := db.QueryRow(`SELECT changes()`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func truncateStoredContent(ns sql.NullString) (*string, bool) {
	if !ns.Valid || ns.String == "" {
		return nil, false
	}
	raw := ns.String
	// Content is stored as JSON (a string or an array of parts). Parse it,
	// truncate any text part body, and re-serialize — exactly how
	// utils.MessageContent.MarshalJSON would have produced it.
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Not valid JSON: leave as-is rather than corrupt it.
		return nil, false
	}
	switch v := parsed.(type) {
	case string:
		trunc, changed := truncateText(v)
		if !changed {
			return &raw, false
		}
		out, _ := json.Marshal(trunc)
		s := string(out)
		return &s, true
	case []interface{}:
		changed := false
		for _, p := range v {
			if m, ok := p.(map[string]interface{}); ok {
				if m["type"] == "text" {
					if s, ok := m["text"].(string); ok && len(s) > maintenanceMaxChars {
						trunc, _ := truncateText(s)
						m["text"] = trunc
						changed = true
					}
				}
			}
		}
		if !changed {
			return &raw, false
		}
		out, _ := json.Marshal(v)
		s := string(out)
		return &s, true
	default:
		return &raw, false
	}
}

func truncateStoredToolCalls(ns sql.NullString) (*string, bool) {
	if !ns.Valid || ns.String == "" {
		return nil, false
	}
	raw := ns.String
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, false
	}
	changed := false
	for _, tc := range parsed {
		if fn, ok := tc["function"].(map[string]interface{}); ok {
			if args, ok := fn["arguments"].(string); ok && len(args) > maintenanceMaxChars {
				trunc, _ := truncateText(args)
				fn["arguments"] = trunc
				changed = true
			}
		}
	}
	if !changed {
		return &raw, false
	}
	out, _ := json.Marshal(parsed)
	s := string(out)
	return &s, true
}

// truncateText bounds one text body to maintenanceMaxChars.
//
// MUST be idempotent, and MUST return a body within maintenanceMaxChars. The
// pre-fix form was HeadBytes(s, max) + banner, i.e. max + ~110 bytes: LONGER
// than the bound it was truncating to, so every boot saw the row still over
// the bound and truncated it again — shaving ~110 bytes off the preserved head
// and stacking a second banner with a new "original length". Observed live:
// 345 stored rows carrying the banner while over the 100000 bound they were
// truncated to, some with FIVE stacked banners. Because the length never
// settled, those rows' BYTES CHANGED ON EVERY BOOT, and KV prompt caching
// matches the longest common prefix: one changed byte mid-history discards the
// whole conversation's cached KV (measured: cache_read pinned flat at 38030
// tokens across consecutive turns while prompt tokens grew 106710 -> 109378).
// textsafe.TruncateWithBanner reserves the banner's room inside max, so the
// stored bytes settle on the first pass and never move again.
func truncateText(s string) (string, bool) {
	return textsafe.TruncateWithBanner(s, maintenanceMaxChars)
}

func nullableString(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}
