package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// waitToolName is the tool ID of the built-in wait tool (ai_tools.WaitToolID).
// Kept as a local constant to avoid importing the ai_tools package here (db is
// imported by ai, so a reverse dependency would create an import cycle).
const waitToolName = "wait"

// RecoverStuckConversations is a startup-time safety net for conversations
// that were left in a non-idle state by a previous server lifetime.
//
// The LLM loop (ai.RunLLMLoop) is the only code that resets a conversation to
// StateIdle after a turn finishes — including after a `wait` tool parks the
// loop in place with an in-memory timer (pauseForWait). If the server is
// restarted or crashes while a conversation is mid-turn (waiting on `wait`,
// executing a long tool, streaming a response), that goroutine dies with the
// process and nothing ever flips the state back. The result is a conversation
// that is stuck at "processing" forever: the UI shows it as active, the user
// sees no progress and cannot reliably tell whether the agent is still
// working.
//
// This function runs once at boot, after the per-user SQLite databases are
// available, and repairs exactly that situation:
//
//   - For every conversation whose persisted state is not StateIdle, it loads
//     the last persisted message.
//   - If that message is a `wait` tool result with a `wake_at` timestamp (the
//     agent was parked on a timer), and the timer has already elapsed, it
//     pushes the same synthetic assistant + tool "Timer is done" pair that
//     pauseForWait would have produced, so the history stays consistent and a
//     later user turn sees the completed timer instead of a dangling "Waiting
//     180s" note.
//   - In all other cases (timer still pending, non-wait tool in flight, or a
//     stream that was cut off) it simply marks the conversation idle. The
//     in-memory loop that was doing the work is gone; keeping the state at
//     "processing" would leave a ghost.
//
// It is deliberately conservative: this is a consistency repair, not an
// attempt to resume work. A user can always send a new message to kick off a
// fresh loop.
func RecoverStuckConversations(ctx context.Context) {
	entries, err := os.ReadDir(userDataPath)
	if err != nil {
		utils.Error("[Recovery] cannot read user data path: %v", err)
		return
	}

	recovered := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		userID := entry.Name()
		if strings.HasPrefix(userID, ".") {
			continue
		}
		// Only touch users that already have a conversations.db. The lazy
		// GetUserDB would create a DB for anything we pass it, so this also
		// avoids materialising DBs for non-user directories.
		dbPath := filepath.Join(userDataPath, userID, "conversations.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		recovered += recoverUserConversations(ctx, userID)
	}

	utils.Log("[Recovery] scan complete: %d stuck conversation(s) repaired across user databases", recovered)
}

// recoverUserConversations repairs all stuck conversations for one user and
// returns how many were touched.
func recoverUserConversations(ctx context.Context, userID string) int {
	userCtx := context.WithValue(ctx, "userID", userID)
	convs, err := GetActiveConversationsForUser(userCtx)
	if err != nil {
		utils.Error("[Recovery] cannot list active conversations for user %s", err, userID)
		return 0
	}

	recovered := 0
	for i := range convs {
		conv := &convs[i]
		full, err := getConversationFromDBFromCtx(userCtx, conv.ID)
		if err != nil {
			utils.Error("[Recovery] cannot load conversation %s for user %s", err, conv.ID, userID)
			continue
		}
		if len(full.Messages) == 0 {
			// Empty conversation with a non-idle state is trivially stale.
			if err := UpdateConversationState(userCtx, conv.ID, utils.StateIdle); err != nil {
				utils.Error("[Recovery] failed to idle empty conversation %s", err, conv.ID)
				continue
			}
			utils.Log("[Recovery] idled empty stuck conversation %s (user %s)", conv.ID, userID)
			recovered++
			continue
		}

		last := full.Messages[len(full.Messages)-1]
		if maybeResumeElapsedWait(userCtx, full, &last) {
			utils.Log("[Recovery] handled wait-parked conversation %s (user %s)", conv.ID, userID)
			recovered++
			continue
		}

		// Plain orphan: the loop that owned this turn is gone.
		if err := UpdateConversationState(userCtx, conv.ID, utils.StateIdle); err != nil {
			utils.Error("[Recovery] failed to idle stuck conversation %s", err, conv.ID)
			continue
		}
		utils.Log("[Recovery] idled stuck conversation %s (user %s, last role=%s, last tool=%s)", conv.ID, userID, last.Role, last.Name)
		recovered++
	}
	return recovered
}

