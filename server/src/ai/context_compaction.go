package ai

import (
	"fmt"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// isContextLengthError classifies an LLM error as "the request exceeded the
// model's context window". Providers report this as context_length_exceeded,
// maximum context length, Input too long, max_prompt_tokens, exceeded the
// context length. When this happens Plurality must NOT dead-end into an
// error/blank UI: the history needs to be compacted and the call retried.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	needles := []string{"context_length_exceeded", "maximum context length", "input too long", "max_prompt_tokens", "context window", "exceeded the context length", "context length is"}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// CompactConversationForContext drops whole old turns (tool + assistant) from
// the tail of the history so the next request fits the context window. It
// keeps the most recent `keep` messages and always keeps the leading system
// message if present. Returns a new slice; does not touch persisted state.
func CompactConversationForContext(messages []utils.Message, keep int) []utils.Message {
	if keep <= 0 {
		keep = 30
	}
	// Always keep the leading system (system-prompt) message if present.
	startIdx := 0
	kept := make([]utils.Message, 0, keep)
	if len(messages) > 0 && messages[0].Role == "system" {
		kept = append(kept, messages[0])
		startIdx = 1
	}
	// Keep the newest `keep` messages after the (optional) system head.
	tailStart := startIdx
	if len(messages)-startIdx > keep {
		tailStart = len(messages) - keep
	}
	for i := tailStart; i < len(messages); i++ {
		kept = append(kept, messages[i])
	}
	// Drop tool results whose originating assistant call was trimmed, so the
	// LLM never sees a tool result without its assistant call.
	kept = dropDanglingToolResults(kept)

	// Hard-per-message bound: a single giant message (multi-MB pasted blob,
	// huge tool result, inline attachment dump) can exceed the context
	// window BY ITSELF. CompactConversationForContext only drops whole
	// turns, so a huge message inside the newest `keep`-window would still
	// be shipped as-is and the retry would fail again with
	// context_length_exceeded (observed: "WordPress Security Review", a
	// ~4.2MB Cosmos config dump inline; every resume dead-ended). Truncate
	// such messages down to a safe per-message bound so the retry can
	// actually succeed.
	return TruncateOversizedMessages(kept)
}

// dropDanglingToolResults removes "tool" messages whose ToolCallID has no
// matching assistant tool_calls inside the (trimmed) slice.
func dropDanglingToolResults(msgs []utils.Message) []utils.Message {
	ids := make(map[string]bool, 0)
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			for _, tc := range msgs[i].ToolCalls {
				ids[tc.ID] = true
			}
		}
	}
	out := make([]utils.Message, 0, len(msgs))
	for i := range msgs {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID != "" && !ids[msgs[i].ToolCallID] {
			continue
		}
		out = append(out, msgs[i])
	}
	return out
}


// maxCompactMessageChars is the per-message hard bound applied when the
// in-memory history is compacted for a context-overflow retry. The eco
// tail-cap uses the same bound so the two paths agree.
const maxCompactMessageChars = 100_000

// TruncateOversizedMessages bounds every message that would be sent to the
// provider to maxCompactMessageChars characters. Messages are NOT dropped
// (their role/turn boundary must be preserved so tool-call pairing stays
// consistent); only their text body is truncated, with a clear banner, so
// the model still sees the head of the content.
//
// Used by CompactConversationForContext so a context-overflow retry cannot
// fail again on a single oversized message. Returns a NEW slice; the input
// is not mutated.
func TruncateOversizedMessages(messages []utils.Message) []utils.Message {
	out := make([]utils.Message, 0, len(messages))
	for _, m := range messages {
		total := m.Content.TotalContentChars()
		if total > maxCompactMessageChars {
			txt := ""
			for _, p := range m.Content.ContentParts() {
				if p.Type == "text" {
					txt += p.Text
				}
			}
			truncLen := len(txt)
			if truncLen > maxCompactMessageChars {
				truncLen = maxCompactMessageChars
			}
			trunc := txt[:truncLen]
			utils.Log("[Compaction] truncating oversized message (%d chars -> %d)", total, maxCompactMessageChars)
			m.Content = utils.NewTextContent(trunc +
				"\n\n[message truncated by Plurality: original length " +
				fmt.Sprintf("%d", total) +
				" chars across all content parts exceeded the per-message context budget]")
		}
		out = append(out, m)
	}
	return out
}
