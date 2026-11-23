package db

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// openTestDB creates an in-memory DB with the full schema (the same schema
// string the server uses) and returns it. Tests must close it.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// Use a real temp file: the FTS5 + vec0 virtual tables are only
	// available through the compiled-in extensions backed by a file DB.
	f, err := os.CreateTemp("", "plurality-maintenance-*.db")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	f.Close()
	path := f.Name()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + "-wal"); os.Remove(path + "-shm") })

	db, err := sql.Open("sqlite3", path+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestRunStorageMaintenanceTruncatesOversizedContent(t *testing.T) {
	db := openTestDB(t)

	// Insert a conversation + an oversized message. Content is stored the way
	// marshalMessageContent stores a single text part: a JSON string.
	bigText := strings.Repeat("A", 200_000)
	contentJSON, _ := json.Marshal(bigText)
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, last_message_at) VALUES ('c1','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (conversation_id, seq, role, content) VALUES ('c1', 1, 'user', ?)`,
		string(contentJSON)); err != nil {
		t.Fatalf("msg: %v", err)
	}

	RunStorageMaintenance(db)

	var raw string
	if err := db.QueryRow(`SELECT content FROM messages WHERE seq=1`).Scan(&raw); err != nil {
		t.Fatalf("read back: %v", err)
	}
	// The stored value should now be a JSON string of a truncated body.
	var text string
	if err := json.Unmarshal([]byte(raw), &text); err != nil {
		t.Fatalf("content not JSON string: %v", err)
	}
	if len(text) > maintenanceMaxChars+200 { // banner adds ~90 chars
		t.Fatalf("content still oversized: %d", len(text))
	}
	if !strings.Contains(text, "message truncated by Plurality") {
		t.Fatalf("missing banner: %q", text[:200])
	}
	if !strings.HasPrefix(text, "A") {
		t.Fatalf("head not preserved")
	}
}

func TestRunStorageMaintenanceDeletesOrphanedVectors(t *testing.T) {
	db := openTestDB(t)

	// The vec_embeddings table is a vec0 virtual table whose blob format is
	// C-library specific, so we cannot fabricate a row in a unit test. What
	// we CAN verify: (1) the messages_vec_delete trigger was created by the
	// schema, and (2) the orphan-cleanup statement RunStorageMaintenance
	// issues is accepted by the database (valid SQL against the real table).
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='messages_vec_delete'`).Scan(&n); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected messages_vec_delete trigger, found %d", n)
	}

	// Orphan cleanup statement must parse & execute (no rows to delete).
	if _, err := db.Exec(`
		DELETE FROM vec_embeddings
		WHERE source_type = 'message'
		  AND source_id NOT IN (SELECT CAST(id AS TEXT) FROM messages)`); err != nil {
		t.Fatalf("orphan cleanup SQL failed: %v", err)
	}
}

func TestRunStorageMaintenanceIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	big := strings.Repeat("B", 150_000)
	j, _ := json.Marshal(big)
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, last_message_at) VALUES ('c1','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (conversation_id, seq, role, content) VALUES ('c1',1,'user',?)`, string(j)); err != nil {
		t.Fatalf("msg: %v", err)
	}
	RunStorageMaintenance(db)
	RunStorageMaintenance(db)
	var raw string
	db.QueryRow(`SELECT content FROM messages WHERE seq=1`).Scan(&raw)
	var text string
	json.Unmarshal([]byte(raw), &text)
	if len(text) > maintenanceMaxChars+200 {
		t.Fatalf("not truncated after 2 runs: %d", len(text))
	}
	// The banner must appear exactly once (idempotency).
	if c := strings.Count(text, "message truncated by Plurality"); c != 1 {
		t.Fatalf("expected 1 banner, got %d", c)
	}
}

// The pre-fix maintenance pass reported "truncated 327 oversized stored
// message blob(s)" on EVERY boot, in every log line, forever. That number is
// the test: an idempotent pass reports 0 after the first run, because there is
// nothing left to fix. It was not idempotent because truncateText returned
// HeadBytes(s, max) + banner = max + ~110 bytes, i.e. a body LONGER than the
// bound it was truncating to, so the next pass saw the row still over the bound
// and rewrote it again.
//
// The cost is not disk space. KV prompt caching reuses the longest common
// PREFIX of the prompt, so a row whose stored bytes change rewrites every byte
// of the prompt after it. Measured on the live deployment: cache_read pinned
// FLAT at 38030 tokens across consecutive turns (10:07, 10:12, 10:21) while
// tok_before grew 106710 -> 108844 -> 109378, with ttfb_ms at 89983-114744.
// 38030 is the shared head of the conversation up to its first banner-bearing
// row — the exact point where the stored bytes stop matching.
//
// The existing TestRunStorageMaintenanceIsIdempotent passes against the broken
// code: it allows max+200 (the over-bound result fits) and counts banners,
// which stays at 1 because the re-shave discards the previous banner. Byte
// identity is the invariant, so that is what this pins.
func TestRunStorageMaintenanceStoredBytesNeverMove(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, last_message_at) VALUES ('c1','t','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("conv: %v", err)
	}
	insert := func(seq int, body string) {
		j, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO messages (conversation_id, seq, role, content) VALUES ('c1', ?, 'tool', ?)`,
			seq, string(j)); err != nil {
			t.Fatalf("msg: %v", err)
		}
	}
	// The live shapes: a fat tool result over the bound, and a legacy row that
	// already carries a stacked banner and is STILL over the bound.
	insert(1, strings.Repeat("A", 200_000))
	legacy := strings.Repeat("B", 120_000) +
		"\n\n[message truncated by Plurality: original length 400000 chars exceeded the per-message context budget]"
	insert(2, legacy)

	// First pass: everything settles.
	first, err := truncateOversizedStoredContent(db)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first == 0 {
		t.Fatal("first pass fixed nothing — the fixture is not over the bound")
	}

	// Every later pass must be a NO-OP. This is the assertion the deployed
	// binary fails: it logs a non-zero count on every single start.
	for pass := 2; pass <= 4; pass++ {
		again, err := truncateOversizedStoredContent(db)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if again != 0 {
			t.Fatalf("pass %d rewrote %d row(s): truncation is not idempotent, "+
				"so these rows' bytes change on every boot and the KV prefix "+
				"cache cannot survive past them", pass, again)
		}
	}

	// Strict bound: the stored body must be WITHIN maintenanceMaxChars, not
	// "max + room for the banner".
	for _, seq := range []int{1, 2} {
		var raw string
		if err := db.QueryRow(`SELECT content FROM messages WHERE seq=?`, seq).Scan(&raw); err != nil {
			t.Fatalf("read seq %d: %v", seq, err)
		}
		var text string
		if err := json.Unmarshal([]byte(raw), &text); err != nil {
			t.Fatalf("seq %d content not a JSON string: %v", seq, err)
		}
		if len(text) > maintenanceMaxChars {
			t.Fatalf("seq %d stored body is %d bytes, over the %d bound — the next "+
				"pass will truncate it again", seq, len(text), maintenanceMaxChars)
		}
	}
}
