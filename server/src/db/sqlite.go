package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/azukaar/plurality/src/utils"
)

import "C"

func init() {
	sqlite_vec.Auto()
}

var (
	userDataPath string
	userDBs      sync.Map // map[string]*sql.DB
)

const schema = `
CREATE TABLE IF NOT EXISTS conversations (
	id              TEXT PRIMARY KEY,
	title           TEXT NOT NULL DEFAULT '',
	last_message_at TEXT NOT NULL,
	model_selected  TEXT NOT NULL DEFAULT '{}',
	state           TEXT NOT NULL DEFAULT 'idle',
	mini_app        TEXT,
	folder          TEXT NOT NULL DEFAULT '',
	icon            TEXT NOT NULL DEFAULT '',
	trigger_type    TEXT,
	trigger_id      TEXT
);

CREATE TABLE IF NOT EXISTS messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	seq             INTEGER NOT NULL,
	role            TEXT NOT NULL,
	content         TEXT,
	tool_calls      TEXT,
	tool_call_id    TEXT,
	name            TEXT,
	timestamp         TEXT,
	total_tokens      INTEGER DEFAULT 0,
	prompt_tokens     INTEGER DEFAULT 0,
	completion_tokens INTEGER DEFAULT 0,
	response_cost     REAL    DEFAULT 0,
	model             TEXT,
	UNIQUE(conversation_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conversation_id);

-- FTS5 for BM25 text search (user and assistant messages only)
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	conversation_id UNINDEXED,
	message_id UNINDEXED,
	content,
	tokenize='porter unicode61'
);

-- Clean up FTS when messages are deleted
CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages
BEGIN
	DELETE FROM messages_fts WHERE message_id = OLD.id;
END;

-- sqlite-vec KNN search
CREATE VIRTUAL TABLE IF NOT EXISTS vec_embeddings USING vec0(
	source_type TEXT NOT NULL,
	source_id   TEXT NOT NULL,
	embedding   float[1536] distance_metric=cosine
);

-- Clean up embeddings when a message is deleted (parity with messages_fts_delete).
-- Previously there was no trigger, so deleting a message left a dangling vector
-- that permanently occupied ~6KB in the vec_embeddings shadow tables.
CREATE TRIGGER IF NOT EXISTS messages_vec_delete AFTER DELETE ON messages
BEGIN
	DELETE FROM vec_embeddings WHERE source_type = 'message' AND source_id = CAST(OLD.id AS TEXT);
END;
`

// InitSQLite reads the USER_DATA_STORAGE env var (default "./users-data"
// relative to the binary) and creates the base directory.
func InitSQLite() {
	userDataPath = os.Getenv("USER_DATA_STORAGE")
	if userDataPath == "" {
		exec, _ := os.Executable()
		userDataPath = filepath.Join(filepath.Dir(exec), "users-data")
	}
	if err := os.MkdirAll(userDataPath, 0755); err != nil {
		utils.Error("[SQLite] Failed to create user data directory", err)
	}
	utils.Log("[SQLite] User data path: %s", userDataPath)
}

