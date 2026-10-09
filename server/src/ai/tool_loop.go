package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

// RunLLMLoop is the autonomous server-side LLM loop. It runs as a goroutine
// that outlives the HTTP connection. It calls the LLM, processes tool calls,
// and broadcasts all events to connected SSE clients.
//
// The loop continues until:
//   - The LLM returns a response with no tool calls (normal completion)
//   - Client-side tool calls are needed (state set to waiting_for_tool)
//   - The context is canceled (user cancellation or timeout)
func (ar *ActiveRequest) RunLLMLoop(ctx context.Context, conversation utils.Conversation, payload ChatPayload) {
	defer ar.cleanup(ctx)

	utils.Log("[LLMLoop] Starting for conversation %s with %d connected clients", ar.ConversationID, ar.ClientCount())

	for {
		select {
		case <-ar.Ctx.Done():
			ar.flushPartialResponse(ctx, conversation)
			return
		default:
		}

		model := SelectModel(payload.ModelSelected, conversation)
		ar.Model = model

		ar.ResetBuffer()
		ar.BroadcastStatus("typing", "")
		utils.Log("[LLMLoop] Calling LLM with model %s", model.Name)
		response, _, err := SendChatCompletion(ar.Ctx, model, conversation, payload)
		if err != nil {
			if ar.Ctx.Err() != nil {
				ar.flushPartialResponse(ctx, conversation)
				return
			}
			if isContextLengthError(err) {
				// The conversation outgrew the model's context window. Shrink
				// the in-memory history and retry ONCE — otherwise a long
				// conversation (this one hit 381k tokens vs 131k limit)
				// dead-ends into an error/blank UI at workflow end.
				utils.Error("[LLMLoop] context length exceeded — compacting history and retrying", err)
				before := len(conversation.Messages)
				conversation.Messages = CompactConversationForContext(conversation.Messages, 24)
				after := len(conversation.Messages)
				utils.Log("[LLMLoop] compacted conversation %s history %d -> %d messages", ar.ConversationID, before, after)
				if after < before {
					ar.Broadcast(SSEEvent{
						Type:           "text",
						Content:        "(The conversation was too long for the model's context window — older messages were compacted. Retrying …)",
						ConversationID: ar.ConversationID,
					})
					continue
				}
				// Compaction left the history unchanged — fall through to the
				// error path rather than looping forever.
			}
			utils.Error("[LLMLoop] Error calling LLM", err)
			ar.Broadcast(SSEEvent{
				Type:           "error",
				Content:        err.Error(),
				ConversationID: ar.ConversationID,
			})
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}

		// Process the streaming response (always OpenAI format via LiteLLM)
		sp := NewStreamProcessor(ar, model, conversation)
		assistantMessage, err := sp.ProcessStandardStream(ar.Ctx, response.(io.ReadCloser))
		if err != nil {
			utils.Error("[LLMLoop] Error processing stream", err)
			if ar.Ctx.Err() != nil {
				ar.flushPartialResponse(ctx, conversation)
				return
			}
			// The stream ended in an error (e.g. SSE idle timeout after 90s
			// without bytes — see idleTimeoutReader). Do NOT log "Stream
			// complete" and do NOT loop back: mark the turn errored and go
			// idle so the UI shows the failure instead of hanging forever
			// with the conversation stuck in "processing".
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			ar.Broadcast(SSEEvent{
				Type:           "error",
				Content:        err.Error(),
				ConversationID: ar.ConversationID,
			})
			return
		}

		utils.Log("[LLMLoop] Stream complete. Text length: %d, Tool calls: %d", len(ar.TextBuffer.String()), len(assistantMessage.ToolCalls))

		// Refresh conversation from DB (finalizeStream already pushed the assistant message)
		// Use the internal variant so any eco checkpoint pair is included —
		// the LLM context filter (filterCheckpointsForRequest) handles
		// whether to keep or drop it based on the user's eco toggle.
		updatedConversation, err := db.GetConversationByIdInternal(ctx, ar.ConversationID)
		if err != nil {
			utils.Error("[LLMLoop] Error refreshing conversation from DB", err)
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}
		conversation = *updatedConversation
		// Keep ar.ModelSelected in sync with the persisted conversation so
		// BroadcastStatus (which is read by all sidebar clients via the global
		// status stream) carries the latest lock / folder / eco snapshot.
		ar.ModelSelected = conversation.ModelSelected

		// If no tool calls, we're done
		if len(assistantMessage.ToolCalls) == 0 {
			// If a long_task list still has outstanding items, inject a
			// synthetic tool result that reminds the LLM to keep going.
			// Returns true when a reminder was injected — in that case we
			// loop back to the LLM instead of going idle.
			if injectLongTaskReminder(ctx, ar, &conversation) {
				continue
			}

			utils.Log("[LLMLoop] No tool calls, setting idle and broadcasting done")
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			modelSelectedSnap := conversation.ModelSelected
			ar.Broadcast(SSEEvent{
				Type:           "done",
				ConversationID: ar.ConversationID,
				Title:          conversation.Title,
				ModelSelected:  &modelSelectedSnap,
			})

			// Auto-generate title for new conversations
			if conversation.Title == "New Chat" {
				titleMsSnap := conversation.ModelSelected
				go func() {
					title, icon, err := generateTitleAndIcon(ctx, conversation)
					if err != nil {
						utils.Error("[LLMLoop] Auto title generation failed", err)
						return
					}
					utils.Log("[LLMLoop] Auto-generated title for %s: %s", ar.ConversationID, title)
					StatusRegistry.BroadcastToUser(ar.UserID, StatusEvent{
						ConversationID: ar.ConversationID,
						State:          string(utils.StateIdle),
						Title:          title,
						Icon:           icon,
						ModelSelected:  &titleMsSnap,
					})
				}()
			}

			// Eco-mode rolling-checkpoint compaction. Spawned at the end of
			// the LLM loop so it can read the just-recorded assistant
			// prompt_tokens. Uses an independent user-scoped context so a
			// client disconnect doesn't interrupt the summary call.
			go runEcoSummary(userContextFor(ar.UserID), ar.ConversationID)

			return
		}

		// Drop tool calls that already have a tool result in THIS TURN — the
		// stream processor pushes synthetic failure results for tool calls
		// whose args were truncated, so re-dispatching them here would just
		// produce duplicate results in DB. Scope to the turn: providers reuse
		// short tool-call ids ("call_0") across turns, so a whole-history
		// lookup would wrongly treat every old "call_0" result as this turn's
		// result and drop the fresh call — the workflow then re-issues the
		// same call forever (crash loop). Unique minted ids make the
		// turn-scoped lookup exact.
		existingResults := make(map[string]bool, len(assistantMessage.ToolCalls))
		// The last user message demarcates this turn: only tool results
		// persisted after it belong to the calls we just received. Results
		// from earlier turns (same provider id "call_0") must never count.
		for i := len(conversation.Messages) - 1; i >= 0; i-- {
			m := conversation.Messages[i]
			if m.Role == "user" {
				break
			}
			if m.Role == "tool" && m.ToolCallID != "" {
				existingResults[m.ToolCallID] = true
			}
		}
		if len(existingResults) > 0 {
			remaining := make([]utils.ToolCall, 0, len(assistantMessage.ToolCalls))
			for _, tc := range assistantMessage.ToolCalls {
				if !existingResults[tc.ID] {
					remaining = append(remaining, tc)
				}
			}
			assistantMessage.ToolCalls = remaining
		}

		// Categorize tool calls: server-side vs client-side
		serverTools, clientTools := categorizeToolCalls(ar.UserID, assistantMessage.ToolCalls)

		// Strict-validate client tool args against the schemas the client
		// advertised in this request. Invalid calls are short-circuited with
		// a synthetic tool-result so the LLM can self-correct on the next
		// turn, and removed from clientTools so the device never executes
		// them.
		if len(clientTools) > 0 {
			var validClient []utils.ToolCall
			for _, tc := range clientTools {
				schema := schemaForClientTool(tc.Function.Name, payload.ClientSideTools)
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				if len(schema) == 0 {
					validClient = append(validClient, tc)
					continue
				}
				ok, errMsg := ai_tools.ValidateToolCallArgs(tc.Function.Name, schema, args)
				if ok {
					validClient = append(validClient, tc)
					continue
				}

				toolMessage := utils.Message{
					Role:       "tool",
					Content:    utils.NewTextContent(errMsg),
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Timestamp:  time.Now().Format(time.RFC3339),
				}
				ar.Broadcast(SSEEvent{
					Type:           "tool_result",
					ToolCallID:     tc.ID,
					ToolName:       tc.Function.Name,
					ToolResult:     errMsg,
					IsServer:       false,
					ConversationID: ar.ConversationID,
				})
				updatedConv, _, pushErr := db.PushMessage(ctx, conversation, toolMessage)
				if pushErr != nil {
					utils.Error("[LLMLoop] Error saving client-tool validation error to DB", pushErr)
				} else {
					conversation = updatedConv
				}
			}
			clientTools = validClient
		}

		// Split server tools into auto-execute vs needs-approval
		var askServerTools []utils.ToolCall
		if payload.ModelSelected.Text != nil && len(payload.ModelSelected.Text.Tools) > 0 {
			var autoServerTools []utils.ToolCall
			for i := range serverTools {
				mode := payload.ModelSelected.Text.Tools[serverTools[i].Function.Name]
				if mode == "ask" {
					askServerTools = append(askServerTools, serverTools[i])
				} else {
					autoServerTools = append(autoServerTools, serverTools[i])
				}
			}
			serverTools = autoServerTools
		}

		// Execute auto server-side tools
		cancelled := false
		for i := range serverTools {
			tc := &serverTools[i]
			enrichToolCallMetadata(tc)

			var resultContent utils.MessageContent

			select {
			case <-ar.Ctx.Done():
				cancelled = true
				resultContent = utils.NewTextContent("Cancelled by user")
			default:
				ar.BroadcastStatus("tool_use", tc.Function.Name)
				msSnap := conversation.ModelSelected
				ar.Broadcast(SSEEvent{
					Type:           "tool_use",
					ToolCall:       tc,
					IsServer:       true,
					ConversationID: ar.ConversationID,
					ModelSelected:  &msSnap,
				})

				resultContent = executeServerTool(ar.Ctx, ar, *tc, payload)
			}

			// Extract blobs from tool result before saving
			toolMessage := utils.Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			if !cancelled {
				if err := storage.ExtractBlobsFromMessage(ar.UserID, &toolMessage); err != nil {
					utils.Error("[LLMLoop] Error extracting blobs from tool result", err)
				}

				ar.Broadcast(SSEEvent{
					Type:           "tool_result",
					ToolCallID:     tc.ID,
					ToolName:       tc.Function.Name,
					ToolResult:     toolMessage.TextContent(),
					IsServer:       true,
					ConversationID: ar.ConversationID,
				})
			}

			updatedConv, _, pushErr := db.PushMessage(ctx, conversation, toolMessage)
			if pushErr != nil {
				utils.Error("[LLMLoop] Error saving tool result to DB", pushErr)
			} else {
				conversation = updatedConv
			}
		}

		// If a wait tool was called, pause the loop in place until the timer
		// elapses (or the request is cancelled), then inject a 'Timer is done'
		// signal and loop back to the LLM. Done in-place rather than handed
		// off to a separate goroutine so the existing ActiveRequest and its
		// SSE clients stay attached for the resume.
		waitSeconds := 0
		for _, tc := range serverTools {
			if tc.Function.Name == ai_tools.WaitToolID {
				if s := ai_tools.ParseWaitSeconds(tc.Function.Arguments); s > waitSeconds {
					waitSeconds = s
				}
			}
		}
		if waitSeconds > 0 && !cancelled {
			utils.Log("[LLMLoop] wait tool called for %ds — pausing loop in place", waitSeconds)
			if !pauseForWait(ctx, ar, &conversation, waitSeconds) {
				ar.flushPartialResponse(ctx, conversation)
				return
			}
			continue
		}

		// If cancelled, also fill in results for ask-server and client-side tools, then stop
		if cancelled {
			allRemaining := append(askServerTools, clientTools...)
			for i := range allRemaining {
				tc := &allRemaining[i]
				toolMessage := utils.Message{
					Role:       "tool",
					Content:    utils.NewTextContent("Cancelled by user"),
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Timestamp:  time.Now().Format(time.RFC3339),
				}
				updatedConv, _, pushErr := db.PushMessage(ctx, conversation, toolMessage)
				if pushErr != nil {
					utils.Error("[LLMLoop] Error saving cancelled tool result to DB", pushErr)
				} else {
					conversation = updatedConv
				}
			}
			ar.setState(ctx, utils.StateIdle)
			ar.BroadcastStatus("", "")
			return
		}

		// If there are ask-server tools or client-side tools, pause and wait.
		// We stamp the conversation's current ModelSelected onto every
		// tool_use event here so any *other* connected client receives the
		// current lock holder before they'd race to dispatch the client-side
		// tools themselves.
		if len(askServerTools) > 0 || len(clientTools) > 0 {
			msSnap := conversation.ModelSelected
			for i := range askServerTools {
				tc := &askServerTools[i]
				enrichToolCallMetadata(tc)
				ar.Broadcast(SSEEvent{
					Type:           "tool_use",
					ToolCall:       tc,
					IsServer:       true,
					ConversationID: ar.ConversationID,
					ModelSelected:  &msSnap,
				})
			}
			for _, toolCall := range clientTools {
				tc := toolCall
				ar.Broadcast(SSEEvent{
					Type:           "tool_use",
					ToolCall:       &tc,
					IsServer:       false,
					ConversationID: ar.ConversationID,
					ModelSelected:  &msSnap,
				})
			}

			// Use waiting_for_approval if any tool (server or client) is in "ask" mode
			waitState := utils.StateWaitingForTool
			hasAskTools := len(askServerTools) > 0
			if !hasAskTools && payload.ModelSelected.Text != nil {
				for _, tc := range clientTools {
					if payload.ModelSelected.Text.Tools[tc.Function.Name] == "ask" {
						hasAskTools = true
						break
					}
				}
			}
			if hasAskTools {
				waitState = utils.StateWaitingForApproval
			}
			ar.setState(ctx, waitState)
			doneSnap := conversation.ModelSelected
			ar.Broadcast(SSEEvent{
				Type:           "state_change",
				State:          string(waitState),
				ConversationID: ar.ConversationID,
				ModelSelected:  &doneSnap,
			})
			ar.Broadcast(SSEEvent{
				Type:           "done",
				ConversationID: ar.ConversationID,
				ModelSelected:  &doneSnap,
			})
			// Wait for the client to POST tool results / approvals — but not
			// forever. A disconnected or unresponsive client would otherwise
			// strand this conversation in waiting_for_tool / waiting_for_approval
			// indefinitely (observed: "Modbus Template Updates" sat parked for
			// 9+ hours on 2026-09-29→30 because the shell_client__exec result
			// from seq 4274 never arrived). Arm a stall watchdog that
			// synthesizes a timeout result and resumes the loop, so the model
			// can self-correct (e.g. fall back to a server-side tool).
			go armClientToolWatchdog(userContextFor(ar.UserID), ar, &conversation, clientTools)
			return
		}

		// All tools were server-side — loop back to LLM with results
		utils.Log("[LLMLoop] All server tools executed, looping back to LLM")
	}
}

