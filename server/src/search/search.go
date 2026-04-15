package search

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/azukaar/plurality/src/utils"
)

// ScoredResult represents a single search hit with its score.
type ScoredResult struct {
	ConversationID string  `json:"conversation_id"`
	SourceID       string  `json:"source_id"`
	Score          float64 `json:"score"`
}

// MatchedMessage is a message returned in search results.
type MatchedMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
}

// SearchResult is the final result returned to the client and AI tools.
type SearchResult struct {
	ConversationID string           `json:"conversation_id"`
	Title          string           `json:"title"`
	Date           string           `json:"date"`
	Score          float64          `json:"score"`
	Messages       []MatchedMessage `json:"messages"`
}

// Search runs a hybrid search combining FTS5 (BM25) and vector (KNN) results via RRF.
func Search(ctx context.Context, db *sql.DB, liteLLMBaseURL string, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}

	// Run FTS5 search
	ftsResults, err := FTSSearch(db, query, limit*2)
	if err != nil {
		utils.Debug("[Search] FTS error: %v", err)
		ftsResults = nil
	}
	utils.Debug("[Search] query=%q fts_results=%d", query, len(ftsResults))

	// Run vector search (if embeddings API is available)
	var vecResults []ScoredResult
	if liteLLMBaseURL != "" {
		queryVec, err := GenerateEmbedding(liteLLMBaseURL, query)
		if err != nil {
			utils.Debug("[Search] Embedding generation error: %v", err)
		} else {
			vecHits, err := VectorSearch(db, queryVec, "message", limit*2)
			if err != nil {
				utils.Debug("[Search] Vector search error: %v", err)
			} else {
				vecResults = resolveMessageConversations(db, vecHits)
			}
		}
	}
	utils.Debug("[Search] vec_results=%d", len(vecResults))

	// Merge via RRF
	merged := RRFMerge(ftsResults, vecResults)
	if len(merged) > limit {
		merged = merged[:limit]
	}

	// Enrich with conversation titles and snippets
	return enrichResults(db, merged)
}

// resolveMessageConversations maps message IDs from vec_embeddings back to conversation IDs.
func resolveMessageConversations(db *sql.DB, hits []ScoredResult) []ScoredResult {
	var results []ScoredResult
	for _, h := range hits {
		var convID string
		err := db.QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, h.SourceID).Scan(&convID)
		if err != nil {
			continue
		}
		results = append(results, ScoredResult{
			ConversationID: convID,
			SourceID:       h.SourceID,
			Score:          h.Score,
		})
	}
	return results
}

// enrichResults fetches conversation metadata and matched messages with context.
func enrichResults(db *sql.DB, results []ScoredResult) ([]SearchResult, error) {
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		var title, date string
		err := db.QueryRow(`SELECT title, last_message_at FROM conversations WHERE id = ?`, r.ConversationID).Scan(&title, &date)
		if err != nil {
			continue
		}

		messages := getMatchedMessages(db, r)

		out = append(out, SearchResult{
			ConversationID: r.ConversationID,
			Title:          title,
			Date:           date,
			Score:          r.Score,
			Messages:       messages,
		})
	}
	return out, nil
}

// getMatchedMessages loads the matched message plus 1 message of surrounding context.
func getMatchedMessages(db *sql.DB, r ScoredResult) []MatchedMessage {
	// Find the seq of the matched message
	var matchedSeq int64
	if r.SourceID != "" {
		msgID, err := strconv.ParseInt(r.SourceID, 10, 64)
		if err == nil {
			db.QueryRow(`SELECT seq FROM messages WHERE id = ?`, msgID).Scan(&matchedSeq)
		}
	}

	// Load matched message + 1 before and 1 after
	startSeq := matchedSeq - 1
	if startSeq < 0 {
		startSeq = 0
	}
	endSeq := matchedSeq + 1

	rows, err := db.Query(
		`SELECT role, content, timestamp FROM messages
		 WHERE conversation_id = ? AND seq >= ? AND seq <= ?
		 ORDER BY seq`,
		r.ConversationID, startSeq, endSeq,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var messages []MatchedMessage
	for rows.Next() {
		var m MatchedMessage
		var content, timestamp sql.NullString
		rows.Scan(&m.Role, &content, &timestamp)
		if content.Valid {
			m.Content = truncate(content.String, 500)
		}
		if timestamp.Valid {
			m.Timestamp = timestamp.String
		}
		messages = append(messages, m)
	}

	// Fallback: if no matched message found, return first user message
	if len(messages) == 0 {
		var m MatchedMessage
		var content, timestamp sql.NullString
		db.QueryRow(
			`SELECT role, content, timestamp FROM messages WHERE conversation_id = ? AND role = 'user' ORDER BY seq LIMIT 1`,
			r.ConversationID,
		).Scan(&m.Role, &content, &timestamp)
		if content.Valid {
			m.Content = truncate(content.String, 500)
			if timestamp.Valid {
				m.Timestamp = timestamp.String
			}
			messages = append(messages, m)
		}
	}

	return messages
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// EmbedAndStore generates an embedding for the given text and stores it.
// Intended to be called asynchronously after a message is saved.
func EmbedAndStore(db *sql.DB, liteLLMBaseURL string, sourceType string, sourceID string, text string) {
	if liteLLMBaseURL == "" || text == "" {
		return
	}

	// Skip very short messages (greetings, "ok", etc.)
	if len(text) < 3 {
		return
	}

	vec, err := GenerateEmbedding(liteLLMBaseURL, truncate(text, 8000))
	if err != nil {
		utils.Debug("[Search] Failed to generate embedding for %s/%s: %v", sourceType, sourceID, err)
		return
	}

	if err := StoreEmbedding(db, sourceType, sourceID, vec); err != nil {
		utils.Debug("[Search] Failed to store embedding for %s/%s: %v", sourceType, sourceID, err)
	}
}

// EmbedMessage is a convenience wrapper for embedding a conversation message.
func EmbedMessage(db *sql.DB, liteLLMBaseURL string, messageID int64, text string) {
	EmbedAndStore(db, liteLLMBaseURL, "message", fmt.Sprintf("%d", messageID), text)
}
