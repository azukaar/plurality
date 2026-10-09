package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/memory"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/utils"
)

var initialAppPrompt = "Your name is Plurality. You are an AI assistant made to appear human and helpful, you have a specific role to play.\n"
var initialAppPromptForCalls = "Your name is Plurality. You are an AI assistant made to appear human and helpful, you have a specific role to play. This is a phone call, therefore avoid formatting, emoji, long sentences, etc... and act as you would normally act on an oral conversations.\n"
var callSystemPrompt = "Your name is Plurality. You are an AI assistant made to appear human and helpful — you have opinions, emotions, and engage proactively. You run as a server that the user connects to through a client app, not as a local AI on the user's own device, so make sure to properly consider whether command/files needs to be ont he server or the client, if necessary clarify with the user. Help the user. This is a phone call: avoid formatting, emoji, and long sentences. Speak naturally as in oral conversation. The time is "

// baseSystemPrompt returns the default assistant system prompt. The persisted
// path is sourced from the PERSIST_VOL env var (defaults to /home/) so deploys
// that mount their persistent volume elsewhere can advertise the correct path.
func baseSystemPrompt() string {
	persistVol := os.Getenv("PERSIST_VOL")
	if persistVol == "" {
		persistVol = "/home/"
	}
	return "Your name is Plurality. You are an AI assistant made to appear human and helpful — you have opinions, emotions, and engage proactively. You run as a server that the user connects to through a client app, not as a local AI on the user's own device, so make sure to properly consider whether command/files needs to be ont he server or the client, if necessary clarify with the user. Help the user. Use Markdown (bold, lists, tables, code blocks with language tags, emoji) for visual structure. Match the user's tone: casual when they're casual, detailed when they need depth. Be concise by default but thorough when warranted. Be inquisitive and critical — don't just agree. If you setup anything in your system, make sure you take into account that only the " + persistVol + " folder, and your skills.md(s)/MCP.json will be persisted upon machine restart! The time is "
}

// webhookExtSuffix returns the WEBHOOK_EXT advertisement appended to system
// prompts when the env var is set, otherwise empty.
func webhookExtSuffix() string {
	if v := os.Getenv("WEBHOOK_EXT"); v != "" {
		return "\n\nThe current exposed domain for webhooks is: " + v + " for external webhooks. Local ones (aka. non-SaaS that you start) must use localhost:8090!"
	}
	return ""
}

// portExtSuffix returns the PORT_EXT advertisement appended to system prompts
// when the env var is set, otherwise empty. PORT_EXT lists the ports that are
// exposed externally so the AI knows which ports it can bind servers to.
func portExtSuffix() string {
	if v := os.Getenv("PORT_EXT"); v != "" {
		return "\n\nThe following ports are exposed externally and available for you to bind servers to: " + v
	}
	return ""
}

