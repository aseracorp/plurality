package ai_tools

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"

	"github.com/azukaar/plurality/src/utils"
)

var SearchTool = utils.AITool{
	Name:        "Search",
	Description: "Search on the internet",
	ToolID:      "search_web",
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "search_web",
			Description: "Search on the internet",
			Parameters: []utils.ParameterToolsRequest{
				{
					Type: "object",
					Properties: map[string]utils.PropertyParameterToolsRequest{
						"query": {
							Type:        "string",
							Description: "The query you want to search for",
						},
					},
				},
			},
		},
	},
	LoadingString: "Searching the web...",
	IconURL:       "https://cdn-icons-png.flaticon.com/512/220/220221.png",
	Exec: func(query string) string {
		GoogleSearchAPIKey := os.Getenv("GOOGLE_SEARCH_API_KEY")
		GoogleSearchEngineID := os.Getenv("GOOGLE_SEARCH_ENGINE_ID")

		if GoogleSearchAPIKey == "" {
			return "Google Search API Key not found"
		}

		if GoogleSearchEngineID == "" {
			return "Google Search Engine ID not found"
		}

		// Parse the query parameter from JSON
		var params map[string]string
		err := json.Unmarshal([]byte(query), &params)
		if err != nil {
			return fmt.Sprintf("Error parsing query: %s", err.Error())
		}

		searchQuery := params["query"]
		if searchQuery == "" {
			return "No search query provided"
		}

		// Build the Google Search API URL
		searchURL := fmt.Sprintf(
			"https://www.googleapis.com/customsearch/v1?key=%s&cx=%s&q=%s",
			GoogleSearchAPIKey,
			GoogleSearchEngineID,
			url.QueryEscape(searchQuery),
		)

		// Make the request
		resp, err := http.Get(searchURL)
		if err != nil {
			return fmt.Sprintf("Error making search request: %s", err.Error())
		}
		defer resp.Body.Close()

		// Read response body
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Sprintf("Error reading response: %s", err.Error())
		}

		// Parse the response
		var searchResults map[string]interface{}
		err = json.Unmarshal(body, &searchResults)
		if err != nil {
			return fmt.Sprintf("Error parsing search results: %s", err.Error())
		}

		// Format the results
		formattedResults := formatSearchResults(searchResults)
		return formattedResults
	},
}

// Helper function to format search results
func formatSearchResults(results map[string]interface{}) string {
	var formattedResults string

	// Check if we have search results
	items, ok := results["items"].([]interface{})
	if !ok || len(items) == 0 {
		return "No results found"
	}

	formattedResults = "### Search Results\n\n"

	// Process the top results
	for i, item := range items {
		if i >= 5 { // Limit to top 5 results
			break
		}

		result := item.(map[string]interface{})
		title := result["title"].(string)
		link := result["link"].(string)
		snippet := result["snippet"].(string)

		formattedResults += fmt.Sprintf("**%s**\n", title)
		formattedResults += fmt.Sprintf("%s\n", snippet)
		formattedResults += fmt.Sprintf("URL: %s\n\n", link)
	}

	return formattedResults
}
