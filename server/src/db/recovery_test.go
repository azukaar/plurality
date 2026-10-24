package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// seedStuckConversation inserts a conversation with the given state and an
// optional final wait tool result into the test DB.
func seedStuckConversation(t *testing.T, db *sql.DB, convID string, state string, waitResult string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, last_message_at, state) VALUES (?, 't', '2026-01-01T00:00:00Z', ?)`,
		convID, state); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	if waitResult != "" {
		contentJSON, _ := json.Marshal(waitResult)
		if _, err := db.Exec(
			`INSERT INTO messages (conversation_id, seq, role, name, content, tool_call_id) VALUES (?, 1, 'tool', 'wait', ?, 'call_x')`,
			convID, string(contentJSON)); err != nil {
			t.Fatalf("seed msg: %v", err)
		}
	}
}

// TestRecoverStuckConversationsWaitElapsed verifies that a conversation left
// with state=processing whose last message is an elapsed wait tool result gets
// the synthetic "Timer is done" pair and is flipped to idle.
func TestRecoverStuckConversationsWaitElapsed(t *testing.T) {
	dir := t.TempDir()
	oldPath := userDataPath
	userDataPath = dir
	defer func() { userDataPath = oldPath }()

	userID := "sam"
	os.MkdirAll(filepath.Join(dir, userID), 0755)
	testDB, err := sql.Open("sqlite3", filepath.Join(dir, userID, "conversations.db")+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer testDB.Close()
	if _, err := testDB.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// A wait result whose wake_at is already in the past (the in-memory timer
	// died with the old server process).
	past := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	waitRes, _ := json.Marshal(map[string]interface{}{
		"wait_seconds": 60,
		"wake_at":      past,
		"status":       "Waiting 60 second(s). The agent will resume automatically.",
	})
	seedStuckConversation(t, testDB, "c_wait_elapsed", "processing", string(waitRes))

	// Put this DB into the package cache so GetUserDB resolves it.
	userDBs.Store(userID, testDB)
	defer userDBs.Delete(userID)

	RecoverStuckConversations(context.Background())

	// State must be idle.
	var state string
	if err := testDB.QueryRow(`SELECT state FROM conversations WHERE id='c_wait_elapsed'`).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != string(utils.StateIdle) {
		t.Fatalf("expected idle, got %s", state)
	}

	// A "Timer is done" tool result must have been appended.
	var count int
	if err := testDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id='c_wait_elapsed' AND role='tool' AND name='wait' AND content LIKE '%Timer is done%'`).Scan(&count); err != nil {
		t.Fatalf("count resume: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 resumed timer result, got %d", count)
	}
}

// TestRecoverStuckConversationsWaitPending verifies that a wait whose wake_at
// is still in the future is NOT synthesised as done, but is still idled (the
// loop that would fire the timer is gone).
func TestRecoverStuckConversationsWaitPending(t *testing.T) {
	dir := t.TempDir()
	oldPath := userDataPath
	userDataPath = dir
	defer func() { userDataPath = oldPath }()

	userID := "sam"
	os.MkdirAll(filepath.Join(dir, userID), 0755)
	testDB, err := sql.Open("sqlite3", filepath.Join(dir, userID, "conversations.db")+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer testDB.Close()
	if _, err := testDB.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	future := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339)
	waitRes, _ := json.Marshal(map[string]interface{}{
		"wait_seconds": 600,
		"wake_at":      future,
		"status":       "Waiting 600 second(s). The agent will resume automatically.",
	})
	seedStuckConversation(t, testDB, "c_wait_future", "processing", string(waitRes))

	userDBs.Store(userID, testDB)
	defer userDBs.Delete(userID)

	RecoverStuckConversations(context.Background())

	var state string
	if err := testDB.QueryRow(`SELECT state FROM conversations WHERE id='c_wait_future'`).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != string(utils.StateIdle) {
		t.Fatalf("expected idle for pending wait, got %s", state)
	}

	// No synthetic completion should exist (the timer hadn't fired).
	var count int
	if err := testDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id='c_wait_future' AND content LIKE '%Timer is done%'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 synthetic completions for pending wait, got %d", count)
	}
}

