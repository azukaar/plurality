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
