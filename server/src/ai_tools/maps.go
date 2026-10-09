package ai_tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

var PlaceSearchTool = utils.AITool{
	Name:              "Place Search",
	Description:       "Search for places on Google Maps",
	ToolID:            "place_search",
	PickerLabel:       "Place Search",
	PickerDescription: "Search locations via Google Maps",
	PickerDefault:     "on",
	PickerOrder:       20,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "place_search",
			Description: "Search for places on Google Maps",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"query": {
						Type:        "string",
						Description: "Place or location to search for",
					},
					"location": {
						Type:        "string",
						Description: "Location to search near (e.g. 'New York, NY')",
					},
					"type": {
						Type:        "string",
						Description: "Place type filter (e.g. restaurant, hotel)",
					},
				},
				Required: []string{"query"},
			},
		},
	},
	LoadingString: "Searching Maps for {{query}}",
	IconURL:       "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAYAAACqaXHeAAAACXBIWXMAAAsTAAALEwEAmpwYAAAFs2lUWHRYTUw6Y29tLmFkb2JlLnhtcAAAAAAAPD94cGFja2V0IGJlZ2luPSLvu78iIGlkPSJXNU0wTXBDZWhpSHpyZVN6TlRjemtjOWQiPz4gPHg6eG1wbWV0YSB4bWxuczp4PSJhZG9iZTpuczptZXRhLyIgeDp4bXB0az0iQWRvYmUgWE1QIENvcmUgOS4xLWMwMDIgNzkuYTZhNjM5NiwgMjAyNC8wMy8xMi0wNzo0ODoyMyAgICAgICAgIj4gPHJkZjpSREYgeG1sbnM6cmRmPSJodHRwOi8vd3d3LnczLm9yZy8xOTk5LzAyLzIyLXJkZi1zeW50YXgtbnMjIj4gPHJkZjpEZXNjcmlwdGlvbiByZGY6YWJvdXQ9IiIgeG1sbnM6eG1wPSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvIiB4bWxuczpkYz0iaHR0cDovL3B1cmwub3JnL2RjL2VsZW1lbnRzLzEuMS8iIHhtbG5zOnBob3Rvc2hvcD0iaHR0cDovL25zLmFkb2JlLmNvbS9waG90b3Nob3AvMS4wLyIgeG1sbnM6eG1wTU09Imh0dHA6Ly9ucy5hZG9iZS5jb20veGFwLzEuMC9tbS8iIHhtbG5zOnN0RXZ0PSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvc1R5cGUvUmVzb3VyY2VFdmVudCMiIHhtcDpDcmVhdG9yVG9vbD0iQWRvYmUgUGhvdG9zaG9wIDI1LjEyIChXaW5kb3dzKSIgeG1wOkNyZWF0ZURhdGU9IjIwMjUtMDMtMTRUMTY6MTg6NDNaIiB4bXA6TW9kaWZ5RGF0ZT0iMjAyNS0wMy0xNFQxNjoxOTo0MVoiIHhtcDpNZXRhZGF0YURhdGU9IjIwMjUtMDMtMTRUMTY6MTk6NDFaIiBkYzpmb3JtYXQ9ImltYWdlL3BuZyIgcGhvdG9zaG9wOkNvbG9yTW9kZT0iMyIgeG1wTU06SW5zdGFuY2VJRD0ieG1wLmlpZDpmOGU5NTc2My1kZmI1LWI3NGQtOGY3NS02NjNhZWNjMmMyMjkiIHhtcE1NOkRvY3VtZW50SUQ9ImFkb2JlOmRvY2lkOnBo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",
	Exec: func(ctx context.Context, query string, _ utils.Conversation) utils.MessageContent {
		GoogleMapsAPIKey := os.Getenv("GOOGLE_SEARCH_API_KEY")

		if GoogleMapsAPIKey == "" {
			return utils.NewTextContent("Google Maps API Key not found")
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

		// Get optional parameters
		location := params["location"]
		placeType := params["type"]

		// Build the search query with location if provided
		textQuery := searchQuery
		if location != "" {
			textQuery = fmt.Sprintf("%s in %s", searchQuery, location)
		}

		// Create request payload
		requestPayload := map[string]interface{}{
			"textQuery": textQuery,
		}

		// Add type if provided
		if placeType != "" {
			requestPayload["includedType"] = placeType
		}

		// Marshal the request payload
		requestBody, err := json.Marshal(requestPayload)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error creating request: %s", err.Error()))
		}

		// Create the HTTP request
		url := "https://places.googleapis.com/v1/places:searchText"
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(requestBody))
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error creating request: %s", err.Error()))
		}

		// Add headers
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Goog-Api-Key", GoogleMapsAPIKey)
		req.Header.Set("X-Goog-FieldMask", "places.displayName,places.formattedAddress,places.location,places.rating,places.types,places.priceLevel,places.websiteUri,places.id")

		// Make the request
		client := utils.HTTPClient
		resp, err := client.Do(req)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error making place search request: %s", err.Error()))
		}
		defer resp.Body.Close()

		// Read response body
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error reading response: %s", err.Error()))
		}

		// Check for errors in the response
		if resp.StatusCode != http.StatusOK {
			return utils.NewTextContent(fmt.Sprintf("API Error (Status %d): %s", resp.StatusCode, string(body)))
		}

		// Parse the response
		var searchResults map[string]interface{}
		err = json.Unmarshal(body, &searchResults)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error parsing search results: %s", err.Error()))
		}

		// Format the results
		formattedResults := formatPlaceSearchResults(searchResults, textQuery)
		return utils.NewTextContent(formattedResults)
	},
}

