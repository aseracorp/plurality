package search

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"

	"github.com/azukaar/plurality/src/utils"
)

var embeddingModel = "text-embedding-3-small"
var embeddingDimension = 1536

func init() {
	if m := os.Getenv("EMBEDDING_MODEL"); m != "" {
		embeddingModel = m
	}
}

// embeddingRequest is the OpenAI-compatible request body for /v1/embeddings.
type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// embeddingResponse is the OpenAI-compatible response from /v1/embeddings.
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// GenerateEmbedding calls the LiteLLM /v1/embeddings endpoint and returns the vector.
func GenerateEmbedding(liteLLMBaseURL string, text string) ([]float32, error) {
	body, err := json.Marshal(embeddingRequest{
		Model: embeddingModel,
		Input: text,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	resp, err := http.Post(liteLLMBaseURL+"/v1/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("calling embeddings API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embeddings API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding returned")
	}

	return result.Data[0].Embedding, nil
}

// StoreEmbedding inserts or replaces a vector in the vec_embeddings table.
func StoreEmbedding(db *sql.DB, sourceType string, sourceID string, embedding []float32) error {
	blob := float32ToBytes(embedding)
	_, err := db.Exec(
		`INSERT INTO vec_embeddings(source_type, source_id, embedding) VALUES (?, ?, ?)`,
		sourceType, sourceID, blob,
	)
	return err
}

// VectorSearch performs KNN search on vec_embeddings and returns ranked source IDs with distances.
// Results with distance above the threshold are discarded.
func VectorSearch(db *sql.DB, queryVec []float32, sourceType string, k int) ([]ScoredResult, error) {
	blob := float32ToBytes(queryVec)
	rows, err := db.Query(
		`SELECT source_id, distance FROM vec_embeddings WHERE embedding MATCH ? AND k = ? AND source_type = ?`,
		blob, k, sourceType,
	)
	if err != nil {
		utils.Debug("[Search] vec search error: %v", err)
		return nil, err
	}
	defer rows.Close()

	const maxDistance = 0.75 // cosine distance: 0=identical, 1=unrelated

	var results []ScoredResult
	for rows.Next() {
		var r ScoredResult
		if err := rows.Scan(&r.SourceID, &r.Score); err != nil {
			return nil, err
		}
		utils.Debug("[Search] vec hit source_id=%s distance=%.4f", r.SourceID, r.Score)
		if r.Score <= maxDistance {
			results = append(results, r)
		}
	}
	return results, rows.Err()
}

func float32ToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}
