package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/utils"
)

// summaryInFlight is the global single-flight gate for eco-mode summary
// jobs. Only one summary runs across the whole server at any time;
// conversations that miss their window pick up on the next user turn.
var summaryInFlight atomic.Bool

// userContextFor builds a background context carrying just the userID
// claim that the db package looks for. Used by the eco summary goroutine
// so a client disconnect or LLM-loop end doesn't cancel it mid-summary.
func userContextFor(userID string) context.Context {
	return context.WithValue(context.Background(), "userID", userID)
}

// filterCheckpointsForRequest applies the eco-mode visibility rules to the
// message slice that's about to be sent to the LLM.
//
//   - ecoOn=false: drop every checkpoint pair entirely; the LLM sees the
//     full pre-summary history. Use when the user has explicitly disabled
//     eco for this conversation.
//   - ecoOn=true: keep the checkpoint pair and drop every message before
//     it. The LLM sees a system prompt, the checkpoint summary, and the
//     live tail. There is at most one checkpoint per conversation.
//
// The system prompt is prepended elsewhere (in SendChatCompletion) and is
// not part of `messages` here.
func filterCheckpointsForRequest(messages []utils.Message, ecoOn bool) []utils.Message {
	if !ecoOn {
		return db.FilterOutCheckpoints(messages)
	}

	// Find the LAST checkpoint pair (assistant half) — the rolling design
	// keeps only one, but defensive against any stale extras.
	lastCheckpointIdx := -1
	for i, m := range messages {
		if m.Role == "assistant" && db.IsCheckpointMessage(m) {
			lastCheckpointIdx = i
		}
	}
	// No checkpoint yet (eco never successfully compacted this conversation):
	// a giant raw history must NOT be shipped whole — that is exactly how
	// "WordPress Security Review" (1866 msgs, ~770k tokens) kept dying with
	// context_length_exceeded. Fall through to the same budget-capped tail
	// so even a never-compacted conversation is bounded on every request.
	if lastCheckpointIdx < 0 {
		return capTail(messages)
	}
	tail := messages[lastCheckpointIdx:]

	// Safety net: even with eco on, the live tail after the last checkpoint
	// can grow unbounded. Never ship a context-busting tail to the provider.
	// Message-count caps are not enough: observed a 600-message tail that was
	// ~400k tokens because every tool result is huge (this conversation: avg
	// 2.5k chars/tool msg). Cap by BUDGET (sum of content length) instead,
	// always starting on a user message so the model still has a complete turn
	// boundary to respond to. The checkpoint summary before the cap keeps the
	// dropped middle coherent.
	return capTail(tail)
}

// maxTailChars is the character budget for the live tail that is sent to
// the provider (after any checkpoint summary). ~100k chars ≈ 25-30k tokens,
// safe for any 128k-context model, leaving room for system + output.
const maxTailChars = 100_000

// capTail bounds a message slice to the live-tail budget so a conversation
// can never ship a context-busting history to the provider. It walks
// backwards from the end, accumulating the total content size, and keeps
// the newest messages that fit the budget, always starting on a user
// message (so the model still has a complete turn boundary to respond to
// and tool results whose parent call is dropped are never orphaned).
// Individual oversized messages are then truncated/dropped by
// truncateOversizedTail.
//
// This is applied BOTH when a checkpoint exists (live tail after it) and
// when there is NO checkpoint yet — a never-compacted conversation with a
// huge raw history must still be bounded on every request, or every turn
// dies with context_length_exceeded (observed: "WordPress Security
// Review", 1866 msgs / ~770k tokens / no checkpoint, eco on).
func capTail(tail []utils.Message) []utils.Message {
	if len(tail) == 0 {
		return tail
	}
	count := 0
	chars := 0
	var start int
	start = len(tail) - 1
	for start >= 0 {
		chars += tail[start].Content.TotalContentChars()
		count++
		if chars > maxTailChars {
			start++
			break
		}
		if start == 0 {
			break
		}
		start--
	}
	if start < 0 {
		start = 0
	}
	// The budget walker can break with start == len(tail) (it incremented
	// past the last in-range index to mark "cap from here"). That is a
	// VALID slice start — tail[len:] is the empty slice — but it is NOT a
	// valid index to dereference. Clamp it down to the last element first
	// so the boundary back-up loop below can never index out of range.
	// (Observed panic: "index out of range [N] with length N" on this
	// conversation's ~3500-message tail.)
	if start > len(tail)-1 {
		start = len(tail) - 1
	}
	// Always back up to a user boundary so we never start mid-turn (which
	// would orphan tool results whose parent is in the dropped section).
	for start > 0 && tail[start].Role != "user" {
		start--
	}
	// The budget must also bound INDIVIDUAL oversized messages. A single
	// huge message (a multi-MB pasted blob, a giant tool result, an inline
	// attachment dump) cannot be removed by the tail cap above — the walker
	// breaks at it and then re-includes it via the user-boundary back-up —
	// so a conversation that contains one keeps shipping it on every turn
	// and eventually dies with context_length_exceeded (observed: the
	// "WordPress Security Review" conversation, 1858 msgs, with a ~4.2MB
	// Cosmos config dump inline; every "continue" resumed into a provider
	// context error and the workflow dead-ended mid-work). Apply per-message
	// bounds to whatever tail is actually kept below.
	if start > 0 {
		utils.Log("[Eco] capping tail by budget: kept %d msgs / %d chars (start idx %d) for context safety", len(tail)-start, chars, start)
		tail = tail[start:]
	}
	return truncateOversizedTail(tail)
}

