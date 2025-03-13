package ai_tools

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/azukaar/plurality/src/utils"
)

var WebTool = utils.AITool{
	Name:        "Web",
	Description: "Visit a link to a website",
	ToolID:      "visit_link",
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "visit_link",
			Description: "Visit a link to a website",
			Parameters: []utils.ParameterToolsRequest{
				{
					Type: "object",
					Properties: map[string]utils.PropertyParameterToolsRequest{
						"URL": {
							Type:        "string",
							Description: "The URL you want to visit",
						},
					},
				},
			},
		},
	},
	LoadingString: "Visiting link...",
	IconURL:       "https://cdn-icons-png.flaticon.com/512/220/220221.png",
	Exec: func(input string) string {
		// Parse the URL parameter from JSON
		var params map[string]string
		err := json.Unmarshal([]byte(input), &params)
		if err != nil {
			return fmt.Sprintf("Error parsing URL parameter: %s", err.Error())
		}

		url := params["URL"]
		if url == "" {
			return "No URL provided"
		}

		// Add protocol if missing
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}

		// Create HTTP client with timeout
		client := &http.Client{
			Timeout: 30 * time.Second,
		}

		// Make the request
		resp, err := client.Get(url)
		if err != nil {
			return fmt.Sprintf("Error visiting URL: %s", err.Error())
		}
		defer resp.Body.Close()

		// Check status code
		if resp.StatusCode != http.StatusOK {
			return fmt.Sprintf("Error: received status code %d when visiting %s", resp.StatusCode, url)
		}

		// Read body
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Sprintf("Error reading response body: %s", err.Error())
		}

		// Parse the HTML
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
		if err != nil {
			return fmt.Sprintf("Error parsing HTML: %s", err.Error())
		}

		// Extract main content
		content := extractMainContent(doc, url)
		
		// Format the result
		result := fmt.Sprintf("## Content from %s\n\n", url)
		result += content
		
		// Add metadata
		result += fmt.Sprintf("\n\n---\n**Source:** [%s](%s)\n", url, url)
		result += fmt.Sprintf("**Retrieved:** %s\n", time.Now().Format("2006-01-02 15:04:05"))
		
		return result
	},
}

// extractMainContent tries to intelligently extract the main content from a webpage
func extractMainContent(doc *goquery.Document, url string) string {
	var content strings.Builder
	
	// Try to get the title
	title := doc.Find("title").Text()
	if title != "" {
		content.WriteString(fmt.Sprintf("# %s\n\n", title))
	}
	
	// Try to extract main content based on common patterns
	mainContent := doc.Find("article, main, #content, .content, .post, .article, .main")
	
	if mainContent.Length() > 0 {
		// Found main content container
		processContentNode(&content, mainContent)
	} else {
		// Fallback: try to get body content more intelligently
		body := doc.Find("body")
		
		// Remove navigation, sidebars, footers, etc.
		body.Find("nav, header, footer, aside, .sidebar, .navigation, .menu, .ads, .advertisement, script, style").Remove()
		
		// Get the remaining content
		processContentNode(&content, body)
	}
	
	result := content.String()
	
	// If the result is too long, truncate it
	const maxLength = 8000
	if len(result) > maxLength {
		result = result[:maxLength] + "...\n\n[Content truncated due to length]"
	}
	
	return result
}

// processContentNode extracts text and important elements from nodes
func processContentNode(builder *strings.Builder, selection *goquery.Selection) {
	selection.Each(func(i int, s *goquery.Selection) {
		// Process headings
		s.Find("h1, h2, h3, h4, h5, h6").Each(func(i int, h *goquery.Selection) {
			level := h.Get(0).Data[1] - '0' // Get heading level (1-6)
			prefix := strings.Repeat("#", int(level))
			builder.WriteString(fmt.Sprintf("\n%s %s\n\n", prefix, strings.TrimSpace(h.Text())))
		})
		
		// Process paragraphs
		s.Find("p").Each(func(i int, p *goquery.Selection) {
			text := strings.TrimSpace(p.Text())
			if text != "" {
				builder.WriteString(text + "\n\n")
			}
		})
		
		// Process lists
		s.Find("ul, ol").Each(func(i int, list *goquery.Selection) {
			builder.WriteString("\n")
			list.Find("li").Each(func(j int, item *goquery.Selection) {
				prefix := "-"
				if list.Get(0).Data == "ol" {
					prefix = fmt.Sprintf("%d.", j+1)
				}
				builder.WriteString(fmt.Sprintf("%s %s\n", prefix, strings.TrimSpace(item.Text())))
			})
			builder.WriteString("\n")
		})
		
		// Process links
		s.Find("a").Each(func(i int, a *goquery.Selection) {
			href, exists := a.Attr("href")
			text := strings.TrimSpace(a.Text())
			if exists && text != "" && !strings.Contains(builder.String(), text) {
				builder.WriteString(fmt.Sprintf("[%s](%s)\n", text, href))
			}
		})
	})
}
