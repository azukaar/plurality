package ai

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/azukaar/plurality/src/utils"
)

// SSEClient represents a single connected SSE listener.
type SSEClient struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	Done    chan struct{}
}

// NewSSEClient creates an SSEClient from an HTTP response writer.
// Returns nil if the writer does not support flushing.
func NewSSEClient(w http.ResponseWriter) *SSEClient {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	return &SSEClient{
		writer:  w,
		flusher: flusher,
		Done:    make(chan struct{}),
	}
}

// Send writes an SSEEvent to this client. Returns false if the write fails.
func (c *SSEClient) Send(event SSEEvent) bool {
	err := WriteSSEEvent(c.writer, event)
	return err == nil
}

// ActiveRequest tracks an in-progress LLM request for a conversation.
// The LLM loop goroutine writes to this; SSE clients subscribe for events.
type ActiveRequest struct {
	ConversationID string
	UserID         string
	Ctx            context.Context
	Cancel         context.CancelFunc
	State          utils.ConversationState
	Model          utils.Model
	ModelSelected  utils.ModelSelected

	mu      sync.RWMutex
	clients map[*SSEClient]bool

	// In-memory buffer for the current assistant turn
	TextBuffer       strings.Builder
	ToolCallBuffer   []utils.ToolCall
	TokenUsage       int
	PromptTokens     int
	CompletionTokens int
	ResponseCost     float64
}

// NewActiveRequest creates a new ActiveRequest with a cancelable context.
func NewActiveRequest(conversationID string, userID string, model utils.Model, modelSelected utils.ModelSelected) *ActiveRequest {
	ctx, cancel := context.WithCancel(context.Background())
	return &ActiveRequest{
		ConversationID: conversationID,
		UserID:         userID,
		Ctx:            ctx,
		Cancel:         cancel,
		State:          utils.StateProcessing,
		Model:          model,
		ModelSelected:  modelSelected,
		clients:        make(map[*SSEClient]bool),
	}
}

// AddClient registers an SSE client to receive broadcast events.
func (ar *ActiveRequest) AddClient(client *SSEClient) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	ar.clients[client] = true
}

// RemoveClient unregisters an SSE client.
func (ar *ActiveRequest) RemoveClient(client *SSEClient) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	delete(ar.clients, client)
}

// ClientCount returns the number of connected SSE clients.
func (ar *ActiveRequest) ClientCount() int {
	ar.mu.RLock()
	defer ar.mu.RUnlock()
	return len(ar.clients)
}

// Broadcast sends an SSEEvent to all connected clients.
// Disconnected clients are automatically removed.
func (ar *ActiveRequest) Broadcast(event SSEEvent) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	if len(ar.clients) == 0 && event.Type != "text" {
		utils.Debug("[Broadcast] No clients connected for %s event on %s", event.Type, ar.ConversationID)
	}
	for client := range ar.clients {
		if !client.Send(event) {
			delete(ar.clients, client)
			select {
			case <-client.Done:
			default:
				close(client.Done)
			}
		}
	}
}

// CloseAllClients signals all connected clients that the stream is done.
func (ar *ActiveRequest) CloseAllClients() {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	for client := range ar.clients {
		select {
		case <-client.Done:
			// already closed
		default:
			close(client.Done)
		}
		delete(ar.clients, client)
	}
}

// BroadcastStatus sends a compact status event to all status stream clients
// for this user. The conversation's current ModelSelected snapshot is
// included so any client with the sidebar open — not just the per-conversation
// SSE viewer — picks up lock / folder / eco / model changes live.
func (ar *ActiveRequest) BroadcastStatus(activity string, toolName string) {
	msSnap := ar.ModelSelected
	StatusRegistry.BroadcastToUser(ar.UserID, StatusEvent{
		ConversationID: ar.ConversationID,
		State:          string(ar.State),
		Activity:       activity,
		ToolName:       toolName,
		ModelSelected:  &msSnap,
	})
}

// ResetBuffer clears the in-memory text and tool call buffers for a new LLM turn.
func (ar *ActiveRequest) ResetBuffer() {
	ar.TextBuffer.Reset()
	ar.ToolCallBuffer = nil
	ar.TokenUsage = 0
	ar.PromptTokens = 0
	ar.CompletionTokens = 0
	ar.ResponseCost = 0
}

// --- Global Registry ---

// ActiveRequestRegistry manages all in-progress requests across conversations.
var RequestRegistry = &activeRequestRegistry{
	requests: make(map[string]*ActiveRequest),
}

type activeRequestRegistry struct {
	mu       sync.RWMutex
	requests map[string]*ActiveRequest
}

// Get returns the ActiveRequest for a conversation, or nil if none exists.
func (r *activeRequestRegistry) Get(conversationID string) *ActiveRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.requests[conversationID]
}

// Set registers an ActiveRequest for a conversation.
func (r *activeRequestRegistry) Set(conversationID string, ar *ActiveRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests[conversationID] = ar
}

// SetIfAbsent registers an ActiveRequest only if none is currently registered
// for the conversation. Returns true when this call won the slot. Used by the
// client-tool stall watchdog to atomically claim the resume — two watchdogs
// racing for the same conversation can never both relaunch the loop.
func (r *activeRequestRegistry) SetIfAbsent(conversationID string, ar *ActiveRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requests[conversationID] != nil {
		return false
	}
	r.requests[conversationID] = ar
	return true
}

// Remove unregisters an ActiveRequest for a conversation.
func (r *activeRequestRegistry) Remove(conversationID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.requests, conversationID)
}

// GetForUser returns all ActiveRequests belonging to a specific user.
func (r *activeRequestRegistry) GetForUser(userID string) []*ActiveRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*ActiveRequest
	for _, ar := range r.requests {
		if ar.UserID == userID {
			result = append(result, ar)
		}
	}
	return result
}

// --- Global Status Stream ---

// StatusClient is a connected listener on the global status stream.
type StatusClient struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	UserID  string
	Done    chan struct{}
}

// Send writes a StatusEvent to this client. Returns false if the write fails.
func (c *StatusClient) Send(event StatusEvent) bool {
	return WriteStatusEvent(c.writer, event) == nil
}

// StatusRegistry manages all connected status stream clients.
var StatusRegistry = &statusRegistry{
	clients: make(map[*StatusClient]bool),
}

type statusRegistry struct {
	mu      sync.RWMutex
	clients map[*StatusClient]bool
}

// Add registers a status stream client.
func (r *statusRegistry) Add(client *StatusClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[client] = true
}

// Remove unregisters a status stream client.
func (r *statusRegistry) Remove(client *StatusClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, client)
}

// BroadcastToUser sends a StatusEvent to all status clients belonging to a specific user.
func (r *statusRegistry) BroadcastToUser(userID string, event StatusEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for client := range r.clients {
		if client.UserID == userID {
			if !client.Send(event) {
				delete(r.clients, client)
				select {
				case <-client.Done:
				default:
					close(client.Done)
				}
			}
		}
	}
}

// CopyUserContext creates a context that carries the userID from the original HTTP
// request, but is NOT canceled when that request ends. Used for goroutines that
// outlive the HTTP connection.
func CopyUserContext(r *http.Request) context.Context {
	userID, _ := r.Context().Value("userID").(string)
	return context.WithValue(context.Background(), "userID", userID)
}