// maxKeepTailChars bounds how many characters of a tail message are kept
// when a single message is too large to fit the budget whole. It preserves
// the head of the message so the model still sees what the text was about.
// (The tail-cap walker above uses the same overall budget; this is the
// per-message bound so one giant message can never blow the context window
// on its own.)
const maxKeepTailChars = 100_000

// truncateOversizedTail applies per-message bounds to the tail slice before
// it is shipped to the provider:
//
//   - a "tool" message whose body is oversized is DROPPED entirely (its
//     parent assistant call was already dropped or is itself being
//     truncated, so a dangling huge result is pure context waste);
//   - any other oversized message (user/assistant) is truncated to
//     maxKeepTailChars with a clear banner, so the model still sees the
//     head of the message and the message role (the turn boundary) is
//     preserved — a 4MB pasted blob can never exceed the context window by
//     itself.
//
// Returning a NEW slice; the input is not mutated.
func truncateOversizedTail(tail []utils.Message) []utils.Message {
	if len(tail) == 0 {
		return tail
	}
	// Which assistant tool_calls survive in the trimmed tail? Only drop an
	// oversized tool result when NO surviving assistant call references it
	// (i.e. the call was dropped too). If the call is still present, we must
	// keep the result (truncated) or OpenAPI rejects the dangling
	// tool_calls/result pairing.
	keptCalls := make(map[string]bool)
	for _, m := range tail {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				keptCalls[tc.ID] = true
			}
		}
	}
	out := make([]utils.Message, 0, len(tail))
	for i := range tail {
		m := tail[i]
		total := m.Content.TotalContentChars()
		if total <= maxKeepTailChars {
			out = append(out, m)
			continue
		}
		if m.Role == "tool" && !keptCalls[m.ToolCallID] {
			utils.Log("[Eco] dropping oversized tool result (%d chars) from tail", total)
			continue
		}
		// Multi-part message whose TOTAL size blows the bound. Keep the first
		// text part (up to the bound, with a banner) and drop the rest so the
		// hidden multi-MB later parts can never reach the provider.
		txt := ""
		for _, p := range m.Content.ContentParts() {
			if p.Type == "text" {
				txt += p.Text
			}
		}
		truncLen := len(txt)
		if truncLen > maxKeepTailChars {
			truncLen = maxKeepTailChars
		}
		trunc := txt[:truncLen]
		m.Content = utils.NewTextContent(trunc +
			"\n\n[message truncated by Plurality: original length " +
			fmt.Sprintf("%d", total) +
			" chars across all content parts exceeded the per-message context budget]")
		out = append(out, m)
	}
	return out
}

// lastAssistantPromptTokens scans the conversation backwards and returns
// the prompt_tokens value of the most recent assistant message that
// reported usage. Returns 0 if no such message exists yet (brand-new
// conversation on its first user turn).
func lastAssistantPromptTokens(messages []utils.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
			return messages[i].PromptTokens
		}
	}
	return 0
}

