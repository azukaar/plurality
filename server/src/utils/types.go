package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// --- OpenAI-Compatible Message Types ---

// ContentImageURL holds the URL for an image content part.
type ContentImageURL struct {
	URL string `json:"url"`
}

// ContentPart represents one element of a multi-part content array.
// Used when a message contains mixed content (text + images).
type ContentPart struct {
	Type     string           `json:"type"` // "text", "image_url", "snippet", "file", "pdf", "docx", "xlsx", "pptx"
	Text     string           `json:"text,omitempty"`
	ImageURL *ContentImageURL `json:"image_url,omitempty"`
	Filename string           `json:"filename,omitempty"`
}

// --- MessageContent ---

// MessageContent holds message content as a normalized []ContentPart slice.
// It implements custom JSON marshaling so that:
//   - A single text-only part is serialized as a plain string (compact, API-compatible)
//   - Multiple parts are serialized as an array of content-part objects
type MessageContent struct {
	parts []ContentPart
}

// NewTextContent creates a MessageContent from a plain string.
func NewTextContent(s string) MessageContent {
	if s == "" {
		return MessageContent{}
	}
	return MessageContent{parts: []ContentPart{{Type: "text", Text: s}}}
}

// NewPartsContent creates a MessageContent from a slice of ContentPart.
func NewPartsContent(parts []ContentPart) MessageContent {
	return MessageContent{parts: parts}
}

// TextContent returns the text of the first "text" content part, or "".
func (mc MessageContent) TextContent() string {
	for _, part := range mc.parts {
		if part.Type == "text" {
			return part.Text
		}
	}
	return ""
}

// TotalContentChars returns the TOTAL character length across ALL text
// content parts. TextContent() only returns the FIRST text part, so a
// multi-part message whose first part is a short preamble can hide a
// multi-MB blob in a later part (observed: conversations__retrieve_conversation
// results — first part "[Attachment att_N, bytes 0-50000 of 4901042]",
// then a ~4.9MB JSON part). Context-safety code must size against this
// total, not TextContent().
func (mc MessageContent) TotalContentChars() int {
	total := 0
	for _, part := range mc.parts {
		if part.Type == "text" {
			total += len(part.Text)
		}
	}
	return total
}

// ContentParts returns the underlying []ContentPart slice.
func (mc MessageContent) ContentParts() []ContentPart {
	return mc.parts
}

// HasImages returns true if any part is an image_url.
func (mc MessageContent) HasImages() bool {
	for _, part := range mc.parts {
		if part.Type == "image_url" {
			return true
		}
	}
	return false
}

// IsZero implements bsoncodec.Zeroer so BSON omitempty works.
func (mc MessageContent) IsZero() bool {
	return len(mc.parts) == 0
}

// --- JSON marshaling ---

func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if len(mc.parts) == 0 {
		return []byte("null"), nil
	}
	if len(mc.parts) == 1 && mc.parts[0].Type == "text" && mc.parts[0].ImageURL == nil {
		return json.Marshal(mc.parts[0].Text)
	}
	return json.Marshal(mc.parts)
}

func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || len(data) == 0 {
		mc.parts = nil
		return nil
	}
	// Try string first (most common)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s != "" {
			mc.parts = []ContentPart{{Type: "text", Text: s}}
		} else {
			mc.parts = nil
		}
		return nil
	}
	// Try array of ContentPart
	var parts []ContentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("MessageContent: expected string or array: %w", err)
	}
	mc.parts = parts
	return nil
}

// FunctionCall holds the function name and JSON-encoded arguments for a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall represents an assistant's request to invoke a tool.
// Loading and IconURL are transient Plurality extensions — included in SSE
// events for live display but NOT persisted to DB.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
	Loading  string       `json:"loading,omitempty"`  // transient: display template
	IconURL  string       `json:"icon_url,omitempty"` // transient: base64 icon
}

