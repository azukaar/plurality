package ai

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// sseHeartbeatInterval is how often idle SSE clients receive a keepalive
// comment. Browsers (Firefox especially) throttle or drop long-lived fetch
// streams in background tabs; without periodic bytes the connection looks
// dead and the tab comes back to a frozen chat. A comment line is invisible
// to SSE parsers (only "data:" lines are read) and keeps proxies from
// buffering the stream away.
const sseHeartbeatInterval = 25 * time.Second

// SSEClient represents a single connected SSE listener.
type SSEClient struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	Done    chan struct{}

	// writeMu serializes concurrent writes to the underlying ResponseWriter.
	// The heartbeat goroutine writes ": ping" comments on its own schedule
	// while Broadcast() may be writing real events — net/http does not allow
	// concurrent writes to the same ResponseWriter, so without this the two
	// paths can interleave bytes and corrupt the SSE framing (or panic).
	writeMu       sync.Mutex
	heartbeatStop chan struct{}
	heartbeatOnce sync.Once
}

// NewSSEClient creates an SSEClient from an HTTP response writer.
// Returns nil if the writer does not support flushing.
func NewSSEClient(w http.ResponseWriter) *SSEClient {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	return &SSEClient{
		writer:        w,
		flusher:       flusher,
		Done:          make(chan struct{}),
		heartbeatStop: make(chan struct{}),
	}
}

// Send writes an SSEEvent to this client. Returns false if the write fails.
func (c *SSEClient) Send(event SSEEvent) bool {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	err := WriteSSEEvent(c.writer, event)
	return err == nil
}

// writeHeartbeat writes a single keepalive comment under the write lock.
// It returns false if the write fails or if the client has already been
// torn down (the heartbeat goroutine should then stop).
func (c *SSEClient) writeHeartbeat() bool {
	select {
	case <-c.Done:
		return false
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.writer.Write([]byte(": ping\n\n")); err != nil {
		return false
	}
	c.flusher.Flush()
	return true
}

// startHeartbeat launches the keepalive goroutine for this client. It writes
// an SSE comment (": ping") every sseHeartbeatInterval until stopHeartbeat
// is called. The first ping is scheduled after one interval so busy streams
// (which broadcast real events) are not interleaved with pings.
//
// The goroutine is fully panic-safe: a late write or flush on a response
// whose HTTP handler has already returned (which net/http finalizes by
// nilling its internal buffer) must never take down the whole process, so
// any panic is recovered and the loop exits.
func (c *SSEClient) startHeartbeat() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Log("[SSEClient] heartbeat goroutine recovered from panic: %v", r)
			}
		}()
		ticker := time.NewTicker(sseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.heartbeatStop:
				return
			case <-c.Done:
				return
			case <-ticker.C:
				if !c.writeHeartbeat() {
					return
				}
			}
		}
	}()
}

// stopHeartbeat idempotently stops the keepalive goroutine.
func (c *SSEClient) stopHeartbeat() {
	// Guard against struct-literal clients that never initialized the
	// channel (see NewSSEClient): closing a nil channel panics, and this
	// can be called from the LLM loop goroutine where a panic would kill
	// a live conversation.
	if c.heartbeatStop == nil {
		return
	}
	c.heartbeatOnce.Do(func() {
		close(c.heartbeatStop)
	})
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
	client.startHeartbeat()
}

// RemoveClient unregisters an SSE client.
func (ar *ActiveRequest) RemoveClient(client *SSEClient) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	delete(ar.clients, client)
	client.stopHeartbeat()
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
			client.stopHeartbeat()
			select {
			case <-client.Done:
				// already closed
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
		client.stopHeartbeat()
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

	writeMu       sync.Mutex
	heartbeatStop chan struct{}
	heartbeatOnce sync.Once
}

// NewStatusClient creates a StatusClient.
func NewStatusClient(w http.ResponseWriter, flusher http.Flusher, userID string) *StatusClient {
	return &StatusClient{
		writer:        w,
		flusher:       flusher,
		UserID:        userID,
		Done:          make(chan struct{}),
		heartbeatStop: make(chan struct{}),
	}
}

// Send writes a StatusEvent to this client. Returns false if the write fails.
func (c *StatusClient) Send(event StatusEvent) bool {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteStatusEvent(c.writer, event) == nil
}

// writeHeartbeat writes a single keepalive comment under the write lock.
// It returns false if the write fails or if the client has been torn down.
func (c *StatusClient) writeHeartbeat() bool {
	select {
	case <-c.Done:
		return false
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.writer.Write([]byte(": ping\n\n")); err != nil {
		return false
	}
	c.flusher.Flush()
	return true
}

// startHeartbeat launches the keepalive goroutine for this client.
// Panic-safe: a flush on a finalized response must never crash the process.
func (c *StatusClient) startHeartbeat() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Log("[StatusClient] heartbeat goroutine recovered from panic: %v", r)
			}
		}()
		ticker := time.NewTicker(sseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.heartbeatStop:
				return
			case <-c.Done:
				return
			case <-ticker.C:
				if !c.writeHeartbeat() {
					return
				}
			}
		}
	}()
}

// stopHeartbeat idempotently stops the keepalive goroutine.
func (c *StatusClient) stopHeartbeat() {
	// Guard against struct-literal clients that never initialized the
	// channel (handleStatusStream used to build one): closing a nil
	// channel panics — and when this runs inside the LLM loop goroutine
	// (StatusRegistry.BroadcastToUser → remove → stopHeartbeat) that panic
	// kills the conversation's loop mid-turn, leaving it stuck in
	// "processing" forever.
	if c.heartbeatStop == nil {
		return
	}
	c.heartbeatOnce.Do(func() {
		close(c.heartbeatStop)
	})
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
	client.startHeartbeat()
}

// Remove unregisters a status stream client.
func (r *statusRegistry) Remove(client *StatusClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, client)
	client.stopHeartbeat()
}

// BroadcastToUser sends a StatusEvent to all status clients belonging to a specific user.
func (r *statusRegistry) BroadcastToUser(userID string, event StatusEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for client := range r.clients {
		if client.UserID == userID {
			if !client.Send(event) {
				delete(r.clients, client)
				client.stopHeartbeat()
				select {
				case <-client.Done:
					// already closed
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
