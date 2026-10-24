package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"

	"github.com/azukaar/plurality/src/utils"
)

var SearchTool = utils.AITool{
	Name:              "Search",
	Description:       "Search on the internet via Google",
	ToolID:            "search_web",
	PickerLabel:       "Search Web",
	PickerDescription: "Search sites via Google",
	PickerDefault:     "on",
	PickerOrder:       10,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "search_web",
			Description: "Search the internet via Google. After searching, use visit_link on relevant results.",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"query": {
						Type:        "string",
						Description: "Search query with meaningful keywords",
					},
				},
			},
		},
	},
	LoadingString: "Search for \"{{query}}\"",
	IconURL:       "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAYAAACqaXHeAAAACXBIWXMAAAsTAAALEwEAmpwYAAAGG2lUWHRYTUw6Y29tLmFkb2JlLnhtcAAAAAAAPD94cGFja2V0IGJlZ2luPSLvu78iIGlkPSJXNU0wTXBDZWhpSHpyZVN6TlRjemtjOWQiPz4gPHg6eG1wbWV0YSB4bWxuczp4PSJhZG9iZTpuczptZXRhLyIgeDp4bXB0az0iQWRvYmUgWE1QIENvcmUgOS4xLWMwMDIgNzkuYTZhNjM5NiwgMjAyNC8wMy8xMi0wNzo0ODoyMyAgICAgICAgIj4gPHJkZjpSREYgeG1sbnM6cmRmPSJodHRwOi8vd3d3LnczLm9yZy8xOTk5LzAyLzIyLXJkZi1zeW50YXgtbnMjIj4gPHJkZjpEZXNjcmlwdGlvbiByZGY6YWJvdXQ9IiIgeG1sbnM6eG1wPSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvIiB4bWxuczpkYz0iaHR0cDovL3B1cmwub3JnL2RjL2VsZW1lbnRzLzEuMS8iIHhtbG5zOnBob3Rvc2hvcD0iaHR0cDovL25zLmFkb2JlLmNvbS9waG90b3Nob3AvMS4wLyIgeG1sbnM6eG1wTU09Imh0dHA6Ly9ucy5hZG9iZS5jb20veGFwLzEuMC9tbS8iIHhtbG5zOnN0RXZ0PSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvc1R5cGUvUmVzb3VyY2VFdmVudCMiIHhtcDpDcmVhdG9yVG9vbD0iQWRvYmUgUGhvdG9zaG9wIDI1LjEyIChXaW5kb3dzKSIgeG1wOkNyZWF0ZURhdGU9IjIwMjUtMDMtMTRUMTE6MDI6MzVaIiB4bXA6TW9kaWZ5RGF0ZT0iMjAyNS0wMy0xNFQxMTowNTo1MloiIHhtcDpNZXRhZGF0YURhdGU9IjIwMjUtMDMtMTRUMTE6MDU6NTJaIiBkYzpmb3JtYXQ9ImltYWdlL3BuZyIgcGhvdG9zaG9wOkNvbG9yTW9kZT0iMyIgeG1wTU06SW5zdGFuY2VJRD0ieG1wLmlpZDoyZGE4ODdjNC1mNWZmLTNlNGQtODQ4NC0zZmYwYmE3NjhhMzYiIHhtcE1NOkRvY3VtZW50SUQ9ImFkb2JlOmRvY2lkOnBob3Rvc2hvcDphMTllNzkzOS1hZDE3LTIwNDctYWUxNy1jZjU5ZjhlZjVmNTIiIHhtcE1NOk9yaWdpbmFsRG9jdW1lbnRJRD0ieG1wLmRpZDphNWI3MmNlZC0yNTJmLWRhNDEtOGZjZi0zNmU2Y2NmNWIwYjgiPiA8eG1wTU06SGlzdG9yeT4gPHJkZjpTZXE+IDxyZGY6bGkgc3RFdnQ6YWN0aW9uPSJjcmVhdGVkIiBzdEV2dDppbnN0YW5jZUlEPSJ4bXAuaWlkOmE1YjcyY2VkLTI1MmYtZGE0MS04ZmNmLTM2ZTZjY2Y1YjBiOCIgc3RFdnQ6d2hlbj0iMjAyNS0wMy0xNFQxMTowMjozNVoiIHN0RXZ0OnNvZnR3YXJlQWdlbnQ9IkFkb2JlIFBob3Rvc2hvcCAyNS4xMiAoV2luZG93cykiLz4gPHJkZjpsaSBzdEV2dDphY3Rpb249ImNvbnZlcnRlZCIgc3RFdnQ6cGFyYW1ldGVycz0iZnJvbSBhcHBsaWNhdGlvbi92bmQuYWRvYmUucGhvdG9zaG9wIHRvIGltYWdlL3BuZyIvPiA8cmRmOmxpIHN0RXZ0OmFjdGlvbj0ic2F2ZWQiIHN0RXZ0Omluc3RhbmNlSUQ9InhtcC5paWQ6MmRhODg3YzQtZjVmZi0zZTRkLTg0ODQtM2ZmMGJhNzY4YTM2IiBzdEV2dDp3aGVuPSIyMDI1LTAzLTE0VDExOjA1OjUyWiIgc3RFdnQ6c29mdHdhcmVBZ2VudD0iQWRvYmUgUGhvdG9zaG9wIDI1LjEyIChXaW5kb3dzKSIgc3RFdnQ6Y2hhbmdlZD0iLyIvPiA8L3JkZjpTZXE+IDwveG1wTU06SGlzdG9yeT4gPC9yZGY6RGVzY3JpcHRpb24+IDwvcmRmOlJERj4gPC94OnhtcG1ldGE+IDw/eHBhY2tldCBlbmQ9InIiPz7GEKGmAAAS10lEQVR42s2be5RdVX3HP3uf533NTDJ5kMckISSAFeShIlUUQZaCC7RLBa2KWIlLQB6KUupCau2ipVUpQq1FqVasVhaCuGyXRC1URMAXgQjkKQmZzCuTed/3PWfvX/84dyaTzMy9d5II3Wvdde89d9999vf3/O7f3kcVi0UaNaXUjO8ictC7tXaq3+T3yc9KqRNE5HQReZVSap2IrAAWA+2AVx82AsaB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",
	Exec: func(_ context.Context, query string, _ utils.Conversation) utils.MessageContent {
		GoogleSearchAPIKey := os.Getenv("GOOGLE_SEARCH_API_KEY")
		GoogleSearchEngineID := os.Getenv("GOOGLE_SEARCH_ENGINE_ID")

		if GoogleSearchAPIKey == "" {
			return utils.NewTextContent("Google Search API Key not found")
		}

		if GoogleSearchEngineID == "" {
			return utils.NewTextContent("Google Search Engine ID not found")
		}

		// Parse the query parameter from JSON
		var params map[string]string
		err := json.Unmarshal([]byte(query), &params)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error parsing query: %s", err.Error()))
		}

		searchQuery := params["query"]
		if searchQuery == "" {
			return utils.NewTextContent("No search query provided")
		}

		// Build the Google Search API URL
		searchURL := fmt.Sprintf(
			"https://www.googleapis.com/customsearch/v1?key=%s&cx=%s&q=%s",
			GoogleSearchAPIKey,
			GoogleSearchEngineID,
			url.QueryEscape(searchQuery),
		)

		// Make the request
		resp, err := utils.HTTPClient.Get(searchURL)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error making search request: %s", err.Error()))
		}
		defer resp.Body.Close()

		// Read response body
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error reading response: %s", err.Error()))
		}

		// Parse the response
		var searchResults map[string]interface{}
		err = json.Unmarshal(body, &searchResults)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error parsing search results: %s", err.Error()))
		}

		// Format the results
		formattedResults := formatSearchResults(searchResults)
		return utils.NewTextContent(formattedResults)
	},
}

func safeString(m map[string]interface{}, key, fallback string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return fallback
}

func formatSearchResults(results map[string]interface{}) string {
	var formattedResults string

	items, ok := results["items"].([]interface{})
	if !ok || len(items) == 0 {
		return "No results found"
	}

	header := "### Search Results\n\n"
	if queries, ok := results["queries"].(map[string]interface{}); ok {
		if request, ok := queries["request"].([]interface{}); ok && len(request) > 0 {
			if reqMap, ok := request[0].(map[string]interface{}); ok {
				if terms, ok := reqMap["searchTerms"].(string); ok {
					header = "### Search Results for " + terms + "\n\n"
				}
			}
		}
	}
	formattedResults = header

	for i, item := range items {
		if i >= 5 {
			break
		}

		result, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		title := safeString(result, "title", "Untitled")
		link := safeString(result, "link", "")
		snippet := safeString(result, "snippet", "")

		formattedResults += fmt.Sprintf("**%s**\n", title)
		if snippet != "" {
			formattedResults += fmt.Sprintf("%s\n", snippet)
		}
		formattedResults += fmt.Sprintf("URL: %s\n\n", link)
	}

	return formattedResults
}
