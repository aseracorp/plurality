// src/ai/google.go
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

// Gemini Structs (based on v1beta API for streaming and function calling)

type GeminiChatRequest struct {
	Contents         []GeminiContent        `json:"contents"`
	Tools            []GeminiTool           `json:"tools,omitempty"`
	GenerationConfig GeminiGenerationConfig `json:"generationConfig,omitempty"`
	SystemInstruction *GeminiContent        `json:"systemInstruction,omitempty"` // Use system instruction
}

type GeminiFunctionResponse struct {
    Name     string         `json:"name"`
    Response map[string]any `json:"response"`
}


type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations"`
}

type GeminiFunctionDeclaration struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Parameters  *utils.ParameterToolsRequest    `json:"parameters,omitempty"`
}

// Simple schema definition, adjust if more complex types are needed
type GeminiSchema struct {
    Type       string                   `json:"type"` // e.g., "object"
    Properties map[string]GeminiSchemaProperty `json:"properties,omitempty"`
    Required   []string                 `json:"required,omitempty"`
}

type GeminiSchemaProperty struct {
    Type        string `json:"type"` // e.g., "string", "number", "integer"
    Description string `json:"description,omitempty"`
}


type GeminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
    // CandidateCount - not typically used for chat, usually 1
	// ResponseMimeType - could be "application/json" for specific tool use cases
}


// --- Response Structs (for streaming) ---

// Gemini streams an array of chunks, often just one per SSE message
type GeminiChatResponseStream struct {
	Candidates     []GeminiCandidate     `json:"candidates"`
	UsageMetadata  *GeminiUsageMetadata  `json:"usageMetadata,omitempty"` // Sometimes present at the end
	PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"` // For safety ratings etc.
}

// --- SendChatCompletionGoogle ---