// TestRecoverStuckConversationsPlainOrphan verifies that a conversation stuck
// in an arbitrary non-idle state (e.g. a tool call that never returned) is
// simply idled.
func TestRecoverStuckConversationsPlainOrphan(t *testing.T) {
	dir := t.TempDir()
	oldPath := userDataPath
	userDataPath = dir
	defer func() { userDataPath = oldPath }()

	userID := "sam"
	os.MkdirAll(filepath.Join(dir, userID), 0755)
	testDB, err := sql.Open("sqlite3", filepath.Join(dir, userID, "conversations.db")+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer testDB.Close()
	if _, err := testDB.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// A conversation mid-tool-call (last message is an assistant tool call
	// without a matching result): pure orphan.
	contentJSON, _ := json.Marshal("checking the build...")
	if _, err := testDB.Exec(
		`INSERT INTO conversations (id, title, last_message_at, state) VALUES ('c_orphan','t','2026-01-01T00:00:00Z','processing')`); err != nil {
		t.Fatalf("conv: %v", err)
	}
	if _, err := testDB.Exec(
		`INSERT INTO messages (conversation_id, seq, role, content) VALUES ('c_orphan', 1, 'assistant', ?)`,
		string(contentJSON)); err != nil {
		t.Fatalf("msg: %v", err)
	}

	userDBs.Store(userID, testDB)
	defer userDBs.Delete(userID)

	RecoverStuckConversations(context.Background())

	var state string
	if err := testDB.QueryRow(`SELECT state FROM conversations WHERE id='c_orphan'`).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != string(utils.StateIdle) {
		t.Fatalf("expected idle for orphan, got %s", state)
	}
}

// TestMaybeResumeElapsedWaitNonWaitMessage guards against consuming a
// conversation whose last message is not a wait result.
func TestMaybeResumeElapsedWaitNonWaitMessage(t *testing.T) {
	dir := t.TempDir()
	oldPath := userDataPath
	userDataPath = dir
	defer func() { userDataPath = oldPath }()

	userID := "sam"
	os.MkdirAll(filepath.Join(dir, userID), 0755)
	testDB, err := sql.Open("sqlite3", filepath.Join(dir, userID, "conversations.db")+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer testDB.Close()
	if _, err := testDB.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// Last message is a shell tool result, not a wait result.
	contentJSON, _ := json.Marshal("some shell output")
	if _, err := testDB.Exec(
		`INSERT INTO conversations (id, title, last_message_at, state) VALUES ('c_shell','t','2026-01-01T00:00:00Z','processing')`); err != nil {
		t.Fatalf("conv: %v", err)
	}
	if _, err := testDB.Exec(
		`INSERT INTO messages (conversation_id, seq, role, name, content, tool_call_id) VALUES ('c_shell', 1, 'tool', 'system_tools__shell_exec', ?, 'call_s')`,
		string(contentJSON)); err != nil {
		t.Fatalf("msg: %v", err)
	}

	userDBs.Store(userID, testDB)
	defer userDBs.Delete(userID)

	RecoverStuckConversations(context.Background())

	var state string
	if err := testDB.QueryRow(`SELECT state FROM conversations WHERE id='c_shell'`).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != string(utils.StateIdle) {
		t.Fatalf("expected idle for shell orphan, got %s", state)
	}
}

// TestRecoveryIgnoresMissingDB ensures non-user directories are skipped.
func TestRecoveryIgnoresMissingDB(t *testing.T) {
	dir := t.TempDir()
	oldPath := userDataPath
	userDataPath = dir
	defer func() { userDataPath = oldPath }()

	// A directory that is not a user dir (no conversations.db).
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0755)
	os.MkdirAll(filepath.Join(dir, "nodbuser"), 0755)

	// Should not panic and should report 0 recoveries.
	RecoverStuckConversations(context.Background())

	// Also verify that a stray user dir got no DB materialised.
	if _, err := os.Stat(filepath.Join(dir, "nodbuser", "conversations.db")); !os.IsNotExist(err) {
		t.Fatalf("expected no DB to be created for non-DB user dir, stat err=%v", err)
	}
}
