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
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// convertMessagesToClaude converts OpenAI-format messages to Claude's format.
// Claude uses content arrays with typed blocks instead of OpenAI's role-based tool messages.
func convertMessagesToClaude(messages []utils.Message, model utils.Model) ([]ClaudeMessageReq, float64) {
	var claudeMessages []ClaudeMessageReq
	var priceEstimate float64

	for _, msg := range messages {
		var contents []ClaudeContentReq

		switch msg.Role {
		case "assistant":
			// Text content
			text := msg.TextContent()
			if text != "" {
				priceEstimate += float64(len(text)) / 4.0
				contents = append(contents, ClaudeContentReq{
					Type: "text",
					Text: text,
				})
			}
			// Tool calls become tool_use content blocks
			for _, tc := range msg.ToolCalls {
				args := utils.ParseJsonString(tc.Function.Arguments)
				if args == nil {
					args = map[string]string{"_placeholder": "true"}
				}
				contents = append(contents, ClaudeContentReq{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: args,
				})
			}

		case "tool":
			// Tool result messages become tool_result content blocks.
			// Claude requires tool_result to be in a "user" role message.
			// If the tool returned image parts, include them as image blocks.
			var toolContent []ClaudeContentReq
			for _, part := range msg.ContentParts() {
				switch part.Type {
				case "image_url":
					if part.ImageURL != nil {
						mediaType, b64Data := parseDataURI(part.ImageURL.URL)
						toolContent = append(toolContent, ClaudeContentReq{
							Type: "image",
							Source: &ClaudeImageSourceReq{
								Type:      "base64",
								MediaType: mediaType,
								Data:      b64Data,
							},
						})
					}
				default:
					text := part.Text
					if text != "" && msg.Name != "conversation_attachments" && ai_tools.ShouldStripResponse(text) {
						text = "Tool result displayed to user."
					}
					if text != "" {
						priceEstimate += float64(len(text)) / 4.0
						toolContent = append(toolContent, ClaudeContentReq{
							Type: "text",
							Text: text,
						})
					}
				}
			}
			if len(toolContent) == 0 {
				continue
			}
			// If only one text block, use simple string content
			if len(toolContent) == 1 && toolContent[0].Type == "text" {
				contents = append(contents, ClaudeContentReq{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content:   toolContent[0].Text,
				})
			} else {
				// Multi-part tool result (text + images)
				contents = append(contents, ClaudeContentReq{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content:   toolContent,
				})
			}
			// Override role to "user" since Claude expects tool results in user messages
			if len(contents) > 0 {
				claudeMessages = append(claudeMessages, ClaudeMessageReq{
					Role:    "user",
					Content: contents,
				})
				continue // skip the append below
			}

		case "user":
			for _, part := range msg.ContentParts() {
				switch part.Type {
				case "image_url":
					if part.ImageURL != nil {
						mediaType, b64Data := parseDataURI(part.ImageURL.URL)
						contents = append(contents, ClaudeContentReq{
							Type: "image",
							Source: &ClaudeImageSourceReq{
								Type:      "base64",
								MediaType: mediaType,
								Data:      b64Data,
							},
						})
					}
				default:
					if part.Text != "" {
						priceEstimate += float64(len(part.Text)) / 4.0
						contents = append(contents, ClaudeContentReq{
							Type: "text",
							Text: part.Text,
						})
					}
				}
			}
		}

		if len(contents) > 0 {
			claudeMessages = append(claudeMessages, ClaudeMessageReq{
				Role:    msg.Role,
				Content: contents,
			})
		}
	}

	return claudeMessages, priceEstimate
}

// SendChatCompletionClaude sends a chat completion request to the Claude API.
func SendChatCompletionClaude(ctx context.Context, model utils.Model, conversation utils.Conversation, systemPrompt string, payload ChatPayload) (io.ReadCloser, int, error) {
	fullSystemPrompt := systemPrompt +
		time.Now().String() +
		" on " +
		strconv.Itoa(time.Now().Day()) + "/" +
		strconv.Itoa(int(time.Now().Month())) + "/" +
		strconv.Itoa(time.Now().Year())

	apiKey := os.Getenv("CLAUDE_API_KEY")
	if apiKey == "" {
		return nil, 0, fmt.Errorf("CLAUDE_API_KEY is not set")
	}

	if model.Name == "" {
		model.Name = "Claude/claude-haiku-4-6"
	}

	// Estimate system prompt cost
	_, priceToken := GetPrice(TEXT_INPUT, CLAUDE, model, fullSystemPrompt)

	// Optimize messages — replace stale attachments with placeholders
	optimizedMessages, hasAttachments := PrepareMessagesForAI(conversation.Messages, model)

	// Convert messages to Claude format
	claudeMessages, contentPrice := convertMessagesToClaude(optimizedMessages, model)
	priceToken += contentPrice

	modelName := strings.TrimPrefix(model.Name, "Claude/")

	temperature := 0.7
	if model.Params != nil {
		if temp, err := strconv.ParseFloat(model.Params["temperature"], 64); err == nil {
			temperature = temp
		}
	}

	maxTokens := 4096
	if model.Params != nil {
		if tokens, err := strconv.Atoi(model.Params["max_tokens"]); err == nil {
			maxTokens = tokens
		}
	}

	requestData := ClaudeChatRequest{
		Model:       modelName,
		Messages:    claudeMessages,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Stream:      true,
		System:      fullSystemPrompt,
	}

	tools := ai_tools.GetClaudeRequests(model, payload.ClientSideTools, hasAttachments)
	if CheckActionModel(model.Name) && len(tools) > 0 {
		requestData.Tools = tools
	}

	canPerform, err := db.CheckSufficientCredits(ctx, priceToken+1.0)
	if err != nil {
		return nil, 0, err
	}
	if !canPerform {
		return nil, 0, fmt.Errorf("insufficient credits for this action")
	}

	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("Claude API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return resp.Body, int(priceToken), nil
}

// parseDataURI splits a "data:mime/type;base64,AAAA..." URI into media type and raw base64.
func parseDataURI(dataURI string) (string, string) {
	commaIdx := strings.Index(dataURI, ",")
	if commaIdx < 0 {
		return "image/jpeg", dataURI
	}
	header := dataURI[5:commaIdx] // strip "data:"
	mediaType := strings.TrimSuffix(header, ";base64")
	return mediaType, dataURI[commaIdx+1:]
}
