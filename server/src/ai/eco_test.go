package ai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// checkpointMsg builds the assistant half of an eco checkpoint pair — a
// synthetic assistant message whose only tool call is the checkpoint tool.
func checkpointMsg() utils.Message {
	return utils.Message{
		Role: "assistant",
		Content: utils.MessageContent{},
		ToolCalls: []utils.ToolCall{{
			ID:   "eco_regression",
			Type: "function",
			Function: utils.FunctionCall{
				Name:      db.EcoCheckpointToolName,
				Arguments: "{}",
			},
		}},
	}
}

// toolMsg builds the tool half of the checkpoint pair.
func toolMsg() utils.Message {
	return utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent("checkpoint summary"),
		ToolCallID: "eco_regression",
		Name:       db.EcoCheckpointToolName,
	}
}

// TestFilterCheckpointsForRequest_BudgetCapLastElementIsPanicRegression
// reproduces the crash that killed the server whenever "Debug WordPress
// Slowness" hit the eco tail-cap path:
//
//	panic: runtime error: index out of range [N] with length N
//	github.com/azukaar/plurality/src/ai.filterCheckpointsForRequest
//	    server/src/ai/eco.go:89
//
// The budget walker runs backwards from the end and, when the LAST message
// alone tips the budget over maxTailChars, increments start one past the
// end (== len(tail)). That is a valid *slice* start but an invalid index,
// and the subsequent "back up to a user boundary" loop dereferenced
// tail[start] before the fix. This test pins the scenario: tail length is
// not huge (the bug is about position, not size).
func TestFilterCheckpointsForRequest_BudgetCapLastElementIsPanicRegression(t *testing.T) {
	msgs := []utils.Message{
		// One checkpoint pair (assistant + tool half).
		checkpointMsg(),
		toolMsg(),
		// One live user turn that alone exceeds the 100k-char budget.
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}

	// Must not panic. With eco ON the tail after the checkpoint is the
	// single oversized user message: the walker breaks at start==1, then
	// increments to start==2 == len(tail) (one-past-end), clamps down to
	// the last element, stays on the user boundary, and keeps the message.
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != 1 {
		t.Fatalf("expected 1 kept message (the oversized user turn), got %d", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("expected kept role user, got %q", got[0].Role)
	}
}

// TestFilterCheckpointsForRequest_BudgetCapLongTail exercises the normal
// capping path: a long tail where the budget is exceeded somewhere in the
// middle must back up to a user boundary and keep the suffix.
func TestFilterCheckpointsForRequest_BudgetCapLongTail(t *testing.T) {
	var msgs []utils.Message
	msgs = append(msgs, checkpointMsg(), toolMsg())
	// 30 alternating user/assistant turns of ~5k chars each (~150k total),
	// so the cap triggers mid-tail and must resync to a user boundary.
	for i := 0; i < 30; i++ {
		msgs = append(msgs,
			utils.Message{Role: "user", Content: utils.NewTextContent(fmt.Sprintf("user %d %s", i, strings.Repeat("a", 5_000)))},
			utils.Message{Role: "assistant", Content: utils.NewTextContent(fmt.Sprintf("assistant %d %s", i, strings.Repeat("b", 5_000)))},
		)
	}

	got := filterCheckpointsForRequest(msgs, true)
	if len(got) == 0 {
		t.Fatal("expected a non-empty tail")
	}
	// The kept tail must start exactly on a user boundary so tool results
	// never get orphaned.
	if got[0].Role != "user" {
		t.Fatalf("expected kept tail to start with a user message, got %q", got[0].Role)
	}
	// Total kept chars must respect the budget (plus up to one boundary
	// message of slack).
	total := 0
	for _, m := range got {
		total += len(m.TextContent())
	}
	if total > 100_000+5_100 {
		t.Fatalf("kept tail %d chars exceeds budget", total)
	}
}

// TestFilterCheckpointsForRequest_EcoOff drops checkpoint pairs entirely.
func TestFilterCheckpointsForRequest_EcoOff(t *testing.T) {
	msgs := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("hi")},
		checkpointMsg(),
		toolMsg(),
		{Role: "assistant", Content: utils.NewTextContent("hello")},
	}
	got := filterCheckpointsForRequest(msgs, false)
	for _, m := range got {
		if db.IsCheckpointMessage(m) {
			t.Fatalf("checkpoint message leaked through with eco off: %+v", m)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
}

// TestFilterCheckpointsForRequest_TruncatesOversizedSingleMessage ensures a
// single oversized message in the tail (e.g. a multi-MB pasted blob or a
// giant inline attachment dump) is truncated to the per-message bound
// instead of being shipped in full. Before this fix, the tail-cap walker
// re-included the whole message via the user-boundary back-up, so the
// "WordPress Security Review" conversation (1858 msgs, ~4.2MB Cosmos dump
// inline) shipped the blob on every resume and dead-ended with
// context_length_exceeded.
func TestFilterCheckpointsForRequest_TruncatesOversizedSingleMessage(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("x", 150_000))},
	}
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) != 1 {
		t.Fatalf("expected 1 kept message, got %d", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("expected kept role user, got %q", got[0].Role)
	}
	text := got[0].TextContent()
	if len(text) < 90_000 || len(text) >= 150_000 {
		t.Fatalf("expected oversized user message truncated (head kept), got %d chars", len(text))
	}
	if !strings.Contains(text, "truncated by Plurality") {
		t.Fatalf("expected truncation banner in retained content")
	}
}