// Message is an OpenAI-compatible message.
//
// Content is a MessageContent that is internally always []ContentPart.
// Use TextContent() and ContentParts() for access.
type Message struct {
	Role       string         `json:"role"` // "system", "user", "assistant", "tool"
	Content    MessageContent `json:"content,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`   // assistant only
	ToolCallID string         `json:"tool_call_id,omitempty"` // tool role only
	Name       string         `json:"name,omitempty"`         // tool role only

	// Metadata — stored in DB, not sent to LLMs
	Timestamp        string  `json:"timestamp,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	ResponseCost     float64 `json:"response_cost,omitempty"`
	Model            Model   `json:"model,omitempty"`
}

// TextContent returns the text of the first "text" content part.
func (m Message) TextContent() string { return m.Content.TextContent() }

// ContentParts returns the content as []ContentPart.
func (m Message) ContentParts() []ContentPart { return m.Content.ContentParts() }

// HasImages returns true if the message contains any image_url content parts.
func (m Message) HasImages() bool { return m.Content.HasImages() }

// --- Conversation State Machine ---

type ConversationState string

const (
	StateIdle               ConversationState = "idle"
	StateProcessing         ConversationState = "processing"
	StateWaitingForTool     ConversationState = "waiting_for_tool"
	StateWaitingForApproval ConversationState = "waiting_for_approval"
)

// --- Model & Configuration ---

type Model struct {
	Name   string            `json:"name,omitempty"`
	Params map[string]string `json:"params,omitempty"`
	Tools  map[string]string `json:"tools,omitempty"`
}

type ModelSelected struct {
	Text            *Model `json:"text,omitempty"`
	Vision          *Model `json:"vision,omitempty"`
	ImageGen        *Model `json:"image_gen,omitempty"`
	ImageEdit       *Model `json:"image_edit,omitempty"`
	AudioTranscribe *Model `json:"audio_transcribe,omitempty"`
	VoiceGen        *Model `json:"voice_gen,omitempty"`
	AudioGen        *Model `json:"audio_gen,omitempty"`
	VideoGen        *Model `json:"video_gen,omitempty"`
	VideoVision     *Model `json:"video_vision,omitempty"`
	Code            *Model `json:"code,omitempty"`

	// ClientFolderPath is an absolute path on the client device of a folder
	// the user has attached to the conversation. When non-empty, the LLM is
	// given the device-side filesystem_client tools (the client sandboxes
	// execution to this directory). Server treats the value as opaque — it
	// round-trips and persists alongside the per-conversation tool toggles.
	ClientFolderPath string `json:"client_folder_path,omitempty"`

	// EcoMode, when true, makes the server roll the oldest turns of the
	// conversation into a single rolling "checkpoint" summary whenever the
	// previous prompt token count exceeds the configured trigger. New
	// conversations default to true; legacy rows that pre-date the field are
	// upgraded to true at load time (see unmarshalModelSelected).
	EcoMode bool `json:"eco_mode"`

	// ClientLock, when non-nil, identifies the single client (by opaque ID)
	// that owns this conversation's client-side tool execution. Other
	// connected clients still read messages but must not run filesystem /
	// shell / MCP tools while a lock is held. Server treats this as opaque —
	// it round-trips alongside ClientFolderPath and EcoMode.
	ClientLock *ClientLock `json:"client_lock,omitempty"`
}

// ClientLock identifies which physical client owns a conversation's
// client-tool execution. ID is the stable identifier (hostname on desktop,
// persisted UUID elsewhere). Label is the human-readable name shown in the
// "locked on X" banner on other clients.
type ClientLock struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// --- Conversation ---

type Conversation struct {
	ID            string            `json:"id,omitempty"`
	UserID        string            `json:"user_id"`
	Messages      []Message         `json:"messages"`
	Title         string            `json:"title"`
	LastMessageAt time.Time         `json:"last_message_at"`
	ModelSelected ModelSelected     `json:"model_selected"`
	State         ConversationState `json:"state"`
	MiniApp       *MiniApp          `json:"mini_app,omitempty"`
	Folder        string            `json:"folder"`
	Icon          string            `json:"icon"`
	TriggerType   string            `json:"trigger_type,omitempty"`
	TriggerID     string            `json:"trigger_id,omitempty"`
}

// --- MiniApps ---

type MiniAppInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Options     []string `json:"options"`
}

type MiniApp struct {
	ID                      string            `json:"id,omitempty"`
	Name                    string            `json:"name"`
	Description             string            `json:"description"`
	IconURL                 string            `json:"icon_url"`
	Author                  string            `json:"author"`
	Prompt                  map[string]string `json:"-"`
	ModelSelected           ModelSelected     `json:"model_selected"`
	Inputs                  []MiniAppInput    `json:"inputs"`
	InitialMessage          map[string]string `json:"initial_message"`
	InputPlaceholderMessage map[string]string `json:"input_placeholder_message"`
	Form                    string            `json:"form"`
	Placeholder             string            `json:"placeholder"`
	// Complexity picks the base shortcut for the preset's models when
	// fields aren't pinned in ModelSelected: "low" -> fast, "medium" -> medium,
	// "high" -> smart. Empty/unknown values default to "medium".
	Complexity string `json:"complexity,omitempty"`
}

// --- Tool Definitions (for LLM function calling) ---

type ToolsRequest struct {
	Type     string               `json:"type"`
	Function FunctionToolsRequest `json:"function"`
}

type FunctionToolsRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  *ParameterToolsRequest `json:"parameters,omitempty"`
	InputSchema *ParameterToolsRequest `json:"input_schema,omitempty"` // Claude-specific
}

type ParameterToolsRequest struct {
	Type       string                                   `json:"type"`
	Properties map[string]PropertyParameterToolsRequest `json:"properties"`
	Required   []string                                 `json:"required,omitempty"`
}

// PropertyParameterToolsRequest describes a single property inside a tool's
// parameters schema. Items is required for array-typed properties — Google's
// Gemini endpoint rejects function declarations whose array properties lack a
// schema-level "items" (GenerateContentRequest proto validation), which broke
// vision-model turns that sent array params. Array params default to
// items:{"type":"string"} when no richer item schema is available.
type PropertyParameterToolsRequest struct {
	Type        string                            `json:"type"`
	Description string                            `json:"description"`
	Enum        []string                          `json:"enum,omitempty"`
	Items       *PropertyParameterToolsRequest    `json:"items,omitempty"`
}

// AITool is a server-side tool registered in the tool registry.
type AITool struct {
	Name          string                                                     `json:"name"`
	Description   string                                                     `json:"description"`
	ToolID        string                                                     `json:"tool_id"`
	BundleName    string                                                     `json:"bundle_name,omitempty"`
	ToolRequest   ToolsRequest                                               `json:"tool_call"`
	LoadingString string                                                     `json:"loading_string"`
	IconURL       string                                                     `json:"icon_url"`
	Author        string                                                     `json:"author"`
	Exec          func(context.Context, string, Conversation) MessageContent `json:"-"`

	// Picker metadata — fields used by HandleListModels to drive the UI tool
	// picker. A tool with PickerDefault == "" is hidden from the picker (used
	// for force-included tools like conversation_attachments).
	PickerLabel       string `json:"-"`
	PickerDescription string `json:"-"`
	PickerDefault     string `json:"-"` // "on", "ask", "off", or "" (hidden)
	PickerOrder       int    `json:"-"`

	// ClientSide marks tools whose schema lives in Registry (so they appear
	// in the picker) but whose execution happens on the device. The tool
	// loop's categorizeToolCalls treats them as client-routed even though
	// GetTool returns them.
	ClientSide bool `json:"-"`
}