// findCutoffIndex returns the index in `messages` that marks the start of
// the live tail to keep — everything strictly before this index goes into
// the new checkpoint summary. messages[cutoff..] is preserved; the
// returned index always sits at a clean turn boundary (no message
// splitting).
//
// Algorithm (walking forward from oldest):
//   - targetDrop = lastPromptTokens − targetTokens. This is how many
//     cumulative tokens we want to shave off the head.
//   - PromptTokens on an assistant message is already cumulative — it's
//     the size of the prompt at the moment that assistant spoke, i.e.
//     system + all preceding messages.
//   - The first assistant whose PromptTokens ≥ targetDrop is the one
//     whose turn pushed the running total past the threshold. Compact
//     through the end of that assistant's turn; cutoff is the next
//     message (typically the next user turn, after any trailing tool
//     results).
//   - If even the last non-final assistant doesn't cross targetDrop, fall
//     back to ending the compaction right before the final assistant's
//     turn so we still make progress.
//   - The cutoff must move past any existing checkpoint pair so we don't
//     loop on the same range.
func findCutoffIndex(messages []utils.Message, lastPromptTokens, targetTokens int, existingCheckpointEndIdx int) int {
	if lastPromptTokens <= targetTokens {
		return -1
	}
	targetDrop := lastPromptTokens - targetTokens

	lastAsstIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			lastAsstIdx = i
			break
		}
	}
	if lastAsstIdx < 0 {
		return -1
	}

	minCutoff := existingCheckpointEndIdx + 1
	if minCutoff < 1 {
		minCutoff = 1
	}

	// Walk from oldest forward. PromptTokens is cumulative, so the first
	// assistant with PT ≥ targetDrop is where we've crossed. But we must
	// pick the LAST such assistant (closest to the end): picking the first
	// one makes cutoff land right after the old checkpoint, so on the next
	// eco tick cutoff < minCutoff and compaction bails forever — leaving
	// the live tail to grow unbounded (observed: one checkpoint at seq 139,
	// then 3,740 raw messages with no further compaction).
	//
	// Walking farthest-forward maximises how much history each compact
	// folds into the summary, so cutoff strictly advances every tick.
	crossingAsst := -1
	lastCrossing := -1
	for i, m := range messages {
		if i >= lastAsstIdx {
			break
		}
		if m.Role != "assistant" || m.PromptTokens <= 0 {
			continue
		}
		if m.PromptTokens >= targetDrop {
			if crossingAsst < 0 {
				crossingAsst = i
			}
			lastCrossing = i
		}
	}
	// Prefer the farthest-forward crossing (strictly past the old
	// checkpoint) so cutoff advances; if none exists beyond the
	// checkpoint, fall back to the plain first crossing.
	if lastCrossing >= 0 {
		crossingAsst = lastCrossing
	}

	// Fallback: nobody crossed. Use the last non-final assistant so we at
	// least compact everything up to the most recent turn we can safely
	// drop. If there is no such assistant (only the final asst exists),
	// no compaction is possible.
	if crossingAsst < 0 {
		for i := lastAsstIdx - 1; i >= 0; i-- {
			if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
				crossingAsst = i
				break
			}
		}
	}
	if crossingAsst < 0 {
		return -1
	}

	// Cutoff = first message after this assistant's turn. If the chosen
	// assistant had tool_calls, the matching role:"tool" results sit
	// right after it — step past every one so the assistant's tool_calls
	// and their results stay together in the dropped section. We never
	// want a kept tail that starts with an orphan tool result whose
	// parent tool_call lives in the (now-summarised) checkpoint.
	cutoff := crossingAsst + 1
	for cutoff < len(messages) && messages[cutoff].Role == "tool" {
		cutoff++
	}

	if cutoff < minCutoff {
		return -1
	}
	if cutoff >= len(messages) {
		// Would leave nothing live — refuse.
		return -1
	}
	return cutoff
}

// maxSummaryInputChars bounds the plain-text input fed to the
// summarisation model. The fast model (used for checkpoints) has a
// 131k-token window; feeding it a huge raw history (observed: ~770k tokens
// on "WordPress Security Review") made every eco compaction fail with
// context_length_exceeded and NO checkpoint was ever written. 80k chars ≈
// 20k tokens leaves ample room for system prompt + output.
const maxSummaryInputChars = 80_000