// Helper function to format place search results
func formatPlaceSearchResults(results map[string]interface{}, query string) string {
	var formattedResults string

	// Check if we have search results
	places, ok := results["places"].([]interface{})
	if !ok || len(places) == 0 {
		return "No places found for your search"
	}

	formattedResults = fmt.Sprintf("### Place Search Results for \"%s\"\n\n", query)

	// Process the top results
	for i, place := range places {
		if i >= 5 { // Limit to top 5 results
			break
		}

		placeData := place.(map[string]interface{})

		// Get name
		name := "Unnamed Location"
		if displayName, exists := placeData["displayName"].(map[string]interface{}); exists {
			if text, exists := displayName["text"].(string); exists {
				name = text
			}
		}

		// Get address
		address := "Address not available"
		if formattedAddress, exists := placeData["formattedAddress"].(string); exists {
			address = formattedAddress
		}

		// Get rating if available
		ratingStr := "N/A"
		if rating, exists := placeData["rating"]; exists {
			ratingStr = fmt.Sprintf("%.1f", rating)
		}

		// Get place types
		types := ""
		if typesList, exists := placeData["types"].([]interface{}); exists && len(typesList) > 0 {
			typeNames := make([]string, 0)
			for _, t := range typesList {
				if typeName, ok := t.(string); ok {
					// Clean up the type name (remove prefix if any)
					typeName = strings.Replace(typeName, "TYPE_", "", 1)
					typeName = strings.Title(strings.ToLower(strings.Replace(typeName, "_", " ", -1)))
					typeNames = append(typeNames, typeName)
					if len(typeNames) >= 2 {
						break
					}
				}
			}
			types = strings.Join(typeNames, ", ")
		}

		// Get price level if available
		priceLevel := ""
		if price, exists := placeData["priceLevel"].(string); exists {
			switch price {
			case "PRICE_LEVEL_FREE":
				priceLevel = "Free"
			case "PRICE_LEVEL_INEXPENSIVE":
				priceLevel = "$ (Inexpensive)"
			case "PRICE_LEVEL_MODERATE":
				priceLevel = "$$ (Moderate)"
			case "PRICE_LEVEL_EXPENSIVE":
				priceLevel = "$$$ (Expensive)"
			case "PRICE_LEVEL_VERY_EXPENSIVE":
				priceLevel = "$$$$ (Very Expensive)"
			}
		}

		// Get website if available
		website := ""
		if websiteUri, exists := placeData["websiteUri"].(string); exists {
			website = websiteUri
		}

		// Get location coordinates if available
		var lat, lng float64
		hasCoordinates := false
		if location, exists := placeData["location"].(map[string]interface{}); exists {
			if latValue, exists := location["latitude"].(float64); exists {
				lat = latValue
				if lngValue, exists := location["longitude"].(float64); exists {
					lng = lngValue
					hasCoordinates = true
				}
			}
		}

		// Create Google Maps links
		var mapsURL string

		// Link using place ID (most reliable)
		if placeID, exists := placeData["id"].(string); exists {
			mapsURL = fmt.Sprintf("https://www.google.com/maps/place/?q=place_id:%s", placeID)
			// directionsURL = fmt.Sprintf("https://www.google.com/maps/dir/?api=1&destination=place_id:%s", placeID)
		} else if hasCoordinates {
			// Fallback to coordinates if place ID not available
			mapsURL = fmt.Sprintf("https://www.google.com/maps/search/?api=1&query=%f,%f", lat, lng)
			// directionsURL = fmt.Sprintf("https://www.google.com/maps/dir/?api=1&destination=%f,%f", lat, lng)
		}

		// Format the result
		formattedResults += fmt.Sprintf("**%s**\n", name)
		formattedResults += fmt.Sprintf("📍 %s\n", address)

		if ratingStr != "N/A" {
			formattedResults += fmt.Sprintf("⭐ Rating: %s\n", ratingStr)
		}

		if types != "" {
			formattedResults += fmt.Sprintf("🏷️ %s\n", types)
		}

		if priceLevel != "" {
			formattedResults += fmt.Sprintf("💰 %s\n", priceLevel)
		}

		if mapsURL != "" {
			formattedResults += fmt.Sprintf("📌 [View on Google Map](%s) \n", mapsURL)
		}

		if website != "" {
			formattedResults += fmt.Sprintf("🌐 [Visit Website](%s)\n\n", website)
		}

		formattedResults += "\n"
	}

	return formattedResults
}