// maybeResumeElapsedWait detects a conversation that was parked on a `wait`
// tool whose in-memory timer died with the server. Returns true if it handled
// the conversation (state was updated either way).
//
// It only fires when the last message is the *result* of a wait tool call
// (role "tool", name "wait", JSON body with wake_at/wait_seconds). If the
// wake-up time has passed, it injects the same synthetic assistant + tool
// "Timer is done" pair pauseForWait produces, then idles. If the wake-up time
// is still in the future — or the parsed fields are missing — it falls back to
// a plain idle so the conversation is never left as a ghost.
func maybeResumeElapsedWait(ctx context.Context, conv *utils.Conversation, last *utils.Message) bool {
	if last.Role != "tool" || last.Name != waitToolName {
		return false
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(last.TextContent()), &data); err != nil {
		utils.Debug("[Recovery] conversation %s: wait result is not JSON, idling as orphan", conv.ID)
		// Fall through to plain-idle handling in the caller by returning
		// false.
		return false
	}
	wakeAtStr, _ := data["wake_at"].(string)
	if wakeAtStr == "" {
		return false
	}
	wakeAt, err := time.Parse(time.RFC3339, wakeAtStr)
	if err != nil {
		utils.Debug("[Recovery] conversation %s: unparseable wake_at %q", conv.ID, wakeAtStr)
		return false
	}
	waitSeconds := 0
	if v, ok := data["wait_seconds"].(float64); ok {
		waitSeconds = int(v)
	}
	if waitSeconds <= 0 {
		utils.Debug("[Recovery] conversation %s: wait result without wait_seconds", conv.ID)
		return false
	}

	if time.Now().UTC().Before(wakeAt) {
		// Timer hasn't fired yet, but the loop that would fire it is dead.
		// We cannot keep the promise of an automatic resume, so idle it and
		// note why. A fresh user message will start a normal turn.
		utils.Log("[Recovery] conversation %s: wait timer would still be running (wake_at %s) but server restarted — idling instead of resuming", conv.ID, wakeAtStr)
		if err := UpdateConversationState(ctx, conv.ID, utils.StateIdle); err != nil {
			utils.Error("[Recovery] failed to idle waiting conversation %s", err, conv.ID)
			return false
		}
		return true
	}

	// Timer elapsed while we were down: synthesize the completion pair so the
	// history is exactly what the user would have seen had the loop survived.
	resumeID := fmt.Sprintf("recovery_wait_resume_%d", time.Now().UnixNano())
	assistantMsg := utils.Message{
		Role: "assistant",
		ToolCalls: []utils.ToolCall{{
			ID:   resumeID,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      waitToolName,
				Arguments: fmt.Sprintf(`{"seconds":%d,"_resume":true}`, waitSeconds),
			},
		}},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	resumeBody, _ := json.Marshal(map[string]interface{}{
		"completed":       true,
		"elapsed_seconds": waitSeconds,
		"status":          "Timer is done. Continue with your task.",
	})
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(string(resumeBody)),
		ToolCallID: resumeID,
		Name:       waitToolName,
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	updated, _, err := PushMessage(ctx, *conv, assistantMsg)
	if err != nil {
		utils.Error("[Recovery] failed to push wait resume assistant message for %s", err, conv.ID)
		return false
	}
	if _, _, err := PushMessage(ctx, updated, toolMsg); err != nil {
		utils.Error("[Recovery] failed to push wait resume tool message for %s", err, conv.ID)
		return false
	}
	if err := UpdateConversationState(ctx, conv.ID, utils.StateIdle); err != nil {
		utils.Error("[Recovery] failed to idle resumed conversation %s", err, conv.ID)
		return false
	}
	return true
}

// getConversationFromDBFromCtx loads a full conversation (with messages) for
// a user context, mirroring GetConversationByIdInternal but with context
// plumbing that does not require the HTTP request middleware.
func getConversationFromDBFromCtx(ctx context.Context, id string) (*utils.Conversation, error) {
	userID, ok := ctx.Value("userID").(string)
	if !ok {
		return nil, fmt.Errorf("user ID not found in request context")
	}
	db, err := GetUserDB(userID)
	if err != nil {
		return nil, err
	}
	return getConversationFromDB(db, id)
}