// GetUserDB returns (or lazily creates) the per-user SQLite database.
func GetUserDB(userID string) (*sql.DB, error) {
	if v, ok := userDBs.Load(userID); ok {
		return v.(*sql.DB), nil
	}

	dir := filepath.Join(userDataPath, userID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating user directory: %w", err)
	}

	dbPath := filepath.Join(dir, "conversations.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=wal&_foreign_keys=1")
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db: %w", err)
	}

	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		utils.Error("[SQLite] Schema creation failed", err)
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	if err := ensureColumn(db, "conversations", "trigger_type", "TEXT"); err != nil {
		utils.Error("[SQLite] migration trigger_type failed", err)
	}
	if err := ensureColumn(db, "conversations", "trigger_id", "TEXT"); err != nil {
		utils.Error("[SQLite] migration trigger_id failed", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_conversations_trigger ON conversations(trigger_type, trigger_id)`); err != nil {
		utils.Error("[SQLite] creating idx_conversations_trigger failed", err)
	}

	if err := ensureColumn(db, "messages", "prompt_tokens", "INTEGER DEFAULT 0"); err != nil {
		utils.Error("[SQLite] migration prompt_tokens failed", err)
	}
	if err := ensureColumn(db, "messages", "completion_tokens", "INTEGER DEFAULT 0"); err != nil {
		utils.Error("[SQLite] migration completion_tokens failed", err)
	}
	if err := ensureColumn(db, "messages", "response_cost", "REAL DEFAULT 0"); err != nil {
		utils.Error("[SQLite] migration response_cost failed", err)
	}

	// Best-effort storage maintenance: truncate oversized stored message
	// blobs (legacy rows persisted before eco/compaction truncated them in
	// memory), drop orphaned embeddings, and rebuild the FTS index. Runs
	// once per process per user on first open. Never blocks startup.
	RunStorageMaintenance(db)

	actual, _ := userDBs.LoadOrStore(userID, db)
	if actual.(*sql.DB) != db {
		db.Close()
	}

	return actual.(*sql.DB), nil
}

// ensureColumn adds a column to an existing table if it does not yet exist.
// SQLite has no "ALTER TABLE ... ADD COLUMN IF NOT EXISTS", so we just attempt
// the ALTER and swallow the "duplicate column" error that signals it's already
// there. This avoids a separate PRAGMA query that would compete for the lone
// connection on SetMaxOpenConns(1) databases.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	if err == nil {
		utils.Log("[SQLite] migration: added %s.%s", table, column)
		return nil
	}
	if strings.Contains(err.Error(), "duplicate column name") {
		return nil
	}
	return err
}

// CloseAllUserDBs closes every cached user database. Call on shutdown.
func CloseAllUserDBs() {
	userDBs.Range(func(key, value any) bool {
		value.(*sql.DB).Close()
		userDBs.Delete(key)
		return true
	})
	utils.Log("[SQLite] Closed all user databases")
}

// GenerateID creates a 24-character hex string from 12 random bytes.
func GenerateID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- JSON helpers for SQLite columns ---

func marshalJSON(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func unmarshalModelSelected(data string) utils.ModelSelected {
	var ms utils.ModelSelected
	if data == "" {
		ms.EcoMode = true
		return ms
	}
	// Detect whether eco_mode was present in the stored JSON. Legacy rows
	// (written before the field existed) don't carry the key and should
	// default to true rather than to Go's zero-value false.
	var probe map[string]json.RawMessage
	hasKey := false
	if json.Unmarshal([]byte(data), &probe) == nil {
		_, hasKey = probe["eco_mode"]
	}
	json.Unmarshal([]byte(data), &ms)
	if !hasKey {
		ms.EcoMode = true
	}
	return ms
}

func marshalMiniApp(ma *utils.MiniApp) *string {
	if ma == nil {
		return nil
	}
	s := marshalJSON(ma)
	return &s
}

func unmarshalMiniApp(data *string) *utils.MiniApp {
	if data == nil {
		return nil
	}
	var ma utils.MiniApp
	json.Unmarshal([]byte(*data), &ma)
	return &ma
}

func marshalMessageContent(mc utils.MessageContent) *string {
	if mc.IsZero() {
		return nil
	}
	data, err := mc.MarshalJSON()
	if err != nil || string(data) == "null" {
		return nil
	}
	s := string(data)
	return &s
}

func unmarshalMessageContent(data *string) utils.MessageContent {
	if data == nil {
		return utils.MessageContent{}
	}
	var mc utils.MessageContent
	mc.UnmarshalJSON([]byte(*data))
	return mc
}

func marshalToolCalls(tcs []utils.ToolCall) *string {
	if len(tcs) == 0 {
		return nil
	}
	s := marshalJSON(tcs)
	return &s
}

func unmarshalToolCalls(data *string) []utils.ToolCall {
	if data == nil {
		return nil
	}
	var tcs []utils.ToolCall
	json.Unmarshal([]byte(*data), &tcs)
	return tcs
}

func marshalModel(m utils.Model) *string {
	if m.Name == "" && len(m.Params) == 0 && len(m.Tools) == 0 {
		return nil
	}
	s := marshalJSON(m)
	return &s
}

func unmarshalModel(data *string) utils.Model {
	if data == nil {
		return utils.Model{}
	}
	var m utils.Model
	json.Unmarshal([]byte(*data), &m)
	return m
}