func SendChatCompletionGoogle(ctx context.Context, model utils.Model, payload utils.Conversation, systemPrompt string) (io.ReadCloser, int, error) {
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		return nil, 0, fmt.Errorf("GOOGLE_API_KEY is not set")
	}

	modelName := strings.TrimPrefix(model.Name, "Gemini/")
	if modelName == "" {
		// Default model if none specified within Gemini prefix
		modelName = "gemini-1.5-flash-latest" // Or gemini-1.5-pro-latest, etc.
		model.Name = "Gemini/" + modelName
	}

	// Construct API URL
	// Use v1beta for streaming function calling support if needed, otherwise v1 might suffice
	apiURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?key=%s&alt=sse", modelName, apiKey)

	// --- Build Gemini Request ---
	geminiContents := make([]GeminiContent, 0, len(payload.Messages))
	basePrice := 0.0 // For potential image costs

	// 1. System Prompt
	fullSystemPrompt := systemPrompt +
		time.Now().String() +
		" on " +
		strconv.Itoa(time.Now().Day()) +
		"/" +
		strconv.Itoa(int(time.Now().Month())) +
		"/" +
		strconv.Itoa(time.Now().Year())
	
	geminiSystemInstruction := &GeminiContent{
		Role: "user", // System instructions are typically given role: user, parts: [...]
		Parts: []GeminiPart{
			{Text: fullSystemPrompt},
		},
	}

	// 2. Map Conversation Messages
	for _, msg := range payload.Messages {
		geminiRole := ""
		switch msg.Role {
		case "user":
			geminiRole = "user"
		case "assistant":
			geminiRole = "model"
		case "tool": // Gemini uses "function" role for tool results
			geminiRole = "function"
		default:
			utils.Warn("Unknown role in conversation, skipping message:", msg.Role)
			continue
		}

		parts := make([]GeminiPart, 0, len(msg.Content))
		for _, content := range msg.Content {
			switch content.Type {
			case "text", "snippet": // Treat snippet as text
				if content.Text != "" {
					parts = append(parts, GeminiPart{Text: content.Text})
				}
			case "image_url":
				// Fetch image, determine mime type, base64 encode
				mimeType, b64Data, err := getImageBase64(content.ImageURL.URL)
				if err != nil {
					utils.Error("Failed to process image URL for Gemini", err, content.ImageURL.URL)
					// Option: Add a text part indicating failure, or skip
					parts = append(parts, GeminiPart{Text: fmt.Sprintf("[Image processing failed: %s]", content.ImageURL.URL)})
					continue // Skip adding the image part if processing failed
				}
				parts = append(parts, GeminiPart{
					InlineData: &GeminiInlineData{
						MimeType: mimeType,
						Data:     b64Data,
					},
				})
				// Add estimated image cost (check Gemini pricing for specifics)
				// Placeholder cost - adjust based on actual Gemini pricing
				basePrice += GetPriceFromTokenUsage(IMAGE_VISION, GOOGLE, model, 0) // Assuming a fixed cost or token equivalent

			case "tool_use":
                 // This represents a function call the *assistant* made previously
                 // Need to marshal args back to map[string]any if they are stored as string
                var argsMap map[string]any
                err := json.Unmarshal([]byte(content.ToolCall.Arguments), &argsMap)
                if err != nil {
                     utils.Warn("Failed to unmarshal tool_use arguments for Gemini, sending as string map", err)
                     argsMap = map[string]any{"arguments_string": content.ToolCall.Arguments} // Fallback
                }
				parts = append(parts, GeminiPart{
					FunctionCall: &GeminiFunctionCall{
						Name: content.ToolCall.Name,
						Args: argsMap,
					},
				})
			case "tool_result":
				// This represents the result of a function call
                // Need to marshal result back to map[string]any if stored as string
                var responseMap map[string]any
                // Assume content.Text contains the JSON result string
                err := json.Unmarshal([]byte(content.Text), &responseMap)
                if err != nil {
                    utils.Warn("Failed to unmarshal tool_result text for Gemini, sending as content map", err)
                    responseMap = map[string]any{"content": content.Text} // Fallback
                }

				parts = append(parts, GeminiPart{
					FunctionResponse: &GeminiFunctionResponse{
						Name: content.ToolUseId, // Assuming ToolUseId stores the original function name for the result
                        Response: responseMap,
					},
				})

			default:
				utils.Warn("Unsupported content type for Gemini, skipping:", content.Type)
			}
		}

		if len(parts) > 0 {
			geminiContents = append(geminiContents, GeminiContent{
				Role:  geminiRole,
				Parts: parts,
			})
		}
	}

    // --- Generation Config ---
	config := GeminiGenerationConfig{
		// Set defaults, override with model.Params if present
	}
    maxTok := 4096 // Default
	temp := 0.7    // Default
	topP := 0.9    // Default (Gemini often uses 0.9 or 1.0)
	topK := 50     // Default

	if pMaxTok, ok := model.Params["max_tokens"]; ok {
		if val, err := strconv.Atoi(pMaxTok); err == nil {
			maxTok = val
		}
	}
    config.MaxOutputTokens = &maxTok

	if pTemp, ok := model.Params["temperature"]; ok {
		if val, err := strconv.ParseFloat(pTemp, 64); err == nil {
			temp = val
		}
	}
    config.Temperature = &temp

    if pTopP, ok := model.Params["top_p"]; ok {
		if val, err := strconv.ParseFloat(pTopP, 64); err == nil {
			topP = val
		}
	}
    config.TopP = &topP

    if pTopK, ok := model.Params["top_k"]; ok {
		if val, err := strconv.Atoi(pTopK); err == nil {
			topK = val
		}
	}
    config.TopK = &topK

    // Stop sequences if needed (example)
	// config.StopSequences = []string{"<|eot_id|>"} // Adjust if Gemini uses specific stop tokens

	// --- Tools ---
	var geminiTools []GeminiTool
	if CheckActionModel(model.Name) {
		registeredTools := ai_tools.GetRequests(model) // Assuming this returns your internal tool format
		if len(registeredTools) > 0 {
			utils.Log("Found registered tools for Gemini chat request")
			declarations := make([]GeminiFunctionDeclaration, 0, len(registeredTools))
			for _, tool := range registeredTools {
				// // Convert your tool parameters schema to Gemini's JSON Schema format
        //         geminiParams, err := convertToGeminiSchema(tool.Function.Parameters)
        //         if err != nil {
        //             utils.Error("Failed to convert tool parameters to Gemini schema", err, tool.Function.Name)
        //             continue // Skip tool if parameters are invalid
        //         }

				declarations = append(declarations, GeminiFunctionDeclaration{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  tool.Function.Parameters,
				})
			}
			if len(declarations) > 0 {
				geminiTools = append(geminiTools, GeminiTool{FunctionDeclarations: declarations})
			}
		}
	}


	// --- Final Request Body ---
	requestData := GeminiChatRequest{
		Contents:         geminiContents,
		Tools:            geminiTools,
		GenerationConfig: config,
		SystemInstruction: geminiSystemInstruction,
	}


	// --- Cost Estimation & Credit Check ---
	// Estimate input cost - Gemini counts tokens differently (especially for images)
	// This is a rough estimate; final cost comes from response metadata.
	// Need a way to estimate token count for Gemini's format (text + images + tool definitions)
    inputMessageEstimate := "" // Build a representative string or use a dedicated Gemini tokenizer
    for _, content := range requestData.Contents {
        inputMessageEstimate += content.Role + " "
        for _, part := range content.Parts {
            if part.Text != "" {
                inputMessageEstimate += part.Text + " "
            } else if part.InlineData != nil {
                inputMessageEstimate += "[IMAGE] " // Placeholder for image token estimation
            } // Add estimations for function calls/responses if needed
        }
    }

	err, priceToken := GetPrice(TEXT_INPUT, GOOGLE, model, inputMessageEstimate) // Use TEXT_INPUT for base text cost
	priceToken += 32.0 + basePrice // Add base cost + calculated image cost

	if err != nil {
		return nil, 0, fmt.Errorf("failed to estimate input price: %w", err)
	}

	canPerform, err := db.CheckSufficientCredits(ctx, priceToken)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to check credits: %w", err)
	}
	if !canPerform {
		return nil, 0, fmt.Errorf("insufficient credits for this action")
	}

	utils.Log("Attempting Gemini chat request with model: %s", modelName)

	// --- Marshal and Send Request ---
	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal Gemini request: %w", err)
	}

    // Log the request body for debugging if needed (careful with sensitive data/images)
	// utils.Debug("Gemini Request Body:", string(jsonData))

	// Optional: Deduct estimated input cost *now*
	// _, err = db.RemoveCredits(ctx, priceToken, utils.UserAction{
	// 	Type:     TEXT_INPUT, // Or a combined type if images are included
	// 	Provider: GOOGLE,
	// 	Model:    model,
	// })
	// if err != nil {
	//     // Decide how to handle: proceed optimistically or fail?
	//     utils.Error("Failed to deduct preliminary input credits for Gemini", err)
	//     // return nil, 0, fmt.Errorf("failed to deduct input credits: %w", err)
	// }


	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create Gemini HTTP request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
    // Note: API key is in the URL parameter for Gemini

	client := &http.Client{Timeout: 5 * time.Minute} // Adjust timeout as needed
	resp, err := client.Do(req.WithContext(ctx)) // Pass context for cancellation
	if err != nil {
		// Consider refunding preliminary input credits if deducted
		return nil, 0, fmt.Errorf("failed to send Gemini request: %w", err)
	}

	// --- Handle Response ---
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close() // Close body even on error
		strStatus := strconv.Itoa(resp.StatusCode)
		utils.Error("Gemini API request failed", fmt.Errorf("status %s", strStatus), string(respBody))
		// Consider refunding preliminary input credits if deducted
		return nil, 0, fmt.Errorf("Gemini API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

    // Return the streaming body, the *estimated* input cost, and no error
	// Actual costs will be calculated during stream processing.
	return resp.Body, int(priceToken), nil
}


