package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/utils"
)

// legacyModelPrefixes were used by an older Go-side naming scheme; the
// canonical names now live in litellm_config.yaml without any prefix.
var legacyModelPrefixes = []string{"ChatGPT/", "Claude/", "Gemini/"}

// stripLegacyPrefix normalises a model name in place by removing any of the
// legacy provider prefixes carried by older conversations.
func stripLegacyPrefix(m *utils.Model) {
	if m == nil {
		return
	}
	for _, p := range legacyModelPrefixes {
		if strings.HasPrefix(m.Name, p) {
			m.Name = strings.TrimPrefix(m.Name, p)
			return
		}
	}
}

// HandleChat is the unified endpoint for sending messages and tool results.
// It creates an ActiveRequest, launches the LLM loop in a goroutine, and
// connects the HTTP client as an SSE listener.
func HandleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body to 100MB
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024*1024)

	var payload ChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Compatibility shim: older conversations may carry "ChatGPT/", "Claude/", or
	// "Gemini/" prefixes from when the Go side maintained its own naming scheme.
	// Strip them so they match the bare names that come from the litellm config.
	stripLegacyPrefix(payload.ModelSelected.Text)
	stripLegacyPrefix(payload.ModelSelected.Vision)
	stripLegacyPrefix(payload.ModelSelected.ImageGen)
	stripLegacyPrefix(payload.ModelSelected.ImageEdit)

	if payload.ModelSelected.Text == nil || !Models.IsKnown(payload.ModelSelected.Text.Name) {
		utils.Error("[HandleChat] Unknown text model", nil)
		http.Error(w, "Unknown text model", http.StatusBadRequest)
		return
	}
	if payload.ModelSelected.Vision != nil && !Models.IsKnown(payload.ModelSelected.Vision.Name) {
		utils.Error("[HandleChat] Unknown vision model", nil)
		http.Error(w, "Unknown vision model", http.StatusBadRequest)
		return
	}

	partialConversation := utils.Conversation{
		ID:            payload.ConversationID,
		ModelSelected: payload.ModelSelected,
	}
	if payload.ConversationID == "" {
		partialConversation.Title = "New Chat"
	}
	if payload.MiniApp.ID != "" {
		partialConversation.MiniApp = &payload.MiniApp
	}

	userID, _ := r.Context().Value("userID").(string)

	var conversation utils.Conversation

	if len(payload.ToolResults) > 0 {
		// Client is submitting tool results — push each to DB, then resume LLM loop
		for i := range payload.ToolResults {
			payload.ToolResults[i].Timestamp = time.Now().Format(time.RFC3339)
			if err := storage.ExtractBlobsFromMessage(userID, &payload.ToolResults[i]); err != nil {
				utils.Error("[HandleChat] Error extracting blobs from tool result", err)
			}
			updatedConversation, _, err := db.PushMessage(r.Context(), partialConversation, payload.ToolResults[i])
			if err != nil {
				utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			conversation = updatedConversation
		}
	} else if len(payload.Messages) > 0 {
		// New user message
		userMessage := payload.Messages[0]
		userMessage.Timestamp = time.Now().Format(time.RFC3339)
		if err := storage.ExtractBlobsFromMessage(userID, &userMessage); err != nil {
			utils.Error("[HandleChat] Error extracting blobs from user message", err)
		}
		updatedConversation, _, err := db.PushMessage(r.Context(), partialConversation, userMessage)
		if err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		conversation = updatedConversation
	} else {
		utils.SendHTTPError(w, "Either messages or tool_results must be provided", http.StatusBadRequest)
		return
	}

	// Select model based on content (text vs vision)
	model := SelectModel(payload.ModelSelected, conversation)

	// Create the ActiveRequest with a cancelable context that carries the userID
	persistCtx := CopyUserContext(r)
	activeRequest := NewActiveRequest(conversation.ID, conversation.UserID, model, payload.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(persistCtx)
	activeRequest.Ctx = cancelCtx
	activeRequest.Cancel = cancelFunc
	RequestRegistry.Set(conversation.ID, activeRequest)
	if err := db.UpdateConversationState(persistCtx, conversation.ID, utils.StateProcessing); err != nil {
		utils.Error("[HandleChat] Error setting conversation state", err)
	}

	// Connect this HTTP client as SSE listener
	SetSSEHeaders(w)
	sseClient := NewSSEClient(w)
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	activeRequest.AddClient(sseClient)

	// Launch the LLM loop in a goroutine that outlives this HTTP connection.
	// The eco-mode rolling-checkpoint summary is spawned at the END of the
	// LLM loop (in tool_loop.go) so it can read the just-recorded assistant
	// message's prompt_tokens.
	go activeRequest.RunLLMLoop(persistCtx, conversation, payload)

	// Block until the client disconnects or the stream ends
	select {
	case <-sseClient.Done:
		// Stream ended normally or client disconnected
	case <-r.Context().Done():
		// HTTP connection closed by client
		activeRequest.RemoveClient(sseClient)
	}
}

// HandleStreamReconnect allows a client to reconnect to an active conversation's SSE stream.
func HandleStreamReconnect(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	conversationID := vars["id"]

	activeRequest := RequestRegistry.Get(conversationID)
	if activeRequest == nil {
		// Expected: the client probes this endpoint on every conversation
		// open / switch / app-resume to detect a live stream, so 404 here
		// is normal control flow rather than an error condition.
		http.Error(w, "No active request for this conversation", http.StatusNotFound)
		return
	}

	SetSSEHeaders(w)
	sseClient := NewSSEClient(w)
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	activeRequest.AddClient(sseClient)

	// Send current buffer as catch-up
	currentText := activeRequest.TextBuffer.String()
	if currentText != "" {
		sseClient.Send(SSEEvent{
			Type:           "text",
			Content:        currentText,
			ConversationID: conversationID,
		})
	}

	select {
	case <-sseClient.Done:
	case <-r.Context().Done():
		activeRequest.RemoveClient(sseClient)
	}
}

// HandleCancel cancels an active LLM request for a conversation.
func HandleCancel(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	conversationID := vars["id"]

	activeRequest := RequestRegistry.Get(conversationID)
	if activeRequest == nil {
		utils.Error("[HandleCancel] No active request for conversation", nil, conversationID)
		http.Error(w, "No active request for this conversation", http.StatusNotFound)
		return
	}

	activeRequest.Cancel()
	w.WriteHeader(http.StatusOK)
}

// HandleApprove handles user approval/denial of server-side "ask" tools.
// Executes approved tools, pushes rejection for denied ones, then relaunches the LLM loop.
// Client-side "ask" tools are handled by the client separately via HandleChat + ToolResults.
func HandleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	conversationID := vars["id"]

	var payload struct {
		Approvals []struct {
			ToolCallID string `json:"tool_call_id"`
			ToolName   string `json:"tool_name"`
			Arguments  string `json:"arguments"`
			Approved   bool   `json:"approved"`
		} `json:"approvals"`
		ModelSelected   utils.ModelSelected          `json:"model_selected"`
		ClientSideTools []utils.FunctionToolsRequest `json:"client_side_tools"`
		AvailableSkills []string                     `json:"available_skills,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Verify user owns this conversation. Use the internal variant so the
	// downstream tool-call processing (and the LLM loop it resumes) sees
	// the raw history including any eco checkpoint pair.
	conversation, err := db.GetConversationByIdInternal(r.Context(), conversationID)
	if err != nil {
		utils.SendHTTPError(w, "Conversation not found", http.StatusNotFound)
		return
	}
	userID, _ := r.Context().Value("userID").(string)
	if conversation.UserID != userID {
		utils.SendHTTPError(w, "Unauthorized", http.StatusForbidden)
		return
	}

	// Process each server-side tool approval (MCP checked first, then builtins).
	for _, approval := range payload.Approvals {
		var toolMessage utils.Message
		if approval.Approved {
			args := approval.Arguments
			if args == "" {
				args = "{}"
			}

			if mcp.IsMCPTool(userID, approval.ToolName) {
				resultContent := mcp.CallTool(r.Context(), userID, approval.ToolName, args, conversationID)
				toolMessage = utils.Message{
					Role: "tool", Content: resultContent,
					ToolCallID: approval.ToolCallID, Name: approval.ToolName,
					Timestamp: time.Now().Format(time.RFC3339),
				}
			} else if tool, ok := ai_tools.GetTool(approval.ToolName); ok {
				resultContent := tool.Exec(r.Context(), args, *conversation)
				toolMessage = utils.Message{
					Role: "tool", Content: resultContent,
					ToolCallID: approval.ToolCallID, Name: approval.ToolName,
					Timestamp: time.Now().Format(time.RFC3339),
				}
			} else {
				toolMessage = utils.Message{
					Role: "tool", Content: utils.NewTextContent("Tool not found: " + approval.ToolName),
					ToolCallID: approval.ToolCallID, Name: approval.ToolName,
					Timestamp: time.Now().Format(time.RFC3339),
				}
			}
		} else {
			toolMessage = utils.Message{
				Role: "tool", Content: utils.NewTextContent("Tool call rejected by user."),
				ToolCallID: approval.ToolCallID, Name: approval.ToolName,
				Timestamp: time.Now().Format(time.RFC3339),
			}
		}
		if err := storage.ExtractBlobsFromMessage(userID, &toolMessage); err != nil {
			utils.Error("[HandleApprove] Error extracting blobs", err)
		}
		updatedConv, _, pushErr := db.PushMessage(r.Context(), *conversation, toolMessage)
		if pushErr != nil {
			utils.Error("[HandleApprove] Error saving tool result", pushErr)
		} else {
			*conversation = updatedConv
		}
	}

	// Relaunch LLM loop
	model := SelectModel(payload.ModelSelected, *conversation)
	persistCtx := CopyUserContext(r)
	activeRequest := NewActiveRequest(conversation.ID, conversation.UserID, model, payload.ModelSelected)
	cancelCtx, cancelFunc := context.WithCancel(persistCtx)
	activeRequest.Ctx = cancelCtx
	activeRequest.Cancel = cancelFunc
	RequestRegistry.Set(conversation.ID, activeRequest)
	if err := db.UpdateConversationState(persistCtx, conversation.ID, utils.StateProcessing); err != nil {
		utils.Error("[HandleApprove] Error setting conversation state", err)
	}

	SetSSEHeaders(w)
	sseClient := NewSSEClient(w)
	if sseClient == nil {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	activeRequest.AddClient(sseClient)

	chatPayload := ChatPayload{
		ConversationID:  conversationID,
		ModelSelected:   payload.ModelSelected,
		ClientSideTools: payload.ClientSideTools,
		AvailableSkills: payload.AvailableSkills,
	}
	go activeRequest.RunLLMLoop(persistCtx, *conversation, chatPayload)

	select {
	case <-sseClient.Done:
	case <-r.Context().Done():
		activeRequest.RemoveClient(sseClient)
	}
}

// HandleStatusStream is a long-lived SSE connection that broadcasts compact
// status updates for all of the user's active conversations.
func HandleStatusStream(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value("userID").(string)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	SetSSEHeaders(w)
	flusher, ok := w.(http.Flusher)
	if !ok {
		utils.SendHTTPError(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// Build through the constructor: it initializes the heartbeatStop channel
	// (and its sync.Once) that stopHeartbeat() closes. A raw struct literal
	// here leaves heartbeatStop nil, so every disconnect panics with
	// "close of nil channel" — and when that panic fires inside the LLM loop
	// goroutine (via StatusRegistry.BroadcastToUser → stopHeartbeat) it kills
	// the conversation's loop mid-turn, wedging it in "processing" forever.
	client := NewStatusClient(w, flusher, userID)
	StatusRegistry.Add(client)
	defer StatusRegistry.Remove(client)

	// Send initial catch-up: current state of all active conversations
	for _, ar := range RequestRegistry.GetForUser(userID) {
		client.Send(StatusEvent{
			ConversationID: ar.ConversationID,
			State:          string(ar.State),
		})
	}

	// Block until client disconnects
	select {
	case <-client.Done:
	case <-r.Context().Done():
	}
}

// --- Non-Chat Endpoints (kept from original, adapted to new types) ---

func API_ListConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	conversations, err := db.ListConversations(r.Context())
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response, err := json.Marshal(conversations)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if string(response) == "null" {
		response = []byte("[]")
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func API_HandleConversation(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	switch r.Method {
	case http.MethodDelete:
		if err := db.DeleteConversation(r.Context(), id); err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		conversation, err := db.GetConversationById(r.Context(), id)
		if err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conversation)

	default:
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// fastShortcutModels returns the text and imagegen model names from the "fast"
// shortcut in data/config.json. validateShortcuts guarantees the shortcut and
// both slots always exist.
func fastShortcutModels() (textModel, imageModel string) {
	ms := ShortcutModelSelected("fast")
	return ms.Text.Name, ms.ImageGen.Name
}

// generateTitleAndIcon generates a title and icon for a conversation and persists them.
// Returns the generated title and icon (base64), or an error.
func generateTitleAndIcon(ctx context.Context, conversation utils.Conversation) (string, string, error) {
	// Build a payload from the first 2 messages for title generation
	messages := conversation.Messages
	if len(messages) > 2 {
		messages = messages[:2]
	}

	titlePayload := ""
	for _, message := range messages {
		content := message.TextContent()
		if len(content) > 2000 {
			content = content[:2000]
		}
		titlePayload += message.Role + ": \n"
		titlePayload += content + "\n\n"
	}

	if len(titlePayload) > 500 {
		titlePayload = titlePayload[:500]
	}

	textModel, imageModel := fastShortcutModels()

	title, err := GenerateTitleForMessage(titlePayload, textModel)
	if err != nil {
		return "", "", fmt.Errorf("generating title: %w", err)
	}

	iconPrompt := "Simple illustration, visual, " + title
	iconData, err := GenerateIconForConversation(iconPrompt, imageModel)
	if err != nil {
		return "", "", fmt.Errorf("generating icon: %w", err)
	}

	// Save icon to file storage
	iconURL, err := storage.ExtractIconBlob(conversation.UserID, iconData)
	if err != nil {
		utils.Error("[generateTitleAndIcon] Error saving icon to storage", err)
		iconURL = "" // non-fatal
	}

	if err := db.UpdateConversationMetadata(ctx, conversation.ID, title, iconURL); err != nil {
		return "", "", fmt.Errorf("updating metadata: %w", err)
	}

	return title, iconURL, nil
}

func API_HandleTitleGeneration(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	conversation, err := db.GetConversationById(r.Context(), id)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	title, iconData, err := generateTitleAndIcon(r.Context(), *conversation)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"title": title, "icon": iconData})
}

func GenerateIconForConversation(prompt, model string) (string, error) {
	request := ImageGenerationRequest{
		Model:          model,
		Prompt:         prompt,
		Width:          128,
		Height:         128,
		N:              1,
		ResponseFormat: "b64_json",
	}

	response, err := GenerateImage(request)
	if err != nil {
		return "", err
	}

	var jsonResponse struct {
		Data []struct {
			B64Json string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &jsonResponse); err != nil {
		return "", err
	}

	if len(jsonResponse.Data) > 0 {
		return jsonResponse.Data[0].B64Json, nil
	}

	return "", fmt.Errorf("no image data returned")
}

func API_UpdateConversationFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	var request struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if strings.Contains(request.Folder, "/") || strings.Contains(request.Folder, "\\") {
		utils.SendHTTPError(w, "Invalid folder name", http.StatusBadRequest)
		return
	}

	if err := db.UpdateConversationFolder(r.Context(), id, request.Folder); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func API_UpdateConversationTitle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	var request struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if strings.Contains(request.Title, "/") || strings.Contains(request.Title, "\\") {
		utils.SendHTTPError(w, "Invalid title", http.StatusBadRequest)
		return
	}

	if err := db.UpdateConversationMetadata(r.Context(), id, request.Title, ""); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