// clientToolStallTimeout is how long the LLM loop waits for a client-side
// tool result before the stall watchdog synthesizes a timeout and resumes.
const clientToolStallTimeout = 90 * time.Second

// armClientToolWatchdog runs in the background while the LLM loop is parked
// waiting for the client to POST results for client-side tools. If the client
// never delivers (disconnected tab, missed event, offline device), the
// conversation would otherwise sit in waiting_for_tool / waiting_for_approval
// indefinitely — the "Modbus Template Updates" stall on 2026-09-29→30 (a
// shell_client__exec result from seq 4274 never arrived; the conversation
// sat parked 21:42 → 06:55 until a fresh user message poked it back to life).
//
// On timeout it pushes a synthetic "Client did not respond within 90s" tool
// result for every still-outstanding client tool call, then re-launches the
// LLM loop exactly like a client tool_results POST would (HandleChat /
// HandleApprove path). The model sees the timeout and can self-correct —
// e.g. fall back to a server-side tool — instead of stalling forever.
func armClientToolWatchdog(ctx context.Context, ar *ActiveRequest, conv *utils.Conversation, clientTools []utils.ToolCall) {
	select {
	case <-time.After(clientToolStallTimeout):
	case <-ar.Ctx.Done():
		return // cancelled or superseded
	}

	// Re-check: only synthesize results for tool calls that still have no
	// matching tool row in the DB (the client may have delivered late).
	outstanding := make([]utils.ToolCall, 0, len(clientTools))
	for _, tc := range clientTools {
		if !hasToolResult(ctx, conv.ID, tc.ID) {
			outstanding = append(outstanding, tc)
		}
	}
	if len(outstanding) == 0 {
		return // everything arrived — nothing to do
	}

	utils.Log("[LLMLoop] client tool stall: %d tool call(s) unanswered for %v — synthesizing timeout result", len(outstanding), clientToolStallTimeout)

	// Load the freshest conversation (may contain late client results or a
	// new user message from another tab).
	cur, err := db.GetConversationByIdInternal(ctx, conv.ID)
	if err != nil {
		utils.Error("[LLMLoop] client tool watchdog: could not reload conversation", err)
		return
	}

	now := time.Now().Format(time.RFC3339)
	for _, tc := range outstanding {
		// Skip if a tool row for this call appeared while we were waiting.
		if hasToolResult(ctx, cur.ID, tc.ID) {
			continue
		}
		toolMsg := utils.Message{
			Role:       "tool",
			Content:    utils.NewTextContent("Client did not respond within " + clientToolStallTimeout.String() + ". The client-side tool was not executed. If this task still needs doing, retry with a server-side tool (e.g. system_tools__shell_exec) or ask the user."),
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Timestamp:  now,
		}
		updated, _, pushErr := db.PushMessage(ctx, *cur, toolMsg)
		if pushErr != nil {
			utils.Error("[LLMLoop] client tool watchdog: failed to push timeout result", pushErr)
			continue
		}
		cur = &updated
	}

	// Re-launch the loop (mirrors HandleChat / HandleApprove). The old
	// ActiveRequest was already cleaned up when its loop returned, so a new
	// one is safe to register.
	model := SelectModel(ar.ModelSelected, *cur)
	persistCtx := userContextFor(ar.UserID)
	newAR := NewActiveRequest(cur.ID, ar.UserID, model, ar.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(persistCtx)
	newAR.Ctx = cancelCtx
	newAR.Cancel = cancelFunc
	// Atomically claim the resume slot: if the client (or a new /chat)
	// already registered a fresh request while we were synthesizing results,
	// that loop is authoritative and this watchdog must stand down.
	if !RequestRegistry.SetIfAbsent(cur.ID, newAR) {
		utils.Log("[LLMLoop] client tool watchdog: conversation %s already has an active request — standing down", cur.ID)
		return
	}
	if err := db.UpdateConversationState(persistCtx, cur.ID, utils.StateProcessing); err != nil {
		utils.Error("[LLMLoop] client tool watchdog: error setting state", err)
	}

	utils.Log("[LLMLoop] client tool watchdog: resuming conversation %s with %d timeout result(s)", cur.ID, len(outstanding))
	newAR.RunLLMLoop(persistCtx, *cur, ChatPayload{
		ConversationID:  cur.ID,
		ModelSelected:   ar.ModelSelected,
		ClientSideTools: nil,
		AvailableSkills: nil,
	})
}

// hasToolResult reports whether a tool row with the given tool_call_id exists
// for the conversation (used to avoid double-synthesizing late-arriving results).
func hasToolResult(ctx context.Context, conversationID string, toolCallID string) bool {
	conv, err := db.GetConversationByIdInternal(ctx, conversationID)
	if err != nil {
		return false
	}
	for _, m := range conv.Messages {
		if m.Role == "tool" && m.ToolCallID == toolCallID {
			return true
		}
	}
	return false
}

// enrichToolCallMetadata populates the Loading and IconURL fields from the tool registry.
func enrichToolCallMetadata(tc *utils.ToolCall) {
	tool, ok := ai_tools.GetTool(tc.Function.Name)
	if ok {
		tc.Loading = tool.LoadingString
		tc.IconURL = tool.IconURL
	}
}

// schemaForClientTool returns the raw JSON Schema for a client-side tool, as
// advertised by the client in this request. Empty result skips validation —
// the client either declared no schema or didn't include the tool at all
// (which is itself an LLM hallucination but handled downstream).
func schemaForClientTool(name string, defs []utils.FunctionToolsRequest) json.RawMessage {
	for i := range defs {
		if defs[i].Name != name {
			continue
		}
		params := defs[i].Parameters
		if params == nil {
			params = defs[i].InputSchema
		}
		if params == nil {
			return nil
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return nil
		}
		return raw
	}
	return nil
}

// categorizeToolCalls splits tool calls into server-side (server-side MCP or
// builtin registry) and client-side. MCP is checked first so that a namespaced
// MCP tool (e.g. "foo__search_web") is not mistakenly matched to a builtin
// with the same bare name. Registry entries with ClientSide: true (e.g.
// shell_client__exec) are routed to the client even though GetTool finds them.
func categorizeToolCalls(userID string, toolCalls []utils.ToolCall) (serverTools, clientTools []utils.ToolCall) {
	for _, tc := range toolCalls {
		if mcp.IsMCPTool(userID, tc.Function.Name) {
			serverTools = append(serverTools, tc)
			continue
		}
		if builtin, isBuiltin := ai_tools.GetTool(tc.Function.Name); isBuiltin {
			if builtin.ClientSide {
				clientTools = append(clientTools, tc)
			} else {
				serverTools = append(serverTools, tc)
			}
			continue
		}
		clientTools = append(clientTools, tc)
	}
	return
}

// executeServerTool runs a server-side tool.
// MCP tools are checked first (so namespaced MCP names don't collide with
// builtins); builtin tools come from ai_tools.Registry.
func executeServerTool(ctx context.Context, ar *ActiveRequest, toolCall utils.ToolCall, payload ChatPayload) (result utils.MessageContent) {
	defer func() {
		if r := recover(); r != nil {
			utils.Error(fmt.Sprintf("[LLMLoop] Tool %s panicked: %v", toolCall.Function.Name, r), nil)
			result = utils.NewTextContent(fmt.Sprintf("Error: tool %s encountered an internal error", toolCall.Function.Name))
		}
	}()

	// Check MCP first — namespaced names must match exactly before we
	// try stripping the namespace for a builtin lookup.
	if mcp.IsMCPTool(ar.UserID, toolCall.Function.Name) {
		args := toolCall.Function.Arguments
		if args == "" {
			args = "{}"
		}
		if schema, ok := mcp.GetToolInputSchema(ar.UserID, toolCall.Function.Name); ok {
			if ok, errMsg := ai_tools.ValidateToolCallArgs(toolCall.Function.Name, schema, args); !ok {
				return utils.NewTextContent(errMsg)
			}
		}
		return mcp.CallTool(ctx, ar.UserID, toolCall.Function.Name, args, ar.ConversationID)
	}

	tool, ok := ai_tools.GetTool(toolCall.Function.Name)
	if !ok {
		return utils.NewTextContent("Tool not found: " + toolCall.Function.Name)
	}

	args := toolCall.Function.Arguments
	if args == "" {
		args = "{}"
	}

	// Validate args against the declared schema BEFORE any server-side
	// injection (e.g. generate_image's _fs_read_enabled / model below) so
	// internal flags don't accidentally trip the unknown-param check.
	if schema := ai_tools.SchemaForBuiltinTool(toolCall.Function.Name); len(schema) > 0 {
		if ok, errMsg := ai_tools.ValidateToolCallArgs(toolCall.Function.Name, schema, args); !ok {
			return utils.NewTextContent(errMsg)
		}
	}

	// Fetch current conversation — all tools receive it. Tools may inspect
	// raw history (read_message etc.), so use the internal variant; if a
	// tool returns conversation slices back to the LLM it's already inside
	// the LLM context so eco checkpoints are fine.
	conv, convErr := db.GetConversationByIdInternal(ctx, ar.ConversationID)
	if convErr != nil {
		utils.Error("[LLMLoop] Error loading conversation for tool execution", convErr)
		return utils.NewTextContent("Error: could not load conversation data")
	}

	// Inject user's selected image model and the fs_read gate into args.
	// The gate flag tells the image tool's Exec it's allowed to honor a 'path'
	// argument — required because the schema-only gate in GetRequests can be
	// bypassed by a hallucinated parameter.
	if tool.ToolID == "generate_image" {
		var argsMap map[string]string
		if err := json.Unmarshal([]byte(args), &argsMap); err == nil {
			if argsMap == nil {
				argsMap = make(map[string]string)
			}
			// Edit vs generate: an attachment/path means there's an input image,
			// so swap in the image-edit model (mirrors SelectModel's text→vision
			// swap). Fall back to the generation model if no edit model is set.
			isEdit := argsMap["attachment"] != "" || argsMap["path"] != ""
			if isEdit && payload.ModelSelected.ImageEdit != nil && payload.ModelSelected.ImageEdit.Name != "" {
				argsMap["model"] = payload.ModelSelected.ImageEdit.Name
			} else if payload.ModelSelected.ImageGen != nil && payload.ModelSelected.ImageGen.Name != "" {
				argsMap["model"] = payload.ModelSelected.ImageGen.Name
			}
			if payload.ModelSelected.Text != nil {
				if _, ok := payload.ModelSelected.Text.Tools["filesystem_server"+mcp.NamespaceSeparator+"fs_read"]; ok {
					argsMap["_fs_read_enabled"] = "true"
				}
			}
			if newArgs, err := json.Marshal(argsMap); err == nil {
				args = string(newArgs)
			}
		}
	}

	return tool.Exec(ctx, args, *conv)
}

// setState updates the conversation state in both the ActiveRequest and the DB.
func (ar *ActiveRequest) setState(ctx context.Context, state utils.ConversationState) {
	ar.State = state
	if err := db.UpdateConversationState(ctx, ar.ConversationID, state); err != nil {
		utils.Error("[LLMLoop] Error updating conversation state", err)
	}
}

// flushPartialResponse saves whatever text was produced so far to the DB on cancellation.
func (ar *ActiveRequest) flushPartialResponse(ctx context.Context, conversation utils.Conversation) {
	text := ar.TextBuffer.String()
	if text == "" {
		ar.setState(ctx, utils.StateIdle)
		ar.BroadcastStatus("", "")
		return
	}

	partialMessage := utils.Message{
		Role:      "assistant",
		Content:   utils.NewTextContent(text),
		Timestamp: time.Now().Format(time.RFC3339),
		Model:     ar.Model,
	}
	db.PushMessage(ctx, conversation, partialMessage)
	ar.setState(ctx, utils.StateIdle)
	ar.BroadcastStatus("", "")
}

// cleanup removes the ActiveRequest from the registry and closes all clients.
func (ar *ActiveRequest) cleanup(ctx context.Context) {
	RequestRegistry.Remove(ar.ConversationID)
	ar.CloseAllClients()
}

// injectLongTaskReminder checks the latest long_task state and, when there are
// unfinished tasks and the reminder cap hasn't been hit, appends a synthetic
// assistant + tool message pair to the conversation so the LLM sees the open
// list on the next iteration. Returns true when a reminder was injected.
//
// The synthetic pair is necessary because OpenAI rejects a 'tool' role
// message that doesn't immediately follow an 'assistant' message with a
// matching tool_calls[].id — so we manufacture both halves.
func injectLongTaskReminder(ctx context.Context, ar *ActiveRequest, conv *utils.Conversation) bool {
	state := ai_tools.ReadLongTaskState(conv.Messages)
	if !state.HasOutstanding() {
		return false
	}
	// A paused list intentionally suppresses the nudge loop — the AI chose
	// to defer the work and any reminder mechanism (cron, user follow-up)
	// is handled outside this loop.
	if state.Paused {
		utils.Log("[LLMLoop] long_task is paused (reason=%q), skipping reminder with %d open task(s)", state.PauseReason, openCount(state))
		return false
	}
	if state.RemindersUsed >= ai_tools.LongTaskMaxReminders {
		// Reminder cap reached with outstanding tasks. Do NOT go silently
		// idle: the LLM would keep answering with empty text (no tool calls,
		// no reminder, no work) and the conversation looks dead to the user.
		// Instead, auto-pause the list and surface an explanatory message so
		// the user sees why work stopped and the LLM can resume/clear the
		// list on the next turn.
		return pauseBlockedLongTask(ctx, ar, conv, state)
	}

	state.RemindersUsed++
	open := openTitles(state)
	state.Nudge = fmt.Sprintf(
		"You still have %d outstanding task(s): %s. Continue working on them. If they can no longer be completed, call long_task with operation='clear'. If you are unable to complete them right now, call long_task with operation='pause' to pause them and set a reminder for later. You can also use \"wait\" tool if the blocker is time eg. waiting for process to complete, or a particular time.",
		len(open),
		strings.Join(open, "; "),
	)
	resultBody := ai_tools.FormatStateJSON(state)

	syntheticID := fmt.Sprintf("longtask_reminder_%d", time.Now().UnixNano())
	tc := utils.ToolCall{
		ID:   syntheticID,
		Type: "function",
		Function: utils.FunctionCall{
			Name:      "long_task",
			Arguments: `{"operation":"_reminder"}`,
		},
	}
	enrichToolCallMetadata(&tc)

	assistantMsg := utils.Message{
		Role:      "assistant",
		ToolCalls: []utils.ToolCall{tc},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(resultBody),
		ToolCallID: syntheticID,
		Name:       "long_task",
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	updated, _, err := db.PushMessage(ctx, *conv, assistantMsg)
	if err != nil {
		utils.Error("[LLMLoop] long_task reminder: failed to push assistant msg", err)
		return false
	}
	updated, _, err = db.PushMessage(ctx, updated, toolMsg)
	if err != nil {
		utils.Error("[LLMLoop] long_task reminder: failed to push tool msg", err)
		return false
	}
	*conv = updated

	ar.Broadcast(SSEEvent{
		Type:           "tool_use",
		ToolCall:       &tc,
		IsServer:       true,
		ConversationID: ar.ConversationID,
	})
	ar.Broadcast(SSEEvent{
		Type:           "tool_result",
		ToolCallID:     syntheticID,
		ToolName:       "long_task",
		ToolResult:     resultBody,
		IsServer:       true,
		ConversationID: ar.ConversationID,
	})

	utils.Log("[LLMLoop] long_task reminder injected (%d/%d), %d open task(s)", state.RemindersUsed, ai_tools.LongTaskMaxReminders, len(open))
	return true
}

// pauseBlockedLongTask resolves a long_task list whose reminder budget has
// been exhausted while tasks are still open. Left as-is, every subsequent
// turn ends with an empty completion ("Text length: 0, Tool calls: 0") and
// the conversation silently goes idle — the user sees no work and no error.
//
// This auto-pauses the list with a reason and pushes a plain assistant
// message explaining what happened, so:
//   - the user sees a real (non-empty) final message instead of a dead turn
//   - the persisted state flips to Paused, which stops the nudge loop
//   - on the next user turn the LLM sees the paused list in context and can
//     call long_task resume/clear (or the user can tell it to)
//
// Returns false: the caller should proceed to idle (the explanatory message
// is already in the conversation).
func pauseBlockedLongTask(ctx context.Context, ar *ActiveRequest, conv *utils.Conversation, state ai_tools.LongTaskState) bool {
	open := openTitles(state)

	state.Paused = true
	state.PauseReason = "reminder cap reached; tasks left open across " + strconv.Itoa(state.RemindersUsed) + " reminders"
	state.Nudge = ""
	resultBody := ai_tools.FormatStateJSON(state)

	assistantMsg := utils.Message{
		Role: "assistant",
		Content: utils.NewTextContent(fmt.Sprintf(
			"The following open task(s) could not be completed after %d reminders: %s. The task list has been automatically paused. Say \"continue\" to resume, or give new instructions.",
			state.RemindersUsed,
			strings.Join(open, "; "),
		)),
		Timestamp: time.Now().Format(time.RFC3339),
	}
	// Persist the paused state as a synthetic long_task tool result so the
	// LLM sees it on the next turn, plus the explanatory assistant message.
	syntheticID := fmt.Sprintf("longtask_paused_%d", time.Now().UnixNano())
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(resultBody),
		ToolCallID: syntheticID,
		Name:       "long_task",
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	updated, _, err := db.PushMessage(ctx, *conv, assistantMsg)
	if err == nil {
		updated, _, err = db.PushMessage(ctx, updated, toolMsg)
	}
	if err != nil {
		utils.Error("[LLMLoop] long_task auto-pause: failed to persist state", err)
		return false
	}
	*conv = updated

	ar.Broadcast(SSEEvent{
		Type:           "text",
		Content:        assistantMsg.TextContent(),
		ConversationID: ar.ConversationID,
	})
	ar.Broadcast(SSEEvent{
		Type:           "tool_result",
		ToolCallID:     syntheticID,
		ToolName:       "long_task",
		ToolResult:     resultBody,
		IsServer:       true,
		ConversationID: ar.ConversationID,
	})

	utils.Log("[LLMLoop] long_task auto-paused after reminder cap (%d/%d), %d open task(s): %s", state.RemindersUsed, ai_tools.LongTaskMaxReminders, len(open), strings.Join(open, "; "))
	return false
}

func openTitles(s ai_tools.LongTaskState) []string {
	out := make([]string, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		if !t.Done {
			out = append(out, t.ID+": "+t.Title)
		}
	}
	return out
}

func openCount(s ai_tools.LongTaskState) int {
	n := 0
	for _, t := range s.Tasks {
		if !t.Done {
			n++
		}
	}
	return n
}

// pauseForWait sleeps the LLM loop in place until the requested duration has
// elapsed, then appends a synthetic assistant + tool message pair carrying a
// "Timer is done" result so the LLM sees a fresh signal on the next iteration.
//
// Returns true on normal completion, false if the request was cancelled
// mid-wait (in which case the caller should bail out of the loop).
//
// Running in-place keeps the same ActiveRequest, goroutine and SSE clients
// attached across the wait — anything broadcast here streams live to the
// open chat. The synthetic pair mirrors the long_task reminder shape because
// OpenAI rejects a 'tool' role message that doesn't immediately follow an
// 'assistant' message with a matching tool_call.id, so we manufacture both
// halves.
func pauseForWait(ctx context.Context, ar *ActiveRequest, conv *utils.Conversation, seconds int) bool {
	select {
	case <-time.After(time.Duration(seconds) * time.Second):
	case <-ar.Ctx.Done():
		return false
	}

	resumeID := fmt.Sprintf("wait_resume_%d", time.Now().UnixNano())
	resumeTC := utils.ToolCall{
		ID:   resumeID,
		Type: "function",
		Function: utils.FunctionCall{
			Name:      ai_tools.WaitToolID,
			Arguments: fmt.Sprintf(`{"seconds":%d,"_resume":true}`, seconds),
		},
	}
	enrichToolCallMetadata(&resumeTC)

	resumeBody, _ := json.Marshal(map[string]interface{}{
		"completed":       true,
		"elapsed_seconds": seconds,
		"status":          "Timer is done. Continue with your task.",
	})

	assistantMsg := utils.Message{
		Role:      "assistant",
		ToolCalls: []utils.ToolCall{resumeTC},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	toolMsg := utils.Message{
		Role:       "tool",
		Content:    utils.NewTextContent(string(resumeBody)),
		ToolCallID: resumeID,
		Name:       ai_tools.WaitToolID,
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	updated, _, err := db.PushMessage(ctx, *conv, assistantMsg)
	if err != nil {
		utils.Error("[Wait] failed to push synthetic assistant message", err)
		return false
	}
	updated, _, err = db.PushMessage(ctx, updated, toolMsg)
	if err != nil {
		utils.Error("[Wait] failed to push synthetic tool result", err)
		return false
	}
	*conv = updated

	ar.Broadcast(SSEEvent{
		Type:           "tool_use",
		ToolCall:       &resumeTC,
		IsServer:       true,
		ConversationID: ar.ConversationID,
	})
	ar.Broadcast(SSEEvent{
		Type:           "tool_result",
		ToolCallID:     resumeID,
		ToolName:       ai_tools.WaitToolID,
		ToolResult:     string(resumeBody),
		IsServer:       true,
		ConversationID: ar.ConversationID,
	})

	return true
}