// --- Helper Functions ---

// getImageBase64 fetches an image URL, detects MIME type, and returns base64 data.
func getImageBase64(imageURL string) (mimeType string, b64Data string, err error) {
    if strings.HasPrefix(imageURL, "data:") {
        // Handle data URI
        parts := strings.SplitN(imageURL, ",", 2)
        if len(parts) != 2 {
            return "", "", fmt.Errorf("invalid data URI format")
        }
        header := parts[0] // e.g., "data:image/png;base64"
        b64Data = parts[1]

        mimeParts := strings.SplitN(strings.TrimPrefix(header, "data:"), ";", 2)
        if len(mimeParts) > 0 {
             mimeType = mimeParts[0]
             if mimeType == "" {
                 mimeType = "application/octet-stream" // Default if empty
             }
        } else {
            return "", "", fmt.Errorf("invalid data URI header")
        }
         // Check if it's actually base64 encoded
        if !strings.HasSuffix(header, "base64") {
            // If not base64, encode it (e.g., URL encoded data)
            // This might need more robust handling depending on expected data URI formats
             decodedBytes, decodeErr := io.ReadAll(strings.NewReader(b64Data)) // Assuming URL encoding or similar
             if decodeErr != nil {
                return "", "", fmt.Errorf("failed to read non-base64 data URI: %w", decodeErr)
             }
             b64Data = base64.StdEncoding.EncodeToString(decodedBytes)
        }

        // Validate base64 data
        _, b64Err := base64.StdEncoding.DecodeString(b64Data)
        if b64Err != nil {
            return "", "", fmt.Errorf("invalid base64 data in data URI: %w", b64Err)
        }
        return mimeType, b64Data, nil
    }


	// Handle external URL
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(imageURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch image URL %s: %w", imageURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("failed to fetch image URL %s: status code %d", imageURL, resp.StatusCode)
	}

	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read image data from %s: %w", imageURL, err)
	}

	// Detect MIME type
	mimeType = http.DetectContentType(imageData)
    // Gemini requires specific image MIME types, validate or default if necessary
    allowedMimeTypes := map[string]bool{
        "image/png":  true,
        "image/jpeg": true,
        "image/webp": true,
        "image/heic": true,
        "image/heif": true,
    }
    if !allowedMimeTypes[mimeType] {
         utils.Warn("Detected MIME type not explicitly supported by Gemini, using anyway:", mimeType)
         // Or you could return an error:
         // return "", "", fmt.Errorf("unsupported image MIME type for Gemini: %s", mimeType)
    }


	b64Data = base64.StdEncoding.EncodeToString(imageData)

	return mimeType, b64Data, nil
}