// SendChatCompletion sends a chat completion request to the LiteLLM proxy,
// which handles routing to the correct provider (OpenAI, Claude, Gemini, Fireworks).
func SendChatCompletion(ctx context.Context, model utils.Model, conv utils.Conversation, payload ChatPayload) (io.ReadCloser, int, error) {
	systemPrompt := baseSystemPrompt()

	miniAppID := payload.MiniApp.ID
	isCall := payload.IsCall

	if isCall {
		systemPrompt = callSystemPrompt
	}

	utils.Log("MiniAppID: %s", miniAppID)

	if miniAppID != "" {
		username, _ := ctx.Value("userID").(string)
		miniApp, err := miniapps.Get(username, miniAppID)
		if err != nil {
			return nil, 0, err
		}

		if miniApp.Prompt["en"] != "" {
			if isCall {
				systemPrompt = initialAppPromptForCalls + miniApp.Prompt["en"] + "\n The time is: \n"
			} else {
				systemPrompt = initialAppPrompt + miniApp.Prompt["en"] + "\n The time is: \n"
			}
		}
	}

	isActionModel := Models.IsActionModel(model.Name)

	if isActionModel {
		if len(payload.AvailableSkills) > 0 {
			skillsList := strings.Join(payload.AvailableSkills, ", ")
			systemPrompt += "\n\nYou have access to the following local skills: " + skillsList +
				". When a user's request matches one of these skills, use the retrieve_skill tool to load the skill's instructions before responding. " +
				"Call retrieve_skill with the skill_name parameter. You can optionally specify a file_name to retrieve a specific file from the skill folder (defaults to SKILL.md)."
		}

		if serverSkillNames := skills.Names(conv.UserID); len(serverSkillNames) > 0 {
			serverSkillsList := strings.Join(serverSkillNames, ", ")
			systemPrompt += "\n\nYou have access to the following server-shared skills: " + serverSkillsList +
				". When a user's request matches one of these skills, use the retrieve_server_skill tool to load the skill's instructions before responding. " +
				"Call retrieve_server_skill with the skill_name parameter. You can optionally specify a file_name to retrieve a specific file from the skill folder (defaults to SKILL.md)."
		}

		// Append MCP server descriptions configured in mcp.json.
		for serverName, desc := range mcp.ServerDescriptions(conv.UserID) {
			systemPrompt += "\n\n[" + serverName + "] " + desc
		}

		if mode, ok := model.Tools["filesystem_server__fs_write"]; ok && mode != "" && mode != "off" && mode != "false" {
			systemPrompt += "\n\nStartup scripts: any *.sh file you place in ~/.plurality/startup/ will be launched automatically with `nohup` on every server boot, with stdout/stderr going to <script>.sh.log next to it. Use this for long-running background processes you want to survive restarts. The ~/.plurality directory is persistent."
		}

		// Append a compact summary of disabled tools so the LLM can suggest enabling them.
		if disabledSummary := ai_tools.GetDisabledToolsSummary(conv.UserID, model, payload.ClientSideTools); disabledSummary != "" {
			systemPrompt += "\n\n" + disabledSummary
		}
	}

	if !LiteLLMReady() {
		return nil, 0, fmt.Errorf("AI proxy is not ready, please try again in a moment")
	}

	// Append the per-user important_memory snippet. It is always present so the
	// LLM can read its current value, and is overwritten via the
	// update_important_memory tool.
	if username, ok := ctx.Value("userID").(string); ok && username != "" {
		systemPrompt += "\n\nThis is your important_memory snippet, you can edit it further but keep it compact:\n```\n" + memory.Get(username) + "\n```"
	}

	systemMsg := utils.Message{
		Role: "system",
		Content: utils.NewTextContent(systemPrompt +
			time.Now().String() +
			" on " +
			strconv.Itoa(time.Now().Day()) +
			"/" +
			strconv.Itoa(int(time.Now().Month())) +
			"/" +
			strconv.Itoa(time.Now().Year()) +
			webhookExtSuffix() +
			portExtSuffix()),
	}

	// Apply eco-mode checkpoint filtering before assembling the request.
	// When eco is on: keep the rolling checkpoint pair and drop everything
	// older than it. When eco is off: drop the checkpoint pair entirely
	// and send the raw history.
	visibleMessages := filterCheckpointsForRequest(conv.Messages, conv.ModelSelected.EcoMode)
	allMessages := append([]utils.Message{systemMsg}, visibleMessages...)
	optimizedMessages, hasAttachments, hasDocAttachments := PrepareMessagesForAI(allMessages, model)
	msgReqList, _ := convertMessagesToOpenAI(optimizedMessages, model)

	requestData := StandardChatRequest{
		Model:         model.Name,
		Messages:      msgReqList,
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
	}

	if isActionModel {
		ait := ai_tools.GetRequests(conv.UserID, model, payload.ClientSideTools, hasAttachments, hasDocAttachments, payload.ModelSelected.ClientFolderPath != "")
		if len(ait) > 0 {
			requestData.Tools = ait
		}
	}

	utils.Debug("A new chat request is being made with the following model: %s", model.Name)

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	client := utils.HTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("LiteLLM API request failed with status", nil, strStatus, ":", string(respBody))
		return nil, 0, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return resp.Body, 0, nil
}

