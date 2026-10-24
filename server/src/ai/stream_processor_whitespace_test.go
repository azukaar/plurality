package ai

import (
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// buildAssistantMessage must never persist whitespace-only text as assistant
// content — that is what rendered an empty "assistant" bubble and made the
// conversation look halted while the tool loop kept working in the background.
// Leading/trailing whitespace around real text is trimmed for cleanliness.
func TestBuildAssistantMessageDropsWhitespaceOnlyText(t *testing.T) {
	newReq := func(initial string) *ActiveRequest {
		ar := NewActiveRequest("test-conv", "test-user", utils.Model{}, utils.ModelSelected{})
		if initial != "" {
			ar.TextBuffer.WriteString(initial)
		}
		return ar
	}

	cases := []struct {
		name     string
		buffer   string
		wantText string // "" means content must be absent
	}{
		{"empty buffer", "", ""},
		{"whitespace only", "\n\n", ""},
		{"whitespace with tabs/spaces", "  \t \n", ""},
		{"leading whitespace trimmed", "\n\nHello", "Hello"},
		{"trailing whitespace trimmed", "Hello\n\n", "Hello"},
		{"wrapped text trimmed", "  Hello world  \n", "Hello world"},
		{"real text preserved", "Hello world", "Hello world"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ar := newReq(tc.buffer)
			sp := &StreamProcessor{request: ar}
			msg := sp.buildAssistantMessage()

			if tc.wantText == "" {
				if msg.Content.TextContent() != "" {
					t.Fatalf("expected no content, got %q", msg.Content.TextContent())
				}
			} else if got := msg.Content.TextContent(); got != tc.wantText {
				t.Fatalf("expected content %q, got %q", tc.wantText, got)
			}
		})
	}
}

// broadcastText must skip whitespace-only chunks: they would otherwise be
// pushed to SSE clients (empty bubble) and accumulate into the buffer that
// buildAssistantMessage persists.
func TestBroadcastTextSkipsWhitespaceOnlyChunks(t *testing.T) {
	ar := NewActiveRequest("test-conv", "test-user", utils.Model{}, utils.ModelSelected{})
	sp := &StreamProcessor{request: ar}

	sp.broadcastText("\n\n")
	sp.broadcastText("  \t")
	if got := sp.request.TextBuffer.String(); got != "" {
		t.Fatalf("whitespace-only chunks must not accumulate in buffer, got %q", got)
	}

	// Real text is still accumulated (and persisted via buildAssistantMessage).
	sp.broadcastText("Hello")
	if got := sp.request.TextBuffer.String(); got != "Hello" {
		t.Fatalf("expected buffer %q, got %q", "Hello", got)
	}
}