// TestFilterCheckpointsForRequest_TruncatesOversizedToolResult ensures a
// huge tool result whose parent assistant call is STILL present in the tail
// is truncated (not dropped), so the OpenAI tool_calls/result pairing stays
// valid. Before this fix the giant result was shipped whole and blew the
// context window.
func TestFilterCheckpointsForRequest_TruncatesOversizedToolResult(t *testing.T) {
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("hi")},
		{Role: "assistant", Content: utils.NewTextContent("calling tool"),
			ToolCalls: []utils.ToolCall{{ID: "call_big", Type: "function", Function: utils.FunctionCall{Name: "some_tool", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_big", Name: "some_tool", Content: utils.NewTextContent(strings.Repeat("y", 120_000))},
		{Role: "assistant", Content: utils.NewTextContent("done")},
	}
	got := filterCheckpointsForRequest(msgs, true)
	if len(got) == 0 {
		t.Fatalf("expected a non-empty tail")
	}
	for _, m := range got {
		if m.Role == "tool" && (len(m.TextContent()) < 90_000 || len(m.TextContent()) >= 150_000) {
			t.Fatalf("expected oversized tool result truncated (head kept), got %d chars", len(m.TextContent()))
		}
	}
	// The tool call must still have its result (truncated, never dropped).
	foundCall := false
	foundResult := false
	for _, m := range got {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID == "call_big" {
					foundCall = true
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID == "call_big" {
			foundResult = true
		}
	}
	if !foundCall || !foundResult {
		t.Fatalf("expected kept tool call call_big AND its truncated result (call=%v result=%v)", foundCall, foundResult)
	}
}

// TestFilterCheckpointsForRequest_DropsDanglingOversizedToolResult ensures
// a huge tool result whose parent call was already dropped from the tail is
// removed entirely (pure context waste).
func TestFilterCheckpointsForRequest_DropsDanglingOversizedToolResult(t *testing.T) {
	// A dangling oversized tool result (no matching assistant tool_call in
	// the tail — e.g. left behind by an interrupted stream) must be removed
	// entirely: it has no parent call, so it is pure context waste.
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("hi")},
		{Role: "tool", ToolCallID: "call_orphan", Name: "other_tool", Content: utils.NewTextContent(strings.Repeat("z", 120_000))},
	}
	got := filterCheckpointsForRequest(msgs, true)
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "call_orphan" {
			t.Fatalf("expected dangling oversized tool result call_orphan to be dropped")
		}
	}
}

// TestCompactConversationForContext_TruncatesOversizedMessage ensures the
// context-overflow recovery path (CompactConversationForContext +
// TruncateOversizedMessages) also bounds a single oversized message that
// sits inside the newest keep-window, so the retry cannot fail again with
// context_length_exceeded on the same giant blob.
func TestCompactConversationForContext_TruncatesOversizedMessage(t *testing.T) {
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("sys")},
		{Role: "user", Content: utils.NewTextContent("hi")},
		{Role: "user", Content: utils.NewTextContent(strings.Repeat("q", 250_000))}, // monster inline blob
		{Role: "assistant", Content: utils.NewTextContent("ok")},
	}
	got := CompactConversationForContext(msgs, 30)
	if len(got) != 4 {
		t.Fatalf("expected all 4 messages preserved (truncated, not dropped), got %d", len(got))
	}
	for _, m := range got {
		if m.Role == "user" && len(m.TextContent()) >= 250_000 {
			t.Fatalf("expected oversized user message truncated, got %d chars", len(m.TextContent()))
		}
		if len(m.TextContent()) > 150_000 {
			t.Fatalf("message still oversized after compaction: %d chars", len(m.TextContent()))
		}
	}
}


// TestFilterCheckpointsForRequest_TruncatesOversizedMultipartMessage guards
// the case TextContent() misses: a multi-part tool result whose FIRST part
// is a short preamble and whose LATER text part carries a multi-MB blob.
// Before TotalContentChars existed, truncation sized against the first part
// only and shipped the hidden megabyte payload (the actual "New Chat stops
// working" bug). It must now truncate to the bound and drop the blob part.
func TestFilterCheckpointsForRequest_TruncatesOversizedMultipartMessage(t *testing.T) {
	preamble := "[Attachment att_99, bytes 0-50000 of 4901042]"
	blob := strings.Repeat("B", 4_900_000)
	msgs := []utils.Message{
		checkpointMsg(),
		toolMsg(),
		{Role: "user", Content: utils.NewTextContent("continue")},
		{Role: "tool", ToolCallID: "call_huge", Name: "conversations__retrieve_conversation",
			Content: utils.NewPartsContent([]utils.ContentPart{
				{Type: "text", Text: preamble},
				{Type: "text", Text: blob},
			})},
	}
	got := filterCheckpointsForRequest(msgs, true)
	for _, m := range got {
		if m.Role != "tool" || m.ToolCallID != "call_huge" {
			continue
		}
		// The giant later part must be gone: total retained text stays <= bound+a little.
		total := m.Content.TotalContentChars()
		if total >= 100_000+200 {
			t.Fatalf("expected multi-part oversized tool result truncated, got %d total chars", total)
		}
		if !strings.Contains(m.TextContent(), "truncated by Plurality") {
			t.Fatalf("expected truncation banner in multi-part result")
		}
	}
}