// convertMessagesToOpenAI converts internal utils.Message slices into
// StandardMessageReq for sending to the LiteLLM proxy (OpenAI-compatible format).
// Returns the converted messages and the concatenated input text (for debugging/logging).
func convertMessagesToOpenAI(messages []utils.Message, _ utils.Model) ([]StandardMessageReq, string) {
	result := make([]StandardMessageReq, 0, len(messages))
	inputText := ""

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			text := msg.TextContent()
			if text == "" {
				continue
			}
			inputText += "system " + text + " {}{}{}{}{}{}{}"
			result = append(result, StandardMessageReq{
				Role:    "system",
				Content: text,
			})

		case "user":
			parts := msg.ContentParts()
			if len(parts) == 0 {
				continue
			}

			contentParts := make([]StandardContentReq, 0, len(parts))
			for _, part := range parts {
				if part.Type == "image_url" && part.ImageURL != nil {
					contentParts = append(contentParts, StandardContentReq{
						Type: "image_url",
						ImageURL: &utils.ContentImageURL{
							URL: part.ImageURL.URL,
						},
					})
					continue
				}
				if part.Text == "" {
					continue
				}
				text := part.Text
				if part.Type != "text" && part.Filename != "" {
					text = fmt.Sprintf("[Attached file: %s]\n%s", part.Filename, text)
				}
				inputText += "user " + text + " {}{}{}{}{}{}{}"
				contentParts = append(contentParts, StandardContentReq{
					Type: "text",
					Text: text,
				})
			}

			if len(contentParts) == 0 {
				continue
			}
			result = append(result, StandardMessageReq{
				Role:    "user",
				Content: contentParts,
			})

		case "assistant":
			text := msg.TextContent()
			smr := StandardMessageReq{
				Role: "assistant",
			}
			if text != "" {
				smr.Content = text
				inputText += "assistant " + text + " {}{}{}{}{}{}{}"
			}
			if len(msg.ToolCalls) > 0 {
				cleaned := make([]utils.ToolCall, len(msg.ToolCalls))
				for i, tc := range msg.ToolCalls {
					fn := tc.Function
					args := strings.TrimSpace(fn.Arguments)
					if args == "" {
						fn.Arguments = "{}"
					} else if !json.Valid([]byte(args)) {
						preview := args
						if len(preview) > 120 {
							preview = preview[:120]
						}
						utils.Log("[convertMessagesToOpenAI] tool_call %s (%s) has invalid JSON args; replacing with {}. First 120 chars: %s",
							tc.ID, fn.Name, preview)
						fn.Arguments = "{}"
					}
					cleaned[i] = utils.ToolCall{
						ID:       tc.ID,
						Type:     tc.Type,
						Function: fn,
					}
				}
				smr.ToolCalls = cleaned
			}
			if text == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			result = append(result, smr)

		case "tool":
			parts := msg.ContentParts()
			hasImages := msg.HasImages()

			if hasImages {
				contentParts := make([]StandardContentReq, 0, len(parts))
				for _, part := range parts {
					if part.Type == "image_url" && part.ImageURL != nil {
						contentParts = append(contentParts, StandardContentReq{
							Type:     "image_url",
							ImageURL: &utils.ContentImageURL{URL: part.ImageURL.URL},
						})
					} else if part.Text != "" {
						text := part.Text
						if msg.Name != "conversation_attachments" && ai_tools.ShouldStripResponse(text) {
							text = "Tool result displayed to user."
						}
						inputText += "tool " + text + " {}{}{}{}{}{}{}"
						contentParts = append(contentParts, StandardContentReq{
							Type: "text",
							Text: text,
						})
					}
				}
				if len(contentParts) == 0 {
					continue
				}
				result = append(result, StandardMessageReq{
					Role:       "tool",
					Content:    contentParts,
					ToolCallID: msg.ToolCallID,
					Name:       msg.Name,
				})
			} else {
				text := msg.TextContent()
				if msg.Name != "conversation_attachments" && ai_tools.ShouldStripResponse(text) {
					text = "Tool result displayed to user."
				}
				if text == "" {
					continue
				}
				inputText += "tool " + text + " {}{}{}{}{}{}{}"
				result = append(result, StandardMessageReq{
					Role:       "tool",
					Content:    text,
					ToolCallID: msg.ToolCallID,
					Name:       msg.Name,
				})
			}
		}
	}

	// Backfill any assistant tool_calls that don't have a matching tool result
	// before the next user/assistant boundary. Strict providers (Fireworks)
	// reject dangling tool_calls. This defense repairs conversations whose
	// history was persisted before the stream-processor fix existed (the old
	// code persisted assistant messages whose tool_calls were cut off
	// mid-stream with no result ever pushed). Providers also REUSE short
	// tool-call ids ("call_0") across turns, so only results inside THIS
	// assistant block are considered — never results from an earlier block.
	//
	// When a call in this block genuinely has no result, the ENTIRE call is
	// dropped rather than inventing an empty "tool" result: an invented
	// result makes the model believe the tool executed and returned nothing,
	// so it either loops calling the same tool (the provider keeps issuing
	// "call_0") or the empty-tool flood pushes the conversation past the
	// model context limit. Dropping is safe — the model just may re-issue a
	// genuinely useful call, which is exactly what a truncated-turn call is.
	out := make([]StandardMessageReq, 0, len(result))
	i := 0
	for i < len(result) {
		if result[i].Role == "assistant" && len(result[i].ToolCalls) > 0 {
			j := i + 1
			for j < len(result) && result[j].Role != "user" && result[j].Role != "assistant" {
				j++
			}
			seen := make(map[string]bool, len(result[i].ToolCalls))
			blockTools := make([]StandardMessageReq, 0, j-(i+1))
			for k := i + 1; k < j; k++ {
				if result[k].Role == "tool" && result[k].ToolCallID != "" {
					seen[result[k].ToolCallID] = true
				}
				blockTools = append(blockTools, result[k])
			}
			// Drop this assistant's tool_calls that have no result in the
			// block. Prune BEFORE emitting, on a copied message.
			kept := make([]utils.ToolCall, 0, len(result[i].ToolCalls))
			for _, tc := range result[i].ToolCalls {
				if !seen[tc.ID] {
					utils.Log("[convertMessagesToOpenAI] dropping dangling tool_call %s (%s) with no result in block", tc.ID, tc.Function.Name)
					continue
				}
				kept = append(kept, tc)
			}
			// Emit the (possibly pruned) assistant only when it still carries
			// something: kept calls, or text (Content == nil means the
			// assistant had no text). An assistant with neither is invalid
			// for the API and only existed to carry the dangling calls.
			if len(kept) > 0 || result[i].Content != nil {
				smr := result[i]
				smr.ToolCalls = kept
				out = append(out, smr)
				for _, bt := range blockTools {
					out = append(out, bt)
				}
			}
			i = j
			continue
		}
		out = append(out, result[i])
		i++
	}
	result = out

	return result, inputText
}

