package ai

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/azukaar/plurality/src/utils"
)

func TestFilterRealConversation(t *testing.T) {
	dbPath := os.Getenv("PLURALITY_TEST_DB")
	if dbPath == "" {
		dbPath = "/app/users-data/sam/conversations.db"
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("no real DB at %s — integration-only", dbPath)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	rows, err := db.Query(`SELECT seq, role, content, tool_calls, tool_call_id, name FROM messages WHERE conversation_id='dcd29e939307fe63b9f073ef' ORDER BY seq`)
	if err != nil { t.Fatal(err) }
	defer rows.Close()
	var msgs []utils.Message
	for rows.Next() {
		var seq int64; var role string; var content, toolCalls, toolCallID, name sql.NullString
		if err := rows.Scan(&seq, &role, &content, &toolCalls, &toolCallID, &name); err != nil { t.Fatal(err) }
		m := utils.Message{Role: role}
		if content.Valid && content.String != "" {
			var c utils.MessageContent
			if err := json.Unmarshal([]byte(content.String), &c); err != nil {
				m.Content = utils.NewTextContent(content.String)
			} else {
				m.Content = c
			}
		}
		if toolCalls.Valid && toolCalls.String != "" {
			_ = json.Unmarshal([]byte(toolCalls.String), &m.ToolCalls)
		}
		if toolCallID.Valid { m.ToolCallID = toolCallID.String }
		if name.Valid { m.Name = name.String }
		msgs = append(msgs, m)
	}
	t.Logf("loaded %d real messages", len(msgs))
	if len(msgs) == 0 {
		t.Skipf("fixture conversation dcd29e... not present in %s — integration-only", dbPath)
	}
	defer func() {
		if r := recover(); r != nil { t.Fatalf("PANIC on real data: %v", r) }
	}()
	out := filterCheckpointsForRequest(msgs, true)
	chars := 0
	for _, m := range out {
		chars += len(m.TextContent())
	}
	t.Logf("filtered to %d msgs, %d chars", len(out), chars)
	if len(out) == 0 { t.Fatal("empty") }
	// No single message may exceed the per-message truncation cap: the
	// upstream filter must not blow a 128k-context provider even when the
	// raw tail is huge.
	const maxKeep = 100_000
	for _, m := range out {
		if len(m.TextContent()) > maxKeep {
			t.Fatalf("message not truncated: %d chars", len(m.TextContent()))
		}
	}
}
