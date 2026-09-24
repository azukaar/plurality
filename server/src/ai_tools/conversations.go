package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/search"
	"github.com/azukaar/plurality/src/utils"
)

var SearchConversationsTool = utils.AITool{
	Name:              "Search Conversations",
	Description:       "Search past conversations by topic or keyword",
	ToolID:            "search_conversations",
	BundleName:        "conversations",
	PickerLabel:       "Search Conversations",
	PickerDescription: "Search past conversations by topic",
	PickerDefault:     "on",
	PickerOrder:       70,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "search_conversations",
			Description: "Search the user's past conversations to find relevant discussions by topic, keyword, or concept. Returns matching conversations with relevant message excerpts. To link to a conversation in your response, use markdown: [title](plurality://conversation/CONVERSATION_ID)",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"query": {
						Type:        "string",
						Description: "The search query — can be a keyword, topic, or natural language question",
					},
				},
				Required: []string{"query"},
			},
		},
	},
	LoadingString: "Searching conversations for \"{{query}}\"",
	Exec: func(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
		parsed := utils.ParseJson(args)
		query, _ := parsed["query"].(string)
		if query == "" {
			return utils.NewTextContent("Error: query parameter is required")
		}

		userDB, err := db.GetUserDB(conv.UserID)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error: %v", err))
		}

		// Generate the embedding (HTTP to LiteLLM) OUTSIDE the SQLite write
		// lock — a slow embedding response used to hold db.LockDBWrite() for
		// its whole round-trip, wedging every PushMessage/eco writer and
		// eventually killing the server right when a workflow ends.
		var queryVec []float32
		if db.LiteLLMBaseURL != "" {
			queryVec, _ = search.GenerateEmbedding(db.LiteLLMBaseURL, query)
		}
		db.LockDBWrite()
		results, err := search.SearchWithVec(userDB, query, queryVec, 10)
		db.UnlockDBWrite()
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error searching: %v", err))
		}

		if len(results) == 0 {
			return utils.NewTextContent("No matching conversations found.")
		}

		data, err := json.Marshal(results)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error: %v", err))
		}

		return utils.NewTextContent(string(data))
	},
}

var RetrieveConversationTool = utils.AITool{
	Name:              "Retrieve Conversation",
	Description:       "Retrieve messages from a specific past conversation",
	ToolID:            "retrieve_conversation",
	BundleName:        "conversations",
	PickerLabel:       "Retrieve Conversation",
	PickerDescription: "Retrieve messages from a past conversation",
	PickerDefault:     "on",
	PickerOrder:       80,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "retrieve_conversation",
			Description: "Retrieve a range of messages from a specific past conversation. Use this after search_conversations to read more of a conversation.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"conversation_id": {
						Type:        "string",
						Description: "The conversation ID to retrieve messages from",
					},
					"start": {
						Type:        "integer",
						Description: "Starting message index (0-based, default 0)",
					},
					"end": {
						Type:        "integer",
						Description: "Ending message index (exclusive, default 20)",
					},
				},
				Required: []string{"conversation_id"},
			},
		},
	},
	LoadingString: "Retrieving conversation messages",
	Exec: func(ctx context.Context, args string, conv utils.Conversation) utils.MessageContent {
		parsed := utils.ParseJson(args)
		convID, _ := parsed["conversation_id"].(string)
		if convID == "" {
			return utils.NewTextContent("Error: conversation_id is required")
		}

		start := 0
		end := 20
		if s, ok := parsed["start"].(float64); ok {
			start = int(s)
		}
		if e, ok := parsed["end"].(float64); ok {
			end = int(e)
		}

		targetConv, err := db.GetConversationById(ctx, convID)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error: conversation not found"))
		}

		totalMessages := len(targetConv.Messages)
		if start < 0 {
			start = 0
		}
		if end > totalMessages {
			end = totalMessages
		}
		if start >= end {
			return utils.NewTextContent("No messages in the requested range.")
		}

		messages := make([]map[string]string, 0, end-start)
		for _, msg := range targetConv.Messages[start:end] {
			messages = append(messages, map[string]string{
				"role":      msg.Role,
				"content":   msg.TextContent(),
				"timestamp": msg.Timestamp,
			})
		}

		result := map[string]interface{}{
			"conversation_id": targetConv.ID,
			"title":           targetConv.Title,
			"date":            targetConv.LastMessageAt.Format("2006-01-02T15:04:05Z"),
			"total_messages":  totalMessages,
			"messages":        messages,
		}

		data, err := json.Marshal(result)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error: %v", err))
		}

		return utils.NewTextContent(string(data))
	},
}
