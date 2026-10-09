package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

func mkMsg(role string, content string, toolName string) utils.Message {
	m := utils.Message{Role: role}
	if role == "assistant" {
		m.Content = utils.NewTextContent(content)
	}
	if role == "tool" {
		m.Content = utils.NewTextContent(content)
		m.Name = toolName
	}
	return m
}

func TestFilterCheckpointsNoOOB(t *testing.T) {
	var msgs []utils.Message
	msgs = append(msgs, utils.Message{
		Role:      "assistant",
		ToolCalls: []utils.ToolCall{{ID: "eco_x", Type: "function", Function: utils.FunctionCall{Name: db.EcoCheckpointToolName, Arguments: "{}"}}},
	})
	msgs = append(msgs, mkMsg("tool", "summary", db.EcoCheckpointToolName))
	seq := 0
	for i := 0; i < 2100; i++ {
		role := "tool"
		if seq%4 == 0 {
			role = "user"
		} else if seq%4 == 1 {
			role = "assistant"
		}
		content := "small payload"
		if i%500 == 42 {
			content = strings.Repeat("X", 200*1024)
		}
		msgs = append(msgs, mkMsg(role, content, "system_tools__shell_exec"))
		seq++
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC: %v", r)
		}
	}()
	for i := 0; i < 100; i++ {
		out := filterCheckpointsForRequest(msgs, true)
		if len(out) == 0 {
			t.Fatalf("empty tail returned")
		}
		total := 0
		for _, m := range out {
			total += len(m.TextContent())
		}
		if total > 160*1024+(200*1024) {
			t.Fatalf("tail chars too big: %d", total)
		}
	}
	out := filterCheckpointsForRequest(msgs, false)
	if len(out) == 0 {
		t.Fatalf("eco off returned empty")
	}
}

func TestFilterCheckpointsTiny(t *testing.T) {
	msgs := []utils.Message{
		mkMsg("user", "hi", ""),
		mkMsg("assistant", "hello", ""),
	}
	out := filterCheckpointsForRequest(msgs, true)
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d", len(out))
	}
}

// TestFilterNoCheckpointHugeHistory_WordPressRegression pins the exact
// failure mode of the "WordPress Security Review" conversation: eco on, NO
// checkpoint ever written (all eco summaries 500'd on the 131k fast model),
// and a ~770k-token raw history. Before the fix, filterCheckpointsForRequest
// returned the WHOLE history when no checkpoint existed, so every turn died
// with context_length_exceeded. After the fix it must return a bounded tail
// (<= maxTailChars + one truncated oversized message).
func TestFilterNoCheckpointHugeHistory_WordPressRegression(t *testing.T) {
	var msgs []utils.Message
	// 1900 tool results ~50k chars each ≈ 95M chars ≈ 24M tokens — far
	// beyond any window, and no checkpoint anywhere.
	for i := 0; i < 1900; i++ {
		role := "tool"
		if i%4 == 0 {
			role = "user"
		} else if i%4 == 1 {
			role = "assistant"
		}
		content := strings.Repeat("y", 50_000)
		if i == 0 {
			content = strings.Repeat("z", 200_000) // one truly giant blob
		}
		msgs = append(msgs, mkMsg(role, content, "system_tools__shell_exec"))
	}
	out := filterCheckpointsForRequest(msgs, true)
	if len(out) == 0 {
		t.Fatal("empty tail")
	}
	total := 0
	max := 0
	for _, m := range out {
		l := len(m.TextContent())
		total += l
		if l > max {
			max = l
		}
	}
	t.Logf("filtered %d msgs -> %d msgs, %d chars, max msg %d", len(msgs), len(out), total, max)
	// Budget + one truncated 100k bound (capTail's truncateOversizedTail).
	if total > maxTailChars+100_000+200_000 {
		t.Fatalf("tail chars too big for context: %d", total)
	}
	if max > 100_000 {
		t.Fatalf("oversized message not truncated: %d", max)
	}
}
