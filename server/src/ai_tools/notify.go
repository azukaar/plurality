package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/utils"
)

const NotifyToolID = "send_notification"

// sanitizeHeader strips non-printable and non-ASCII bytes so the value is safe
// to put in an HTTP header (NTFY's Title header is plain ASCII).
func sanitizeHeader(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}

func notifyError(msg string) utils.MessageContent {
	b, _ := json.Marshal(map[string]string{"status": "error", "error": msg})
	return utils.NewTextContent(string(b))
}

// NotifyTool sends a push notification to the user's device via the
// admin-configured NTFY server. It is force-included in GetRequests when
// auth.NotificationsEnabled() returns true; otherwise the LLM never sees it.
//
// PickerDefault is "" so it's hidden from the per-model picker — gating is
// purely admin-side via config.json / NTFY_* env vars.
var NotifyTool = utils.AITool{
	Name:          "Send Notification",
	Description:   "Send a push notification to the user's device via the configured NTFY server.",
	ToolID:        NotifyToolID,
	PickerDefault: "",
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        NotifyToolID,
			Description: "Send a push notification to the user's device via the configured NTFY (UnifiedPush) server. Use sparingly — only when the user has explicitly asked to be notified, or to signal completion of long-running or asynchronous work the user is not actively watching. Do NOT send a notification for every reply.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"title": {
						Type:        "string",
						Description: "Short notification title (ASCII only — non-ASCII characters will be replaced).",
					},
					"message": {
						Type:        "string",
						Description: "Notification body text. Supports newlines.",
					},
					"priority": {
						Type:        "integer",
						Description: "Optional NTFY priority 1-5 (1=min, 3=default, 5=max/urgent). Omit for default.",
					},
					"tags": {
						Type:        "array",
						Description: "Optional list of NTFY tag shortcodes that render as emoji prefixes (e.g. [\"white_check_mark\"], [\"warning\"]).",
						Items:       &utils.PropertyParameterToolsRequest{Type: "string", Description: "NTFY tag shortcode."},
					},
					"call": {
						Type:        "string",
						Description: "Optional. If set, NTFY will phone the user and read the message via TTS (requires Twilio configured on the NTFY server, with a verified number). Pass \"yes\" to use the user's first verified number, or an E.164 phone number like \"+15551234567\". Use only when the user has explicitly asked to be called, or for genuinely urgent matters — phone calls are intrusive.",
					},
					"deep_link": {
						Type:        "string",
						Description: "Optional URL opened when the user taps the notification. Defaults to the current conversation (plurality://conversation/{id}). Override only to deep-link the user elsewhere — e.g. plurality://conversation/<other-id> to point at a different conversation. Must be a fully-qualified URL.",
					},
				},
				Required: []string{"title", "message"},
			},
		},
	},
	LoadingString: "Sending notification: {{title}}",
	Exec: func(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
		var p struct {
			Title    string   `json:"title"`
			Message  string   `json:"message"`
			Priority int      `json:"priority"`
			Tags     []string `json:"tags"`
			Call     string   `json:"call"`
			DeepLink string   `json:"deep_link"`
		}
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return notifyError("invalid arguments: " + err.Error())
		}
		if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Message) == "" {
			return notifyError("'title' and 'message' are both required and must be non-empty")
		}

		cfg := auth.GetConfig().Notifications
		if cfg.NtfyURL == "" || cfg.Topic == "" {
			return notifyError("notifications are not configured on the server")
		}

		url := strings.TrimRight(cfg.NtfyURL, "/") + "/" + strings.TrimLeft(cfg.Topic, "/")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(p.Message))
		if err != nil {
			return notifyError("build request: " + err.Error())
		}
		req.Header.Set("Title", sanitizeHeader(p.Title))
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		if p.Priority >= 1 && p.Priority <= 5 {
			req.Header.Set("Priority", strconv.Itoa(p.Priority))
		}
		if len(p.Tags) > 0 {
			cleaned := make([]string, 0, len(p.Tags))
			for _, t := range p.Tags {
				t = strings.TrimSpace(t)
				if t != "" {
					cleaned = append(cleaned, sanitizeHeader(t))
				}
			}
			if len(cleaned) > 0 {
				req.Header.Set("Tags", strings.Join(cleaned, ","))
			}
		}
		if call := strings.TrimSpace(p.Call); call != "" {
			req.Header.Set("Call", sanitizeHeader(call))
		}
		click := strings.TrimSpace(p.DeepLink)
		if click == "" && conv.ID != "" {
			click = "plurality://conversation/" + conv.ID
		}
		if click != "" {
			req.Header.Set("Click", sanitizeHeader(click))
		}
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return notifyError("ntfy request failed: " + err.Error())
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return notifyError(fmt.Sprintf("ntfy returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		}

		out, _ := json.Marshal(map[string]string{"status": "sent"})
		return utils.NewTextContent(string(out))
	},
}