// renderMessagesForSummary produces a plain-text dump of the given message
// slice suitable for feeding to the summarisation model, bounded to
// maxSummaryInputChars characters. Walks every content part of every
// message and includes inline text — pasted snippets, attached file
// contents that travelled inline, and tool results. Skips image_url parts
// and parts whose Text is a storage path rather than real content
// (post-ExtractBlobsFromMessage, document parts like pdf/docx/xlsx/pptx
// have part.Text rewritten to a URL path, so we surface only the filename
// in that case).
//
// The budget keeps the NEWEST messages (the most recent context the live
// model will lose) up to the cap, so the summary preserves the most
// actionable recent history when the compacted range is enormous; a banner
// notes that older messages were omitted. Never returns a context-busting
// blob to the summary model.
func renderMessagesForSummary(messages []utils.Message) string {
	var b strings.Builder
	budget := maxSummaryInputChars
	// Walk backwards so a huge range keeps its most recent turns; then
	// reverse the kept slice so the output stays chronological.
	var kept []utils.Message
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if db.IsCheckpointMessage(m) {
			continue
		}
		if int64(budget) <= 0 {
			break
		}
		// Estimate this message's rendered size and stop before exceeding
		// the budget (per-message cap keeps one giant message from blowing
		// the whole budget).
		mBytes := m.Content.TotalContentChars()
		if mBytes > perMsgBudget {
			mBytes = perMsgBudget
		}
		if mBytes > budget {
			continue
		}
		budget -= mBytes
		kept = append(kept, m)
	}
	if len(kept) < len(messages) {
		b.WriteString("[NOTE: older messages omitted to fit the summary input budget — the following is the most recent portion of the compacted range; earlier context was dropped by the context-safety cap]\n\n")
	}
	for i := len(kept) - 1; i >= 0; i-- {
		m := kept[i]
		switch m.Role {
		case "user":
			b.WriteString("USER:\n")
			writeContentPartsBudget(&b, m.ContentParts())
			b.WriteString("\n")
		case "assistant":
			b.WriteString("ASSISTANT:\n")
			writeContentPartsBudget(&b, m.ContentParts())
			for _, tc := range m.ToolCalls {
				b.WriteString("[tool_call ")
				b.WriteString(tc.Function.Name)
				b.WriteString("] args=")
				b.WriteString(tc.Function.Arguments)
				b.WriteString("\n")
			}
			b.WriteString("\n")
		case "tool":
			b.WriteString("TOOL_RESULT ")
			b.WriteString(m.Name)
			b.WriteString(":\n")
			writeContentPartsBudget(&b, m.ContentParts())
			b.WriteString("\n")
		}
	}
	return b.String()
}

// writeContentPartsBudget is writeContentParts with a per-message cap: any
// single part is truncated to perMsgBudget chars so one giant tool dump
// (observed: 58KB shell outputs) can never blow the summary input budget.
// The budget is applied conservatively relative to maxSummaryInputChars.
const perMsgBudget = 20_000

