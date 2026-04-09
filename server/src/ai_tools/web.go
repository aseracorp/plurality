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
	Cost:        200,
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "visit_link",
			Description: "Visit a link to a website",
			Parameters: &utils.ParameterToolsRequest{
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
	LoadingString: "Visiting {{URL}}",
	IconURL:       "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAYAAACqaXHeAAAACXBIWXMAAAsTAAALEwEAmpwYAAAGG2lUWHRYTUw6Y29tLmFkb2JlLnhtcAAAAAAAPD94cGFja2V0IGJlZ2luPSLvu78iIGlkPSJXNU0wTXBDZWhpSHpyZVN6TlRjemtjOWQiPz4gPHg6eG1wbWV0YSB4bWxuczp4PSJhZG9iZTpuczptZXRhLyIgeDp4bXB0az0iQWRvYmUgWE1QIENvcmUgOS4xLWMwMDIgNzkuYTZhNjM5NiwgMjAyNC8wMy8xMi0wNzo0ODoyMyAgICAgICAgIj4gPHJkZjpSREYgeG1sbnM6cmRmPSJodHRwOi8vd3d3LnczLm9yZy8xOTk5LzAyLzIyLXJkZi1zeW50YXgtbnMjIj4gPHJkZjpEZXNjcmlwdGlvbiByZGY6YWJvdXQ9IiIgeG1sbnM6eG1wPSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvIiB4bWxuczpkYz0iaHR0cDovL3B1cmwub3JnL2RjL2VsZW1lbnRzLzEuMS8iIHhtbG5zOnBob3Rvc2hvcD0iaHR0cDovL25zLmFkb2JlLmNvbS9waG90b3Nob3AvMS4wLyIgeG1sbnM6eG1wTU09Imh0dHA6Ly9ucy5hZG9iZS5jb20veGFwLzEuMC9tbS8iIHhtbG5zOnN0RXZ0PSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvc1R5cGUvUmVzb3VyY2VFdmVudCMiIHhtcDpDcmVhdG9yVG9vbD0iQWRvYmUgUGhvdG9zaG9wIDI1LjEyIChXaW5kb3dzKSIgeG1wOkNyZWF0ZURhdGU9IjIwMjUtMDMtMTRUMTE6MDM6MTBaIiB4bXA6TW9kaWZ5RGF0ZT0iMjAyNS0wMy0xNFQxMTowNTozNloiIHhtcDpNZXRhZGF0YURhdGU9IjIwMjUtMDMtMTRUMTE6MDU6MzZaIiBkYzpmb3JtYXQ9ImltYWdlL3BuZyIgcGhvdG9zaG9wOkNvbG9yTW9kZT0iMyIgeG1wTU06SW5zdGFuY2VJRD0ieG1wLmlpZDo5Y2U5MWExNC1mYTU5LWFiNGEtOGNmZC01ODAzNGQ3OThhZDMiIHhtcE1NOkRvY3VtZW50SUQ9ImFkb2JlOmRvY2lkOnBob3Rvc2hvcDoxYzAzZDFjMS01YmE5LTFhNGEtYTc2YS0zNGMxOTdhNTc3ZjEiIHhtcE1NOk9yaWdpbmFsRG9jdW1lbnRJRD0ieG1wLmRpZDpkMzg5MGM0My01M2FiLWI2NGUtYWY1YS0zMzhhNzM1ZDJkYTUiPiA8eG1wTU06SGlzdG9yeT4gPHJkZjpTZXE+IDxyZGY6bGkgc3RFdnQ6YWN0aW9uPSJjcmVhdGVkIiBzdEV2dDppbnN0YW5jZUlEPSJ4bXAuaWlkOmQzODkwYzQzLTUzYWItYjY0ZS1hZjVhLTMzOGE3MzVkMmRhNSIgc3RFdnQ6d2hlbj0iMjAyNS0wMy0xNFQxMTowMzoxMFoiIHN0RXZ0OnNvZnR3YXJlQWdlbnQ9IkFkb2JlIFBob3Rvc2hvcCAyNS4xMiAoV2luZG93cykiLz4gPHJkZjpsaSBzdEV2dDphY3Rpb249ImNvbnZlcnRlZCIgc3RFdnQ6cGFyYW1ldGVycz0iZnJvbSBhcHBsaWNhdGlvbi92bmQuYWRvYmUucGhvdG9zaG9wIHRvIGltYWdlL3BuZyIvPiA8cmRmOmxpIHN0RXZ0OmFjdGlvbj0ic2F2ZWQiIHN0RXZ0Omluc3RhbmNlSUQ9InhtcC5paWQ6OWNlOTFhMTQtZmE1OS1hYjRhLThjZmQtNTgwMzRkNzk4YWQzIiBzdEV2dDp3aGVuPSIyMDI1LTAzLTE0VDExOjA1OjM2WiIgc3RFdnQ6c29mdHdhcmVBZ2VudD0iQWRvYmUgUGhvdG9zaG9wIDI1LjEyIChXaW5kb3dzKSIgc3RFdnQ6Y2hhbmdlZD0iLyIvPiA8L3JkZjpTZXE+IDwveG1wTU06SGlzdG9yeT4gPC9yZGY6RGVzY3JpcHRpb24+IDwvcmRmOlJERj4gPC94OnhtcG1ldGE+IDw/eHBhY2tldCBlbmQ9InIiPz5XLidmAAAGaElEQVR42u2beUxUVxTGf2/2YUAlLgmhMeAarbahpkIUbRG1dYsmNdbaYqpGS0QsuNWVCrEJdtEmlpaYmjaS1qhtbCTVaFKXtLbRpBpjK1EUAaHUVMoywAAzw+sf80CWebPPm6F4kpdM5t13z/m+d8+955x3ryCKIkGW6cDz0jUaGAtEy7StA0qB+8BN6boSTOOEIBAgAK8DC4BpwCg/+ysDfgV+BI4DYrgSMAzIBlYAcUF6YeXAt8BB4HG4EBAtAc8ColBGzMCnEhF1fvUkiqI/V6YoinVi6KROssFnDL6OgFjgMDCf8JAzwDqgWgkXWAZ8BUQQXtICrAJOePOQyksle6WZONzAI9l0XLIxKCMgH3gv2CjsVZW0Xz4POr3Xz6oio9DNno+g1e0HtgeSgH3ArqCDLyulcccGv/pQDR3OoI8KEUyRHwC7A+ECuUqARxRpLjzgdzcdtf9gKTqMZHOuvwSkATlKOHBHQz32irKA9GUrK+38mSNhkBWNi3uTgKNOX5bFQvulc3Q0mf0yVDN+Itrnpjh80WAAtRrsdv8HU0+7jgI3gD+8JeAHZ39ar1+j+fOPEc0NAXlbuuQUTJnbEQxGDEuW0/r9NwGIbwVnWMZ44wK5UubWQ1pPn6Tpw5yAgQdo/+UibedOA2BcthL9rFcDk471lNFy84GzVWAscNdZ4+YvPqH90vmA+786fgyD8guerAaVDxAb6x0GRg7Cev0aluNfe74SDB/B4M+KnN0aJ6XbLl2gUPEQRug5ENUj43sRfyBQmgqBVFcuMB6YpTR+XeJ05yvD33/RkP4G9vJ7gVI1S8IoS8BBxcEnp2BYsrwv+Ec1NO7MpKPu30CrPCjnAnHAPF971U5OQD3+WS9GvQrN5BfQjJvQNyKsqaJx8ztgtwWD83kS1vLeBGT49SZfmoNuRmpAcgFz3la34HVJM7E/LMdeXemLmgxga3cXUAFr/Irk6mr9j+Du/Enj5rWIDfUu2+lfXYwpexdie5uvqtZ0Yu8kIAH5Sq0iYistwZyzyW07fcorRKxa74j4GmWIcp/fRUuYuwhYFErw9rK7mHdnebBaJBOR7iBJbLUg2mTCZpvVE7WLuhOQEjLw5fdo3LnR/ST74jRMm/Y8mUS1WoQIk/NAaNgIT1SndBIwFJgYEvCVDxzg3dQktFOSiNzyfq/wUYM+Za5zN5mz0BP1E4GhKiARR01fWfBVFY7ih5vsTzcjlchtztN644o16JJm9vjPsGgpupfnemLCMCBRg6PCqyz46oeYd2SCzfVSZ1j4Gsa0dU7TccFoBEHAlL0LQ3UaYl0tgikKdfwYb0yJ1QATlAQvtrVh3pPldgnTp85zCr7lyCHaf/4J1YgYdFOTMSx9E3XsSIgd6Ys5EzRAjJIEtJ46htjc5HbYR6zL6lMyM+duxVZyyzGKKsqwVJQhtrZgfGutr+bEqACLkgRYb/7u+s3Pno9pw7a+dYNrV7rA9yC0+Ds6HtX4ao5FpbT/a0bJ+6huZioRa9+VzQxl55SaKp/tUZwA4/K3EfQG5+Ftxjb55MlgkL+nN/QfAoSowUTtL0CXmIxgNKIaEk3E6oyu8FbxEQkYlVaqjnkG06Y9juVMpwW1JlSBqFED1IRKu2A0EmKpUQElDFwpUeHDN/X/kVSrgKsEaL9NP5PHwFUVUAvcHoAE3AZqO5fBiwOQgIvd44DiAUhAcXcCbuDndjN/ojGP+jdFBrK7OglzV1m8AzgCbPE5zW0yI1qCl1e5ygV8kCMS5h7fBQr8IcBy6hitxSeDV0cILLkF3UPhTikHzuLr1yGrFdFq7Q++f1bC6jQZyh4Ak1+2q2zwDnBB9lGbLSwRidZ2T5tekDC6TIfT5WeijrAkQDVosKdN0z2pB5QCec6eNixehnZKUliB10xKQB3nUSU4j167Q8D1Rsl7ONknhCjS8uUh2n+7DB32kAEXDBFoE6ZiXL0eQatz1/w+MpukXBEwCbgl63dtbSF1CUGvB5XHBa3JyGyTc7dVNg2ZvYL9SFYCRbLzh5uHi+Tmg34iea7ASy7t0cmKfWL/k32eYPPmeEl+PwKf7ykub8/Y7O0H4PcG+8zQgD4yg6RgHI6DSuEiZySbTnj7oK9fhqpxnAzdCNSHEHi9ZMMCfKxuD/iDk0+Pzj49PK3c8flE0dwwBK0uXjAY41wUKxU9Pv8fqXyZC+iXgWMAAAAASUVORK5CYII=",
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
	/*mainContent := doc.Find("article, main, #content, .content, .post, .article, .main")

	if mainContent.Length() > 0 {
		// Found main content container
		processContentNode(&content, mainContent)
	} else {*/
	// Fallback: try to get body content more intelligently
	body := doc.Find("body")

	// Remove navigation, sidebars, footers, etc.
	body.Find("footer, aside, .ads, .advertisement, script, style").Remove()

	// Get the remaining content
	processContentNode(&content, body)
	//}

	result := content.String()

	// If the result is too long, truncate it
	const maxLength = 8000
	if len(result) > maxLength {
		result = result[:maxLength] + "...\n\n[Content truncated due to length]"
	}

	// search for all links
	result += "\n\n## Links\n\n"
	links := doc.Find("a")
	links.Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		// append all links to the end of the result
		if exists && !strings.Contains(result, href) {
			result += fmt.Sprintf("[%s](%s)\n", href, href)
		}
	})

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