// SelectModel picks the vision model if any message contains images and the
// current text model doesn't already support vision; otherwise returns the text model.
//
// Evaluation runs on the post-strip ephemeral view (PrepareMessagesForAI) so
// that an image which has been stripped from the actual payload doesn't trigger
// a vision swap — otherwise we'd pay for the vision model on text-only turns.
func SelectModel(modelSelected utils.ModelSelected, conv utils.Conversation) utils.Model {
	if modelSelected.Vision != nil && modelSelected.Text != nil &&
		!Models.IsVisionModel(modelSelected.Text.Name) {
		prepared, _, _ := PrepareMessagesForAI(conv.Messages, *modelSelected.Text)
		for _, m := range prepared {
			if m.HasImages() {
				utils.Log("Vision model selected: %s", modelSelected.Vision.Name)
				return *modelSelected.Vision
			}
		}
	}

	if modelSelected.Text != nil {
		return *modelSelected.Text
	}
	if modelSelected.Vision != nil {
		return *modelSelected.Vision
	}
	return utils.Model{}
}

// GenerateImage sends an image generation request to the litellm proxy,
// which routes it to the upstream provider configured in litellm_config.yaml.
func GenerateImage(request ImageGenerationRequest) ([]byte, error) {
	if !LiteLLMReady() {
		return nil, fmt.Errorf("AI proxy is not ready")
	}

	jsonData, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/images/generations", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := utils.HTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("Image generation request failed with status", nil, strStatus, ":", string(body))
		return nil, fmt.Errorf("image generation failed with status %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GenerateTitleForMessage generates a short title for a conversation
// using the LiteLLM proxy. The model name is supplied by the caller — typically
// the "fast" shortcut's text model from data/config.json.
func GenerateTitleForMessage(message, model string) (string, error) {
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}

	msgReqList := []StandardMessageReq{
		{
			Role:    "system",
			Content: "You generate short titles for conversations. Respond with ONLY the title, no quotes, no formatting, no explanation. The title must be 3-4 words max.",
		},
		{
			Role:    "user",
			Content: "Generate a title for this conversation:\n\n" + message,
		},
	}

	maxTokens := 500
	requestData := StandardChatRequest{
		Model:           model,
		Messages:        msgReqList,
		MaxTokens:       &maxTokens,
		Stream:          false,
		ReasoningEffort: "low",
	}

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	client := utils.HTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		utils.Error("[TitleGen] API request failed", nil, strconv.Itoa(resp.StatusCode), string(body))
		return "", fmt.Errorf("API request failed with status %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse title response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in title response")
	}

	title := strings.TrimSpace(result.Choices[0].Message.Content)

	return title, nil
}

