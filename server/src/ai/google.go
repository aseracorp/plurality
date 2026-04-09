package ai

import (
	"bytes"
	"context"
	"encoding/base64"
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

// --- Gemini Request Types ---

type GeminiChatRequest struct {
	Contents          []GeminiContent        `json:"contents"`
	Tools             []GeminiTool           `json:"tools,omitempty"`
	GenerationConfig  GeminiGenerationConfig `json:"generationConfig,omitempty"`
	SystemInstruction *GeminiContent         `json:"systemInstruction,omitempty"`
}

type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations"`
}

type GeminiFunctionDeclaration struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Parameters  *utils.ParameterToolsRequest `json:"parameters,omitempty"`
}

type GeminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

// convertMessagesToGemini converts OpenAI-format messages to Gemini's content format.
func convertMessagesToGemini(messages []utils.Message, model utils.Model) ([]GeminiContent, float64) {
	var contents []GeminiContent
	var basePrice float64

	for _, msg := range messages {
		geminiRole := ""
		switch msg.Role {
		case "user":
			geminiRole = "user"
		case "assistant":
			geminiRole = "model"
		case "tool":
			geminiRole = "function"
		default:
			continue
		}

		var parts []GeminiPart

		if msg.Role == "assistant" {
			// Text content
			text := msg.TextContent()
			if text != "" {
				parts = append(parts, GeminiPart{Text: text})
			}
			// Tool calls become FunctionCall parts
			for _, tc := range msg.ToolCalls {
				var argsMap map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &argsMap); err != nil {
					argsMap = map[string]interface{}{"arguments_string": tc.Function.Arguments}
				}
				parts = append(parts, GeminiPart{
					FunctionCall: &GeminiFunctionCall{
						Name: tc.Function.Name,
						Args: argsMap,
					},
				})
			}
		} else if msg.Role == "tool" {
			// Tool results become FunctionResponse parts
			var responseMap map[string]interface{}
			resultText := msg.TextContent()
			if ai_tools.ShouldStripResponse(resultText) {
				resultText = "Tool result displayed to user."
			}
			if err := json.Unmarshal([]byte(resultText), &responseMap); err != nil {
				responseMap = map[string]interface{}{"content": resultText}
			}
			parts = append(parts, GeminiPart{
				FunctionResponse: &GeminiFunctionResponse{
					Name:     msg.Name,
					Response: responseMap,
				},
			})
		} else {
			// User messages: text + images
			for _, contentPart := range msg.ContentParts() {
				switch contentPart.Type {
				case "text":
					if contentPart.Text != "" {
						parts = append(parts, GeminiPart{Text: contentPart.Text})
					}
				case "image_url":
					if contentPart.ImageURL != nil {
						mimeType, b64Data, err := getImageBase64(contentPart.ImageURL.URL)
						if err != nil {
							parts = append(parts, GeminiPart{Text: fmt.Sprintf("[Image processing failed: %s]", err)})
							continue
						}
						parts = append(parts, GeminiPart{
							InlineData: &GeminiInlineData{
								MimeType: mimeType,
								Data:     b64Data,
							},
						})
						basePrice += GetPriceFromTokenUsage(IMAGE_VISION, GOOGLE, model, 0)
					}
				}
			}
		}

		if len(parts) > 0 {
			contents = append(contents, GeminiContent{
				Role:  geminiRole,
				Parts: parts,
			})
		}
	}

	return contents, basePrice
}

// SendChatCompletionGoogle sends a chat completion request to the Gemini API.
func SendChatCompletionGoogle(ctx context.Context, model utils.Model, conversation utils.Conversation, systemPrompt string, payload ChatPayload) (io.ReadCloser, int, error) {
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		return nil, 0, fmt.Errorf("GOOGLE_API_KEY is not set")
	}

	modelName := strings.TrimPrefix(model.Name, "Gemini/")
	if modelName == "" {
		modelName = "gemini-2.5-flash"
	}

	fullSystemPrompt := systemPrompt +
		time.Now().String() +
		" on " +
		strconv.Itoa(time.Now().Day()) + "/" +
		strconv.Itoa(int(time.Now().Month())) + "/" +
		strconv.Itoa(time.Now().Year())

	// Convert messages
	geminiContents, basePrice := convertMessagesToGemini(conversation.Messages, model)

	// System instruction
	systemInstruction := &GeminiContent{
		Role:  "user",
		Parts: []GeminiPart{{Text: fullSystemPrompt}},
	}

	// Generation config
	maxTokens := 4096
	temperature := 0.7
	topP := 0.9
	topK := 50

	if model.Params != nil {
		if v, err := strconv.Atoi(model.Params["max_tokens"]); err == nil {
			maxTokens = v
		}
		if v, err := strconv.ParseFloat(model.Params["temperature"], 64); err == nil {
			temperature = v
		}
		if v, err := strconv.ParseFloat(model.Params["top_p"], 64); err == nil {
			topP = v
		}
		if v, err := strconv.Atoi(model.Params["top_k"]); err == nil {
			topK = v
		}
	}

	config := GeminiGenerationConfig{
		Temperature:     &temperature,
		TopP:            &topP,
		TopK:            &topK,
		MaxOutputTokens: &maxTokens,
	}

	// Tools
	var geminiTools []GeminiTool
	if CheckActionModel(model.Name) {
		registeredTools := ai_tools.GetRequests(model, payload.ClientSideTools)
		if len(registeredTools) > 0 {
			var declarations []GeminiFunctionDeclaration
			for _, tool := range registeredTools {
				declarations = append(declarations, GeminiFunctionDeclaration{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  tool.Function.Parameters,
				})
			}
			geminiTools = append(geminiTools, GeminiTool{FunctionDeclarations: declarations})
		}
	}

	requestData := GeminiChatRequest{
		Contents:          geminiContents,
		Tools:             geminiTools,
		GenerationConfig:  config,
		SystemInstruction: systemInstruction,
	}

	// Cost estimation
	inputEstimate := fullSystemPrompt
	for _, content := range geminiContents {
		for _, part := range content.Parts {
			if part.Text != "" {
				inputEstimate += part.Text + " "
			}
		}
	}

	err, priceToken := GetPrice(TEXT_INPUT, GOOGLE, model, inputEstimate)
	priceToken += 32.0 + basePrice
	if err != nil {
		return nil, 0, err
	}

	canPerform, err := db.CheckSufficientCredits(ctx, priceToken)
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

	apiURL := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?key=%s&alt=sse",
		modelName, apiKey,
	)

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, 0, fmt.Errorf("Gemini API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return resp.Body, int(priceToken), nil
}

// --- Helper: Image Base64 Encoding ---

func getImageBase64(imageURL string) (mimeType string, b64Data string, err error) {
	if strings.HasPrefix(imageURL, "data:") {
		parts := strings.SplitN(imageURL, ",", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("invalid data URI format")
		}
		header := parts[0]
		b64Data = parts[1]

		mimeParts := strings.SplitN(strings.TrimPrefix(header, "data:"), ";", 2)
		if len(mimeParts) > 0 && mimeParts[0] != "" {
			mimeType = mimeParts[0]
		} else {
			mimeType = "application/octet-stream"
		}
		return mimeType, b64Data, nil
	}

	// External URL
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(imageURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("failed to fetch image: status %d", resp.StatusCode)
	}

	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	mimeType = http.DetectContentType(imageData)
	b64Data = base64.StdEncoding.EncodeToString(imageData)
	return mimeType, b64Data, nil
}
