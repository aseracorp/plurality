package search

import (
	"database/sql"
	"strconv"
	"strings"
)

// FTSSearch runs a BM25 full-text search on the messages_fts table.
// Returns results ranked by BM25 score (lower rank = better match).
func FTSSearch(db *sql.DB, query string, limit int) ([]ScoredResult, error) {
	// Sanitize query for FTS5: wrap each token in quotes to prevent syntax errors
	sanitized := sanitizeFTSQuery(query)
	if sanitized == "" {
		return nil, nil
	}

	rows, err := db.Query(
		`SELECT conversation_id, message_id, rank
		 FROM messages_fts
		 WHERE messages_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		sanitized, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []ScoredResult
	for rows.Next() {
		var r ScoredResult
		var messageID int64
		if err := rows.Scan(&r.ConversationID, &messageID, &r.Score); err != nil {
			return nil, err
		}
		r.SourceID = strconv.FormatInt(messageID, 10) // store for reference
		results = append(results, r)
	}
	return results, rows.Err()
}

// sanitizeFTSQuery wraps each word in quotes so special FTS5 characters don't cause errors.
func sanitizeFTSQuery(query string) string {
	words := strings.Fields(query)
	if len(words) == 0 {
		return ""
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		// Remove any existing quotes and wrap in double quotes
		w = strings.ReplaceAll(w, "\"", "")
		if w != "" {
			quoted[i] = "\"" + w + "\""
		}
	}
	return strings.Join(quoted, " ")
}