// GenerateCheckpointSummary asks the configured fast model to produce a
// dense summary of a slice of conversation history, used by the eco mode
// rolling checkpoint. The input may include a previously-stored summary
// prefixed with "PRIOR CHECKPOINT:" — the system prompt instructs the
// model to fold it in rather than repeat it verbatim.
func GenerateCheckpointSummary(text, model string) (string, error) {
	if !LiteLLMReady() {
		return "", fmt.Errorf("AI proxy is not ready")
	}

	msgReqList := []StandardMessageReq{
		{
			Role: "system",
			Content: "You are summarising older conversation history that the live LLM will no longer see directly. " +
				"Your summary REPLACES those messages in the live context, so any detail you drop is lost to the model. " +
				"Rules:\n" +
				"1. Preserve verbatim: code blocks, file paths, identifiers, IDs, error messages, command outputs, URLs, numeric values, exact quoted user requests.\n" +
				"2. Preserve the FULL text content of any inline pasted snippet or text attachment shown to you under '[attachment: ...]' headers — copy them into a clearly-labeled section, do not paraphrase code or text the user pasted.\n" +
				"3. Preserve tool_call → TOOL_RESULT outcomes: which tool was called, the key arguments, and the substantive result (paths created, errors returned, data retrieved). Drop only obviously verbose framing.\n" +
				"4. For image / document / binary attachments noted as '[image attachment omitted]' or '[document attachment: ...]', just record that the attachment existed (filename and any context the user gave).\n" +
				"5. Preserve user goals, decisions, plans, and rejected options chronologically so the live LLM understands where the conversation stands.\n" +
				"6. The optional 'PRIOR CHECKPOINT' section at the start is a summary you produced earlier — fold its facts into your new summary in-place, do not repeat it verbatim and do not lose information from it.\n" +
				"7. Be dense and structured (sections / bullets), but do NOT be brief at the cost of dropping concrete content. Length should match the input's information density.\n" +
				"Respond with the summary text only — no preamble, no closing remarks.",
		},
		{
			Role:    "user",
			Content: text,
		},
	}

	maxTokens := 8192
	requestData := StandardChatRequest{
		Model:       model,
		Messages:    msgReqList,
		MaxTokens:   &maxTokens,
		Stream:      false,
	}

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", LiteLLMBaseURL+"/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := utils.HTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		utils.Error("[EcoSummary] API request failed", nil, strconv.Itoa(resp.StatusCode), string(body))
		return "", fmt.Errorf("API request failed with status %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse summary response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in summary response")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}
