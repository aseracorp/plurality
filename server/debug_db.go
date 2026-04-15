//go:build ignore

package main

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
	"path/filepath"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

import "C"

func main() {
	sqlite_vec.Auto()

	if len(os.Args) < 2 {
		entries, err := os.ReadDir("build/users-data")
		if err != nil {
			fmt.Println("Usage: go run debug_db.go <path-to-conversations.db>")
			os.Exit(1)
		}
		for _, e := range entries {
			if e.IsDir() {
				os.Args = append(os.Args, filepath.Join("build/users-data", e.Name(), "conversations.db"))
				break
			}
		}
		if len(os.Args) < 2 {
			os.Exit(1)
		}
	}

	dbPath := os.Args[1]
	fmt.Printf("Opening: %s\n\n", dbPath)

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
	defer db.Close()

	// Conversations
	fmt.Println("=== CONVERSATIONS ===")
	rows, _ := db.Query(`SELECT id, title FROM conversations ORDER BY last_message_at DESC`)
	for rows.Next() {
		var id, title string
		rows.Scan(&id, &title)
		fmt.Printf("  [%s] %q\n", id, title)
	}
	rows.Close()

	// FTS
	fmt.Println("\n=== MESSAGES_FTS ===")
	rows, _ = db.Query(`SELECT conversation_id, message_id, substr(content, 1, 60) FROM messages_fts`)
	for rows.Next() {
		var convID string
		var msgID int64
		var content string
		rows.Scan(&convID, &msgID, &content)
		fmt.Printf("  conv=%s msg#%d %q\n", convID, msgID, content)
	}
	rows.Close()

	// Vec embeddings
	fmt.Println("\n=== VEC_EMBEDDINGS ===")
	rows, _ = db.Query(`SELECT source_type, source_id FROM vec_embeddings`)
	for rows.Next() {
		var srcType, srcID string
		rows.Scan(&srcType, &srcID)
		fmt.Printf("  type=%s id=%s\n", srcType, srcID)
	}
	rows.Close()

	// Vec search tests
	queries := []string{"food", "animal", "bird", "car"}
	fmt.Println("\n=== VECTOR SEARCH TESTS ===")
	for _, q := range queries {
		vec, err := getEmbedding(q)
		if err != nil {
			fmt.Printf("\n  %q: error getting embedding: %v\n", q, err)
			continue
		}
		blob := float32ToBytes(vec)
		rows, err := db.Query(
			`SELECT source_id, distance FROM vec_embeddings WHERE embedding MATCH ? AND k = 10 AND source_type = 'message'`,
			blob,
		)
		if err != nil {
			fmt.Printf("\n  %q: query error: %v\n", q, err)
			continue
		}
		fmt.Printf("\n  %q:\n", q)
		for rows.Next() {
			var srcID string
			var dist float64
			rows.Scan(&srcID, &dist)
			// Look up message content
			var content sql.NullString
			db.QueryRow(`SELECT substr(content, 1, 50) FROM messages WHERE id = ?`, srcID).Scan(&content)
			c := ""
			if content.Valid {
				c = content.String
			}
			fmt.Printf("    msg#%-3s dist=%.4f  %q\n", srcID, dist, c)
		}
		rows.Close()
	}
}

func getEmbedding(text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]string{"model": "text-embedding-3-small", "input": text})
	resp, err := http.Post("http://127.0.0.1:4000/v1/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(b))
	}
	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	return result.Data[0].Embedding, nil
}

func float32ToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}