func writeContentPartsBudget(b *strings.Builder, parts []utils.ContentPart) {
	for _, p := range parts {
		switch {
		case p.Type == "text" || p.Type == "snippet":
			if p.Text == "" {
				continue
			}
			if p.Filename != "" {
				b.WriteString("[attachment: ")
				b.WriteString(p.Filename)
				b.WriteString("]\n")
			}
			if len(p.Text) > perMsgBudget {
				b.WriteString(p.Text[:perMsgBudget])
				b.WriteString("\n[...truncated by Plurality context-safety: original length ")
				b.WriteString(fmt.Sprintf("%d", len(p.Text)))
				b.WriteString(" chars]\n")
			} else {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		case p.Type == "image_url":
			b.WriteString("[image attachment omitted]\n")
		case docsupport.IsDocumentType(p.Type):
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[document attachment: ")
			b.WriteString(label)
			b.WriteString("] (binary, content not inlined)\n")
		default:
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[binary attachment: ")
			b.WriteString(label)
			b.WriteString("]\n")
		}
	}
}

// writeContentParts serialises a content-part slice for the summary
// input. We whitelist the types we know carry real inline text — anything
// else surfaces as an attachment marker (filename only) so arbitrary
// binaries (.iso, .zip, raw uploads) can never leak data URIs or
// storage paths into the summariser as if they were the user's prose.
//
// Whitelisted as inline text:
//   - "text"     — plain text content part
//   - "snippet"  — user-pasted text/code snippet
//
// Marker only (no inline body):
//   - "image_url"                  — image, omitted
//   - docsupport.IsDocumentType(t) — pdf/docx/xlsx/pptx etc.; stored text
//     is a storage path, not real content
//   - everything else              — generic "file" or unknown type; we
//     can't tell if Text is inline content
//     or a binary reference, so we only
//     surface the filename
func writeContentParts(b *strings.Builder, parts []utils.ContentPart) {
	for _, p := range parts {
		switch {
		case p.Type == "text" || p.Type == "snippet":
			if p.Text == "" {
				continue
			}
			if p.Filename != "" {
				b.WriteString("[attachment: ")
				b.WriteString(p.Filename)
				b.WriteString("]\n")
			}
			b.WriteString(p.Text)
			b.WriteString("\n")
		case p.Type == "image_url":
			b.WriteString("[image attachment omitted]\n")
		case docsupport.IsDocumentType(p.Type):
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[document attachment: ")
			b.WriteString(label)
			b.WriteString("] (binary, content not inlined)\n")
		default:
			// Generic file or unknown type — never inline Text since it
			// could be a data: URI or storage path. Just surface the
			// filename (or type as a last-resort label).
			label := p.Filename
			if label == "" {
				label = p.Type
			}
			b.WriteString("[binary attachment: ")
			b.WriteString(label)
			b.WriteString("]\n")
		}
	}
}

// runEcoSummary is the entry point spawned as a goroutine on every user
// message. It returns quickly without doing work in any of these cases:
//   - global single-flight slot is busy
//   - conversation has eco mode disabled
//   - last prompt_tokens is below the configured trigger
//   - no satisfying cutoff (conversation isn't long enough yet)
//
// When all gates pass, it summarises the oldest turns into a new checkpoint
// pair and atomically swaps out any prior pair via db.ReplaceCheckpoint.
// Errors are logged and swallowed — the next user turn will retry.
func runEcoSummary(ctx context.Context, conversationID string) {
	utils.Log("[Eco] tick for conv %s", conversationID)
	if !summaryInFlight.CompareAndSwap(false, true) {
		utils.Log("[Eco] summary already in flight — skipping conv %s", conversationID)
		return
	}
	defer summaryInFlight.Store(false)

	// Bound every DB access in this goroutine. The single SQLite connection
	// (SetMaxOpenConns(1)) is shared with live reads/writes (message pushes,
	// the embed worker, conversation loads); if the pool is wedged we must
	// fail fast and release the global single-flight slot so normal traffic
	// and future compactions are not starved. The summary LLM call
	// (GenerateCheckpointSummary) runs on its own HTTP client and is not
	// affected by this deadline.
	dbCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	conv, err := db.GetConversationByIdInternal(dbCtx, conversationID)
	if err != nil {
		utils.Error("[Eco] could not load conversation", err)
		return
	}
	if !conv.ModelSelected.EcoMode {
		utils.Log("[Eco] conv %s has eco mode off — skip", conversationID)
		return
	}

	cfg := auth.GetConfig().Eco
	if cfg.TriggerTokens <= 0 || cfg.TargetTokens <= 0 {
		utils.Log("[Eco] eco config disabled (trigger=%d target=%d) — skip", cfg.TriggerTokens, cfg.TargetTokens)
		return
	}

	lastPT := lastAssistantPromptTokens(conv.Messages)
	if lastPT == 0 {
		utils.Log("[Eco] conv %s has no assistant prompt_tokens yet — skip", conversationID)
		return
	}
	if lastPT <= cfg.TriggerTokens {
		utils.Log("[Eco] conv %s lastPT=%d below trigger=%d — skip", conversationID, lastPT, cfg.TriggerTokens)
		return
	}
	utils.Log("[Eco] conv %s lastPT=%d > trigger=%d — compacting toward target=%d", conversationID, lastPT, cfg.TriggerTokens, cfg.TargetTokens)

	// Locate the existing checkpoint (if any) and compute where the new
	// cutoff must land.
	existingCheckpointEndIdx := -1
	var oldPairIDs []int64
	var priorSummary string
	if pair, err := db.GetCheckpoint(dbCtx, conversationID); err == nil && pair != nil {
		priorSummary = pair.Summary
		oldPairIDs = []int64{pair.AssistantID}
		if pair.ToolID != 0 {
			oldPairIDs = append(oldPairIDs, pair.ToolID)
		}
		// existingCheckpointEndIdx is the index in conv.Messages of the
		// tool half of the pair. We find the assistant half by tool-call
		// name match, then add 1 for the tool half.
		for i, m := range conv.Messages {
			if m.Role == "assistant" && db.IsCheckpointMessage(m) {
				existingCheckpointEndIdx = i + 1 // assume tool half follows immediately
				break
			}
		}
	}

	cutoff := findCutoffIndex(conv.Messages, lastPT, cfg.TargetTokens, existingCheckpointEndIdx)
	if cutoff < 0 {
		utils.Log("[Eco] no satisfying cutoff for conv %s (lastPT=%d target=%d, %d messages) — skip", conversationID, lastPT, cfg.TargetTokens, len(conv.Messages))
		return
	}
	utils.Log("[Eco] conv %s cutoff=%d (of %d messages), prior checkpoint end=%d", conversationID, cutoff, len(conv.Messages), existingCheckpointEndIdx)

	// Slice of messages to fold into the new summary: everything strictly
	// between the existing checkpoint and the cutoff.
	startIdx := 0
	if existingCheckpointEndIdx >= 0 {
		startIdx = existingCheckpointEndIdx + 1
	}
	if startIdx >= cutoff {
		return
	}

	excerpt := renderMessagesForSummary(conv.Messages[startIdx:cutoff])
	var input string
	if priorSummary != "" {
		input = "PRIOR CHECKPOINT:\n" + priorSummary + "\n\nNEW MESSAGES:\n" + excerpt
	} else {
		input = excerpt
	}

	textModel, _ := fastShortcutModels()
	summary, err := GenerateCheckpointSummary(input, textModel)
	if err != nil {
		utils.Error("[Eco] checkpoint summary generation failed", err)
		return
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		utils.Error("[Eco] checkpoint summary returned empty", nil)
		return
	}

	// Build the assistant + tool pair. Look up the actual DB seq for the
	// message at the cutoff slice position — gaps can exist after prior
	// checkpoint replacements, so slice index != seq in general.
	insertSeq, err := db.MessageSeqAt(dbCtx, conversationID, cutoff)
	if err != nil {
		utils.Error("[Eco] could not resolve insert seq", err)
		return
	}
	toolCallID := "eco_" + randomHex(8)
	now := time.Now().Format(time.RFC3339)

	assistantMsg := utils.Message{
		Role:      "assistant",
		Content:   utils.MessageContent{},
		Timestamp: now,
		ToolCalls: []utils.ToolCall{{
			ID:   toolCallID,
			Type: "function",
			Function: utils.FunctionCall{
				Name:      db.EcoCheckpointToolName,
				Arguments: "{}",
			},
		}},
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(summary),
		ToolCallID: toolCallID,
		Name:       db.EcoCheckpointToolName,
		Timestamp:  now,
	}

	if err := db.ReplaceCheckpoint(dbCtx, conversationID, oldPairIDs, assistantMsg, toolMsg, insertSeq); err != nil {
		utils.Error("[Eco] persisting checkpoint failed", err)
		return
	}
	utils.Log("[Eco] checkpoint written for conv %s (cutoff=%d, lastPT=%d → kept tail ≈%d tokens)", conversationID, cutoff, lastPT, lastPT-tokensBeforeCutoff(conv.Messages, cutoff))
}

// tokensBeforeCutoff is a logging helper — it returns the prompt_tokens
// value of the first assistant message at or after the cutoff, which
// approximates "tokens of system + everything before cutoff". Returns 0 if
// no assistant follows.
func tokensBeforeCutoff(messages []utils.Message, cutoff int) int {
	for i := cutoff; i < len(messages); i++ {
		if messages[i].Role == "assistant" && messages[i].PromptTokens > 0 {
			return messages[i].PromptTokens
		}
	}
	return 0
}

// randomHex mirrors db.GenerateID but with a caller-chosen byte width. Used
// for synthesising tool_call_id values that won't collide with real LLM
// output (those are model-generated strings).
func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
