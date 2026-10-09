package search

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync/atomic"

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
			if err := db.QueryRow(`SELECT seq FROM messages WHERE id = ?`, msgID).Scan(&matchedSeq); err != nil && err != sql.ErrNoRows {
				utils.Debug("[Search] getMatchedMessages: failed to load seq for message %d: %v", msgID, err)
			}
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
		if err := rows.Scan(&m.Role, &content, &timestamp); err != nil {
			utils.Debug("[Search] getMatchedMessages: row scan failed for conversation %s: %v", r.ConversationID, err)
			return messages
		}
		if content.Valid {
			m.Content = truncate(content.String, 500)
		}
		if timestamp.Valid {
			m.Timestamp = timestamp.String
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		utils.Debug("[Search] getMatchedMessages: row iteration error for conversation %s: %v", r.ConversationID, err)
		return messages
	}

	// Fallback: if no matched message found, return first user message
	if len(messages) == 0 {
		var m MatchedMessage
		var content, timestamp sql.NullString
		if err := db.QueryRow(
			`SELECT role, content, timestamp FROM messages WHERE conversation_id = ? AND role = 'user' ORDER BY seq LIMIT 1`,
			r.ConversationID,
		).Scan(&m.Role, &content, &timestamp); err != nil && err != sql.ErrNoRows {
			utils.Debug("[Search] getMatchedMessages: fallback query failed for conversation %s: %v", r.ConversationID, err)
		}
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

// ---------------------------------------------------------------------------
// Serialized embedding worker.
//
// With SetMaxOpenConns(1) the server has ONE SQLite connection per user DB.
// Spawning a goroutine per message to run EmbedAndStore (the pre-fix path)
// means N concurrent db.Exec calls queue up waiting for that single
// connection. Combined with PushMessage's own conn usage and the eco
// compaction's GetCheckpoint, this pile-up wedges the pool: goroutine dumps
// showed StoreEmbedding, GetCheckpoint and getConversationFromDB ALL waiting
// at database/sql.(*DB).conn while connectionOpener itself blocked — a
// virtual deadlock that hangs the backend and reads as a crash (conversation
// "Modbus Template Updates" stalled 10:44→10:57 & 11:03→11:03:47 on
// 2026-09-29).
//
// Fix: route every embed through ONE worker goroutine so at most ONE embed
// runs at a time, and message pushes / live reads are never starved by a
// backlog of concurrent embeds. The store happens on the shared pool, which
// is safe here: a single serialized consumer can never deadlock against
// itself.
// ---------------------------------------------------------------------------
type embedJob struct {
	db             *sql.DB
	liteLLMBaseURL string
	sourceType     string
	sourceID       string
	text           string
}

var (
	embedQueue         = make(chan embedJob, 256)
	embedWorkerStarted atomic.Bool
)

func embedWorker() {
	for job := range embedQueue {
		EmbedAndStore(job.db, job.liteLLMBaseURL, job.sourceType, job.sourceID, job.text)
	}
}

// InitEmbedWorker starts the single background embed consumer. Call once at
// boot (index.go). Safe to call multiple times.
func InitEmbedWorker() {
	if embedWorkerStarted.CompareAndSwap(false, true) {
		go embedWorker()
	}
}

// EnqueueEmbed queues an embedding job for the worker. Never blocks: if the
// queue is full the job is dropped (embeddings are best-effort search index
// enrichment; a dropped embed is far cheaper than a wedged backend).
func EnqueueEmbed(db *sql.DB, liteLLMBaseURL string, sourceType string, sourceID string, text string) {
	if liteLLMBaseURL == "" || text == "" || len(text) < 3 {
		return
	}
	select {
	case embedQueue <- embedJob{db, liteLLMBaseURL, sourceType, sourceID, text}:
	default:
		utils.Log("[Search] embed queue full — dropping embedding for %s/%s", sourceType, sourceID)
	}
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