// convertToGeminiSchema converts your internal tool parameter structure to Gemini's format.
// This is a basic example and might need adjustment based on your ai_tools.FunctionParameter structure.
func convertToGeminiSchema(params interface{}) (*GeminiSchema, error) {
    // Assuming params is already in a map[string]interface{} structure similar to JSON Schema
    // If params is a struct, you might need reflection or specific mapping logic.

    // If params is nil or not provided, return nil schema
    if params == nil {
        return nil, nil // No parameters needed
    }

    // Example: Assume params is map[string]interface{} representing properties
    paramMap, ok := params.(map[string]interface{})
    if !ok {
        // If params is already a correctly structured *ai_tools.FunctionParameters, adapt this.
        // For now, assume it needs to be a map representing the root 'properties'.
        // Let's try marshaling and unmarshaling as a fallback
        tempJson, err := json.Marshal(params)
        if err != nil {
             return nil, fmt.Errorf("failed to marshal params for schema conversion: %w", err)
        }
        var genericMap map[string]interface{}
        err = json.Unmarshal(tempJson, &genericMap)
         if err != nil {
             return nil, fmt.Errorf("failed to unmarshal params into map for schema conversion: %w", err)
        }
        paramMap = genericMap
        // return nil, fmt.Errorf("tool parameters must be a map[string]interface{} representing properties, got %T", params)
    }

    // Check if the map represents the schema root (e.g. contains 'type' and 'properties')
    // or just the properties themselves. Adapt as needed.
    // This example assumes paramMap *is* the properties map.

    geminiProps := make(map[string]GeminiSchemaProperty)
    required := []string{} // Populate this if your structure defines required fields

    for name, propInterface := range paramMap {
        propMap, ok := propInterface.(map[string]interface{})
        if !ok {
            return nil, fmt.Errorf("invalid format for parameter property '%s', expected map[string]interface{}", name)
        }

        prop := GeminiSchemaProperty{}
        if t, ok := propMap["type"].(string); ok {
            prop.Type = t
        } else {
            return nil, fmt.Errorf("parameter '%s' missing 'type'", name)
        }
        if d, ok := propMap["description"].(string); ok {
            prop.Description = d
        }
        // Handle 'required' status if available in your source structure
        // if req, ok := propMap["required"].(bool); ok && req {
        //     required = append(required, name)
        // }

        geminiProps[name] = prop
    }

    schema := &GeminiSchema{
        Type:       "object", // Standard for function parameters
        Properties: geminiProps,
        Required:   required, // Assign collected required fields
    }

     if len(schema.Properties) == 0 {
         // If there are no properties, Gemini might prefer nil parameters
         return nil, nil
     }


    return schema, nil
}